package engine

import (
	"errors"
	"net/http"
	"strings"

	"github.com/getmockd/mockd/pkg/mock"
	"github.com/getmockd/mockd/pkg/oauth"
)

var (
	defaultUnauthenticated = mock.AuthFailure{Status: http.StatusUnauthorized, Body: `{"error":"unauthorized"}`}
	defaultForbidden       = mock.AuthFailure{Status: http.StatusForbidden, Body: `{"error":"forbidden"}`}

	errAuthUnauthenticated = errors.New("unauthenticated")
	errAuthForbidden       = errors.New("forbidden")
)

// getOAuthProvider returns the provider registered by an OAuth mock id.
func (h *Handler) getOAuthProvider(id string) *oauth.Provider {
	h.oauthMu.RLock()
	defer h.oauthMu.RUnlock()
	return h.oauthProviders[id]
}

// authorizeHTTP evaluates a mock's auth guard. It returns 0 when the request
// may proceed, otherwise the status of the rejection it has written.
func (h *Handler) authorizeHTTP(w http.ResponseWriter, r *http.Request, m *mock.Mock, pathParams map[string]string) int {
	guard := m.HTTP.Auth
	provider := h.getOAuthProvider(guard.Bearer.Provider)
	if provider == nil {
		// Fail closed: a guard that cannot be evaluated never admits a request.
		h.log.Error("auth guard references an unregistered OAuth provider", "mock", m.ID, "provider", guard.Bearer.Provider)
		return writeAuthFailure(w, mock.AuthFailure{Status: http.StatusInternalServerError, Body: `{"error":"auth provider not registered"}`}, false)
	}

	err := checkBearer(r, provider, guard.Bearer, pathParams)
	if err == nil {
		return 0
	}
	if guard.IsWarnOnly() {
		h.log.Warn("auth guard violation (warn mode)", "mock", m.ID, "reason", err.Error())
		return 0
	}
	if errors.Is(err, errAuthForbidden) {
		return writeAuthFailure(w, merged(guard.OnForbidden, defaultForbidden), false)
	}
	return writeAuthFailure(w, merged(guard.OnUnauthenticated, defaultUnauthenticated), true)
}

func checkBearer(r *http.Request, provider *oauth.Provider, bearer *mock.BearerAuth, pathParams map[string]string) error {
	header := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return errAuthUnauthenticated
	}
	claims, err := provider.ValidateToken(token)
	if err != nil {
		return errAuthUnauthenticated
	}
	if bearer.Audience != "" && !hasAudience(claims["aud"], bearer.Audience) {
		return errAuthUnauthenticated
	}
	for _, req := range bearer.Claims {
		want := req.Equals
		if req.EqualsPathParam != "" {
			want = pathParams[req.EqualsPathParam]
			if want == "" {
				return errAuthForbidden // misconfigured or absent parameter: fail closed
			}
		}
		got, ok := claimAt(claims, req.Claim).(string)
		if !ok || got != want {
			return errAuthForbidden
		}
	}
	return nil
}

func claimAt(claims map[string]interface{}, path string) interface{} {
	var current interface{} = claims
	for _, segment := range strings.Split(path, ".") {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current = m[segment]
	}
	return current
}

func hasAudience(aud interface{}, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []interface{}:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func merged(configured *mock.AuthFailure, fallback mock.AuthFailure) mock.AuthFailure {
	if configured == nil {
		return fallback
	}
	out := *configured
	if out.Status == 0 {
		out.Status = fallback.Status
	}
	if out.Body == "" && len(out.Headers) == 0 {
		out.Body = fallback.Body
	}
	return out
}

func writeAuthFailure(w http.ResponseWriter, f mock.AuthFailure, challenge bool) int {
	if challenge && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	if _, ok := f.Headers["Content-Type"]; !ok {
		w.Header().Set("Content-Type", "application/json")
	}
	for k, v := range f.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(f.Status)
	if f.Body != "" {
		_, _ = w.Write([]byte(f.Body))
	}
	return f.Status
}
