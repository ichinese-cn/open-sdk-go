package opensdk

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

type vectors struct {
	SigningSecret      string `json:"signing_secret"`
	EncryptKey         string `json:"encrypt_key"`
	Timestamp          string `json:"timestamp"`
	Nonce              string `json:"nonce"`
	PlainBody          string `json:"plain_body"`
	PlainSignature     string `json:"plain_signature"`
	EncryptedBody      string `json:"encrypted_body"`
	EncryptedSignature string `json:"encrypted_signature"`
	Decrypted          string `json:"decrypted"`
}

func loadVectors(t *testing.T) (vectors, time.Time) {
	t.Helper()
	b, err := os.ReadFile("testdata/test-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	ts, _ := strconv.ParseInt(v.Timestamp, 10, 64)
	return v, time.Unix(ts, 0)
}

func headersFor(v vectors, sig string) http.Header {
	// Lowercase keys as a hand-built map: lookups are case-insensitive.
	return http.Header{"x-ich-timestamp": {v.Timestamp}, "x-ich-nonce": {v.Nonce}, "x-ich-signature": {sig}}
}

func TestSignMatchesVectors(t *testing.T) {
	v, _ := loadVectors(t)
	if got := Sign(v.SigningSecret, v.Timestamp, v.Nonce, []byte(v.PlainBody)); got != v.PlainSignature {
		t.Fatalf("plain signature = %s", got)
	}
	if got := Sign(v.SigningSecret, v.Timestamp, v.Nonce, []byte(v.EncryptedBody)); got != v.EncryptedSignature {
		t.Fatalf("encrypted signature = %s", got)
	}
}

func TestParsePlainEvent(t *testing.T) {
	v, now := loadVectors(t)
	p, err := ParseEvent(headersFor(v, v.PlainSignature), []byte(v.PlainBody), ParseOptions{SigningSecret: v.SigningSecret, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.IsURLVerification() || p.Header.EventType != EventAppInstalled || p.Header.EventID != "ev_01HZY3V6Q2" {
		t.Fatalf("push = %+v", p)
	}
	var ev map[string]string
	if err := p.DecodeEvent(&ev); err != nil || ev["名称"] != "演示" {
		t.Fatalf("event = %v %v", ev, err)
	}
}

func TestParseEncryptedEvent(t *testing.T) {
	v, now := loadVectors(t)
	var wrapper struct {
		Encrypt string `json:"encrypt"`
	}
	_ = json.Unmarshal([]byte(v.EncryptedBody), &wrapper)
	plain, err := Decrypt(wrapper.Encrypt, v.EncryptKey)
	if err != nil || string(plain) != v.Decrypted {
		t.Fatalf("decrypted = %s, %v", plain, err)
	}
	if _, err := Decrypt(wrapper.Encrypt, "wrong"); err == nil {
		t.Fatal("decrypting with a wrong key must fail")
	}
	h := http.Header{}
	h.Set("X-Ich-Timestamp", v.Timestamp)
	h.Set("X-Ich-Nonce", v.Nonce)
	h.Set("X-Ich-Signature", v.EncryptedSignature)
	opts := ParseOptions{SigningSecret: v.SigningSecret, EncryptKey: v.EncryptKey, Now: now}
	p, err := ParseEvent(h, []byte(v.EncryptedBody), opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Raw) != v.Decrypted || p.Header.TenantKey == "" {
		t.Fatalf("push = %+v", p)
	}
	opts.EncryptKey = ""
	if _, err := ParseEvent(h, []byte(v.EncryptedBody), opts); err == nil {
		t.Fatal("an encrypted push without an encrypt key must fail")
	}
}

func TestVerifyRejects(t *testing.T) {
	v, now := loadVectors(t)
	body := []byte(v.PlainBody)
	cases := map[string]struct {
		h   http.Header
		now time.Time
	}{
		"expired":     {headersFor(v, v.PlainSignature), now.Add(301 * time.Second)},
		"future":      {headersFor(v, v.PlainSignature), now.Add(-301 * time.Second)},
		"wrong sig":   {headersFor(v, v.EncryptedSignature), now},
		"missing sig": {http.Header{"X-Ich-Timestamp": {v.Timestamp}, "X-Ich-Nonce": {v.Nonce}}, now},
	}
	for name, c := range cases {
		err := VerifySignature(c.h, body, v.SigningSecret, VerifyOptions{Now: c.now})
		e, ok := AsError(err)
		if !ok || e.Code != CodeInvalidSignature || e.Status != 401 {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := VerifySignature(headersFor(v, v.PlainSignature), body, v.SigningSecret, VerifyOptions{Now: now.Add(299 * time.Second)}); err != nil {
		t.Fatalf("within tolerance: %v", err)
	}
	if err := VerifySignature(headersFor(v, v.PlainSignature), body, v.SigningSecret, VerifyOptions{Now: now.Add(time.Hour), Tolerance: 2 * time.Hour}); err != nil {
		t.Fatalf("custom tolerance: %v", err)
	}
}

func TestURLVerification(t *testing.T) {
	body := []byte(`{"type":"url_verification","challenge":"ch-1"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("X-Ich-Timestamp", ts)
	h.Set("X-Ich-Nonce", "n")
	h.Set("X-Ich-Signature", Sign("s", ts, "n", body))
	p, err := ParseEvent(h, body, ParseOptions{SigningSecret: "s"})
	if err != nil || !p.IsURLVerification() || ChallengeResponse(p)["challenge"] != "ch-1" {
		t.Fatalf("push = %+v, %v", p, err)
	}
}
