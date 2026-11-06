// Package opensdk is the open platform's server SDK for Go: every operation of the open API as a method
// (generated from docs/api/open-api.yaml), plus what each app would otherwise write itself — getting the
// app and tenant tokens and renewing them before they expire, retrying after rate limits and server
// errors with backoff, paging through lists, typed errors, exchanging the web login's codes and checking
// event pushes.
//
//	client := opensdk.NewClient(os.Getenv("APP_ID"), os.Getenv("APP_SECRET"))
//	for m, err := range client.ListTenantMembersAll(ctx, nil, opensdk.WithTenantKey(tenantKey)) {
//		if err != nil {
//			return err
//		}
//		fmt.Println(m["name"])
//	}
//	user, err := client.ExchangeAuthCode(ctx, code) // the code from the JS SDK
//	me, err := client.GetUserInfo(ctx, opensdk.WithUserAccessToken(user.AccessToken))
package opensdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client calls the open API as one app. It is safe for concurrent use; make one per app and reuse it
// (it caches the app and tenant tokens).
type Client struct {
	appID         string
	appSecret     string
	baseURL       string
	httpClient    *http.Client
	maxRetries    int
	timeout       time.Duration
	refreshBefore time.Duration
	userAgent     string
	// retryBase is the base of the retry backoff (tests make it small).
	retryBase time.Duration
	now       func() time.Time

	mu       sync.Mutex
	tokens   map[string]cachedToken
	inflight map[string]*flight
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// flight is one token request that concurrent callers of the same key wait for.
type flight struct {
	done  chan struct{}
	token string
	err   error
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the open API's address (default DefaultBaseURL). One address serves every region:
// tokens carry their region and the gateway routes by it. A trailing "/" or "/open-apis/v1" is dropped.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// WithHTTPClient uses your own *http.Client (proxies, transports, tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithMaxRetries sets how often a call is retried after a rate limit (429), a server error (5xx) or a
// network failure. Default 3.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithTimeout sets the timeout of each request. Default 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithRefreshBefore renews a cached app or tenant token this long before it expires. Default 5 minutes.
func WithRefreshBefore(d time.Duration) Option {
	return func(c *Client) { c.refreshBefore = d }
}

// WithUserAgent appends to the User-Agent header.
func WithUserAgent(s string) Option {
	return func(c *Client) { c.userAgent += " " + s }
}

// NewClient makes a client for the app with this App ID and App Secret. It panics when either is empty
// (a configuration error). Keep the App Secret on your server.
func NewClient(appID, appSecret string, opts ...Option) *Client {
	if appID == "" || appSecret == "" {
		panic("opensdk: appID and appSecret are required")
	}
	c := &Client{
		appID:         appID,
		appSecret:     appSecret,
		baseURL:       DefaultBaseURL,
		httpClient:    http.DefaultClient,
		maxRetries:    3,
		timeout:       30 * time.Second,
		refreshBefore: 5 * time.Minute,
		userAgent:     "ichinese-open-sdk-go/" + Version,
		retryBase:     500 * time.Millisecond,
		now:           time.Now,
		tokens:        map[string]cachedToken{},
		inflight:      map[string]*flight{},
	}
	for _, o := range opts {
		o(c)
	}
	c.baseURL = strings.TrimSuffix(strings.TrimRight(c.baseURL, "/"), APIPrefix)
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	return c
}

// AppID is the client's App ID.
func (c *Client) AppID() string { return c.appID }

// BaseURL is the open API's address the client calls (without /open-apis/v1).
func (c *Client) BaseURL() string { return c.baseURL }

// ── tokens ──────────────────────────────────────────────────────────────────

// AppAccessToken returns the app token (cached, renewed before it expires).
func (c *Client) AppAccessToken(ctx context.Context) (string, error) {
	return c.cached(ctx, "app", func(ctx context.Context) (string, int, error) {
		var r appAccessToken
		if err := c.post(ctx, "/auth/app_access_token", c.credentials(), true, &r); err != nil {
			return "", 0, err
		}
		return r.AppAccessToken, r.ExpiresIn, nil
	})
}

// TenantAccessToken returns the tenant token of an institution that installed the app (cached per
// institution, renewed before it expires).
func (c *Client) TenantAccessToken(ctx context.Context, tenantKey string) (string, error) {
	return c.cached(ctx, "tenant:"+tenantKey, func(ctx context.Context) (string, int, error) {
		body := c.credentials()
		body["tenant_key"] = tenantKey
		var r TenantAccessToken
		if err := c.post(ctx, "/auth/tenant_access_token", body, true, &r); err != nil {
			return "", 0, err
		}
		return r.TenantAccessToken, r.ExpiresIn, nil
	})
}

// ExchangeAuthCode is the web login (免登): it exchanges the code your page got from the JS SDK's
// requestAuthCode() for a user token and the user's open_id. A code from the plain-browser fallback
// (standard OIDC) also needs WithRedirectURI and WithCodeVerifier. Exchange codes on your backend only:
// the App Secret never goes to the page. A code works once, so this is never retried.
func (c *Client) ExchangeAuthCode(ctx context.Context, code string, opts ...ExchangeOption) (*UserAccessToken, error) {
	body := c.credentials()
	body["code"] = code
	for _, o := range opts {
		o(body)
	}
	var r UserAccessToken
	if err := c.post(ctx, "/auth/exchange_auth_code", body, false, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// RefreshUserToken gets a new pair for a refresh token (the old one stops working).
func (c *Client) RefreshUserToken(ctx context.Context, refreshToken string) (*UserAccessToken, error) {
	body := c.credentials()
	body["refresh_token"] = refreshToken
	var r UserAccessToken
	if err := c.post(ctx, "/auth/refresh_user_token", body, false, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// RevokeToken revokes a user or refresh token (when the user signs out of your app).
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	body := c.credentials()
	body["token"] = token
	return c.post(ctx, "/auth/revoke", body, true, nil)
}

// ClearTokenCache forgets the cached app and tenant tokens (they are fetched again when next needed).
func (c *Client) ClearTokenCache() {
	c.mu.Lock()
	c.tokens = map[string]cachedToken{}
	c.mu.Unlock()
}

// ── calls ───────────────────────────────────────────────────────────────────

// Request makes any open API call by method and path ("/tenant/members"), for what the generated
// methods do not cover. Without a token option it uses the app token.
func (c *Client) Request(ctx context.Context, method, path string, query Params, body any, opts ...CallOption) (Object, error) {
	return c.call(ctx, &callSpec{
		id: path, method: strings.ToUpper(method), path: path, query: query, body: body,
		tokenTypes: []TokenType{TokenApp, TokenTenant, TokenUser},
	}, opts)
}

// callSpec is one call of an operation (made by the generated methods).
type callSpec struct {
	id         string
	method     string
	path       string
	pathParams map[string]string
	query      Params
	body       any
	tokenTypes []TokenType
	// idempotent retries a POST like a GET (one the server makes idempotent: a post with its uuid,
	// completing an upload).
	idempotent bool
}

func (c *Client) call(ctx context.Context, spec *callSpec, opts []CallOption) (Object, error) {
	var o callOptions
	for _, f := range opts {
		f(&o)
	}
	path := spec.path
	for strings.Contains(path, "{") {
		start := strings.Index(path, "{")
		end := strings.Index(path[start:], "}")
		if end < 0 {
			break
		}
		name := path[start+1 : start+end]
		v := spec.pathParams[name]
		if v == "" {
			return nil, fmt.Errorf("opensdk: %s: %s is required", spec.id, name)
		}
		path = path[:start] + url.PathEscape(v) + path[start+end+1:]
	}
	data, err := c.send(ctx, spec.method, path, spec.query, spec.body, &callAuth{spec: spec, opts: o}, true)
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

func (c *Client) iterate(ctx context.Context, spec *callSpec, opts []CallOption) iter.Seq2[Object, error] {
	return func(yield func(Object, error) bool) {
		query := Params{}
		for k, v := range spec.query {
			query[k] = v
		}
		if _, ok := query["page_size"]; !ok {
			query["page_size"] = 100
		}
		for {
			s := *spec
			s.query = query
			page, err := c.call(ctx, &s, opts)
			if err != nil {
				yield(nil, err)
				return
			}
			items, _ := page["items"].([]any)
			for _, it := range items {
				obj, _ := it.(map[string]any)
				if !yield(obj, nil) {
					return
				}
			}
			hasMore, _ := page["has_more"].(bool)
			next, _ := page["page_token"].(string)
			if !hasMore || next == "" {
				return
			}
			q := Params{}
			for k, v := range query {
				q[k] = v
			}
			q["page_token"] = next
			query = q
		}
	}
}

// ── internals ───────────────────────────────────────────────────────────────

var retryStatuses = map[int]bool{500: true, 502: true, 503: true, 504: true}

type callAuth struct {
	spec *callSpec
	opts callOptions
}

func (c *Client) credentials() Params {
	return Params{"app_id": c.appID, "app_secret": c.appSecret}
}

// post calls a token endpoint (credentials in the body, no token).
func (c *Client) post(ctx context.Context, path string, body Params, retry bool, out any) error {
	data, err := c.send(ctx, http.MethodPost, path, nil, body, nil, retry)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return newError(0, CodeInvalidPayload, fmt.Sprintf("POST %s: %v", path, err), nil)
	}
	return nil
}

func (c *Client) cached(ctx context.Context, key string, fetch func(context.Context) (string, int, error)) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[key]; ok && c.now().Before(t.expiresAt.Add(-c.refreshBefore)) {
		c.mu.Unlock()
		return t.token, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.token, f.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.mu.Unlock()

	token, expiresIn, err := fetch(ctx)
	c.mu.Lock()
	delete(c.inflight, key)
	if err == nil {
		c.tokens[key] = cachedToken{token: token, expiresAt: c.now().Add(time.Duration(expiresIn) * time.Second)}
	}
	c.mu.Unlock()
	f.token, f.err = token, err
	close(f.done)
	return token, err
}

func (c *Client) dropToken(key string) {
	c.mu.Lock()
	delete(c.tokens, key)
	c.mu.Unlock()
}

// tokenFor picks the token of a call, and the cache key it came from (dropped and fetched again after a 401).
func (c *Client) tokenFor(ctx context.Context, a *callAuth) (token, cacheKey string, err error) {
	switch {
	case a.opts.accessToken != "":
		return a.opts.accessToken, "", nil
	case a.opts.userAccessToken != "":
		return a.opts.userAccessToken, "", nil
	case a.opts.tenantKey != "":
		t, err := c.TenantAccessToken(ctx, a.opts.tenantKey)
		return t, "tenant:" + a.opts.tenantKey, err
	}
	for _, t := range a.spec.tokenTypes {
		if t == TokenApp {
			tok, err := c.AppAccessToken(ctx)
			return tok, "app", err
		}
	}
	types := make([]string, len(a.spec.tokenTypes))
	for i, t := range a.spec.tokenTypes {
		types[i] = string(t)
	}
	return "", "", newError(0, CodeTokenRequired, fmt.Sprintf(
		"%s takes a %s token: pass WithTenantKey or WithUserAccessToken", a.spec.id, strings.Join(types, " or ")), nil)
}

func (c *Client) url(path string, query Params) string {
	u := c.baseURL + APIPrefix + path
	if len(query) == 0 {
		return u
	}
	values := url.Values{}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := query[k]
		if v == nil {
			continue
		}
		rv := reflect.ValueOf(v)
		if (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && rv.Type().Elem().Kind() != reflect.Uint8 {
			for i := 0; i < rv.Len(); i++ {
				values.Add(k, fmt.Sprint(rv.Index(i).Interface()))
			}
			continue
		}
		s := fmt.Sprint(v)
		if s == "" {
			continue
		}
		values.Set(k, s)
	}
	if enc := values.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.retryBase * time.Duration(1<<attempt)
	d := base + time.Duration(rand.Float64()*float64(base)*0.25)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type answer struct {
	status int
	header http.Header
	data   []byte
}

// do sends one request; a failure before an answer (or while reading it) comes back as err, with
// timedOut set when the per-request timeout ran out.
func (c *Client) do(ctx context.Context, method, u string, body []byte, header http.Header) (ans answer, timedOut bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, u, r)
	if err != nil {
		return ans, false, err
	}
	req.Header = header
	res, err := c.httpClient.Do(req)
	if err != nil {
		return ans, errors.Is(rctx.Err(), context.DeadlineExceeded), err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return ans, errors.Is(rctx.Err(), context.DeadlineExceeded), err
	}
	return answer{status: res.StatusCode, header: res.Header, data: data}, false, nil
}

// send makes a call with its retries. retryPost: a POST without a token (the token endpoints) is safe to
// repeat; exchanging a one-time code is not.
func (c *Client) send(ctx context.Context, method, path string, query Params, body any, auth *callAuth, retryPost bool) ([]byte, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opensdk: %s %s: encoding the body: %w", method, path, err)
		}
		payload = b
	}
	u := c.url(path, query)
	idempotent := method == http.MethodGet || method == http.MethodPut || method == http.MethodDelete ||
		(method == http.MethodPost && retryPost && auth == nil) ||
		(auth != nil && auth.spec.idempotent)
	refreshed := false
	for attempt := 0; ; attempt++ {
		header := http.Header{}
		header.Set("Accept", "application/json")
		header.Set("User-Agent", c.userAgent)
		cacheKey := ""
		if auth != nil {
			for k, vs := range auth.opts.headers {
				for _, v := range vs {
					header.Add(k, v)
				}
			}
			token, key, err := c.tokenFor(ctx, auth)
			if err != nil {
				return nil, err
			}
			header.Set("Authorization", "Bearer "+token)
			cacheKey = key
		}
		if payload != nil {
			header.Set("Content-Type", "application/json")
		}
		canRetry := attempt < c.maxRetries

		ans, timedOut, err := c.do(ctx, method, u, payload, header)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if canRetry && idempotent {
				if err := sleep(ctx, c.backoff(attempt)); err != nil {
					return nil, err
				}
				continue
			}
			code := CodeNetwork
			if timedOut {
				code = CodeTimeout
			}
			return nil, newError(0, code, fmt.Sprintf("%s %s: %v", method, path, err), nil)
		}
		if ans.status >= 200 && ans.status < 300 {
			return ans.data, nil
		}
		e := toError(ans)
		if ans.status == http.StatusTooManyRequests && canRetry && e.Reason != ReasonDailyQuotaExceeded {
			wait := c.backoff(attempt)
			if s, ok := seconds(ans.header.Get("Retry-After")); ok {
				wait = s
			} else if s, ok := seconds(ans.header.Get("X-RateLimit-Reset")); ok && s > 0 {
				wait = s
			}
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		if retryStatuses[ans.status] && canRetry && idempotent {
			if err := sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if ans.status == http.StatusUnauthorized && cacheKey != "" && !refreshed &&
			(e.Reason == ReasonTokenExpired || e.Reason == ReasonInvalidToken) {
			c.dropToken(cacheKey)
			refreshed = true
			attempt-- // not counted as a retry
			continue
		}
		return nil, e
	}
}

func seconds(h string) (time.Duration, bool) {
	if h == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(h), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

func toError(a answer) *Error {
	var body struct {
		Error *struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			RequestID string         `json:"request_id"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.data, &body)
	e := newError(a.status, fmt.Sprintf("HTTP_%d", a.status), http.StatusText(a.status), nil)
	if body.Error != nil {
		if body.Error.Code != "" {
			e.Code = body.Error.Code
		}
		if body.Error.Message != "" {
			e.Message = body.Error.Message
		}
		e.RequestID = body.Error.RequestID
		if body.Error.Details != nil {
			e.Details = body.Error.Details
			e.Reason, _ = body.Error.Details["reason"].(string)
		}
	}
	if e.RequestID == "" {
		e.RequestID = a.header.Get("X-Request-Id")
	}
	if s, ok := seconds(a.header.Get("Retry-After")); ok {
		e.RetryAfter = s
	}
	return e
}

// decodeObject decodes an answer: nil for an empty one (204); a JSON value that is not an object comes
// back as {"data": value}.
func decodeObject(data []byte) (Object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, newError(0, CodeInvalidPayload, "the answer is not JSON: "+err.Error(), nil)
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return Object{"data": v}, nil
}
