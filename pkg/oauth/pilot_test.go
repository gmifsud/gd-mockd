package oauth

// Increment A tests for the Wolt price-and-stock pilot.
// Contract IDs (T*, R*) refer to examples/wolt/PRD-price-stock-pilot.md §5.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	pilotClient   = "pilot-client"
	pilotSecret   = "pilot-secret"
	pilotRedirect = "http://localhost/cb"
	pilotIssuer   = "http://localhost:4280/oauth2"
)

func pilotConfig(mutate func(*OAuthConfig)) *OAuthConfig {
	cfg := &OAuthConfig{
		ID:            "pilot",
		Issuer:        pilotIssuer,
		TokenExpiry:   "1h",
		RefreshExpiry: "30d",
		DefaultScopes: []string{"offline"},
		Clients: []ClientConfig{
			{ClientID: pilotClient, ClientSecret: pilotSecret, RedirectURIs: []string{pilotRedirect}, GrantTypes: []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}},
			{ClientID: "other-client", ClientSecret: "other-secret", RedirectURIs: []string{pilotRedirect}, GrantTypes: []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}},
		},
		AuthorizeClaims: []AuthorizeClaim{
			{Param: "venue_id", Claim: "integration.venue_id", Pattern: `^[A-Za-z0-9_-]{1,64}$`, Required: true},
		},
		RotateRefreshTokens:        true,
		RevokePreviousAccessToken:  true,
		RefreshTokenReuseDetection: true,
		Enabled:                    true,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func newPilot(t *testing.T, mutate func(*OAuthConfig)) (*Provider, *Handler) {
	t.Helper()
	p, err := NewProvider(pilotConfig(mutate))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(p.Stop)
	return p, NewHandler(p)
}

// authorize drives GET /authorize and returns the issued code ("" if none) and the redirect.
func authorize(t *testing.T, h *Handler, extra url.Values) (string, *url.URL) {
	t.Helper()
	q := url.Values{"client_id": {pilotClient}, "redirect_uri": {pilotRedirect}, "response_type": {"code"}, "state": {"s"}}
	for k, v := range extra {
		q[k] = v
	}
	w := httptest.NewRecorder()
	h.HandleAuthorize(w, httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil))
	if w.Code != http.StatusFound {
		t.Fatalf("authorize: status %d body %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("authorize: bad Location: %v", err)
	}
	return loc.Query().Get("code"), loc
}

func tokenRequest(h *Handler, client, secret string, form url.Values) (int, map[string]any) {
	r := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(client, secret)
	w := httptest.NewRecorder()
	h.HandleToken(w, r)
	body := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func exchange(h *Handler, code string) (int, map[string]any) {
	return tokenRequest(h, pilotClient, pilotSecret, url.Values{"grant_type": {GrantTypeAuthorizationCode}, "code": {code}, "redirect_uri": {pilotRedirect}})
}

func refreshWith(h *Handler, client, secret, rt string) (int, map[string]any) {
	return tokenRequest(h, client, secret, url.Values{"grant_type": {GrantTypeRefreshToken}, "refresh_token": {rt}})
}

func onboard(t *testing.T, h *Handler, venue string) map[string]any {
	t.Helper()
	code, _ := authorize(t, h, url.Values{"venue_id": {venue}})
	if code == "" {
		t.Fatalf("no code issued for venue %q", venue)
	}
	status, body := exchange(h, code)
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, body)
	}
	return body
}

func str(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	s, ok := body[key].(string)
	if !ok || s == "" {
		t.Fatalf("missing %q in %v", key, body)
	}
	return s
}

func venueOf(t *testing.T, p *Provider, token string) string {
	t.Helper()
	claims, err := p.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	integration, _ := claims["integration"].(map[string]interface{})
	v, _ := integration["venue_id"].(string)
	return v
}

// ── A1: claim binding (T1, T3, T4, R6) ────────────────────────────────────────

func TestAuthorizeClaims_VenueA(t *testing.T) {
	p, h := newPilot(t, nil)
	body := onboard(t, h, "venue-a")
	if got := venueOf(t, p, str(t, body, "access_token")); got != "venue-a" {
		t.Fatalf("integration.venue_id = %q, want venue-a", got)
	}
	if body["token_type"] != "Bearer" || body["expires_in"] != float64(3600) || body["scope"] != "offline" {
		t.Fatalf("T1 response shape: %v", body)
	}
}

