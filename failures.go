package kosa8

import (
	"context"
	"errors"
	"net"
	"regexp"
)

// FailureClass says whose fault a failure was, so a trainer running
// thousands of rollouts can mask what the platform got wrong instead of
// scoring it as reward 0. kosa8 Cloud sends it on every error answer under
// /v1/sandboxes and on every exec; ClassifyFailure works it out for any
// error.
type FailureClass string

const (
	FailureOK          FailureClass = "ok"           // the call worked (an exec that exited 0)
	FailureExitNonzero FailureClass = "exit_nonzero" // the command ran and exited non-zero: the caller's code
	FailureTimeout     FailureClass = "timeout"      // the command ran past its limit
	FailureInfra       FailureClass = "infra"        // a host failed, had no room or went away; 5xx
	FailureQuota       FailureClass = "quota"        // a plan, running, payment or spending-cap limit, or a rate limit
	FailureClient      FailureClass = "client"       // the request was wrong: 400, 404, 409, 410, 413, 422
	FailureError       FailureClass = "error"        // not kosa8's answer: your own code failed (never sent by the server)
)

// codeIdempotencyInProgress: a request with this Idempotency-Key is still
// running; the same call answers once it is done.
const codeIdempotencyInProgress = "idempotency_in_progress"

var (
	infraCodes = setOf("host_failed", "not_ready", "no_capacity", "host_unreachable", "host_unavailable", "host_busy",
		"host_restarting", "wake_failed", "try_again", "internal", "kernel_failed", "egress_failed", "dns_unreachable",
		"host_lost", "host_removed", "insufficient_storage", "starting")
	rateCodes  = setOf("rate_limited", "slow_down", "too_soon")
	quotaCodes = setOf("quota_exceeded", "running_limit", "plan_required", "plan_limit", "plan_feature", "payment_required",
		"payment_declined", "spending_cap_reached", "snapshot_limit", "org_frozen", "org_suspended", "too_many_runs")
	timeoutCodeRE = regexp.MustCompile(`timeout`)
)

func setOf(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// FailureOf is kosa8 Cloud's own rule (failures.js) for an HTTP status and
// error code: the class, and whether the same call may work if sent again.
func FailureOf(status int, code string) (FailureClass, bool) {
	switch {
	case status < 400:
		return FailureOK, false
	case code == "exec_timeout":
		return FailureTimeout, false
	case timeoutCodeRE.MatchString(code):
		return FailureTimeout, true
	case rateCodes[code]:
		return FailureQuota, true
	case quotaCodes[code] || status == 402 || status == 423:
		return FailureQuota, false
	case status == 429:
		return FailureQuota, code != "quota_exceeded"
	case code == "not_available" || code == "not_supported" || status == 501:
		return FailureInfra, false
	case infraCodes[code] || status >= 500:
		return FailureInfra, true
	}
	return FailureClient, false
}

// execFailure is the class of an exec that ran: ok on exit 0, exit_nonzero
// on any other exit, infra when the host reported no exit code.
func execFailure(exit *int) (FailureClass, bool) {
	if exit == nil {
		return FailureInfra, true
	}
	if *exit == 0 {
		return FailureOK, false
	}
	return FailureExitNonzero, false
}

// ClassifyFailure is (failure class, retryable) for any error.
//
// A kosa8 Cloud error carries the server's FailureClass and Retryable as
// sent. Otherwise: no answer (a connection error or a timeout) is infra and
// retryable; an exec cut off is timeout; an HTTP answer follows FailureOf;
// 409 idempotency_in_progress is retryable (the first request with that key
// is still running). A cancelled context, a usage error or an error that is
// not the SDK's is FailureError, not retryable: your own code failed.
func ClassifyFailure(err error) (FailureClass, bool) {
	if err == nil {
		return FailureOK, false
	}
	if errors.Is(err, context.Canceled) {
		return FailureError, false
	}
	var ge *GradeError
	if errors.As(err, &ge) {
		return ge.Result.FailureClass, ge.Result.Retryable
	}
	var it *ItemTimeoutError
	if errors.As(err, &it) {
		return FailureTimeout, false
	}
	e, ok := AsError(err)
	if !ok {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
			return FailureInfra, true
		}
		return FailureError, false
	}
	if e.Code == codeIdempotencyInProgress {
		return FailureClient, true
	}
	if e.FailureClass != "" {
		return e.FailureClass, e.Retryable
	}
	switch e.Kind {
	case KindExecTimeout:
		return FailureTimeout, false
	case KindConnection, KindTimeout:
		return FailureInfra, true
	case KindUsage, KindCheckpoint:
		return FailureError, false
	}
	if e.Status == 0 {
		return FailureError, false
	}
	return FailureOf(e.Status, e.Code)
}
