package kosa8

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// slept records the waits of retries and pollers; tests never really sleep
// for a backoff.
var (
	sleptMu sync.Mutex
	slept   []time.Duration
)

func TestMain(m *testing.M) {
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		sleptMu.Lock()
		slept = append(slept, d)
		sleptMu.Unlock()
		return ctx.Err()
	}
	os.Exit(m.Run())
}

func resetSlept() {
	sleptMu.Lock()
	slept = nil
	sleptMu.Unlock()
}

func sleeps() []time.Duration {
	sleptMu.Lock()
	defer sleptMu.Unlock()
	return append([]time.Duration(nil), slept...)
}