func TestAuthorizeClaims_VenueB(t *testing.T) {
	p, h := newPilot(t, nil)
	a := onboard(t, h, "venue-a")
	b := onboard(t, h, "venue-b")
	if venueOf(t, p, str(t, a, "access_token")) != "venue-a" || venueOf(t, p, str(t, b, "access_token")) != "venue-b" {
		t.Fatal("each code must carry its own venue")
	}
}

func TestAuthorizeClaims_MissingOrInvalidParam_Rejected(t *testing.T) {
	_, h := newPilot(t, nil)
	for name, extra := range map[string]url.Values{
		"missing":      nil,
		"empty":        {"venue_id": {""}},
		"bad-chars":    {"venue_id": {"venue a/../b"}},
		"too-long":     {"venue_id": {strings.Repeat("x", 65)}},
		"multi-valued": {"venue_id": {"a", "b"}},
	} {
		t.Run(name, func(t *testing.T) {
			code, loc := authorize(t, h, extra)
			if code != "" || loc.Query().Get("error") != ErrInvalidRequest {
				t.Fatalf("want invalid_request and no code, got code=%q loc=%s", code, loc)
			}
		})
	}
}

func TestAuthorizeClaims_OptionalAbsentParamOmitsClaim(t *testing.T) {
	p, h := newPilot(t, func(c *OAuthConfig) { c.AuthorizeClaims[0].Required = false })
	code, _ := authorize(t, h, nil)
	_, body := exchange(h, code)
	claims, err := p.ValidateToken(str(t, body, "access_token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claims["integration"]; ok {
		t.Fatal("absent optional param must not create the claim")
	}
}

func TestAuthorizeClaims_InvalidConfigRejected(t *testing.T) {
	for name, claim := range map[string]AuthorizeClaim{
		"reserved-sub":        {Param: "x", Claim: "sub"},
		"reserved-exp":        {Param: "x", Claim: "exp"},
		"reserved-iss-nested": {Param: "x", Claim: "iss.y"},
		"empty-param":         {Param: "", Claim: "a"},
		"empty-segment":       {Param: "x", Claim: "a..b"},
		"bad-pattern":         {Param: "x", Claim: "a", Pattern: "("},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewProvider(pilotConfig(func(c *OAuthConfig) { c.AuthorizeClaims = []AuthorizeClaim{claim} }))
			if err == nil {
				t.Fatal("want config error")
			}
		})
	}
	_, err := NewProvider(pilotConfig(func(c *OAuthConfig) {
		c.AuthorizeClaims = append(c.AuthorizeClaims, AuthorizeClaim{Param: "y", Claim: "integration.venue_id"})
	}))
	if err == nil {
		t.Fatal("duplicate claim path must be rejected")
	}
}

func TestRefresh_PreservesBoundClaims(t *testing.T) {
	p, h := newPilot(t, nil)
	first := onboard(t, h, "venue-a")
	status, second := refreshWith(h, pilotClient, pilotSecret, str(t, first, "refresh_token"))
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, second)
	}
	if got := venueOf(t, p, str(t, second, "access_token")); got != "venue-a" {
		t.Fatalf("R6: refreshed venue = %q", got)
	}
}

func TestRefresh_PreservesBoundClaims_WithoutRotation(t *testing.T) {
	p, h := newPilot(t, func(c *OAuthConfig) {
		c.RotateRefreshTokens = false
		c.RevokePreviousAccessToken = false
		c.RefreshTokenReuseDetection = false
	})
	first := onboard(t, h, "venue-a")
	_, second := refreshWith(h, pilotClient, pilotSecret, str(t, first, "refresh_token"))
	if got := venueOf(t, p, str(t, second, "access_token")); got != "venue-a" {
		t.Fatalf("R6 (no rotation): venue = %q", got)
	}
}

func TestRefresh_NewJTIPerGeneration(t *testing.T) {
	p, h := newPilot(t, func(c *OAuthConfig) { c.RevokePreviousAccessToken = false })
	g1 := onboard(t, h, "venue-a")
	_, g2 := refreshWith(h, pilotClient, pilotSecret, str(t, g1, "refresh_token"))
	c1, _ := p.ValidateToken(str(t, g1, "access_token"))
	c2, _ := p.ValidateToken(str(t, g2, "access_token"))
	if c1["jti"] == nil || c1["jti"] == c2["jti"] {
		t.Fatalf("jti must differ per generation: %v vs %v", c1["jti"], c2["jti"])
	}
}

// ── T2, T5, T6 / F8: authorization codes ──────────────────────────────────────

func TestCodeExchange_IssuesBoundPair(t *testing.T) {
	_, h := newPilot(t, nil)
	body := onboard(t, h, "venue-a")
	str(t, body, "refresh_token")
}

