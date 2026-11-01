// Package mcpoauth authorizes the MCP client to HTTP MCP servers with OAuth
// 2.1 as the MCP 2025-11-25 authorization specification requires: protected
// resource metadata (RFC 9728), authorization server metadata (RFC 8414 or
// OpenID Connect discovery), dynamic client registration (RFC 7591) or a
// pre-registered client, PKCE with S256, resource indicators (RFC 8707), a
// loopback redirect, and refresh-token rotation. Tokens live in an owner-only
// credential store, one file per configured server.
package mcpoauth

import "strings"

// Challenge is the Bearer challenge from a 401 or 403 WWW-Authenticate header
// (RFC 6750 section 3, RFC 9728 section 5.1).
type Challenge struct {
	ResourceMetadata string
	Scope            string
	Error            string
	Description      string
}

// ParseBearer returns the first Bearer challenge among the header values.
func ParseBearer(values []string) (Challenge, bool) {
	for _, value := range values {
		p := challengeParser{text: value}
		for {
			scheme, params, ok := p.next()
			if !ok {
				break
			}
			if strings.EqualFold(scheme, "Bearer") {
				return Challenge{ResourceMetadata: params["resource_metadata"], Scope: params["scope"], Error: params["error"], Description: params["error_description"]}, true
			}
		}
	}
	return Challenge{}, false
}

// challengeParser walks RFC 9110 challenges: a scheme token followed by
// comma-separated auth-params whose values are tokens or quoted strings.
type challengeParser struct {
	text string
	at   int
}

func (p *challengeParser) next() (string, map[string]string, bool) {
	p.skip(" \t,")
	scheme := p.token()
	if scheme == "" {
		return "", nil, false
	}
	params := map[string]string{}
	for {
		p.skip(" \t")
		start := p.at
		name := p.token()
		if name == "" {
			// token68 or garbage: skip to the next comma.
			for p.at < len(p.text) && p.text[p.at] != ',' {
				p.at++
			}
			if p.at >= len(p.text) {
				return scheme, params, true
			}
			p.at++
			continue
		}
		p.skip(" \t")
		if p.at >= len(p.text) || p.text[p.at] != '=' {
			// A bare token starts the next challenge.
			p.at = start
			return scheme, params, true
		}
		p.at++
		p.skip(" \t")
		value := ""
		if p.at < len(p.text) && p.text[p.at] == '"' {
			value = p.quoted()
		} else {
			value = p.token()
		}
		params[strings.ToLower(name)] = value
		p.skip(" \t")
		if p.at >= len(p.text) {
			return scheme, params, true
		}
		if p.text[p.at] == ',' {
			p.at++
		}
	}
}

func (p *challengeParser) skip(set string) {
	for p.at < len(p.text) && strings.IndexByte(set, p.text[p.at]) >= 0 {
		p.at++
	}
}

func (p *challengeParser) token() string {
	start := p.at
	for p.at < len(p.text) && strings.IndexByte(" \t,=\"", p.text[p.at]) < 0 {
		p.at++
	}
	return p.text[start:p.at]
}

func (p *challengeParser) quoted() string {
	p.at++
	var out strings.Builder
	for p.at < len(p.text) {
		c := p.text[p.at]
		p.at++
		switch {
		case c == '\\' && p.at < len(p.text):
			out.WriteByte(p.text[p.at])
			p.at++
		case c == '"':
			return out.String()
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
