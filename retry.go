package kosa8

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// RetryPolicy is how often and how long to retry a failure that can pass:
// infra (no answer, a 5xx, a host or capacity problem) and rate limits.
// Quota, payment and bad requests are returned at once.
//
// Attempts counts the first try (1 turns retries off). The n-th retry waits
// a random time up to min(Max, Base * 2^(n-1)) ("full jitter", so a
// thousand rollouts that failed together do not come back together), and
// never less than the server's Retry-After (capped at MaxRetryAfter).
type RetryPolicy struct {
	Attempts      int
	Base          time.Duration
	Max           time.Duration
	MaxRetryAfter time.Duration
}

// DefaultRetryPolicy is 3 tries, 0.5 s base, 8 s cap, Retry-After up to 60 s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Attempts: 3, Base: 500 * time.Millisecond, Max: 8 * time.Second, MaxRetryAfter: 60 * time.Second}
}

// NoRetry tries once.
var NoRetry = RetryPolicy{Attempts: 1}

func (p RetryPolicy) attempts() int {
	if p.Attempts < 1 {
		return 1
	}
	return p.Attempts
}

// ShouldRetry reports whether to try again after err on attempt (1-based).
func (p RetryPolicy) ShouldRetry(err error, attempt int) bool {
	if attempt >= p.attempts() {
		return false
	}
	_, retryable := ClassifyFailure(err)
	return retryable
}

var (
	jitterMu  sync.Mutex
	jitterRNG = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// Delay is how long to wait after err on attempt (1-based) before the next.
func (p RetryPolicy) Delay(err error, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := p.Base
	for i := 1; i < attempt && ceil < p.Max; i++ {
		ceil *= 2
	}
	if ceil > p.Max {
		ceil = p.Max
	}
	var wait time.Duration
	if ceil > 0 {
		jitterMu.Lock()
		wait = time.Duration(jitterRNG.Int63n(int64(ceil) + 1))
		jitterMu.Unlock()
	}
	if e, ok := AsError(err); ok && e.RetryAfter > 0 {
		ra := e.RetryAfter
		if p.MaxRetryAfter > 0 && ra > p.MaxRetryAfter {
			ra = p.MaxRetryAfter
		}
		if ra > wait {
			wait = ra
		}
	}
	return wait
}

// sleepCtx waits d or until ctx is done. A seam for tests.
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Retry calls fn until it succeeds, retrying what p allows with backoff and
// jitter. It returns fn's last error, or ctx's error if ctx ends while
// waiting.
func Retry(ctx context.Context, p RetryPolicy, fn func(ctx context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || !p.ShouldRetry(err, attempt) {
			return err
		}
		if serr := sleepCtx(ctx, p.Delay(err, attempt)); serr != nil {
			return err
		}
	}
}