func TestCodeExchange_ConsumedCode_InvalidGrant(t *testing.T) {
	_, h := newPilot(t, nil)
	code, _ := authorize(t, h, url.Values{"venue_id": {"venue-a"}})
	if s, _ := exchange(h, code); s != http.StatusOK {
		t.Fatalf("first exchange: %d", s)
	}
	s, body := exchange(h, code)
	if s != http.StatusBadRequest || body["error"] != ErrInvalidGrant || body["access_token"] != nil {
		t.Fatalf("T2: replayed code must fail with invalid_grant: %d %v", s, body)
	}
}

func TestCodeExchange_UnknownCode_InvalidGrant(t *testing.T) {
	_, h := newPilot(t, nil)
	s, body := exchange(h, "not-a-code")
	if s != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Fatalf("T5: %d %v", s, body)
	}
}

func TestCodeExchange_ExpiryConfigurable(t *testing.T) {
	p, h := newPilot(t, func(c *OAuthConfig) { c.AuthorizationCodeExpiry = "1h" })
	clock := time.Now()
	p.SetClock(func() time.Time { return clock })
	code, _ := authorize(t, h, url.Values{"venue_id": {"venue-a"}})
	clock = clock.Add(59 * time.Minute)
	if s, _ := exchange(h, code); s != http.StatusOK {
		t.Fatalf("code must be valid at 59m: %d", s)
	}
	code, _ = authorize(t, h, url.Values{"venue_id": {"venue-a"}})
	clock = clock.Add(61 * time.Minute)
	if s, body := exchange(h, code); s != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Fatalf("code must expire after 1h: %d %v", s, body)
	}
}

func TestCodeExchange_DefaultExpiryUnchanged(t *testing.T) {
	p, _ := newPilot(t, func(c *OAuthConfig) { c.AuthorizationCodeExpiry = "" })
	if p.AuthorizationCodeExpiry() != 10*time.Minute {
		t.Fatalf("default code expiry = %v", p.AuthorizationCodeExpiry())
	}
}

func TestCodeExchange_WrongClientDoesNotConsume(t *testing.T) {
	_, h := newPilot(t, nil)
	code, _ := authorize(t, h, url.Values{"venue_id": {"venue-a"}})
	s, body := tokenRequest(h, "other-client", "other-secret", url.Values{"grant_type": {GrantTypeAuthorizationCode}, "code": {code}, "redirect_uri": {pilotRedirect}})
	if s == http.StatusOK || body["access_token"] != nil {
		t.Fatalf("T6: wrong client must not receive tokens: %d %v", s, body)
	}
	if s, _ := exchange(h, code); s != http.StatusOK {
		t.Fatalf("T6/D6: legitimate client's code was burned by the wrong client: %d", s)
	}
}

// ── A2/A3: rotation (R1–R10) ──────────────────────────────────────────────────

func TestRotate_IssuesNewPair(t *testing.T) {
	_, h := newPilot(t, nil)
	g1 := onboard(t, h, "venue-a")
	s, g2 := refreshWith(h, pilotClient, pilotSecret, str(t, g1, "refresh_token"))
	if s != http.StatusOK || str(t, g2, "refresh_token") == str(t, g1, "refresh_token") || g2["expires_in"] != float64(3600) {
		t.Fatalf("R1/R2: %d %v", s, g2)
	}
}

func TestRotate_Replay_InvalidGrant(t *testing.T) {
	_, h := newPilot(t, func(c *OAuthConfig) { c.RefreshTokenReuseDetection = false })
	g1 := onboard(t, h, "venue-a")
	rt1 := str(t, g1, "refresh_token")
	s, g2 := refreshWith(h, pilotClient, pilotSecret, rt1)
	if s != http.StatusOK {
		t.Fatal(s)
	}
	if s, body := refreshWith(h, pilotClient, pilotSecret, rt1); s != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Fatalf("R2: replay must fail: %d %v", s, body)
	}
	// Reuse detection off: the successor chain survives a replay.
	if s, _ := refreshWith(h, pilotClient, pilotSecret, str(t, g2, "refresh_token")); s != http.StatusOK {
		t.Fatalf("winner chain must stay valid without reuse detection: %d", s)
	}
}

func TestRotate_RevokesPreviousAccessToken(t *testing.T) {
	p, h := newPilot(t, nil)
	g1 := onboard(t, h, "venue-a")
	at1 := str(t, g1, "access_token")
	_, g2 := refreshWith(h, pilotClient, pilotSecret, str(t, g1, "refresh_token"))
	if _, err := p.ValidateToken(at1); err == nil {
		t.Fatal("R3: previous access token must be revoked at refresh")
	}
	if _, err := p.ValidateToken(str(t, g2, "access_token")); err != nil {
		t.Fatalf("R3: new access token must be valid: %v", err)
	}
}

