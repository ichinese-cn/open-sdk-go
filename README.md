# iChinese Open Platform SDK for Go

> **Prerelease 1.0.0-rc.2.** 这是预发布版，开放平台还没有正式开放，SDK 的默认地址可能连不上。This is a prerelease: the open platform is not open to the public yet, so the SDK's default address may not answer.

爱中文开放平台的 Go 服务端 SDK：开放接口（`/open-apis/v1`）的每个接口都是一个方法（由契约生成），外加取应用令牌和机构令牌并在过期前续期、限流和服务端错误的退避重试、分页迭代、带类型的错误、网页免登换码、事件推送验签解密。只依赖 Go 标准库。

The server SDK of the iChinese open platform. Every operation of the open API is a method on `*Client`
(generated from the contract), plus what each app would otherwise write itself: getting the app and
tenant tokens and renewing them before they expire, retrying after rate limits and server errors with
backoff, paging through lists, typed errors, exchanging the web login's codes, and verifying and
decrypting event pushes. Standard library only.

## Install

```sh
go get github.com/ichinese-cn/open-sdk-go
```

Requires Go 1.23 or newer (list iteration uses range-over-func `iter.Seq2`).

```go
import opensdk "github.com/ichinese-cn/open-sdk-go"
```

## Quick start

### List an institution's members (tenant token)

```go
client := opensdk.NewClient(os.Getenv("APP_ID"), os.Getenv("APP_SECRET"))
ctx := context.Background()

tenant, err := client.GetTenant(ctx, opensdk.WithTenantKey(tenantKey))

for m, err := range client.ListTenantMembersAll(ctx, nil, opensdk.WithTenantKey(tenantKey)) {
	if err != nil {
		return err
	}
	fmt.Println(m["open_id"], m["name"])
}
```

`WithTenantKey` makes the client fetch the institution's tenant token, cache it and renew it 5 minutes
before it expires; concurrent calls share one token request. The app token is used automatically for
operations that accept it.

### Web login backend (免登)

Your page gets a one-time code from the JS SDK's `requestAuthCode()` and posts it to your backend:

```go
user, err := client.ExchangeAuthCode(ctx, code) // never retried: a code works once
me, err := client.GetUserInfo(ctx, opensdk.WithUserAccessToken(user.AccessToken))
// user.OpenID, user.RefreshToken, user.DeniedScopes …
fresh, err := client.RefreshUserToken(ctx, user.RefreshToken)
err = client.RevokeToken(ctx, fresh.AccessToken) // when the user signs out
```

A code from the plain-browser fallback (standard OIDC) also needs
`opensdk.WithRedirectURI(uri), opensdk.WithCodeVerifier(verifier)` (both returned by the JS SDK).
Exchange codes on your backend only: the App Secret never goes to the page.

### Receive events

```go
http.HandleFunc("POST /open/events", func(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body) // verify the raw bytes, not re-serialized JSON
	push, err := opensdk.ParseEvent(r.Header, raw, opensdk.ParseOptions{
		SigningSecret: os.Getenv("SIGNING_SECRET"),
		EncryptKey:    os.Getenv("ENCRYPT_KEY"), // "" when pushes are not encrypted
	})
	if err != nil { // code INVALID_SIGNATURE or INVALID_PAYLOAD
		http.Error(w, "invalid", http.StatusUnauthorized)
		return
	}
	if push.IsURLVerification() {
		json.NewEncoder(w).Encode(opensdk.ChallengeResponse(push))
		return
	}
	// Answer 2xx within 5 seconds; dedupe by push.Header.EventID (retries keep it).
	var ev map[string]any
	push.DecodeEvent(&ev)
})
```

`ParseEvent` checks `X-Ich-Signature` (`v1=` HMAC-SHA256 over `timestamp.nonce.body`, constant-time),
rejects timestamps more than 5 minutes off (`Tolerance` changes it), and decrypts `{"encrypt": …}`
bodies (AES-256-GCM, key = SHA-256 of the encrypt key). `VerifySignature`, `Decrypt` and `Sign` are
available on their own; card callbacks use `ParseCardAction` (same signature and encryption).

See [examples/](examples): `tenant-members`, `web-login-backend`, `events-server`.

## Calling operations

Each operation is a method named after its contract id (`listClassMembers` → `ListClassMembers`):
path parameters by position, then `query opensdk.Params` (when the operation has query parameters),
then `body any` (when it has a body), then call options.

```go
page, err := client.ListClassMembers(ctx, classID, opensdk.Params{"page_size": 20}, opensdk.WithTenantKey(tk))
_, err = client.AddClassMember(ctx, classID, map[string]any{"open_id": openID}, opensdk.WithTenantKey(tk))
```

