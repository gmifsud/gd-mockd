package mock

import (
	"fmt"
	"strings"
)

// Auth guard modes.
const (
	AuthModeEnforce = "enforce" // reject failing requests (default)
	AuthModeWarn    = "warn"    // log failures and continue
)

// HTTPAuth guards an HTTP mock with request authentication. It is evaluated
// after matching and before validation, stateful bindings and responses, so a
// rejected request never changes state.
type HTTPAuth struct {
	// Bearer requires an access token issued by a registered OAuth mock.
	Bearer *BearerAuth `json:"bearer,omitempty" yaml:"bearer,omitempty"`
	// OnUnauthenticated is returned for a missing, malformed, invalid, expired
	// or revoked token (default 401).
	OnUnauthenticated *AuthFailure `json:"onUnauthenticated,omitempty" yaml:"onUnauthenticated,omitempty"`
	// OnForbidden is returned for a valid token that fails a claim requirement
	// (default 403).
	OnForbidden *AuthFailure `json:"onForbidden,omitempty" yaml:"onForbidden,omitempty"`
	// Mode is "enforce" (default) or "warn" (log and continue; demos only).
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// BearerAuth validates a bearer JWT against an OAuth mock's signing key,
// issuer, expiry and revocation list, then checks claim requirements.
type BearerAuth struct {
	// Provider is the ID of the OAuth mock that issues accepted tokens.
	Provider string `json:"provider" yaml:"provider"`
	// Audience, when set, must appear in the token's aud claim.
	Audience string `json:"audience,omitempty" yaml:"audience,omitempty"`
	// Claims must all hold; values are compared as strings.
	Claims []ClaimRequirement `json:"claims,omitempty" yaml:"claims,omitempty"`
}

// ClaimRequirement requires a (dotted-path) string claim to equal a literal or
// a path parameter of the matched request. Exactly one of Equals and
// EqualsPathParam must be set.
type ClaimRequirement struct {
	Claim           string `json:"claim" yaml:"claim"`
	Equals          string `json:"equals,omitempty" yaml:"equals,omitempty"`
	EqualsPathParam string `json:"equalsPathParam,omitempty" yaml:"equalsPathParam,omitempty"`
}

// AuthFailure is the response written when the guard rejects a request.
type AuthFailure struct {
	Status  int               `json:"status,omitempty" yaml:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Body    string            `json:"body,omitempty" yaml:"body,omitempty"`
}

// IsWarnOnly reports whether failures are logged instead of rejected.
func (a *HTTPAuth) IsWarnOnly() bool { return a != nil && a.Mode == AuthModeWarn }

// Validate checks the guard's configuration.
func (a *HTTPAuth) Validate() error {
	if a.Bearer == nil {
		return &ValidationError{Field: "http.auth.bearer", Message: "bearer is required"}
	}
	switch a.Mode {
	case "", AuthModeEnforce, AuthModeWarn:
	default:
		return &ValidationError{Field: "http.auth.mode", Message: fmt.Sprintf("mode must be %q or %q", AuthModeEnforce, AuthModeWarn)}
	}
	if strings.TrimSpace(a.Bearer.Provider) == "" {
		return &ValidationError{Field: "http.auth.bearer.provider", Message: "provider (OAuth mock id) is required"}
	}
	for i, c := range a.Bearer.Claims {
		field := fmt.Sprintf("http.auth.bearer.claims[%d]", i)
		for _, segment := range strings.Split(c.Claim, ".") {
			if segment == "" {
				return &ValidationError{Field: field + ".claim", Message: fmt.Sprintf("invalid claim path %q", c.Claim)}
			}
		}
		if (c.Equals == "") == (c.EqualsPathParam == "") {
			return &ValidationError{Field: field, Message: "exactly one of equals or equalsPathParam is required"}
		}
	}
	for name, f := range map[string]*AuthFailure{"onUnauthenticated": a.OnUnauthenticated, "onForbidden": a.OnForbidden} {
		if f != nil && f.Status != 0 && (f.Status < 400 || f.Status > 599) {
			return &ValidationError{Field: "http.auth." + name + ".status", Message: "status must be 4xx or 5xx"}
		}
	}
	return nil
}
