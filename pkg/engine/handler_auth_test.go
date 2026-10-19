package engine

// Bearer guard tests (G1–G6 in examples/wolt/PRD-price-stock-pilot.md §5.3).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getmockd/mockd/internal/storage"
	"github.com/getmockd/mockd/pkg/mock"
	"github.com/getmockd/mockd/pkg/oauth"
	"github.com/getmockd/mockd/pkg/stateful"
)

type guardFixture struct {
	h        *Handler
	provider *oauth.Provider
	bridge   *stateful.Bridge
	auth     *mock.HTTPAuth
}

func newGuardFixture(t *testing.T, mutate func(*mock.HTTPAuth)) *guardFixture {
	t.Helper()
	cfg := &oauth.OAuthConfig{
		ID:                         "wolt-auth",
		Issuer:                     "http://localhost:4280/oauth2",
		TokenExpiry:                "1h",
		RefreshExpiry:              "30d",
		DefaultScopes:              []string{"offline"},
		RotateRefreshTokens:        true,
		RevokePreviousAccessToken:  true,
		RefreshTokenReuseDetection: true,
		AuthorizeClaims:            []oauth.AuthorizeClaim{{Param: "venue_id", Claim: "integration.venue_id", Required: true}},
		Clients: []oauth.ClientConfig{{ClientID: "c", ClientSecret: "s", RedirectURIs: []string{"http://cb"},
			GrantTypes: []string{oauth.GrantTypeAuthorizationCode, oauth.GrantTypeRefreshToken}}},
		Enabled: true,
	}
	provider, err := oauth.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.Stop)

	state := stateful.NewStateStore()
	if err := state.Register("", &stateful.ResourceConfig{Name: "events", IDField: "id"}); err != nil {
		t.Fatal(err)
	}
	bridge := stateful.NewBridge(state)

	h := NewHandler(storage.NewInMemoryMockStore())
	h.SetStatefulBridge(bridge)
	h.RegisterOAuthHandler(cfg, oauth.NewHandler(provider))

	auth := &mock.HTTPAuth{
		Bearer: &mock.BearerAuth{
			Provider: "wolt-auth",
			Claims:   []mock.ClaimRequirement{{Claim: "integration.venue_id", EqualsPathParam: "venueId"}},
		},
		OnUnauthenticated: &mock.AuthFailure{Status: 401, Body: `{"detail":"Unauthorized"}`},
		OnForbidden:       &mock.AuthFailure{Status: 403, Body: `{"detail":"Forbidden"}`},
	}
	if mutate != nil {
		mutate(auth)
	}
	enabled := true
	m := &mock.Mock{
		ID: "update-items", Type: mock.TypeHTTP, Enabled: &enabled,
		HTTP: &mock.HTTPSpec{
			Matcher:         &mock.HTTPMatcher{Method: http.MethodPost, Path: "/venues/{venueId}/items"},
			Auth:            auth,
			StatefulBinding: &mock.StatefulBinding{Table: "events", Action: "create"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("mock validation: %v", err)
	}
	if err := h.store.Set(m); err != nil {
		t.Fatal(err)
	}
	return &guardFixture{h: h, provider: provider, bridge: bridge, auth: auth}
}

// token runs the real authorize → token flow and returns the token response.
func (f *guardFixture) token(t *testing.T, venue string) map[string]any {
	t.Helper()
	q := url.Values{"client_id": {"c"}, "redirect_uri": {"http://cb"}, "response_type": {"code"}, "venue_id": {venue}}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil))
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize: %d %s", w.Code, w.Header().Get("Location"))
	}
	return f.tokenCall(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://cb"}})
}

func (f *guardFixture) tokenCall(t *testing.T, form url.Values) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth("c", "s")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	body := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusOK {
		t.Fatalf("token: %d %v", w.Code, body)
	}
	return body
}

func (f *guardFixture) patch(venue, authorization string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/venues/"+venue+"/items", strings.NewReader(`{"data":[{"sku":"000123","price":100}]}`))
	r.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *guardFixture) events(t *testing.T) int {
	t.Helper()
	res := f.bridge.Execute(context.Background(), &stateful.OperationRequest{Resource: "events", Action: stateful.ActionList, Filter: &stateful.QueryFilter{Limit: 1000}})
	if res.Error != nil || res.List == nil {
		t.Fatalf("list events: %v", res.Error)
	}
	return len(res.List.Data)
}

func bearer(body map[string]any) string { return "Bearer " + body["access_token"].(string) }

func b64url(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

func TestBearerGuard_Missing_NoMutation(t *testing.T) {
	f := newGuardFixture(t, nil)
	for name, header := range map[string]string{"missing": "", "basic": "Basic YTpi", "empty": "Bearer ", "garbage": "Bearer not.a.jwt", "two-tokens": "Bearer a b"} {
		w := f.patch("venue-a", header)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Unauthorized") || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("G1 %s: %d %q", name, w.Code, w.Body.String())
		}
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("G1: rejected requests mutated state (%d events)", n)
	}
}

func TestBearerGuard_VenueMatch_Passes(t *testing.T) {
	f := newGuardFixture(t, nil)
	w := f.patch("venue-a", bearer(f.token(t, "venue-a")))
	if w.Code >= 400 {
		t.Fatalf("G3: %d %s", w.Code, w.Body.String())
	}
	if n := f.events(t); n != 1 {
		t.Fatalf("G3: want 1 event, got %d", n)
	}
}

func TestBearerGuard_VenueMismatch_403_NoMutation(t *testing.T) {
	f := newGuardFixture(t, nil)
	w := f.patch("venue-b", bearer(f.token(t, "venue-a")))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Forbidden") {
		t.Fatalf("G4: %d %s", w.Code, w.Body.String())
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("G4: venue B mutated (%d events)", n)
	}
}

