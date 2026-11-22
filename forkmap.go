package kosa8

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// preforkMax is the most children forked ahead in one call.
const preforkMax = 100

// cleanupTimeout bounds the deletes a pool makes when it closes, even after
// the caller's context has ended.
const cleanupTimeout = 60 * time.Second

// ErrCancelled is the error of an item that never ran: the map's context
// ended (or the pool closed) first.
var ErrCancelled = errors.New("kosa8: cancelled before it ran")

// ItemTimeoutError is an item that ran past its Timeout; its sandbox was
// deleted, so the abandoned call fails fast.
type ItemTimeoutError struct{ Timeout time.Duration }

func (e *ItemTimeoutError) Error() string {
	return fmt.Sprintf("gave up after %v; its sandbox was deleted", e.Timeout)
}

// childPool hands out sandboxes (forks of base, or made by mk) to workers.
// Every child it made and has not released is tracked, so close deletes
// them all, even mid-call.
type childPool struct {
	base   *CloudSandbox
	c      *Cloud
	mk     func(ctx context.Context) (*CloudSandbox, error)
	labels map[string]string
	runID  string
	leaseS int

	mu     sync.Mutex
	ready  []*CloudSandbox
	live   map[string]*CloudSandbox
	closed bool
}

func newChildPool(c *Cloud, base *CloudSandbox, labels map[string]string, runID string, leaseS int) *childPool {
	return &childPool{c: c, base: base, labels: labels, runID: runID, leaseS: leaseS, live: map[string]*CloudSandbox{}}
}

func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// adopt tracks kids; if the pool closed meanwhile they are deleted instead.
func (p *childPool) adopt(ctx context.Context, kids []*CloudSandbox) error {
	p.mu.Lock()
	closed := p.closed
	if !closed {
		for _, k := range kids {
			p.live[k.ID()] = k
		}
	}
	p.mu.Unlock()
	if closed {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		for _, k := range kids {
			quietDelete(cctx, k)
		}
		return ErrCancelled
	}
	return nil
}

func (p *childPool) fork(ctx context.Context, n int, partial *bool) ([]*CloudSandbox, error) {
	r, err := p.base.ForkWith(ctx, CloudForkOptions{N: n, Labels: p.labels, RunID: p.runID, LeaseS: p.leaseS, PartialOK: partial})
	if r != nil {
		if aerr := p.adopt(ctx, r.Sandboxes); aerr != nil {
			return nil, aerr
		}
	}
	if err != nil {
		return nil, err
	}
	if len(r.Sandboxes) == 0 {
		if len(r.Failed) > 0 {
			return nil, r.Failed[0].Err()
		}
		return nil, &Error{Kind: KindAPI, Message: "the fork made no sandbox", FailureClass: FailureInfra, Retryable: true}
	}
	return r.Sandboxes, nil
}

// prefork forks up to n children ahead, in calls of at most 100. Failures
// are ignored: each worker then forks its own and sees the error.
func (p *childPool) prefork(ctx context.Context, n int) {
	if p.mk != nil {
		return
	}
	for n > 0 {
		k := n
		if k > preforkMax {
			k = preforkMax
		}
		kids, err := p.fork(ctx, k, Ptr(true))
		if err != nil {
			return
		}
		p.mu.Lock()
		p.ready = append(p.ready, kids...)
		p.mu.Unlock()
		n -= k
	}
}

func (p *childPool) take(ctx context.Context) (*CloudSandbox, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrCancelled
	}
	if len(p.ready) > 0 {
		k := p.ready[len(p.ready)-1]
		p.ready = p.ready[:len(p.ready)-1]
		p.mu.Unlock()
		return k, nil
	}
	p.mu.Unlock()
	if p.mk != nil {
		k, err := p.mk(ctx)
		if k != nil {
			if aerr := p.adopt(ctx, []*CloudSandbox{k}); aerr != nil {
				return nil, aerr
			}
		}
		if err != nil {
			return nil, err
		}
		return k, nil
	}
	kids, err := p.fork(ctx, 1, nil)
	if err != nil {
		return nil, err
	}
	return kids[0], nil
}

// release deletes a child taken from the pool (once; later calls do nothing).
func (p *childPool) release(ctx context.Context, k *CloudSandbox) {
	p.mu.Lock()
	_, mine := p.live[k.ID()]
	delete(p.live, k.ID())
	p.mu.Unlock()
	if mine {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		quietDelete(cctx, k)
	}
}

// forget stops tracking a child without deleting it (Keep).
func (p *childPool) forget(k *CloudSandbox) {
	p.mu.Lock()
	delete(p.live, k.ID())
	p.mu.Unlock()
}

func (p *childPool) liveIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.live))
	for id := range p.live {
		out = append(out, id)
	}
	return out
}

