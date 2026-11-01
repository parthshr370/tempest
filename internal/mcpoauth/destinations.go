package mcpoauth

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The MCP server is the operator's choice, but every other URL in OAuth
// discovery (resource_metadata, authorization_servers, the endpoints in
// authorization server metadata) is named by that server. A server must not
// be able to make Tempest fetch an internal service with them, so requests
// to any other origin may reach only public addresses and addresses in the
// network the configured server itself answered from (MCP security best
// practices, "Server-Side Request Forgery"; RFC 9728 section 7.7).
//
// That network is the class of the address the server's challenge arrived
// from over a direct connection (Challenge.Peer). It is fixed for one login
// and stored with the tokens for refresh. The server's host name is never
// resolved for it: a later DNS answer is the server's own to choose, so it
// could grant the server any network it liked.
//
// The check runs on the address actually connected to: at dial time for
// direct connections, and through an environment proxy by resolving the
// host once, checking every answer and asking the proxy to tunnel to the
// checked address itself, with TLS still verifying the host name. Neither
// DNS rebinding, the proxy's own resolver nor address spellings slip past.

// netClass is the network an address belongs to.
type netClass uint8

const (
	classPublic netClass = iota
	classLoopback
	classPrivate
	classLinkLocal
	classReserved
)

func (c netClass) String() string {
	return [...]string{"public", "loopback", "private", "link-local", "reserved"}[c]
}

var (
	sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")
	nat64              = netip.MustParsePrefix("64:ff9b::/96")
	reservedPrefixes   = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("fec0::/10"),
	}
)

func classify(addr netip.Addr) netClass {
	addr = addr.Unmap().WithZone("")
	if nat64.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	switch {
	case addr.IsLoopback():
		return classLoopback
	case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		return classLinkLocal
	case addr.IsPrivate() || sharedAddressSpace.Contains(addr):
		return classPrivate
	case !addr.IsGlobalUnicast():
		return classReserved
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(addr) {
			return classReserved
		}
	}
	return classPublic
}

// loopbackHost reports whether a URL host names this machine.
func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && classify(addr) == classLoopback
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[strings.ToLower(u.Scheme)]
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// destinations applies the policy to one configured server's OAuth traffic.
type destinations struct {
	server   string
	origin   string
	loopback bool
	// base is the environment's transport: its proxy choice and TLS trust.
	base *http.Transport
	// lookup resolves a discovered host that a proxy will tunnel to.
	lookup func(context.Context, string) ([]netip.Addr, error)
}

func newDestinations(server string, mcpURL *url.URL, base *http.Transport) *destinations {
	return &destinations{server: server, origin: origin(mcpURL), loopback: loopbackHost(mcpURL.Hostname()), base: base,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}}
}

// serverNetwork is the network beyond public that discovery may reach for
// a server that answered from peer. A zero peer, whose connection a proxy
// carried, allows nothing beyond public.
func serverNetwork(peer netip.Addr) netClass {
	if !peer.IsValid() {
		return classPublic
	}
	if class := classify(peer); class != classReserved {
		return class
	}
	return classPublic
}

// parseNetClass reads a stored netClass name; any other name is public.
func parseNetClass(name string) netClass {
	for class := classLoopback; class < classReserved; class++ {
		if class.String() == name {
			return class
		}
	}
	return classPublic
}

type (
	networkKey struct{}
	proxyKey   struct{}
)

// within carries the configured server's network for requests made with ctx.
func within(ctx context.Context, network netClass) context.Context {
	return context.WithValue(ctx, networkKey{}, network)
}

// peer is the address a response to r arrived from over remote, or zero
// when a proxy carried it and the server's own address is unknown.
func (d *destinations) peer(r *http.Request, remote net.Addr) netip.Addr {
	if remote == nil {
		return netip.Addr{}
	}
	if d.base.Proxy != nil {
		if proxy, err := d.base.Proxy(r); err != nil || proxy != nil {
			return netip.Addr{}
		}
	}
	addrPort, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return netip.Addr{}
	}
	return addrPort.Addr()
}

const (
	// dialTimeout bounds connecting to a destination or a proxy, and the
	// proxy's answer to CONNECT.
	dialTimeout = 10 * time.Second
	keepAlive   = 30 * time.Second
	// maxHeaders bounds response headers, a proxy's CONNECT answer included.
	maxHeaders = 64 << 10
)

// refusedError is a destination the policy does not let discovery reach.
type refusedError struct{ error }

// secure enforces the HTTPS rule for discovered URLs: OAuth 2.1 section 1.5
// allows plain HTTP only on loopback, and only a loopback MCP server may
// send Tempest to one.
func (d *destinations) secure(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return refusedError{fmt.Errorf("invalid endpoint %q", raw)}
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && d.loopback && loopbackHost(u.Hostname()):
		return nil
	case u.Scheme == "http" && loopbackHost(u.Hostname()):
		return refusedError{fmt.Errorf("endpoint %q uses plain HTTP on loopback, which only a loopback MCP server may name", raw)}
	}
	return refusedError{fmt.Errorf("endpoint %q must use HTTPS", raw)}
}