func TestRotate_PreviousAccessTokenKeptWhenFlagOff(t *testing.T) {
	p, h := newPilot(t, func(c *OAuthConfig) { c.RevokePreviousAccessToken = false })
	g1 := onboard(t, h, "venue-a")
	refreshWith(h, pilotClient, pilotSecret, str(t, g1, "refresh_token"))
	if _, err := p.ValidateToken(str(t, g1, "access_token")); err != nil {
		t.Fatalf("flag off: previous access token must remain valid: %v", err)
	}
}

func TestRotate_ReuseRevokesFamily(t *testing.T) {
	p, h := newPilot(t, nil)
	g1 := onboard(t, h, "venue-a")
	rt1 := str(t, g1, "refresh_token")
	_, g2 := refreshWith(h, pilotClient, pilotSecret, rt1)
	s, body := refreshWith(h, pilotClient, pilotSecret, rt1)
	if s != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Fatalf("R4: reuse must fail: %d %v", s, body)
	}
	if _, err := p.ValidateToken(str(t, g2, "access_token")); err == nil {
		t.Fatal("R4: reuse must revoke the newly issued access token")
	}
	if s, _ := refreshWith(h, pilotClient, pilotSecret, str(t, g2, "refresh_token")); s != http.StatusBadRequest {
		t.Fatalf("R4: reuse must revoke the newly issued refresh token: %d", s)
	}
}

func TestRotate_ReuseDoesNotTouchOtherFamilies(t *testing.T) {
	p, h := newPilot(t, nil)
	a1 := onboard(t, h, "venue-a")
	b1 := onboard(t, h, "venue-b")
	rtA := str(t, a1, "refresh_token")
	refreshWith(h, pilotClient, pilotSecret, rtA)
	refreshWith(h, pilotClient, pilotSecret, rtA) // reuse → revokes A's family only
	if _, err := p.ValidateToken(str(t, b1, "access_token")); err != nil {
		t.Fatalf("venue B must be unaffected by A's reuse: %v", err)
	}
	if s, _ := refreshWith(h, pilotClient, pilotSecret, str(t, b1, "refresh_token")); s != http.StatusOK {
		t.Fatalf("venue B refresh must still work: %d", s)
	}
}

func TestRotate_WrongClientCannotConsumeOrRevoke(t *testing.T) {
	p, h := newPilot(t, nil)
	g1 := onboard(t, h, "venue-a")
	rt1 := str(t, g1, "refresh_token")
	// Bad secret: client authentication fails first.
	if s, body := refreshWith(h, pilotClient, "wrong", rt1); s != http.StatusUnauthorized || body["error"] != ErrInvalidClient {
		t.Fatalf("R8: %d %v", s, body)
	}
	// Different, valid client presenting A's live token.
	if s, _ := refreshWith(h, "other-client", "other-secret", rt1); s != http.StatusBadRequest {
		t.Fatalf("R8: other client must not use the token: %d", s)
	}
	s, g2 := refreshWith(h, pilotClient, pilotSecret, rt1)
	if s != http.StatusOK {
		t.Fatalf("R8: legitimate client's token was consumed by another client: %d", s)
	}
	// Different, valid client presenting A's consumed token must not revoke A's chain.
	refreshWith(h, "other-client", "other-secret", rt1)
	if _, err := p.ValidateToken(str(t, g2, "access_token")); err != nil {
		t.Fatalf("R8: another client must not trigger reuse revocation: %v", err)
	}
}

func TestRotate_MintFailureDoesNotConsume(t *testing.T) {
	p, _ := newPilot(t, nil)
	p.StoreRefreshToken(&RefreshTokenData{Token: "rt0", ClientID: pilotClient, UserID: "u", Scope: "offline", ExpiresAt: time.Now().Add(time.Hour)})
	boom := errors.New("signer unavailable")
	_, err := p.RotateRefreshToken("rt0", pilotClient, func(RefreshTokenData) (string, error) { return "", boom })
	if !errors.Is(err, ErrTokenMint) {
		t.Fatalf("R9: want ErrTokenMint, got %v", err)
	}
	res, err := p.RotateRefreshToken("rt0", pilotClient, func(d RefreshTokenData) (string, error) { return "access", nil })
	if err != nil || res.RefreshToken == "" || res.RefreshToken == "rt0" {
		t.Fatalf("R9: token must survive a local mint failure: %v %+v", err, res)
	}
}

