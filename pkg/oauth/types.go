package oauth

import "time"

// OAuthConfig configures the OAuth mock provider
type OAuthConfig struct {
	ID            string                 `json:"id" yaml:"id"`
	Issuer        string                 `json:"issuer" yaml:"issuer"`
	TokenExpiry   string                 `json:"tokenExpiry" yaml:"tokenExpiry"`     // e.g., "1h"
	RefreshExpiry string                 `json:"refreshExpiry" yaml:"refreshExpiry"` // e.g., "7d"
	DefaultScopes []string               `json:"defaultScopes" yaml:"defaultScopes"`
	DefaultClaims map[string]interface{} `json:"defaultClaims" yaml:"defaultClaims"`
	Clients       []ClientConfig         `json:"clients" yaml:"clients"`
	Users         []UserConfig           `json:"users" yaml:"users"`
	Enabled       bool                   `json:"enabled" yaml:"enabled"`
	// RotateRefreshTokens makes refresh tokens single-use: each refresh_token
	// grant invalidates the presented token and returns a new one.
	RotateRefreshTokens bool `json:"rotateRefreshTokens,omitempty" yaml:"rotateRefreshTokens,omitempty"`
	// RevokePreviousAccessToken revokes the access token issued alongside a
	// refresh token when that refresh token is rotated. Requires RotateRefreshTokens.
	RevokePreviousAccessToken bool `json:"revokePreviousAccessToken,omitempty" yaml:"revokePreviousAccessToken,omitempty"`
	// RefreshTokenReuseDetection treats a second presentation of a consumed
	// refresh token (by the client it was issued to) as reuse and revokes the
	// chain's current access and refresh tokens. Requires RotateRefreshTokens.
	RefreshTokenReuseDetection bool `json:"refreshTokenReuseDetection,omitempty" yaml:"refreshTokenReuseDetection,omitempty"`
	// AuthorizationCodeExpiry is the authorization code lifetime (default "10m").
	AuthorizationCodeExpiry string `json:"authorizationCodeExpiry,omitempty" yaml:"authorizationCodeExpiry,omitempty"`
	// AuthorizeClaims bind /authorize request parameters to the issued code as
	// token claims; they are carried into every access token derived from it.
	AuthorizeClaims []AuthorizeClaim `json:"authorizeClaims,omitempty" yaml:"authorizeClaims,omitempty"`
}

// AuthorizeClaim copies one /authorize request parameter into a claim bound to
// the issued authorization code. Claim is a dotted path ("integration.venue_id"
// produces {"integration":{"venue_id":...}}); its first segment must not be a
// reserved claim. Pattern, when set, must match the whole parameter value.
type AuthorizeClaim struct {
	Param    string `json:"param" yaml:"param"`
	Claim    string `json:"claim" yaml:"claim"`
	Pattern  string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

// ClientConfig defines an OAuth client configuration
type ClientConfig struct {
	ClientID     string   `json:"clientId" yaml:"clientId"`
	ClientSecret string   `json:"clientSecret" yaml:"clientSecret"`
	RedirectURIs []string `json:"redirectUris" yaml:"redirectUris"`
	GrantTypes   []string `json:"grantTypes" yaml:"grantTypes"` // authorization_code, client_credentials, refresh_token
}

// UserConfig defines a user for the resource owner password credentials flow
type UserConfig struct {
	Username string                 `json:"username" yaml:"username"`
	Password string                 `json:"password" yaml:"password"`
	Claims   map[string]interface{} `json:"claims" yaml:"claims"` // sub, email, name, etc.
}

// TokenResponse represents an OAuth token response
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ErrorResponse represents an OAuth error response
type ErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// AuthorizationCode represents a stored authorization code
type AuthorizationCode struct {
	Code                string
	ClientID            string
	RedirectURI         string
	Scope               string
	UserID              string
	Nonce               string
	CodeChallenge       string                 // PKCE: stored code_challenge
	CodeChallengeMethod string                 // PKCE: "S256" or "plain"
	Claims              map[string]interface{} // bound via AuthorizeClaims; treated as immutable
	ExpiresAt           time.Time
}

// RefreshTokenData represents stored refresh token data
type RefreshTokenData struct {
	Token     string
	ClientID  string
	UserID    string
	Scope     string
	ExpiresAt time.Time
	// Claims bound at authorization; copied to every successor. Treated as immutable.
	Claims map[string]interface{}
	// AccessToken is the access token issued together with this refresh token.
	AccessToken string
	// Family identifies the rotation chain (the chain's first refresh token).
	Family string
}

// OpenIDConfiguration represents the OIDC discovery document
type OpenIDConfiguration struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	UserInfoEndpoint                  string   `json:"userinfo_endpoint"`
	JwksURI                           string   `json:"jwks_uri"`
	RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
	IntrospectionEndpoint             string   `json:"introspection_endpoint,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	SubjectTypesSupported             []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClaimsSupported                   []string `json:"claims_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
}

// IntrospectionResponse represents an OAuth 2.0 Token Introspection response (RFC 7662)
type IntrospectionResponse struct {
	Active    bool   `json:"active"`
	Scope     string `json:"scope,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	Username  string `json:"username,omitempty"`
	TokenType string `json:"token_type,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	Subject   string `json:"sub,omitempty"`
	Audience  string `json:"aud,omitempty"`
	Issuer    string `json:"iss,omitempty"`
	TokenID   string `json:"jti,omitempty"`
}

// JWKS represents a JSON Web Key Set
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK represents a JSON Web Key
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Standard OAuth error codes
const (
	ErrInvalidRequest          = "invalid_request"
	ErrUnauthorizedClient      = "unauthorized_client"
	ErrAccessDenied            = "access_denied"
	ErrUnsupportedResponseType = "unsupported_response_type"
	ErrInvalidScope            = "invalid_scope"
	ErrServerError             = "server_error"
	ErrInvalidClient           = "invalid_client"
	ErrInvalidGrant            = "invalid_grant"
	ErrUnsupportedGrantType    = "unsupported_grant_type"
)

// Grant types
const (
	GrantTypeAuthorizationCode = "authorization_code"
	GrantTypeClientCredentials = "client_credentials"
	GrantTypeRefreshToken      = "refresh_token"
	GrantTypePassword          = "password"
)

// Response types
const (
	ResponseTypeCode  = "code"
	ResponseTypeToken = "token"
)
