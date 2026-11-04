package kosa8

import (
	"errors"
	"fmt"
	"strings"
)

// Error is every error this package returns. Kind says which; the daemon's own
// words are kept in DaemonMessage and Remediation, exactly as @kosa8/sdk does.
type Error struct {
	Kind          Kind
	Message       string
	Status        int    // HTTP status, when the daemon answered
	DaemonMessage string // the daemon's "error" text, verbatim
	Remediation   string // the daemon's "remediation": what to do about it
	Method, Path  string
	Body          any            // the decoded response body
	Approval      map[string]any // set when Kind == KindApprovalRequired: the daemon's refusal
	Stdout        string         // KindStream: output produced before the error
	Stderr        string
	Err           error // the underlying cause, if any
	// Code is kosa8 Cloud's stable error code (payment_required,
	// quota_exceeded, ...); empty for daemon errors.
	Code string
}

// Kind classifies an Error.
type Kind string

const (
	KindConnection       Kind = "connection"        // daemon unreachable
	KindAPI              Kind = "api"               // any other HTTP error
	KindBadRequest       Kind = "bad_request"       // 400
	KindAuth             Kind = "auth"              // 401
	KindPolicy           Kind = "policy"            // 403
	KindApprovalRequired Kind = "approval_required" // 403 needing a signed approval
	KindNotFound         Kind = "not_found"         // 404
	KindConflict         Kind = "conflict"          // 409
	KindPaymentRequired  Kind = "payment_required"  // 402: kosa8 Cloud wants a payment method first
	KindRateLimit        Kind = "rate_limit"        // 429: too many requests, or a quota reached
	KindUnsupported      Kind = "unsupported"       // 501
	KindStream           Kind = "stream"            // a stream ended with an error frame
	KindCheckpoint       Kind = "checkpoint"        // client-side checkpoint bookkeeping
	KindTimeout          Kind = "timeout"
	KindUsage            Kind = "usage" // bad arguments
)

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// Is lets errors.Is(err, kosa8.ErrNotFound) and friends work.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t.Message != "" {
		return false
	}
	if t.Kind == KindPolicy && e.Kind == KindApprovalRequired {
		return true // an approval refusal is a policy refusal
	}
	if t.Kind == KindAPI {
		switch e.Kind {
		case KindAPI, KindBadRequest, KindAuth, KindPolicy, KindApprovalRequired, KindNotFound, KindConflict, KindPaymentRequired, KindRateLimit, KindUnsupported:
			return true
		}
	}
	return t.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrConnection       = &Error{Kind: KindConnection}
	ErrAPI              = &Error{Kind: KindAPI}
	ErrBadRequest       = &Error{Kind: KindBadRequest}
	ErrAuth             = &Error{Kind: KindAuth}
	ErrPolicy           = &Error{Kind: KindPolicy}
	ErrApprovalRequired = &Error{Kind: KindApprovalRequired}
	ErrNotFound         = &Error{Kind: KindNotFound}
	ErrConflict         = &Error{Kind: KindConflict}
	ErrPaymentRequired  = &Error{Kind: KindPaymentRequired}
	ErrRateLimit        = &Error{Kind: KindRateLimit}
	ErrUnsupported      = &Error{Kind: KindUnsupported}
	ErrStream           = &Error{Kind: KindStream}
	ErrCheckpoint       = &Error{Kind: KindCheckpoint}
	ErrTimeout          = &Error{Kind: KindTimeout}
	ErrUsage            = &Error{Kind: KindUsage}
)

// AsError returns err as *Error, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

func usage(format string, a ...any) error {
	return &Error{Kind: KindUsage, Message: fmt.Sprintf(format, a...)}
}

var byStatus = map[int]Kind{
	400: KindBadRequest, 401: KindAuth, 402: KindPaymentRequired, 403: KindPolicy, 404: KindNotFound, 409: KindConflict,
	429: KindRateLimit, 501: KindUnsupported,
}

// errorFromResponse builds the typed error for an HTTP error answer. The
// message is the daemon's own, with its remediation in parentheses, the way
// the kosa8 CLI and MCP server print it.
func errorFromResponse(status int, body any, method, path string) *Error {
	e := &Error{Kind: KindAPI, Status: status, Body: body, Method: method, Path: path}
	if k, ok := byStatus[status]; ok {
		e.Kind = k
	}
	switch b := body.(type) {
	case map[string]any:
		if s, _ := b["error"].(string); s != "" {
			e.DaemonMessage = s
		} else if s, _ := b["message"].(string); s != "" {
			e.DaemonMessage = s
		}
		if s, _ := b["remediation"].(string); s != "" {
			e.Remediation = s
		}
		if a, ok := b["approval"].(map[string]any); ok && status == 403 {
			if _, ok := a["why"].(string); ok {
				e.Kind = KindApprovalRequired
				e.Approval = a
			}
		}
	case string:
		e.DaemonMessage = strings.TrimSpace(b)
	}
	base := e.DaemonMessage
	if base == "" {
		base = fmt.Sprintf("request failed with HTTP %d", status)
	}
	if e.Remediation != "" {
		e.Message = base + " (" + e.Remediation + ")"
	} else {
		e.Message = base
	}
	return e
}
