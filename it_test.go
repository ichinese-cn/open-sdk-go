package opensdk

// Integration test against a running `DEV_SLOT=n pnpm dev:regions`: `pnpm sdk:test` sets ICH_OPEN_IT to
// the fixture (sdks/tools/src/it-fixture.ts), the JSON itself or the path of a file holding it; without
// it the test is skipped.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

type itFixture struct {
	Base      string `json:"base"`
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	TenantKey string `json:"tenant_key"`
	CodeURL   string `json:"code_url"`
}

func loadFixture(t *testing.T) itFixture {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("ICH_OPEN_IT"))
	if raw == "" {
		t.Skip("ICH_OPEN_IT is not set")
	}
	data := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		b, err := os.ReadFile(raw)
		if err != nil {
			t.Fatal(err)
		}
		data = b
	}
	var fx itFixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func freshCode(t *testing.T, fx itFixture) string {
	t.Helper()
	res, err := http.Get(fx.CodeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Code == "" {
		t.Fatalf("code_url: %v", err)
	}
	return out.Code
}

func TestIntegration(t *testing.T) {
	fx := loadFixture(t)
	ctx := context.Background()
	client := func() *Client { return NewClient(fx.AppID, fx.AppSecret, WithBaseURL(fx.Base)) }

	t.Run("a tenant token lists the members page by page", func(t *testing.T) {
		c := client()
		tenant, err := c.GetTenant(ctx, WithTenantKey(fx.TenantKey))
		if err != nil {
			t.Fatal(err)
		}
		if tenant["tenant_key"] != fx.TenantKey {
			t.Fatalf("tenant_key = %v", tenant["tenant_key"])
		}
		var members []Object
		for m, err := range c.ListTenantMembersAll(ctx, Params{"page_size": 1}, WithTenantKey(fx.TenantKey)) {
			if err != nil {
				t.Fatal(err)
			}
			members = append(members, m)
		}
		if len(members) == 0 {
			t.Fatal("no members")
		}
		if id, _ := members[0]["open_id"].(string); !strings.HasPrefix(id, "ou_") {
			t.Fatalf("open_id = %v", members[0]["open_id"])
		}
	})

	t.Run("exchangeAuthCode gives a user token; the code works once", func(t *testing.T) {
		c := client()
		code := freshCode(t, fx)
		user, err := c.ExchangeAuthCode(ctx, code)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(user.OpenID, "ou_") {
			t.Fatalf("open_id = %s", user.OpenID)
		}
		me, err := c.GetUserInfo(ctx, WithUserAccessToken(user.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		if me["open_id"] != user.OpenID {
			t.Fatalf("user/info open_id = %v, want %s", me["open_id"], user.OpenID)
		}
		_, err = c.ExchangeAuthCode(ctx, code)
		if e, ok := AsError(err); !ok || e.Status != 400 || e.Reason != ReasonCodeUsed {
			t.Fatalf("second exchange: %v", err)
		}
		refreshed, err := c.RefreshUserToken(ctx, user.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		if refreshed.OpenID != user.OpenID {
			t.Fatalf("refreshed open_id = %s", refreshed.OpenID)
		}
	})

	t.Run("a tenant endpoint refuses a user token with a typed error", func(t *testing.T) {
		c := client()
		user, err := c.ExchangeAuthCode(ctx, freshCode(t, fx))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.GetTenant(ctx, WithUserAccessToken(user.AccessToken))
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("err = %v", err)
		}
		if e.Status != 403 || e.RequestID == "" {
			t.Fatalf("error = %+v", e)
		}
	})
}
