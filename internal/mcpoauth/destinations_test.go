package mcpoauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestClassifySeesThroughAddressSpellings(t *testing.T) {
	for addr, want := range map[string]netClass{
		"127.0.0.1": classLoopback, "::1": classLoopback, "::ffff:127.0.0.1": classLoopback,
		"169.254.169.254": classLinkLocal, "64:ff9b::a9fe:a9fe": classLinkLocal, "fe80::1%en0": classLinkLocal,
		"10.1.2.3": classPrivate, "192.168.0.1": classPrivate, "100.64.0.1": classPrivate, "fd00::1": classPrivate,
		"0.0.0.0": classReserved, "0.1.2.3": classReserved, "::": classReserved, "239.1.1.1": classReserved, "240.0.0.1": classReserved,
		"8.8.8.8": classPublic, "2606:4700::1111": classPublic,
	} {
		if got := classify(netip.MustParseAddr(addr)); got != want {
			t.Errorf("classify(%s) = %s, want %s", addr, got, want)
		}
	}
}

func testDestinations(t *testing.T, server string) *destinations {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	return newDestinations("srv", u, http.DefaultTransport.(*http.Transport).Clone())
}

func TestSecureAllowsPlainHTTPOnlyFromLoopbackServerToLoopback(t *testing.T) {
	remote := testDestinations(t, "https://mcp.example/mcp")
	for endpoint, ok := range map[string]bool{
		"https://auth.example/": true, "https://10.0.0.1/": true,
		"http://127.0.0.1:8080/": false, "http://localhost/": false, "http://auth.example/": false, "https://user@auth.example/": false,
	} {
		if err := remote.secure(endpoint); (err == nil) != ok {
			t.Errorf("remote server, %s: %v", endpoint, err)
		}
	}
	local := testDestinations(t, "http://127.0.0.1:3000/mcp")
	for endpoint, ok := range map[string]bool{"http://localhost:9000/": true, "http://[::1]:9000/": true, "http://auth.example/": false} {
		if err := local.secure(endpoint); (err == nil) != ok {
			t.Errorf("loopback server, %s: %v", endpoint, err)
		}
	}
}

func TestCheckAllowsPublicAndTheServersOwnNetworkOnly(t *testing.T) {
	cases := []struct {
		peer  string
		allow map[string]bool
	}{
		{"93.184.216.34", map[string]bool{"8.8.8.8": true, "10.0.0.1": false, "127.0.0.1": false, "::ffff:127.0.0.1": false, "169.254.169.254": false, "0.0.0.0": false}},
		{"10.0.0.5", map[string]bool{"8.8.8.8": true, "10.9.9.9": true, "127.0.0.1": false, "169.254.169.254": false}},
		{"127.0.0.1", map[string]bool{"127.0.0.1": true, "::1": true, "10.0.0.1": false}},
		// A reserved peer or none at all (a proxy carried the challenge)
		// grants nothing beyond public.
		{"0.0.0.0", map[string]bool{"8.8.8.8": true, "0.0.0.0": false, "10.0.0.1": false}},
		{"", map[string]bool{"8.8.8.8": true, "127.0.0.1": false, "10.0.0.1": false, "169.254.169.254": false}},
	}
	d := testDestinations(t, "https://mcp.example/mcp")
	for _, c := range cases {
		var peer netip.Addr
		if c.peer != "" {
			peer = netip.MustParseAddr(c.peer)
		}
		ctx := within(t.Context(), serverNetwork(peer))
		for addr, ok := range c.allow {
			err := d.check(ctx, netip.MustParseAddr(addr))
			if (err == nil) != ok {
				t.Errorf("server at %q, destination %s: %v", c.peer, addr, err)
			}
			if err != nil && !strings.Contains(err.Error(), "refusing to reach") {
				t.Errorf("unexpected refusal text: %v", err)
			}
		}
	}
	for _, class := range []netClass{classPublic, classLoopback, classPrivate, classLinkLocal} {
		if got := parseNetClass(class.String()); got != class {
			t.Errorf("stored %s reads back as %s", class, got)
		}
	}
	if got := parseNetClass("reserved"); got != classPublic {
		t.Errorf("stored reserved reads back as %s, want public", got)
	}
}

// policyClient builds a client for server whose transports trust target's
// certificate, choose proxy as an environment proxy would, and dial only
// loopback, so an unexpected request to the server's own origin fails fast
// instead of leaving the machine.
func policyClient(t *testing.T, server string, target *httptest.Server, proxy func(*http.Request) (*url.URL, error)) *Client {
	t.Helper()
	c, err := New(mcpconfig.Server{Name: "untrusted", URL: server}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := c.dest.base.Clone()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	base.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if proxy == nil {
		proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	base.Proxy = proxy
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(address); !loopbackHost(host) {
			return nil, fmt.Errorf("test network reaches only loopback, not %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	c.dest.base = base
	c.http.Transport = c.dest.transport()
	return c
}

func metadataServer(t *testing.T, hits *atomic.Int32, seen func(*http.Request)) *httptest.Server {
	t.Helper()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if seen != nil {
			seen(r)
		}
		// The issuer is the fixture itself, whose answer is not issuer
		// metadata, so a login that gets that far stops there.
		io.WriteString(w, `{"resource":"https://mcp.example/mcp","authorization_servers":["https://`+r.Host+`"]}`)
	}))
	t.Cleanup(target.Close)
	return target
}

// A server reached at a public address cannot widen the policy by changing
// its DNS answer before discovery: its network is never looked up again.
func TestServerNetworkIsNotResolvedAgainForDiscovery(t *testing.T) {
	var hits atomic.Int32
	target := metadataServer(t, &hits, nil)
	c := policyClient(t, "https://mcp.example/mcp", target, nil)
	c.dest.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		t.Errorf("looked up %s; the server's network comes from its connection", host)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	_, err := c.Login(t.Context(), Challenge{ResourceMetadata: target.URL + "/internal-metadata"}, nil)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("login = %v after %d requests to the loopback metadata", err, hits.Load())
	}
}