func (d *destinations) check(ctx context.Context, addr netip.Addr) error {
	class := classify(addr)
	own, _ := ctx.Value(networkKey{}).(netClass)
	if class == classPublic || (class != classReserved && class == own) {
		return nil
	}
	return refusedError{fmt.Errorf("mcp_auth: refusing to reach %s, a %s address named by server %s's OAuth metadata; only public addresses and the server's own network are allowed", addr, class, d.server)}
}

// transport sends requests for the configured origin unchecked and checks
// every other destination's address when connecting to it.
func (d *destinations) transport() http.RoundTripper {
	own := d.base
	own.MaxResponseHeaderBytes = maxHeaders
	guarded := own.Clone()
	guarded.Proxy = nil
	guarded.DialContext = d.dial
	// A connection was checked for the server network of the request that
	// made it, so no later request, which may carry another, reuses it.
	guarded.DisableKeepAlives = true
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if origin(r.URL) == d.origin {
			return own.RoundTrip(r)
		}
		if own.Proxy == nil {
			return guarded.RoundTrip(r)
		}
		proxy, err := own.Proxy(r)
		if err != nil {
			return nil, err
		}
		if proxy == nil {
			return guarded.RoundTrip(r)
		}
		if r.URL.Scheme != "https" {
			return nil, refusedError{fmt.Errorf("mcp_auth: refusing to send plain HTTP to %s through proxy %s", r.URL.Host, proxy.Host)}
		}
		return guarded.RoundTrip(r.WithContext(context.WithValue(r.Context(), proxyKey{}, proxy)))
	})
}

func (d *destinations) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if proxy, ok := ctx.Value(proxyKey{}).(*url.URL); ok {
		return d.tunnel(ctx, proxy, address)
	}
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive, Control: func(_, address string, _ syscall.RawConn) error {
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return d.check(ctx, addrPort.Addr())
	}}
	return dialer.DialContext(ctx, network, address)
}

// tunnel connects to address through proxy. Given a host name, the proxy
// would resolve it again, perhaps to another address, so Tempest resolves
// it, checks every answer and asks the proxy for a checked address. The
// transport then runs TLS over the tunnel for the original host name.
func (d *destinations) tunnel(ctx context.Context, proxy *url.URL, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{addr}
	} else if addrs, err = d.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("mcp_auth: %s has no address", host)
	}
	for _, addr := range addrs {
		if err := d.check(ctx, addr); err != nil {
			return nil, err
		}
	}
	var first error
	for _, addr := range addrs {
		conn, err := d.connect(ctx, proxy, net.JoinHostPort(addr.Unmap().WithZone("").String(), port), host)
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

// connect opens a CONNECT tunnel to target, an IP address and port, through
// an http or https proxy, as net/http would for the host name.
func (d *destinations) connect(ctx context.Context, proxy *url.URL, target, host string) (net.Conn, error) {
	port := proxy.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[proxy.Scheme]
	}
	if proxy.Scheme != "http" && proxy.Scheme != "https" {
		return nil, fmt.Errorf("mcp_auth: a %s proxy cannot be asked to reach the checked address of %s; OAuth discovery through a proxy needs an http or https proxy", proxy.Scheme, host)
	}
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(dialTimeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	raw.SetDeadline(deadline)
	// Cancelling ctx expires the deadline, which ends any read or write below.
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Unix(1, 0)) })
	fail := func(err error) (net.Conn, error) {
		stop()
		raw.Close()
		return nil, err
	}
	tunnel := raw
	if proxy.Scheme == "https" {
		config := d.base.TLSClientConfig.Clone()
		if config == nil {
			config = &tls.Config{}
		}
		config.ServerName, config.NextProtos = proxy.Hostname(), nil
		secure := tls.Client(raw, config)
		if err := secure.Handshake(); err != nil {
			return fail(fmt.Errorf("mcp_auth: proxy %s TLS: %w", proxy.Host, err))
		}
		tunnel = secure
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if user := proxy.User; user != nil {
		password, _ := user.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err := request.Write(tunnel); err != nil {
		return fail(err)
	}
	reader := bufio.NewReader(io.LimitReader(tunnel, maxHeaders))
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return fail(fmt.Errorf("mcp_auth: proxy %s answering CONNECT %s: %w", proxy.Host, target, err))
	}
	if response.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("mcp_auth: proxy %s refused to tunnel to %s, the checked address of %s (HTTP %d); OAuth discovery reaches it through a proxy only at an address Tempest checked", proxy.Host, target, host, response.StatusCode))
	}
	if reader.Buffered() > 0 {
		return fail(errors.New("mcp_auth: proxy " + proxy.Host + " sent data before the tunnel's TLS handshake"))
	}
	if !stop() {
		raw.Close()
		return nil, ctx.Err()
	}
	raw.SetDeadline(time.Time{})
	return tunnel, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
