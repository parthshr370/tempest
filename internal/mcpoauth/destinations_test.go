package mcpoauth

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
	"testing"
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

func testDestinations(t *testing.T, server string, addrs ...string) *destinations {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	d := newDestinations("srv", u)
	d.lookup = func(context.Context, string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, addr := range addrs {
			out = append(out, netip.MustParseAddr(addr))
		}
		return out, nil
	}
	return d
}

func TestSecureAllowsPlainHTTPOnlyFromLoopbackServerToLoopback(t *testing.T) {
	remote := testDestinations(t, "https://mcp.example/mcp", "93.184.216.34")
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
		server string
		addrs  []string
		allow  map[string]bool
	}{
		{"https://mcp.example/mcp", []string{"93.184.216.34"}, map[string]bool{"8.8.8.8": true, "10.0.0.1": false, "127.0.0.1": false, "::ffff:127.0.0.1": false, "169.254.169.254": false, "0.0.0.0": false}},
		{"https://mcp.corp/mcp", []string{"10.0.0.5"}, map[string]bool{"8.8.8.8": true, "10.9.9.9": true, "127.0.0.1": false, "169.254.169.254": false}},
		{"http://localhost:3000/mcp", nil, map[string]bool{"127.0.0.1": true, "::1": true, "10.0.0.1": false}},
	}
	for _, c := range cases {
		d := testDestinations(t, c.server, c.addrs...)
		for addr, ok := range c.allow {
			err := d.check(t.Context(), netip.MustParseAddr(addr))
			if (err == nil) != ok {
				t.Errorf("server %s, destination %s: %v", c.server, addr, err)
			}
			if err != nil && !strings.Contains(err.Error(), "refusing to reach") {
				t.Errorf("unexpected refusal text: %v", err)
			}
		}
	}
}
