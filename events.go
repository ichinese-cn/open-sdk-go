package opensdk

// Event pushes and card callbacks from the platform to your request address: checking the signature,
// decrypting, and answering the address check.
//
//	X-Ich-Signature: v1=hex(HMAC-SHA256(signing secret, timestamp + "." + nonce + "." + raw body))
//	encrypted body:  {"encrypt": base64(12-byte IV ‖ AES-256-GCM ciphertext ‖ 16-byte tag)}, key = SHA-256(encrypt key)
//
// Always verify against the raw body bytes as received (not re-serialized JSON).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how far a push's timestamp may be from now.
const DefaultTolerance = 5 * time.Minute

// EventHeader is the header of an event push.
type EventHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CreateTime string `json:"create_time"`
	AppID      string `json:"app_id"`
	TenantKey  string `json:"tenant_key,omitempty"`
}

// Push is a parsed event push, card callback or address check (Type "url_verification").
type Push struct {
	// Type is "url_verification" for the address check ("" for events and card callbacks).
	Type      string `json:"type,omitempty"`
	Challenge string `json:"challenge,omitempty"`

	Schema string      `json:"schema,omitempty"`
	Header EventHeader `json:"header"`
	// Event is the event's own payload (decode it with DecodeEvent).
	Event json.RawMessage `json:"event,omitempty"`

	// Card callbacks: who clicked, the button's value and the card's message.
	Operator  *CardOperator `json:"operator,omitempty"`
	Action    *CardAction   `json:"action,omitempty"`
	MessageID string        `json:"message_id,omitempty"`

	// Raw is the push's JSON (decrypted).
	Raw json.RawMessage `json:"-"`
}

// CardOperator is who clicked a card's button.
type CardOperator struct {
	OpenID string `json:"open_id,omitempty"`
}

// CardAction is the clicked button's value.
type CardAction struct {
	Value map[string]any `json:"value"`
}

// IsURLVerification reports the address check: answer it with ChallengeResponse.
func (p *Push) IsURLVerification() bool { return p.Type == "url_verification" }

// DecodeEvent decodes the event payload into v.
func (p *Push) DecodeEvent(v any) error { return json.Unmarshal(p.Event, v) }

// ChallengeResponse is the answer to the address check ({"challenge": …}, as JSON).
func ChallengeResponse(p *Push) map[string]string {
	return map[string]string{"challenge": p.Challenge}
}

// Sign returns the signature the platform puts in X-Ich-Signature for these values.
func Sign(signingSecret, timestamp, nonce string, body []byte) string {
	m := hmac.New(sha256.New, []byte(signingSecret))
	m.Write([]byte(timestamp + "." + nonce + "."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// VerifyOptions tunes VerifySignature and ParseEvent.
type VerifyOptions struct {
	// Tolerance is how far the timestamp may be from now (default 5 minutes).
	Tolerance time.Duration
	// Now replaces the current time (tests).
	Now time.Time
}

// header looks a header up case-insensitively, also in maps built by hand with lowercase keys.
func header(h http.Header, name string) string {
	if v := h.Get(name); v != "" {
		return v
	}
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// VerifySignature checks a push's signature and age. It returns an *Error with code INVALID_SIGNATURE
// (status 401) when either is wrong.
func VerifySignature(h http.Header, rawBody []byte, signingSecret string, opts ...VerifyOptions) error {
	var o VerifyOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	fail := func(msg string) error { return newError(401, CodeInvalidSignature, msg, nil) }
	timestamp := header(h, "X-Ich-Timestamp")
	nonce := header(h, "X-Ich-Nonce")
	signature := header(h, "X-Ich-Signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return fail("missing X-Ich-Timestamp, X-Ich-Nonce or X-Ich-Signature")
	}
	tolerance := o.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts < 0 {
		return fail("the timestamp is too far from now")
	}
	diff := now.Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(tolerance/time.Second) {
		return fail("the timestamp is too far from now")
	}
	expected := Sign(signingSecret, timestamp, nonce, rawBody)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return fail("the signature does not match")
	}
	return nil
}

// Decrypt decrypts the "encrypt" value of an encrypted push with the app's encrypt key.
func Decrypt(encrypted, encryptKey string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, newError(400, CodeInvalidPayload, "not base64", nil)
	}
	if len(data) < 12+16 {
		return nil, newError(400, CodeInvalidPayload, "too short", nil)
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, newError(400, CodeInvalidPayload, err.Error(), nil)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, newError(400, CodeInvalidPayload, err.Error(), nil)
	}
	plain, err := gcm.Open(nil, data[:12], data[12:], nil)
	if err != nil {
		return nil, newError(400, CodeInvalidPayload, "cannot decrypt (wrong encrypt key?)", nil)
	}
	return plain, nil
}

// ParseOptions are the app's event secrets for ParseEvent.
type ParseOptions struct {
	SigningSecret string
	// EncryptKey decrypts encrypted pushes ("" when the app does not encrypt them).
	EncryptKey string
	Tolerance  time.Duration
	Now        time.Time
}

// ParseEvent verifies, decrypts and parses a push. Answer an address check (IsURLVerification) with
// ChallengeResponse, and an event with any 2xx within 5 seconds (dedupe by Header.EventID: retries keep it).
func ParseEvent(h http.Header, rawBody []byte, opts ParseOptions) (*Push, error) {
	if err := VerifySignature(h, rawBody, opts.SigningSecret, VerifyOptions{Tolerance: opts.Tolerance, Now: opts.Now}); err != nil {
		return nil, err
	}
	raw := rawBody
	var wrapper struct {
		Encrypt *string `json:"encrypt"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, newError(400, CodeInvalidPayload, "the body is not JSON", nil)
	}
	if wrapper.Encrypt != nil {
		if opts.EncryptKey == "" {
			return nil, newError(400, CodeInvalidPayload, "encrypted push but no EncryptKey", nil)
		}
		plain, err := Decrypt(*wrapper.Encrypt, opts.EncryptKey)
		if err != nil {
			return nil, err
		}
		raw = plain
	}
	var p Push
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newError(400, CodeInvalidPayload, "the push is not JSON", nil)
	}
	p.Raw = append(json.RawMessage(nil), raw...)
	return &p, nil
}

// ParseCardAction parses a card callback (a button of a bot's card was clicked): same signature and
// encryption as events.
func ParseCardAction(h http.Header, rawBody []byte, opts ParseOptions) (*Push, error) {
	return ParseEvent(h, rawBody, opts)
}
