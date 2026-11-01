package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const maxMetadata = 1 << 20

// ResourceMetadata is RFC 9728 protected resource metadata.
type ResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// ServerMetadata is RFC 8414 (or OpenID Connect) authorization server metadata.
type ServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// canonical is the RFC 8707 resource form of an MCP URL: lowercase scheme
// and host, no default port, no fragment, no trailing slash.
func canonical(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("invalid resource URL %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("resource URL %q must be http(s)", raw)
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	out := scheme + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// covers reports whether a protected resource identifier is the MCP URL or
// one of its parent paths on the same origin.
func covers(resource, target string) bool {
	r, err := canonical(resource)
	if err != nil {
		return false
	}
	t, err := canonical(target)
	if err != nil {
		return false
	}
	t, _, _ = strings.Cut(t, "?")
	r, _, _ = strings.Cut(r, "?")
	return t == r || strings.HasPrefix(t, r+"/")
}

// secureEndpoint enforces the specification's HTTPS rule for authorization
// endpoints; loopback HTTP stays allowed for local servers and tests.
func secureEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("invalid endpoint %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); u.Scheme == "http" && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("endpoint %q must use HTTPS", raw)
}

// wellKnown inserts a well-known suffix before an escaped URL path (RFC 8615).
func wellKnown(base *url.URL, suffix, escapedPath string) string {
	return base.Scheme + "://" + base.Host + "/.well-known/" + suffix + escapedPath
}

// resourceMetadata follows the challenge's resource_metadata URL, or probes
// the path-inserted and then the root well-known URI.
func (c *Client) resourceMetadata(ctx context.Context, challenge Challenge) (ResourceMetadata, error) {
	var candidates []string
	if challenge.ResourceMetadata != "" {
		candidates = []string{challenge.ResourceMetadata}
	} else {
		u, err := url.Parse(c.url)
		if err != nil {
			return ResourceMetadata{}, err
		}
		if path := strings.TrimSuffix(u.EscapedPath(), "/"); path != "" {
			candidates = append(candidates, wellKnown(u, "oauth-protected-resource", path))
		}
		candidates = append(candidates, wellKnown(u, "oauth-protected-resource", ""))
	}
	var metadata ResourceMetadata
	found, err := c.firstJSON(ctx, candidates, &metadata)
	if err != nil {
		return metadata, err
	}
	if !found {
		return metadata, fmt.Errorf("server %s publishes no OAuth protected resource metadata", c.server)
	}
	if metadata.Resource == "" || len(metadata.AuthorizationServers) == 0 {
		return metadata, fmt.Errorf("server %s protected resource metadata lacks resource or authorization_servers", c.server)
	}
	if !covers(metadata.Resource, c.url) {
		return metadata, fmt.Errorf("server %s protected resource metadata names resource %q, which does not cover %s", c.server, metadata.Resource, c.url)
	}
	return metadata, nil
}

// serverMetadata tries the RFC 8414 and OpenID Connect locations in the
// order the specification lists, and requires PKCE S256 support.
func (c *Client) serverMetadata(ctx context.Context, issuer string) (ServerMetadata, error) {
	if err := secureEndpoint(issuer); err != nil {
		return ServerMetadata{}, err
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return ServerMetadata{}, err
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	candidates := []string{wellKnown(u, "oauth-authorization-server", path), wellKnown(u, "openid-configuration", path)}
	if path != "" {
		candidates = append(candidates, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration")
	}
	var metadata ServerMetadata
	found, err := c.firstJSON(ctx, candidates, &metadata)
	if err != nil {
		return metadata, err
	}
	if !found {
		return metadata, fmt.Errorf("authorization server %s publishes no metadata", issuer)
	}
	if strings.TrimSuffix(metadata.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return metadata, fmt.Errorf("authorization server metadata issuer %q does not match %q", metadata.Issuer, issuer)
	}
	if !slices.Contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return metadata, fmt.Errorf("authorization server %s does not advertise PKCE S256; refusing to authorize", issuer)
	}
	for _, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint} {
		if endpoint == "" {
			return metadata, fmt.Errorf("authorization server %s metadata lacks authorization or token endpoint", issuer)
		}
		if err := secureEndpoint(endpoint); err != nil {
			return metadata, err
		}
	}
	if metadata.RegistrationEndpoint != "" {
		if err := secureEndpoint(metadata.RegistrationEndpoint); err != nil {
			return metadata, err
		}
	}
	return metadata, nil
}

// firstJSON GETs each candidate until one answers 200 with a JSON object.
// Other statuses move to the next candidate; transport failures stop.
func (c *Client) firstJSON(ctx context.Context, candidates []string, dst any) (bool, error) {
	for _, candidate := range candidates {
		if err := secureEndpoint(candidate); err != nil {
			return false, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate, nil)
		if err != nil {
			return false, err
		}
		request.Header.Set("Accept", "application/json")
		response, err := c.http.Do(request)
		if err != nil {
			return false, fmt.Errorf("fetching %s: %w", candidate, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxMetadata+1))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		if len(data) > maxMetadata {
			return false, fmt.Errorf("metadata at %s exceeds 1 MiB", candidate)
		}
		if err := json.Unmarshal(data, dst); err != nil {
			return false, fmt.Errorf("metadata at %s is not valid JSON: %w", candidate, err)
		}
		return true, nil
	}
	return false, nil
}

// postForm sends an OAuth form request and decodes a JSON object reply.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, basic [2]string, dst any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if basic[0] != "" {
		request.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	return c.doJSON(request, dst)
}

func (c *Client) doJSON(request *http.Request, dst any) (int, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadata+1))
	if err != nil {
		return response.StatusCode, err
	}
	if len(data) > maxMetadata {
		return response.StatusCode, errors.New("authorization server response exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return response.StatusCode, fmt.Errorf("authorization server answered HTTP %d without a JSON object", response.StatusCode)
	}
	return response.StatusCode, nil
}
