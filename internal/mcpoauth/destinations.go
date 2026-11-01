package mcpoauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The MCP server is the operator's choice, but every other URL in OAuth
// discovery (resource_metadata, authorization_servers, the endpoints in
// authorization server metadata) is named by that server. A server must not
// be able to make Tempest fetch an internal service with them, so requests
// to any other origin may reach only public addresses and addresses of the
// network class the configured server itself lives in (MCP security best
// practices, "Server-Side Request Forgery"; RFC 9728 section 7.7). The
// check runs on the address actually dialed, so DNS rebinding and address
// spellings cannot slip past it.

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
	host     string
	origin   string
	loopback bool
	lookup   func(context.Context, string) ([]netip.Addr, error)

	mu       sync.Mutex
	resolved bool
	classes  map[netClass]bool
}

func newDestinations(server string, mcpURL *url.URL) *destinations {
	return &destinations{server: server, host: mcpURL.Hostname(), origin: origin(mcpURL), loopback: loopbackHost(mcpURL.Hostname()),
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}}
}

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

// allowed returns the network classes beyond public that discovered URLs
// may reach: those of the configured server's own addresses.
func (d *destinations) allowed(ctx context.Context) map[netClass]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resolved {
		return d.classes
	}
	classes := map[netClass]bool{}
	var addrs []netip.Addr
	if d.loopback {
		classes[classLoopback] = true
	} else if addr, err := netip.ParseAddr(d.host); err == nil {
		addrs = []netip.Addr{addr}
	} else if found, err := d.lookup(ctx, d.host); err == nil {
		addrs = found
	} else {
		// Unresolvable now: allow public only, and try again next time.
		return classes
	}
	for _, addr := range addrs {
		if class := classify(addr); class != classReserved {
			classes[class] = true
		}
	}
	d.resolved, d.classes = true, classes
	return classes
}

func (d *destinations) check(ctx context.Context, addr netip.Addr) error {
	class := classify(addr)
	if class == classPublic || (class != classReserved && d.allowed(ctx)[class]) {
		return nil
	}
	return refusedError{fmt.Errorf("mcp_auth: refusing to reach %s, a %s address named by server %s's OAuth metadata; only public addresses and the server's own network are allowed", addr, class, d.server)}
}

// transport sends requests for the configured origin unchecked and checks
// every other destination: at dial time for direct connections, and by
// resolving the host first when an environment proxy makes the connection.
func (d *destinations) transport() http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxResponseHeaderBytes = 64 << 10
	own := base
	guarded := base.Clone()
	guarded.Proxy = nil
	guarded.DialContext = d.dial
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if origin(r.URL) == d.origin {
			return own.RoundTrip(r)
		}
		proxy, err := own.Proxy(r)
		if err != nil {
			return nil, err
		}
		if proxy == nil {
			return guarded.RoundTrip(r)
		}
		addrs := []netip.Addr{}
		if addr, err := netip.ParseAddr(r.URL.Hostname()); err == nil {
			addrs = append(addrs, addr)
		} else if addrs, err = d.lookup(r.Context(), r.URL.Hostname()); err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if err := d.check(r.Context(), addr); err != nil {
				return nil, err
			}
		}
		return own.RoundTrip(r)
	})
}

func (d *destinations) dial(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return d.check(ctx, addrPort.Addr())
	}}
	return dialer.DialContext(ctx, network, address)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