func TestBearerGuard_Expired(t *testing.T) {
	f := newGuardFixture(t, nil)
	clock := time.Now()
	f.provider.SetClock(func() time.Time { return clock })
	tok := bearer(f.token(t, "venue-a"))
	clock = clock.Add(61 * time.Minute)
	if w := f.patch("venue-a", tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("G2 expired: %d", w.Code)
	}
}

func TestBearerGuard_Tampered(t *testing.T) {
	f := newGuardFixture(t, nil)
	tok := f.token(t, "venue-a")["access_token"].(string)
	parts := strings.Split(tok, ".")
	// Re-point the payload at venue-b, keep the original signature.
	payload := `{"integration":{"venue_id":"venue-b"},"iss":"http://localhost:4280/oauth2","exp":4102444800}`
	parts[1] = strings.TrimRight(b64url(payload), "=")
	if w := f.patch("venue-b", "Bearer "+strings.Join(parts, ".")); w.Code != http.StatusUnauthorized {
		t.Fatalf("G2 tampered: %d", w.Code)
	}
}

func TestBearerGuard_AlgNone(t *testing.T) {
	f := newGuardFixture(t, nil)
	tok := strings.TrimRight(b64url(`{"alg":"none","typ":"JWT"}`), "=") + "." +
		strings.TrimRight(b64url(`{"integration":{"venue_id":"venue-a"},"iss":"http://localhost:4280/oauth2","exp":4102444800}`), "=") + ".mock-signature"
	if w := f.patch("venue-a", "Bearer "+tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("G2 alg=none: %d", w.Code)
	}
}

func TestBearerGuard_WrongIssuer(t *testing.T) {
	f := newGuardFixture(t, nil)
	tok, _ := f.provider.GenerateToken(map[string]interface{}{"iss": "http://elsewhere", "integration": map[string]interface{}{"venue_id": "venue-a"}})
	if w := f.patch("venue-a", "Bearer "+tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("G2 wrong issuer: %d", w.Code)
	}
}

func TestBearerGuard_Revoked(t *testing.T) {
	f := newGuardFixture(t, nil)
	g1 := f.token(t, "venue-a")
	f.tokenCall(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g1["refresh_token"].(string)}})
	if w := f.patch("venue-a", bearer(g1)); w.Code != http.StatusUnauthorized {
		t.Fatalf("G2 revoked (R3): previous access token accepted: %d", w.Code)
	}
}

func TestBearerGuard_RefreshedToken_Passes(t *testing.T) {
	f := newGuardFixture(t, nil)
	g1 := f.token(t, "venue-a")
	g2 := f.tokenCall(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g1["refresh_token"].(string)}})
	if w := f.patch("venue-a", bearer(g2)); w.Code >= 400 {
		t.Fatalf("G5: %d %s", w.Code, w.Body.String())
	}
	if w := f.patch("venue-b", bearer(g2)); w.Code != http.StatusForbidden {
		t.Fatalf("G5: refreshed token must stay bound to venue-a: %d", w.Code)
	}
}

func TestBearerGuard_WarnMode(t *testing.T) {
	f := newGuardFixture(t, func(a *mock.HTTPAuth) { a.Mode = mock.AuthModeWarn })
	if w := f.patch("venue-a", ""); w.Code >= 400 {
		t.Fatalf("G6: warn mode must admit: %d", w.Code)
	}
}

func TestBearerGuard_UnregisteredProvider_FailsClosed(t *testing.T) {
	f := newGuardFixture(t, func(a *mock.HTTPAuth) { a.Bearer.Provider = "nope" })
	if w := f.patch("venue-a", bearer(f.token(t, "venue-a"))); w.Code != http.StatusInternalServerError {
		t.Fatalf("unregistered provider must fail closed: %d", w.Code)
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("fail-closed guard mutated state (%d events)", n)
	}
}

func TestBearerGuard_ProviderUnregisteredWithIssuer(t *testing.T) {
	f := newGuardFixture(t, nil)
	tok := bearer(f.token(t, "venue-a"))
	f.h.UnregisterOAuthHandler("http://localhost:4280/oauth2")
	if w := f.patch("venue-a", tok); w.Code != http.StatusInternalServerError {
		t.Fatalf("guard must fail closed after its provider is removed: %d", w.Code)
	}
}

func TestHTTPAuthValidate(t *testing.T) {
	ok := &mock.HTTPAuth{Bearer: &mock.BearerAuth{Provider: "p", Claims: []mock.ClaimRequirement{{Claim: "a.b", Equals: "x"}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]*mock.HTTPAuth{
		"no-bearer":   {},
		"no-provider": {Bearer: &mock.BearerAuth{}},
		"bad-mode":    {Mode: "maybe", Bearer: &mock.BearerAuth{Provider: "p"}},
		"both-equals": {Bearer: &mock.BearerAuth{Provider: "p", Claims: []mock.ClaimRequirement{{Claim: "a", Equals: "x", EqualsPathParam: "y"}}}},
		"none-equals": {Bearer: &mock.BearerAuth{Provider: "p", Claims: []mock.ClaimRequirement{{Claim: "a"}}}},
		"empty-claim": {Bearer: &mock.BearerAuth{Provider: "p", Claims: []mock.ClaimRequirement{{Claim: "a..b", Equals: "x"}}}},
		"bad-status":  {Bearer: &mock.BearerAuth{Provider: "p"}, OnForbidden: &mock.AuthFailure{Status: 200}},
	} {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
