package opensdk

import (
	"encoding/json"
	"net/http"
)

// Params holds an operation's query parameters (page_size, page_token, role, from, to, …). Values may
// be strings, numbers, booleans or slices of them (a slice repeats the parameter); nil and "" are left out.
type Params = map[string]any

// Object is a decoded JSON answer of the open API. Use Decode to turn it into your own struct.
type Object = map[string]any

// TokenType is a kind of access token an operation accepts.
type TokenType string

// The token types of the open API.
const (
	TokenApp    TokenType = "app"
	TokenTenant TokenType = "tenant"
	TokenUser   TokenType = "user"
)

// UserAccessToken is what ExchangeAuthCode and RefreshUserToken answer.
type UserAccessToken struct {
	AccessToken      string   `json:"access_token"`
	TokenType        string   `json:"token_type"`
	ExpiresIn        int      `json:"expires_in"`
	RefreshToken     string   `json:"refresh_token"`
	RefreshExpiresIn int      `json:"refresh_expires_in"`
	OpenID           string   `json:"open_id"`
	Scopes           []string `json:"scopes"`
	DeniedScopes     []string `json:"denied_scopes"`
	TenantKey        string   `json:"tenant_key,omitempty"`
}

// TenantAccessToken is what POST /auth/tenant_access_token answers.
type TenantAccessToken struct {
	TenantAccessToken string   `json:"tenant_access_token"`
	ExpiresIn         int      `json:"expires_in"`
	TenantKey         string   `json:"tenant_key"`
	Scopes            []string `json:"scopes"`
}

type appAccessToken struct {
	AppAccessToken string `json:"app_access_token"`
	ExpiresIn      int    `json:"expires_in"`
}

// Decode converts an Object (or any JSON-shaped value) into T through JSON, e.g.
//
//	type Member struct {
//		OpenID string `json:"open_id"`
//		Name   string `json:"name"`
//	}
//	m, err := opensdk.Decode[Member](obj)
func Decode[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// CallOption chooses the token of one call and adds request headers.
//
// The token is picked in this order: WithAccessToken, WithUserAccessToken, WithTenantKey (the client
// gets and renews the institution's tenant token), then the app token when the operation accepts one;
// otherwise the call fails with code TOKEN_REQUIRED.
type CallOption func(*callOptions)

type callOptions struct {
	tenantKey       string
	userAccessToken string
	accessToken     string
	headers         http.Header
}

// WithTenantKey calls as an institution that installed the app (its tenant token, cached and renewed).
func WithTenantKey(tenantKey string) CallOption {
	return func(o *callOptions) { o.tenantKey = tenantKey }
}

// WithUserAccessToken calls as a user (a token from ExchangeAuthCode or RefreshUserToken).
func WithUserAccessToken(token string) CallOption {
	return func(o *callOptions) { o.userAccessToken = token }
}

// WithAccessToken uses a token you already hold (any type) as is.
func WithAccessToken(token string) CallOption {
	return func(o *callOptions) { o.accessToken = token }
}

// WithHeader adds a request header to the call.
func WithHeader(key, value string) CallOption {
	return func(o *callOptions) {
		if o.headers == nil {
			o.headers = http.Header{}
		}
		o.headers.Add(key, value)
	}
}

// ExchangeOption adds what a code from the plain-browser fallback (standard OIDC) also needs.
type ExchangeOption func(Params)

// WithRedirectURI passes the redirect_uri the JS SDK returned with the code.
func WithRedirectURI(uri string) ExchangeOption {
	return func(p Params) { p["redirect_uri"] = uri }
}

// WithCodeVerifier passes the PKCE code_verifier the JS SDK returned with the code.
func WithCodeVerifier(verifier string) ExchangeOption {
	return func(p Params) { p["code_verifier"] = verifier }
}