Answers are `opensdk.Object` (`map[string]any`). Decode them into your own types with `Decode`:

```go
type Member struct {
	OpenID string `json:"open_id"`
	Name   string `json:"name"`
}
m, err := opensdk.Decode[Member](obj)
```

`client.Request(ctx, method, path, query, body, opts...)` calls anything the generated methods do not
cover.

### Which token

Per call, the first that applies: `WithAccessToken(token)` (used as is) → `WithUserAccessToken(token)`
→ `WithTenantKey(key)` → the app token when the operation accepts one. Otherwise the call fails with
code `TOKEN_REQUIRED` (status 0) before any request. `WithHeader(k, v)` adds request headers.

### Paging

Every paged list (`page_token` in, `items` + `has_more` + `page_token` out) also has an `…All` method
returning `iter.Seq2[opensdk.Object, error]` that yields every item, page after page (`page_size` 100
unless the query sets it). Breaking out of the loop stops paging. An error is yielded once, then the
iteration ends.

```go
for item, err := range client.ListClassSessionsAll(ctx, classID, opensdk.Params{"from": from, "to": to}, opensdk.WithTenantKey(tk)) {
	…
}
```

## Configuration

```go
client := opensdk.NewClient(appID, appSecret,
	opensdk.WithBaseURL("https://open.example.com"), // default opensdk.DefaultBaseURL
	opensdk.WithTimeout(10*time.Second),               // per request, default 30s
	opensdk.WithMaxRetries(5),                         // default 3
	opensdk.WithHTTPClient(&http.Client{Transport: myTransport}),
	opensdk.WithRefreshBefore(10*time.Minute),         // renew cached tokens earlier, default 5 min
)
```

The default address (`DefaultBaseURL`) comes from the release configuration; one address serves every
region (tokens carry their region and the gateway routes by it). A trailing `/open-apis/v1` in the base
URL is ignored. Cancel a call (and its retries) with its `ctx`.

### Retries

- **429**: waits `Retry-After` seconds (else `X-RateLimit-Reset`, else backoff) and retries — except
  `daily_quota_exceeded`, which does not come back within a retry.
- **500 / 502 / 503 / 504 and network errors**: retried with backoff (`min(30s, 500ms·2^n + jitter)`)
  for GET, PUT and DELETE only — a POST or PATCH may already have taken effect.
- **401 `token_expired` / `invalid_token`** on a cached app or tenant token: the token is fetched again
  and the call repeated once (not counted as a retry).

## Errors

Failed calls return an `*opensdk.Error` (use `errors.As` or `opensdk.AsError`):

| Field | |
|---|---|
| `Status` | HTTP status; 0 when no answer came (`NETWORK`, `TIMEOUT`, `TOKEN_REQUIRED`) |
| `Code` | `UNAUTHENTICATED`, `FORBIDDEN`, `NOT_FOUND`, `VALIDATION_FAILED`, `RATE_LIMITED`, … (`HTTP_<status>` without a JSON answer) |
| `Reason` | `details.reason`: `scope_denied`, `token_expired`, `code_used`, … (`opensdk.ErrorReasons`, `opensdk.Reason…` constants) |
| `RequestID` | quote it when asking for support |
| `Details`, `RetryAfter`, `Message` | |

When a user or institution refused a scope, degrade instead of failing:

```go
profile, err := client.GetUserLearningProfile(ctx, opensdk.WithUserAccessToken(tok))
if e, ok := opensdk.AsError(err); ok && e.IsScopeError() {
	// e.Reason: scope_denied / scope_pending / scope_not_declared / gold_only
	// e.Scope(): the refused scope; e.NeverAsk(): the user chose not to be asked again
	profile = nil // carry on without it
} else if err != nil {
	return err
}
```

## Constants

`opensdk.Version`, `opensdk.DefaultBaseURL`, `opensdk.Scopes` (and `Scope…` constants),
`opensdk.EventTypes` (`Event…`), `opensdk.ErrorReasons` (`Reason…`) and `opensdk.OperationTokens`
(the token types each operation accepts).

## Development

`operations_gen.go`, `meta_gen.go` and `testdata/test-vectors.json` are generated from
`docs/api/open-api.yaml` by `sdks/tools` (`pnpm sdk:gen`); do not edit them.

```sh
go vet ./... && go test ./...
ICH_OPEN_IT=/path/to/fixture.json go test -run Integration -v ./...   # against a running environment
```

## License

MIT, see [LICENSE](LICENSE).
