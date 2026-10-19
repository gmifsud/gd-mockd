package oauth

import (
	"errors"
	"fmt"
	"time"
)

// ErrTokenMint reports a local failure to mint replacement credentials. The
// presented refresh token is not consumed when it is returned.
var ErrTokenMint = errors.New("failed to mint replacement tokens")

// errRefreshTokenInvalid is returned for unknown, expired, foreign or already
// consumed refresh tokens (mapped to invalid_grant by the handler).
var errRefreshTokenInvalid = errors.New("invalid refresh token")

// refreshTombstone records a consumed refresh token so a later presentation can
// be recognised as reuse of that chain.
type refreshTombstone struct {
	family    string
	clientID  string
	expiresAt time.Time
}

// RotationMint mints the access token for a rotation, given a copy of the
// refresh token being consumed. It runs without provider locks held.
type RotationMint func(consumed RefreshTokenData) (accessToken string, err error)

// RotationResult is the outcome of a committed rotation.
type RotationResult struct {
	AccessToken  string
	RefreshToken string
	Consumed     RefreshTokenData
}

// Now returns the provider's current time (time.Now unless overridden).
func (p *Provider) Now() time.Time {
	if f, ok := p.clock.Load().(func() time.Time); ok && f != nil {
		return f()
	}
	return time.Now()
}

// SetClock overrides the provider's clock; nil restores time.Now. Intended for tests.
func (p *Provider) SetClock(now func() time.Time) {
	p.clock.Store(now)
}

// AuthorizationCodeExpiry returns the authorization code lifetime.
func (p *Provider) AuthorizationCodeExpiry() time.Duration {
	return p.codeExpiry
}

// RotateRefreshToken consumes a single-use refresh token and publishes exactly
// one successor, as one indivisible state transition:
//
//  1. snapshot — look up the token and check expiry and client ownership;
//  2. mint     — build the access token and successor value with no lock held,
//     so a local failure (ErrTokenMint) never consumes the presented token;
//  3. commit   — under refreshTokensMu, compare-and-swap: the token must still
//     map to the snapshotted record. The first committer wins; every other
//     concurrent or later presentation fails with invalid_grant and, with
//     RefreshTokenReuseDetection, revokes the chain's current pair.
//
// Tokens are 64-character random values that are never re-inserted, so the
// pointer comparison in step 3 cannot suffer ABA.
func (p *Provider) RotateRefreshToken(presented, clientID string, mint RotationMint) (*RotationResult, error) {
	// 1. Snapshot.
	p.refreshTokensMu.Lock()
	snapshot, ok := p.refreshTokens[presented]
	if !ok {
		p.handleReuseLocked(presented, clientID)
		p.refreshTokensMu.Unlock()
		return nil, errRefreshTokenInvalid
	}
	if p.Now().After(snapshot.ExpiresAt) {
		delete(p.refreshTokens, presented)
		p.refreshTokensMu.Unlock()
		return nil, errors.New("refresh token has expired")
	}
	if snapshot.ClientID != clientID {
		p.refreshTokensMu.Unlock()
		return nil, errors.New("client_id mismatch")
	}
	consumed := *snapshot
	p.refreshTokensMu.Unlock()

	// 2. Mint (fallible work, no locks).
	accessToken, err := mint(consumed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenMint, err)
	}
	successor, err := p.GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenMint, err)
	}

	// 3. Commit.
	p.refreshTokensMu.Lock()
	defer p.refreshTokensMu.Unlock()
	if current, ok := p.refreshTokens[presented]; !ok || current != snapshot {
		// Lost the race to a concurrent rotation of the same token.
		p.handleReuseLocked(presented, clientID)
		return nil, errRefreshTokenInvalid
	}
	delete(p.refreshTokens, presented)
	if p.config.RefreshTokenReuseDetection {
		p.tombstones[presented] = refreshTombstone{family: consumed.Family, clientID: consumed.ClientID, expiresAt: consumed.ExpiresAt}
	}
	p.storeRefreshTokenLocked(&RefreshTokenData{
		Token:       successor,
		ClientID:    consumed.ClientID,
		UserID:      consumed.UserID,
		Scope:       consumed.Scope,
		Claims:      consumed.Claims,
		AccessToken: accessToken,
		Family:      consumed.Family,
		ExpiresAt:   p.Now().Add(p.refreshExpiry),
	})
	if p.config.RevokePreviousAccessToken && consumed.AccessToken != "" {
		p.revokeAccessLocked(consumed.AccessToken)
	}
	return &RotationResult{AccessToken: accessToken, RefreshToken: successor, Consumed: consumed}, nil
}

// handleReuseLocked revokes a chain's current refresh and access tokens when a
// consumed token of that chain is presented again by the client it was issued
// to. A different client never triggers revocation. Caller holds refreshTokensMu.
func (p *Provider) handleReuseLocked(presented, clientID string) {
	if !p.config.RefreshTokenReuseDetection {
		return
	}
	tomb, ok := p.tombstones[presented]
	if !ok || tomb.clientID != clientID {
		return
	}
	head, ok := p.families[tomb.family]
	if !ok {
		return
	}
	delete(p.families, tomb.family)
	if data, ok := p.refreshTokens[head]; ok {
		delete(p.refreshTokens, head)
		p.tombstones[head] = refreshTombstone{family: tomb.family, clientID: data.ClientID, expiresAt: data.ExpiresAt}
		if data.AccessToken != "" {
			p.revokeAccessLocked(data.AccessToken)
		}
	}
}

// revokeAccessLocked adds an access token to the revocation list. Lock order is
// refreshTokensMu → revokedTokensMu, matching RevokeToken.
func (p *Provider) revokeAccessLocked(accessToken string) {
	p.revokedTokensMu.Lock()
	p.revokedTokens[accessToken] = p.Now()
	p.revokedTokensMu.Unlock()
}

// cleanupChainsLocked drops expired tombstones and chains whose head is gone.
// Caller holds refreshTokensMu.
func (p *Provider) cleanupChainsLocked(now time.Time) {
	for token, tomb := range p.tombstones {
		if now.After(tomb.expiresAt) {
			delete(p.tombstones, token)
		}
	}
	for family, head := range p.families {
		if _, ok := p.refreshTokens[head]; !ok {
			delete(p.families, family)
		}
	}
	if len(p.tombstones) > maxRevokedTokens {
		for token := range p.tombstones {
			delete(p.tombstones, token)
			if len(p.tombstones) <= maxRevokedTokens {
				break
			}
		}
	}
}
