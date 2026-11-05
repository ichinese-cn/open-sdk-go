// Receives event pushes: verifies the signature, decrypts, answers the address check and handles events.
//
//	SIGNING_SECRET=… [ENCRYPT_KEY=…] go run ./examples/events-server
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	opensdk "github.com/ichinese-cn/open-sdk-go"
)

func main() {
	secrets := opensdk.ParseOptions{
		SigningSecret: os.Getenv("SIGNING_SECRET"),
		EncryptKey:    os.Getenv("ENCRYPT_KEY"),
	}
	var seen sync.Map // event_id → handled (retries keep the id); use your database in production

	http.HandleFunc("POST /open/events", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		push, err := opensdk.ParseEvent(r.Header, raw, secrets)
		if err != nil {
			log.Printf("rejected push: %v", err)
			http.Error(w, "invalid", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if push.IsURLVerification() {
			_ = json.NewEncoder(w).Encode(opensdk.ChallengeResponse(push))
			return
		}
		if _, dup := seen.LoadOrStore(push.Header.EventID, true); !dup {
			// Answer within 5 seconds: hand long work to a queue.
			switch push.Header.EventType {
			case opensdk.EventAppInstalled:
				log.Printf("installed by %s", push.Header.TenantKey)
			case opensdk.EventAppUninstalled:
				log.Printf("uninstalled by %s", push.Header.TenantKey)
			default:
				var ev map[string]any
				_ = push.DecodeEvent(&ev)
				log.Printf("%s: %v", push.Header.EventType, ev)
			}
		}
		_, _ = w.Write([]byte(`{}`))
	})

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
