package opensdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mock is a local open API: token endpoints plus whatever handler a test sets.
type mock struct {
	t           *testing.T
	srv         *httptest.Server
	tokenCalls  atomic.Int32
	apiCalls    atomic.Int32
	tokenSeq    atomic.Int32
	handler     func(w http.ResponseWriter, r *http.Request, n int32)
	lastHeaders http.Header
	mu          sync.Mutex
}

func newMock(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int32)) *mock {
	m := &mock{t: t, handler: handler}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, APIPrefix)
		if strings.HasPrefix(path, "/auth/") {
			m.tokenCalls.Add(1)
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["app_id"] != "cn.cli_test" || body["app_secret"] != "secret" {
				writeErr(w, 401, "UNAUTHENTICATED", "invalid_credentials", "")
				return
			}
			seq := m.tokenSeq.Add(1)
			switch path {
			case "/auth/app_access_token":
				writeJSON(w, 200, map[string]any{"app_access_token": fmt.Sprintf("app-%d", seq), "expires_in": 7200})
			case "/auth/tenant_access_token":
				writeJSON(w, 200, map[string]any{"tenant_access_token": fmt.Sprintf("t-%s-%d", body["tenant_key"], seq), "expires_in": 7200, "tenant_key": body["tenant_key"], "scopes": []string{}})
			default:
				m.handler(w, r, 0)
			}
			return
		}
		n := m.apiCalls.Add(1)
		m.mu.Lock()
		m.lastHeaders = r.Header.Clone()
		m.mu.Unlock()
		m.handler(w, r, n)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mock) client(opts ...Option) *Client {
	c := NewClient("cn.cli_test", "secret", append([]Option{WithBaseURL(m.srv.URL + "/open-apis/v1/")}, opts...)...)
	c.retryBase = time.Millisecond
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, reason, requestID string) {
	details := map[string]any{}
	if reason != "" {
		details["reason"] = reason
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": code + " message", "request_id": requestID, "details": details}})
}

var ctx = context.Background()

func TestTenantTokenFetchedOnceAndCached(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		writeJSON(w, 200, map[string]any{"tenant_key": "cn.t1", "auth": r.Header.Get("Authorization")})
	})
	c := m.client()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetTenant(ctx, WithTenantKey("cn.t1")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := c.GetTenant(ctx, WithTenantKey("cn.t1"))
	if err != nil {
		t.Fatal(err)
	}
	if n := m.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}
	if got["auth"] != "Bearer t-cn.t1-1" {
		t.Fatalf("auth = %v", got["auth"])
	}
	h := m.lastHeaders
	if !strings.HasPrefix(h.Get("User-Agent"), "ichinese-open-sdk-go/"+Version) || h.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	// Another institution gets its own token.
	if _, err := c.GetTenant(ctx, WithTenantKey("cn.t2")); err != nil {
		t.Fatal(err)
	}
	if n := m.tokenCalls.Load(); n != 2 {
		t.Fatalf("token requests = %d, want 2", n)
	}
}

func TestTokenRenewedBeforeExpiry(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) { writeJSON(w, 200, map[string]any{}) })
	c := m.client()
	now := time.Now()
	c.now = func() time.Time { return now }
	if _, err := c.AppAccessToken(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7200*time.Second - 299*time.Second) // inside the 300s renewal window
	tok, err := c.AppAccessToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "app-2" || m.tokenCalls.Load() != 2 {
		t.Fatalf("token = %s after %d requests", tok, m.tokenCalls.Load())
	}
}

func TestTokenExpiredRefetchesAndRetries(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if r.Header.Get("Authorization") == "Bearer t-cn.t1-1" {
			writeErr(w, 401, "UNAUTHENTICATED", "token_expired", "req-1")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	c := m.client(WithMaxRetries(0))
	got, err := c.GetTenant(ctx, WithTenantKey("cn.t1"))
	if err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true || m.tokenCalls.Load() != 2 || m.apiCalls.Load() != 2 {
		t.Fatalf("ok=%v tokens=%d calls=%d", got["ok"], m.tokenCalls.Load(), m.apiCalls.Load())
	}
}

func TestUserTokenExpiredIsNotRetried(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		writeErr(w, 401, "UNAUTHENTICATED", "token_expired", "req-1")
	})
	c := m.client()
	_, err := c.GetUserInfo(ctx, WithUserAccessToken("u-1"))
	if ReasonOf(err) != "token_expired" || m.apiCalls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, m.apiCalls.Load())
	}
}

func TestRateLimitRetriesAfterRetryAfter(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			writeErr(w, 429, "RATE_LIMITED", "rate_limited", "")
			return
		}
		writeJSON(w, 200, map[string]any{"n": n})
	})
	c := m.client()
	// A POST is retried after a 429 too (the request was not processed).
	got, err := c.SendNotification(ctx, map[string]any{"title": "x"}, WithTenantKey("cn.t1"))
	if err != nil {
		t.Fatal(err)
	}
	if got["n"] != float64(2) {
		t.Fatalf("got %v", got)
	}
}

