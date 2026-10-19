package oauth

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// reservedClaims cannot be bound through AuthorizeClaims: the provider owns them.
var reservedClaims = map[string]bool{
	"iss": true, "sub": true, "aud": true, "exp": true, "nbf": true, "iat": true,
	"jti": true, "client_id": true, "scope": true, "auth_time": true, "nonce": true,
}

type compiledAuthorizeClaim struct {
	AuthorizeClaim
	path    []string
	pattern *regexp.Regexp
}

func compileAuthorizeClaims(claims []AuthorizeClaim) ([]compiledAuthorizeClaim, error) {
	compiled := make([]compiledAuthorizeClaim, 0, len(claims))
	for i, c := range claims {
		if c.Param == "" {
			return nil, fmt.Errorf("authorizeClaims[%d]: param is required", i)
		}
		path := strings.Split(c.Claim, ".")
		for _, segment := range path {
			if segment == "" {
				return nil, fmt.Errorf("authorizeClaims[%d]: invalid claim path %q", i, c.Claim)
			}
		}
		if reservedClaims[path[0]] {
			return nil, fmt.Errorf("authorizeClaims[%d]: claim %q is reserved", i, path[0])
		}
		entry := compiledAuthorizeClaim{AuthorizeClaim: c, path: path}
		if c.Pattern != "" {
			// Anchor so the whole value must match, whatever the pattern's own anchors.
			re, err := regexp.Compile(`^(?:` + c.Pattern + `)$`)
			if err != nil {
				return nil, fmt.Errorf("authorizeClaims[%d]: invalid pattern: %w", i, err)
			}
			entry.pattern = re
		}
		for _, other := range compiled {
			if isPathPrefix(other.path, path) || isPathPrefix(path, other.path) {
				return nil, fmt.Errorf("authorizeClaims[%d]: claim %q conflicts with %q", i, c.Claim, other.Claim)
			}
		}
		compiled = append(compiled, entry)
	}
	return compiled, nil
}

func isPathPrefix(prefix, path []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if prefix[i] != path[i] {
			return false
		}
	}
	return true
}

// BindAuthorizeClaims builds the claims bound to an authorization code from the
// /authorize parameters. It returns a non-empty message when a parameter is
// missing, repeated or does not match its pattern.
func (p *Provider) BindAuthorizeClaims(params url.Values) (map[string]interface{}, string) {
	if len(p.authorizeClaims) == 0 {
		return nil, ""
	}
	bound := map[string]interface{}{}
	for _, c := range p.authorizeClaims {
		values, present := params[c.Param]
		if !present || (len(values) == 1 && values[0] == "") {
			if c.Required {
				return nil, fmt.Sprintf("%s is required", c.Param)
			}
			continue
		}
		if len(values) != 1 {
			return nil, fmt.Sprintf("%s must be given once", c.Param)
		}
		if c.pattern != nil && !c.pattern.MatchString(values[0]) {
			return nil, fmt.Sprintf("%s is invalid", c.Param)
		}
		setClaimPath(bound, c.path, values[0])
	}
	if len(bound) == 0 {
		return nil, ""
	}
	return bound, ""
}

func setClaimPath(m map[string]interface{}, path []string, value interface{}) {
	for _, segment := range path[:len(path)-1] {
		next, ok := m[segment].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			m[segment] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

// accessClaims assembles access-token claims: bound claims first, then the
// provider-owned subject, client and scope (which bound claims cannot override).
func accessClaims(bound map[string]interface{}, subject, clientID, scope string) map[string]interface{} {
	claims := make(map[string]interface{}, len(bound)+3)
	for k, v := range bound {
		claims[k] = v
	}
	claims["sub"] = subject
	claims["client_id"] = clientID
	claims["scope"] = scope
	return claims
}
