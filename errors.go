package kosa8

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	// UpgradeURL is where a person adds a payment method or credit; kosa8
	// Cloud sends it with a 402. Empty when absent.
	UpgradeURL string
	// AgentMessage is a sentence an AI agent should relay to its person word
	// for word; kosa8 Cloud sends it with a 402 and a 429 quota_exceeded.
	// Empty when absent.
	AgentMessage string
	// RequestURL is where to ask for a higher limit; kosa8 Cloud sends it
	// with a 429 quota_exceeded. Empty when absent.
	RequestURL string
	// Quota is a 402 plan_limit / plan_feature's detail (the limit, what is
	// used, the plan and the ways past it); nil when the answer has none.
	Quota *QuotaDetail
	// RetryAfter is the server's Retry-After header (a 429, usually); zero
	// when it sent none.
	RetryAfter time.Duration
	// FailureClass and Retryable are kosa8 Cloud's word on whose fault the
	// failure was and whether the same call may work if sent again (see
	// ClassifyFailure, which also covers errors without them).
	FailureClass FailureClass
	Retryable    bool
	// RequestID identifies the request to kosa8 support: the answer's
	// X-Request-Id header, else Cloudflare's cf-ray. Empty when absent.
	RequestID string

	// cloud marks an error answer from the kosa8 Cloud API, whose Error()
	// adds the status, code, wait and request id to the sentence.
	cloud bool
}

// QuotaDetail is what a kosa8 Cloud 402 plan_limit or plan_feature says
// about the limit that was reached. Fields the answer left out are zero.
type QuotaDetail struct {
	LimitName string        // sandbox_hours, running, size, persistent or snapshots (Starter: credit, memory, ...)
	Limit     *float64      // the plan's limit
	Used      *float64      // what the organization has used (for size, the vCPU asked for)
	Unit      string        // starter_limit only: the limit's unit, when the answer names one
	Plan      string        // the organization's plan
	NextPlan  string        // the first plan that includes more; "" when none (contact sales)
	Feature   string        // plan_feature: the feature the plan lacks
	PackURL   string        // where to add the pack for this limit, when one is sold
	Options   []QuotaOption // the ways past the limit, cheaper a month first
}

// QuotaOption is one way past a plan limit: an add-on pack (Kind "pack",
// Addon) or the next plan (Kind "upgrade", Plan).
type QuotaOption struct {
	Kind       string  `json:"kind"`
	Addon      string  `json:"addon,omitempty"`
	Plan       string  `json:"plan,omitempty"`
	Label      string  `json:"label,omitempty"`
	MonthlyUSD float64 `json:"monthly_usd,omitempty"`
	URL        string  `json:"url,omitempty"`
}

// quotaOf reads a QuotaDetail from an error body; nil when it has none.
func quotaOf(m map[string]any) *QuotaDetail {
	q := &QuotaDetail{}
	found := false
	str := func(k string) string {
		v, _ := m[k].(string)
		if v != "" {
			found = true
		}
		return v
	}
	num := func(k string) *float64 {
		if v, ok := m[k].(float64); ok {
			found = true
			return &v
		}
		return nil
	}
	q.LimitName = str("limit_name")
	q.Limit = num("limit")
	q.Used = num("used")
	// 402/429 starter_limit nests it: limit: {name, value, used, unit}.
	if lm, ok := m["limit"].(map[string]any); ok {
		found = true
		if v, ok := lm["name"].(string); ok && q.LimitName == "" {
			q.LimitName = v
		}
		if v, ok := lm["value"].(float64); ok {
			q.Limit = &v
		}
		if v, ok := lm["used"].(float64); ok && q.Used == nil {
			q.Used = &v
		}
		q.Unit, _ = lm["unit"].(string)
	}
	q.Plan = str("plan")
	q.NextPlan = str("next_plan")
	q.Feature = str("feature")
	q.PackURL = str("pack_url")
	if opts, ok := m["options"].([]any); ok {
		for _, o := range opts {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			var qo QuotaOption
			qo.Kind, _ = om["kind"].(string)
			qo.Addon, _ = om["addon"].(string)
			qo.Plan, _ = om["plan"].(string)
			qo.Label, _ = om["label"].(string)
			qo.MonthlyUSD, _ = om["monthly_usd"].(float64)
			qo.URL, _ = om["url"].(string)
			q.Options = append(q.Options, qo)
			found = true
		}
	}
	if !found {
		return nil
	}
	return q
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
	KindUnsupported      Kind = "unsupported"       // 501: not_available / not_supported
	KindOrgFrozen        Kind = "org_frozen"        // 423: the org is frozen in the Security Center
	KindExecTimeout      Kind = "exec_timeout"      // a kosa8 Cloud exec ran past the 90 s limit
	KindStream           Kind = "stream"            // a stream ended with an error frame
	KindCheckpoint       Kind = "checkpoint"        // client-side checkpoint bookkeeping
	KindTimeout          Kind = "timeout"
	KindUsage            Kind = "usage" // bad arguments
)

// Error is the message; for a kosa8 Cloud API answer it is followed by what
// to do about a refused key and by the status, code, wait and request id:
//
//	kosa8 Cloud has no room right now [HTTP 503 no_capacity; retry in 10 s; request a5e7...-BOS]
func (e *Error) Error() string {
	if !e.cloud {
		return e.Message
	}
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Status == 401 {
		b.WriteString(" (" + keyHint + ")")
	}
	b.WriteString(" [HTTP ")
	b.WriteString(strconv.Itoa(e.Status))
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.RetryAfter > 0 {
		b.WriteString("; retry in " + strconv.FormatFloat(e.RetryAfter.Seconds(), 'f', -1, 64) + " s")
	} else if e.Retryable {
		b.WriteString("; retryable")
	}
	if e.UpgradeURL != "" && !strings.Contains(e.Message, e.UpgradeURL) {
		b.WriteString("; upgrade: " + e.UpgradeURL)
	}
	if e.RequestID != "" {
		b.WriteString("; request " + e.RequestID)
	}
	b.WriteString("]")
	return b.String()
}

// keyHint is what to do about a missing or refused API key.
const keyHint = "set KOSA8_API_KEY or pass CloudOptions.APIKey; make a key at https://app.kosa8.com/app#keys"

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
	if t.Kind == KindTimeout && e.Kind == KindExecTimeout {
		return true // an exec cut off by the cloud is a timeout too
	}
	if t.Kind == KindAPI {
		switch e.Kind {
		case KindAPI, KindBadRequest, KindAuth, KindPolicy, KindApprovalRequired, KindNotFound, KindConflict, KindPaymentRequired, KindRateLimit, KindUnsupported, KindOrgFrozen, KindExecTimeout:
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
	// ErrOrgFrozen: kosa8 Cloud answered 423 org_frozen. An owner or admin
	// froze the organization in the Security Center and must unfreeze it.
	ErrOrgFrozen = &Error{Kind: KindOrgFrozen}
	// ErrExecTimeout: a kosa8 Cloud exec was cut off (504, code exec_timeout,
	// or a 502 host_failed on /exec that says "timed out"). A buffered exec
	// gets at most 90 s; run long jobs in the background and poll. Also
	// matches ErrTimeout and ErrAPI.
	ErrExecTimeout = &Error{Kind: KindExecTimeout}
	ErrStream      = &Error{Kind: KindStream}
	ErrCheckpoint  = &Error{Kind: KindCheckpoint}
	ErrTimeout     = &Error{Kind: KindTimeout}
	ErrUsage       = &Error{Kind: KindUsage}
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