func TestDailyQuotaIsNotRetried(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		w.Header().Set("Retry-After", "0")
		writeErr(w, 429, "RATE_LIMITED", "daily_quota_exceeded", "")
	})
	_, err := m.client().GetTenant(ctx, WithTenantKey("cn.t1"))
	e, ok := AsError(err)
	if !ok || e.Status != 429 || e.Reason != ReasonDailyQuotaExceeded || m.apiCalls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, m.apiCalls.Load())
	}
	if e.RetryAfter != 0 {
		t.Fatalf("retryAfter = %v", e.RetryAfter)
	}
}

func TestGetRetriesServerErrors(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if n < 3 {
			writeErr(w, 503, "UNAVAILABLE", "", "")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	got, err := m.client().GetClass(ctx, "c 1", WithTenantKey("cn.t1"))
	if err != nil || got["ok"] != true || m.apiCalls.Load() != 3 {
		t.Fatalf("got=%v err=%v calls=%d", got, err, m.apiCalls.Load())
	}
}

func TestGetGivesUpAfterMaxRetries(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) { writeErr(w, 502, "BAD_GATEWAY", "", "") })
	_, err := m.client(WithMaxRetries(2)).GetTenant(ctx, WithTenantKey("cn.t1"))
	if StatusOf(err) != 502 || m.apiCalls.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, m.apiCalls.Load())
	}
}

func TestPostIsNotRetriedOnServerErrors(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		writeErr(w, 503, "UNAVAILABLE", "", "req-503")
	})
	_, err := m.client().CreateAssignment(ctx, "c1", map[string]any{"title": "x"}, WithTenantKey("cn.t1"))
	e, ok := AsError(err)
	if !ok || e.Status != 503 || e.RequestID != "req-503" || m.apiCalls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, m.apiCalls.Load())
	}
}

func TestExchangeAuthCodeIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		calls.Add(1)
		writeErr(w, 503, "UNAVAILABLE", "", "")
	})
	_, err := m.client().ExchangeAuthCode(ctx, "code-1")
	if StatusOf(err) != 503 || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestExchangeAuthCodeSendsTheCode(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "code-1" || body["redirect_uri"] != "https://app.example/cb" || body["code_verifier"] != "v" {
			writeErr(w, 400, "VALIDATION_FAILED", "code_invalid", "")
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "u-1", "refresh_token": "r-1", "open_id": "ou_1", "expires_in": 7200, "scopes": []string{"openid"}})
	})
	u, err := m.client().ExchangeAuthCode(ctx, "code-1", WithRedirectURI("https://app.example/cb"), WithCodeVerifier("v"))
	if err != nil {
		t.Fatal(err)
	}
	if u.AccessToken != "u-1" || u.OpenID != "ou_1" || u.ExpiresIn != 7200 || len(u.Scopes) != 1 {
		t.Fatalf("user = %+v", u)
	}
}

func TestPaginationIterates(t *testing.T) {
	var queries []string
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Path != "/open-apis/v1/classes/c1/members" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.URL.Query().Get("page_token") {
		case "":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"open_id": "ou_1"}, map[string]any{"open_id": "ou_2"}}, "has_more": true, "page_token": "p2"})
		case "p2":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"open_id": "ou_3"}}, "has_more": true, "page_token": "p3"})
		default:
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"open_id": "ou_4"}}, "has_more": false, "page_token": nil})
		}
	})
	c := m.client()
	var ids []string
	for item, err := range c.ListClassMembersAll(ctx, "c1", nil, WithTenantKey("cn.t1")) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item["open_id"].(string))
	}
	if strings.Join(ids, ",") != "ou_1,ou_2,ou_3,ou_4" {
		t.Fatalf("ids = %v", ids)
	}
	if queries[0] != "page_size=100" || queries[1] != "page_size=100&page_token=p2" {
		t.Fatalf("queries = %v", queries)
	}
	// Stopping early stops paging.
	before := m.apiCalls.Load()
	for range c.ListClassMembersAll(ctx, "c1", Params{"page_size": 2}, WithTenantKey("cn.t1")) {
		break
	}
	if m.apiCalls.Load() != before+1 || queries[len(queries)-1] != "page_size=2" {
		t.Fatalf("calls = %d, queries = %v", m.apiCalls.Load()-before, queries)
	}
	// Decode into your own type.
	type member struct {
		OpenID string `json:"open_id"`
	}
	page, err := c.ListClassMembers(ctx, "c1", Params{"page_token": "p2", "role": []string{"a", "b"}, "empty": ""}, WithTenantKey("cn.t1"))
	if err != nil {
		t.Fatal(err)
	}
	type pageOf struct {
		Items     []member `json:"items"`
		HasMore   bool     `json:"has_more"`
		PageToken string   `json:"page_token"`
	}
	p, err := Decode[pageOf](page)
	if err != nil || len(p.Items) != 1 || p.Items[0].OpenID != "ou_3" || !p.HasMore || p.PageToken != "p3" {
		t.Fatalf("page = %+v, %v", p, err)
	}
	if queries[len(queries)-1] != "page_token=p2&role=a&role=b" {
		t.Fatalf("query = %s", queries[len(queries)-1])
	}
}