// connectProxy is an HTTP proxy whose own DNS sends example.com to target,
// as a split-horizon resolver would. It records each CONNECT target and
// tunnels only to target, answering 502 for any other address.
func connectProxy(t *testing.T, target *httptest.Server, targets chan<- string, auth chan<- string) *httptest.Server {
	t.Helper()
	targetURL, _ := url.Parse(target.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy got %s, want CONNECT", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		targets <- r.Host
		if auth != nil {
			auth <- r.Header.Get("Proxy-Authorization")
		}
		host, port, _ := net.SplitHostPort(r.Host)
		if port != targetURL.Port() || (host != "example.com" && host != targetURL.Hostname()) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		upstream, err := net.Dial("tcp", targetURL.Host)
		if err != nil {
			t.Error(err)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			t.Error(err)
			return
		}
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		go func() { io.Copy(upstream, conn); upstream.Close() }()
		go func() { io.Copy(conn, upstream); conn.Close() }()
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// Through a proxy, Tempest tunnels to the address it checked, not to a name
// the proxy resolves again, so a split-horizon answer cannot redirect it.
func TestProxyTunnelsToTheCheckedAddressOnly(t *testing.T) {
	var hits atomic.Int32
	target := metadataServer(t, &hits, nil)
	targets := make(chan string, 4)
	proxy := connectProxy(t, target, targets, nil)
	proxyURL, _ := url.Parse(proxy.URL)
	c := policyClient(t, "https://mcp.example/mcp", target, func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	c.dest.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}
	port := target.Listener.Addr().(*net.TCPAddr).Port
	var dst ResourceMetadata
	found, err := c.firstJSON(t.Context(), []string{fmt.Sprintf("https://example.com:%d/internal-metadata", port)}, &dst)
	if err == nil || found || hits.Load() != 0 {
		t.Fatalf("found=%v err=%v; the internal service answered %d times", found, err, hits.Load())
	}
	if got, want := <-targets, fmt.Sprintf("192.0.2.10:%d", port); got != want {
		t.Fatalf("proxy was asked to CONNECT %s, want the checked address %s", got, want)
	}
	if !strings.Contains(err.Error(), "refused to tunnel to 192.0.2.10") {
		t.Fatalf("a proxy refusing the checked address gives %v, want a refusal naming it", err)
	}
}

// A challenge from loopback puts the server on loopback, so its cross-origin
// loopback metadata is in reach without resolving the server's name.
func TestLoginReachesTheNetworkTheChallengeCameFrom(t *testing.T) {
	var hits atomic.Int32
	target := metadataServer(t, &hits, nil)
	c := policyClient(t, "https://mcp.example/mcp", target, nil)
	c.dest.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		t.Errorf("looked up %s", host)
		return nil, errors.New("no lookup expected")
	}
	challenge := Challenge{ResourceMetadata: target.URL + "/metadata", Peer: netip.MustParseAddr("127.0.0.1")}
	_, err := c.Login(t.Context(), challenge, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") || hits.Load() != 2 {
		t.Fatalf("login = %v after %d requests, want resource and issuer metadata both fetched", err, hits.Load())
	}
}

// The tunnel goes to the checked address, while the request still names the
// host and TLS verifies the host's certificate; proxy credentials are sent.
func TestProxyTunnelKeepsTheHostsIdentity(t *testing.T) {
	var hits atomic.Int32
	seen := make(chan *http.Request, 1)
	target := metadataServer(t, &hits, func(r *http.Request) { seen <- r })
	targets, auth := make(chan string, 4), make(chan string, 4)
	proxy := connectProxy(t, target, targets, auth)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("tempest", "pass word")
	c := policyClient(t, "https://mcp.example/mcp", target, func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	c.dest.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	port := target.Listener.Addr().(*net.TCPAddr).Port
	endpoint := fmt.Sprintf("https://example.com:%d/metadata", port)
	var dst ResourceMetadata
	if _, err := c.firstJSON(t.Context(), []string{endpoint}, &dst); err == nil || !strings.Contains(err.Error(), "refusing to reach 127.0.0.1") {
		t.Fatalf("a remote server's loopback destination through the proxy = %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("the proxy was asked for %s, a refused address", <-targets)
	}
	found, err := c.firstJSON(within(t.Context(), classLoopback), []string{endpoint}, &dst)
	if err != nil || !found {
		t.Fatalf("a loopback server's loopback destination through the proxy: found=%v err=%v", found, err)
	}
	if got, want := <-targets, fmt.Sprintf("127.0.0.1:%d", port); got != want {
		t.Fatalf("proxy was asked to CONNECT %s, want %s", got, want)
	}
	if got, want := <-auth, "Basic dGVtcGVzdDpwYXNzIHdvcmQ="; got != want {
		t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
	}
	request := <-seen
	if request.TLS == nil || request.TLS.ServerName != "example.com" || request.Host != fmt.Sprintf("example.com:%d", port) {
		t.Fatalf("the tunnelled request named %q with TLS %+v, want example.com", request.Host, request.TLS)
	}
}
