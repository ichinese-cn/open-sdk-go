// The backend of the web login (免登): the page gets a code from the JS SDK's requestAuthCode() and
// posts it here; the backend exchanges it (the App Secret stays on the server) and reads the user.
//
//	APP_ID=… APP_SECRET=… [OPEN_API_BASE=…] go run ./examples/web-login-backend
//	curl -X POST localhost:8080/api/login -d '{"code":"…"}'
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	opensdk "github.com/ichinese-cn/open-sdk-go"
)

func main() {
	var opts []opensdk.Option
	if base := os.Getenv("OPEN_API_BASE"); base != "" {
		opts = append(opts, opensdk.WithBaseURL(base))
	}
	client := opensdk.NewClient(os.Getenv("APP_ID"), os.Getenv("APP_SECRET"), opts...)

	http.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
			// Only for a code from the plain-browser fallback (standard OIDC).
			RedirectURI  string `json:"redirect_uri"`
			CodeVerifier string `json:"code_verifier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}
		var ex []opensdk.ExchangeOption
		if in.RedirectURI != "" {
			ex = append(ex, opensdk.WithRedirectURI(in.RedirectURI), opensdk.WithCodeVerifier(in.CodeVerifier))
		}
		user, err := client.ExchangeAuthCode(r.Context(), in.Code, ex...)
		if err != nil {
			if e, ok := opensdk.AsError(err); ok && e.Status == http.StatusBadRequest {
				// code_used / code_expired / code_invalid: ask the page for a new code.
				http.Error(w, e.Reason, http.StatusUnauthorized)
				return
			}
			log.Print(err)
			http.Error(w, "login failed", http.StatusBadGateway)
			return
		}
		me, err := client.GetUserInfo(r.Context(), opensdk.WithUserAccessToken(user.AccessToken))
		if err != nil {
			log.Print(err)
			http.Error(w, "login failed", http.StatusBadGateway)
			return
		}
		// Start your own session here, keyed by user.OpenID; keep user.RefreshToken server-side.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"open_id":       user.OpenID,
			"name":          me["name"],
			"denied_scopes": user.DeniedScopes,
		})
	})

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