func TestErrorParsing(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if n == 1 {
			writeJSON(w, 403, map[string]any{"error": map[string]any{
				"code": "FORBIDDEN", "message": "scope denied", "request_id": "req-42",
				"details": map[string]any{"reason": "scope_denied", "scope": "class:readonly", "never_ask": true},
			}})
			return
		}
		w.Header().Set("X-Request-Id", "req-plain")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, "not json")
	})
	c := m.client()
	_, err := c.ListClasses(ctx, nil, WithUserAccessToken("u-1"))
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	if e.Status != 403 || e.Code != "FORBIDDEN" || e.Reason != "scope_denied" || e.RequestID != "req-42" ||
		e.Scope() != "class:readonly" || !e.NeverAsk() || !e.IsScopeError() || !IsScopeError(err) {
		t.Fatalf("error = %+v", e)
	}
	if !strings.Contains(err.Error(), "FORBIDDEN") || !strings.Contains(err.Error(), "req-42") {
		t.Fatalf("message = %s", err.Error())
	}
	_, err = c.ListClasses(ctx, nil, WithAccessToken("any"))
	e, _ = AsError(fmt.Errorf("wrapped: %w", err))
	if e == nil || e.Status != 404 || e.Code != "HTTP_404" || e.RequestID != "req-plain" || e.IsScopeError() {
		t.Fatalf("error = %+v", e)
	}
}

func TestTokenRequired(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) { writeJSON(w, 200, map[string]any{}) })
	c := m.client()
	_, err := c.GetTenant(ctx)
	e, ok := AsError(err)
	if !ok || e.Code != CodeTokenRequired || e.Status != 0 || m.apiCalls.Load() != 0 || m.tokenCalls.Load() != 0 {
		t.Fatalf("err = %v", err)
	}
	// An operation that takes the app token uses it without options.
	if _, err := c.ListPublicContent(ctx, "courses", nil); err != nil {
		t.Fatal(err)
	}
	if m.lastHeaders.Get("Authorization") != "Bearer app-1" {
		t.Fatalf("auth = %s", m.lastHeaders.Get("Authorization"))
	}
	// A missing path parameter fails before any request.
	if _, err := c.GetClass(ctx, "", WithTenantKey("cn.t1")); err == nil {
		t.Fatal("want an error for an empty class_id")
	}
}

func TestNetworkErrorAndTimeout(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		time.Sleep(100 * time.Millisecond)
		writeJSON(w, 200, map[string]any{})
	})
	c := m.client(WithTimeout(10*time.Millisecond), WithMaxRetries(1))
	_, err := c.GetTenant(ctx, WithAccessToken("x"))
	e, ok := AsError(err)
	if !ok || e.Code != CodeTimeout || e.Status != 0 || m.apiCalls.Load() != 2 {
		t.Fatalf("err = %v calls=%d", err, m.apiCalls.Load())
	}
	dead := NewClient("a", "b", WithBaseURL("http://127.0.0.1:1"), WithMaxRetries(0))
	_, err = dead.GetTenant(ctx, WithAccessToken("x"))
	if e, ok := AsError(err); !ok || e.Code != CodeNetwork {
		t.Fatalf("err = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.GetTenant(cctx, WithAccessToken("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestBaseURLAndRequest(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		writeJSON(w, 200, map[string]any{"path": r.URL.Path, "x": r.Header.Get("X-Trace")})
	})
	c := m.client()
	if c.BaseURL() != m.srv.URL {
		t.Fatalf("base = %s", c.BaseURL())
	}
	got, err := c.Request(ctx, "get", "/tenant", nil, nil, WithAccessToken("x"), WithHeader("X-Trace", "t1"))
	if err != nil || got["path"] != "/open-apis/v1/tenant" || got["x"] != "t1" {
		t.Fatalf("got %v %v", got, err)
	}
	if NewClient("a", "b").BaseURL() != DefaultBaseURL {
		t.Fatal("default base")
	}
}

func TestMetaConstants(t *testing.T) {
	if len(Scopes) == 0 || len(EventTypes) == 0 || len(ErrorReasons) == 0 {
		t.Fatal("empty constants")
	}
	if tt := OperationTokens["getTenant"]; len(tt) != 1 || tt[0] != TokenTenant {
		t.Fatalf("getTenant tokens = %v", tt)
	}
	if ReasonScopeDenied != "scope_denied" || ScopeOpenid != "openid" {
		t.Fatal("constants")
	}
}
