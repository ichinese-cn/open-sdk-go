package opensdk

import (
	"errors"
	"fmt"
	"time"
)

// Error codes the SDK sets itself (the platform's own codes come from the answer: UNAUTHENTICATED,
// FORBIDDEN, NOT_FOUND, VALIDATION_FAILED, CONFLICT, RATE_LIMITED, …).
const (
	CodeTokenRequired    = "TOKEN_REQUIRED"
	CodeNetwork          = "NETWORK"
	CodeTimeout          = "TIMEOUT"
	CodeInvalidSignature = "INVALID_SIGNATURE"
	CodeInvalidPayload   = "INVALID_PAYLOAD"
)

// Error is an error answer of the open API ({"error": {"code", "message", "request_id", "details"}}),
// or a failure before one came (Code NETWORK / TIMEOUT / TOKEN_REQUIRED, Status 0). Get it with
// errors.As or AsError.
type Error struct {
	// Status is the HTTP status (0: no answer).
	Status int
	// Code is the stable error code, HTTP_<status> when the answer had none.
	Code    string
	Message string
	// RequestID identifies the request for support (error.request_id or the X-Request-Id header).
	RequestID string
	// Reason is details.reason, e.g. scope_denied, token_expired, code_used ("" when none).
	Reason  string
	Details map[string]any
	// RetryAfter is the Retry-After of a 429 (0 when the answer had none).
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	s := fmt.Sprintf("opensdk: %s", e.Code)
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	s += ": " + msg
	if e.Reason != "" {
		s += " [" + e.Reason + "]"
	}
	if e.RequestID != "" {
		s += " request_id=" + e.RequestID
	}
	return s
}

// Scope is the scope the user or institution refused (scope_denied): the app should do without it.
func (e *Error) Scope() string {
	s, _ := e.Details["scope"].(string)
	return s
}

// NeverAsk reports that the user chose「不再询问」for the refused scope: do not ask again.
func (e *Error) NeverAsk() bool {
	b, _ := e.Details["never_ask"].(bool)
	return b
}

// IsScopeError reports a refused, not yet asked or undeclared scope (403): degrade gracefully.
func (e *Error) IsScopeError() bool {
	if e.Status != 403 {
		return false
	}
	switch e.Reason {
	case ReasonScopeDenied, ReasonScopePending, ReasonScopeNotDeclared, ReasonGoldOnly:
		return true
	}
	return false
}

// AsError returns the *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsScopeError reports whether err is an *Error for a refused, pending or undeclared scope.
func IsScopeError(err error) bool {
	e, ok := AsError(err)
	return ok && e.IsScopeError()
}

// ReasonOf returns details.reason of err when it is an *Error ("" otherwise).
func ReasonOf(err error) string {
	if e, ok := AsError(err); ok {
		return e.Reason
	}
	return ""
}

// StatusOf returns the HTTP status of err when it is an *Error (0 otherwise).
func StatusOf(err error) int {
	if e, ok := AsError(err); ok {
		return e.Status
	}
	return 0
}

func newError(status int, code, message string, details map[string]any) *Error {
	if details == nil {
		details = map[string]any{}
	}
	reason, _ := details["reason"].(string)
	return &Error{Status: status, Code: code, Message: message, Details: details, Reason: reason}
}