func TestRotate_DisabledPreservesLegacy(t *testing.T) {
	_, h := newPilot(t, func(c *OAuthConfig) {
		c.RotateRefreshTokens = false
		c.RevokePreviousAccessToken = false
		c.RefreshTokenReuseDetection = false
	})
	g1 := onboard(t, h, "venue-a")
	rt1 := str(t, g1, "refresh_token")
	for i := 0; i < 3; i++ {
		s, body := refreshWith(h, pilotClient, pilotSecret, rt1)
		if s != http.StatusOK || body["refresh_token"] != rt1 {
			t.Fatalf("R10: legacy mode must echo the presented token: %d %v", s, body)
		}
	}
}

// R5: N concurrent refreshes of one token → exactly one commit.
func TestRotate_ConcurrentSameToken_ExactlyOneWinner(t *testing.T) {
	for _, n := range []int{2, 16} {
		for _, reuse := range []bool{false, true} {
			trials := 200
			if testing.Short() {
				trials = 20
			}
			p, h := newPilot(t, func(c *OAuthConfig) { c.RefreshTokenReuseDetection = reuse })
			for trial := 0; trial < trials; trial++ {
				first := onboard(t, h, "venue-"+strconv.Itoa(trial))
				rt := str(t, first, "refresh_token")
				var (
					wg      sync.WaitGroup
					mu      sync.Mutex
					winners []map[string]any
					losers  int
				)
				start := make(chan struct{})
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						s, body := refreshWith(h, pilotClient, pilotSecret, rt)
						mu.Lock()
						defer mu.Unlock()
						switch {
						case s == http.StatusOK:
							winners = append(winners, body)
						case s == http.StatusBadRequest && body["error"] == ErrInvalidGrant:
							losers++
						default:
							t.Errorf("unexpected response %d %v", s, body)
						}
					}()
				}
				close(start)
				wg.Wait()
				if len(winners) != 1 || losers != n-1 {
					t.Fatalf("n=%d reuse=%v trial=%d: winners=%d losers=%d", n, reuse, trial, len(winners), losers)
				}
				winnerAccess := winners[0]["access_token"].(string)
				winnerRefresh := winners[0]["refresh_token"].(string)
				_, accessErr := p.ValidateToken(winnerAccess)
				s, _ := refreshWith(h, pilotClient, pilotSecret, winnerRefresh)
				if reuse {
					// Wolt: losers are reuses, so the winner's pair is revoked.
					if accessErr == nil || s == http.StatusOK {
						t.Fatalf("n=%d trial=%d: reuse detection must revoke the winner's pair", n, trial)
					}
				} else if accessErr != nil || s != http.StatusOK {
					t.Fatalf("n=%d trial=%d: winner chain must stay valid: %v %d", n, trial, accessErr, s)
				}
			}
		}
	}
}

// ── Task 4: ValidateToken hardening (G2 at the provider level) ────────────────

func TestValidateToken_ExpiryUsesClock(t *testing.T) {
	p, h := newPilot(t, nil)
	clock := time.Now()
	p.SetClock(func() time.Time { return clock })
	at := str(t, onboard(t, h, "venue-a"), "access_token")
	clock = clock.Add(59 * time.Minute)
	if _, err := p.ValidateToken(at); err != nil {
		t.Fatalf("valid at 59m: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := p.ValidateToken(at); err == nil {
		t.Fatal("must be expired at 61m")
	}
}

func TestValidateToken_RejectsAlgNoneAndForeignIssuer(t *testing.T) {
	p, _ := newPilot(t, nil)
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJpbnRlZ3JhdGlvbiI6eyJ2ZW51ZV9pZCI6InZlbnVlLWEifSwiZXhwIjo0MTAyNDQ0ODAwfQ.mock-signature"
	if _, err := p.ValidateToken(none); err == nil {
		t.Fatal("alg=none must be rejected")
	}
	other, _ := newPilot(t, func(c *OAuthConfig) { c.Issuer = "http://elsewhere/oauth2" })
	foreign, _ := other.GenerateToken(map[string]interface{}{"sub": "x"})
	if _, err := p.ValidateToken(foreign); err == nil {
		t.Fatal("token from another provider must be rejected")
	}
	// Same key, wrong issuer claim.
	wrongIss, _ := p.GenerateToken(map[string]interface{}{"iss": "http://evil"})
	if _, err := p.ValidateToken(wrongIss); err == nil {
		t.Fatal("wrong iss must be rejected")
	}
}