// close deletes every child not yet released (queued or in use): one drain
// call per 1000, then one by one for anything it missed.
func (p *childPool) close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	kids := make([]*CloudSandbox, 0, len(p.live))
	for _, k := range p.live {
		kids = append(kids, k)
	}
	p.live = map[string]*CloudSandbox{}
	p.ready = nil
	p.mu.Unlock()
	if len(kids) == 0 {
		return
	}
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	for i := 0; i < len(kids); i += CloudMaxBatch {
		chunk := kids[i:min(i+CloudMaxBatch, len(kids))]
		ids := make([]string, len(chunk))
		for j, k := range chunk {
			ids[j] = k.ID()
		}
		r, err := p.c.Sandboxes.Drain(cctx, CloudSelector{IDs: ids})
		if err == nil && len(r.Failed) == 0 {
			continue
		}
		gone := map[string]bool{}
		for _, id := range r.IDs {
			gone[id] = true
		}
		for _, k := range chunk {
			if !gone[k.ID()] {
				quietDelete(cctx, k)
			}
		}
	}
}

// callWithTimeout runs fn; past timeout it calls onTimeout (delete the
// sandbox, so the abandoned call fails fast) and returns ItemTimeoutError.
// A panic in fn is returned as an error.
func callWithTimeout[R any](ctx context.Context, timeout time.Duration, fn func(ctx context.Context) (R, error), onTimeout func()) (R, error) {
	type res struct {
		v   R
		err error
	}
	run := func(ctx context.Context) (out res) {
		defer func() {
			if p := recover(); p != nil {
				out.err = fmt.Errorf("panic: %v", p)
			}
		}()
		v, err := fn(ctx)
		return res{v, err}
	}
	if timeout <= 0 {
		r := run(ctx)
		return r.v, r.err
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ch := make(chan res, 1)
	go func() { ch <- run(ictx) }()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ictx.Done():
		var zero R
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		onTimeout()
		return zero, &ItemTimeoutError{Timeout: timeout}
	}
}

// runPool runs worker(i) for i in [0, n) on at most concurrency goroutines.
func runPool(ctx context.Context, n, concurrency int, worker func(i int)) {
	if concurrency > n {
		concurrency = n
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				worker(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

// ForkMapOptions configure ForkMap.
type ForkMapOptions struct {
	Concurrency int // items at once; default 16
	// Policy retries an item that failed retryably (infra, a rate limit) on
	// a fresh fork, with backoff and jitter. Default DefaultRetryPolicy() (3
	// attempts); &NoRetry tries once.
	Policy  *RetryPolicy
	Timeout time.Duration // per attempt; 0 none. Past it the child is deleted
	Labels  map[string]string
	RunID   string // labels every child kosa8.run=<RunID>
	LeaseS  int    // leases every child, so a crashed process leaks none
}

// ForkMapResult is one item's outcome.
type ForkMapResult[T, R any] struct {
	Item         T
	Value        R
	Err          error
	FailureClass FailureClass // ok, or the class of Err (see ClassifyFailure)
	Attempts     int
	SandboxID    string // the child of the last attempt
	Duration     time.Duration
}

// OK reports whether the item succeeded.
func (r ForkMapResult[T, R]) OK() bool { return r.Err == nil }

// ForkMap runs fn(ctx, child, item) for every item, each in its own fork of
// base, at most Concurrency at once, and returns the results in item order.
// Retryable failures are retried on a fresh fork; an item's error is in its
// result, not returned. Every child is deleted when its item is done, and
// any still alive when ForkMap returns (an error, a panic, ctx ending) are
// deleted then. The error is only for bad options or ctx ending.
func ForkMap[T, R any](ctx context.Context, base *CloudSandbox, items []T, fn func(ctx context.Context, child *CloudSandbox, item T) (R, error), o *ForkMapOptions) ([]ForkMapResult[T, R], error) {
	if base == nil || fn == nil {
		return nil, usage("ForkMap needs a base sandbox and a function")
	}
	if o == nil {
		o = &ForkMapOptions{}
	}
	conc := o.Concurrency
	if conc == 0 {
		conc = 16
	}
	if conc < 1 {
		return nil, usage("Concurrency must be at least 1")
	}
	policy := DefaultRetryPolicy()
	if o.Policy != nil {
		policy = *o.Policy
	}
	out := make([]ForkMapResult[T, R], len(items))
	if len(items) == 0 {
		return out, nil
	}
	pool := newChildPool(base.c, base, o.Labels, o.RunID, o.LeaseS)
	defer pool.close(ctx)
	pool.prefork(ctx, min(len(items), conc))
	runPool(ctx, len(items), conc, func(i int) {
		res := &out[i]
		res.Item = items[i]
		t0 := time.Now()
		defer func() { res.Duration = time.Since(t0) }()
		for attempt := 1; ; attempt++ {
			res.Attempts = attempt
			if ctx.Err() != nil {
				res.Err, res.FailureClass = ErrCancelled, FailureError
				return
			}
			child, err := pool.take(ctx)
			if err == nil {
				res.SandboxID = child.ID()
				res.Value, err = callWithTimeout(ctx, o.Timeout, func(ctx context.Context) (R, error) {
					return fn(ctx, child, items[i])
				}, func() { pool.release(ctx, child) })
				pool.release(ctx, child)
			}
			if err == nil {
				res.Err, res.FailureClass = nil, FailureOK
				return
			}
			var zero R
			res.Value, res.Err = zero, err
			fc, retryable := ClassifyFailure(err)
			res.FailureClass = fc
			if !retryable || attempt >= policy.attempts() {
				return
			}
			if sleepCtx(ctx, policy.Delay(err, attempt)) != nil {
				return
			}
		}
	})
	return out, ctx.Err()
}
