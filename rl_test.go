package kosa8

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReq is one request the fake kosa8 Cloud saw.
type fakeReq struct {
	Method, Path, Key string
	Body              map[string]any
	Raw               []byte
}

// fakeAns is its answer.
type fakeAns struct {
	status  int
	body    any
	headers map[string]string
	raw     []byte
}

type fakeCloud struct {
	mu   sync.Mutex
	seen []fakeReq
}

func (f *fakeCloud) reqs(method, prefix string) []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.seen {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func newFake(t *testing.T, answer func(r fakeReq) fakeAns, opts ...func(*CloudOptions)) (*fakeCloud, *Cloud) {
	t.Helper()
	f := &fakeCloud{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fr := fakeReq{Method: r.Method, Path: r.URL.RequestURI(), Key: r.Header.Get("Idempotency-Key"), Raw: raw}
		_ = json.Unmarshal(raw, &fr.Body)
		f.mu.Lock()
		f.seen = append(f.seen, fr)
		f.mu.Unlock()
		a := answer(fr)
		for k, v := range a.headers {
			w.Header().Set(k, v)
		}
		if a.status == 0 {
			a.status = 200
		}
		if a.raw != nil {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(a.status)
			_, _ = w.Write(a.raw)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		if a.body != nil {
			_ = json.NewEncoder(w).Encode(a.body)
		}
	}))
	t.Cleanup(srv.Close)
	o := CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Getenv: func(string) string { return "" }}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := NewCloud(o)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func box(id string) map[string]any {
	return map[string]any{"id": id, "state": "running", "template": "python", "labels": map[string]any{}}
}

func errBody(code, msg string, extra map[string]any) map[string]any {
	b := map[string]any{"error": code, "message": msg}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func TestFailureOfMatchesServer(t *testing.T) {
	cases := []struct {
		status int
		code   string
		fc     FailureClass
		retry  bool
	}{
		{200, "", FailureOK, false},
		{504, "exec_timeout", FailureTimeout, false},
		{408, "request_timeout", FailureTimeout, true},
		{429, "rate_limited", FailureQuota, true},
		{429, "slow_down", FailureQuota, true},
		{429, "quota_exceeded", FailureQuota, false},
		{429, "other", FailureQuota, true},
		{402, "payment_required", FailureQuota, false},
		{423, "org_frozen", FailureQuota, false},
		{403, "plan_limit", FailureQuota, false},
		{501, "not_available", FailureInfra, false},
		{400, "not_supported", FailureInfra, false},
		{503, "no_capacity", FailureInfra, true},
		{409, "not_ready", FailureInfra, true},
		{500, "", FailureInfra, true},
		{502, "host_failed", FailureInfra, true},
		{400, "invalid_count", FailureClient, false},
		{404, "not_found", FailureClient, false},
		{409, "secrets", FailureClient, false},
	}
	for _, tc := range cases {
		fc, rt := FailureOf(tc.status, tc.code)
		if fc != tc.fc || rt != tc.retry {
			t.Errorf("%d %s: %s %v, want %s %v", tc.status, tc.code, fc, rt, tc.fc, tc.retry)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		err   error
		fc    FailureClass
		retry bool
	}{
		{nil, FailureOK, false},
		{errors.New("mine"), FailureError, false},
		{context.Canceled, FailureError, false},
		{context.DeadlineExceeded, FailureInfra, true},
		{&Error{Kind: KindConnection}, FailureInfra, true},
		{&Error{Kind: KindTimeout}, FailureInfra, true},
		{&Error{Kind: KindExecTimeout, Status: 504}, FailureTimeout, false},
		{&Error{Kind: KindUsage}, FailureError, false},
		{&Error{Kind: KindAPI, Status: 409, Code: "idempotency_in_progress", FailureClass: FailureClient}, FailureClient, true},
		{&Error{Kind: KindAPI, Status: 400, FailureClass: FailureInfra, Retryable: true}, FailureInfra, true},
		{&Error{Kind: KindAPI, Status: 503}, FailureInfra, true},
		{&Error{Kind: KindAPI}, FailureError, false},
		{&ItemTimeoutError{Timeout: time.Second}, FailureTimeout, false},
		{&GradeError{Result: CloudGradeResult{FailureClass: FailureInfra, Retryable: true}}, FailureInfra, true},
		{fmt.Errorf("wrapped: %w", &Error{Kind: KindAPI, Status: 402, Code: "payment_required"}), FailureQuota, false},
	}
	for i, tc := range cases {
		fc, rt := ClassifyFailure(tc.err)
		if fc != tc.fc || rt != tc.retry {
			t.Errorf("%d %v: %s %v, want %s %v", i, tc.err, fc, rt, tc.fc, tc.retry)
		}
	}
}

func TestErrorsCarryFailureClass(t *testing.T) {
	_, c := newFake(t, func(r fakeReq) fakeAns {
		if strings.HasSuffix(r.Path, "/a") {
			return fakeAns{status: 409, body: errBody("secrets", "has secrets", map[string]any{"failure_class": "client", "retryable": false})}
		}
		if strings.HasSuffix(r.Path, "/b") {
			return fakeAns{status: 400, body: errBody("odd", "server said infra", map[string]any{"failure_class": "infra"})}
		}
		return fakeAns{status: 402, body: errBody("payment_required", "pay", nil)}
	}, func(o *CloudOptions) { o.Retry = &NoRetry })
	ctx := context.Background()
	err := c.request(ctx, "GET", "/v1/sandboxes/a", nil, nil)
	e, _ := AsError(err)
	if e.FailureClass != FailureClient || e.Retryable {
		t.Fatalf("%+v", e)
	}
	err = c.request(ctx, "GET", "/v1/sandboxes/b", nil, nil)
	if e, _ := AsError(err); e.FailureClass != FailureInfra || !e.Retryable {
		t.Fatalf("defaulted retryable: %+v", e)
	}
	err = c.request(ctx, "GET", "/v1/me", nil, nil)
	if e, _ := AsError(err); e.FailureClass != FailureQuota || e.Retryable {
		t.Fatalf("derived: %+v", e)
	}
}

func TestExecResultFailureClass(t *testing.T) {
	var answer atomic.Value
	_, c := newFake(t, func(r fakeReq) fakeAns { return fakeAns{body: answer.Load()} })
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1"}}
	cases := []struct {
		body  map[string]any
		fc    FailureClass
		retry bool
	}{
		{map[string]any{"exit": 0, "stdout": "hi"}, FailureOK, false},
		{map[string]any{"exit": 3}, FailureExitNonzero, false},
		{map[string]any{"exit": nil}, FailureInfra, true},
		{map[string]any{"exit": 1, "failure_class": "exit_nonzero", "retryable": false}, FailureExitNonzero, false},
		{map[string]any{"failure_class": "infra", "retryable": true}, FailureInfra, true},
	}
	for _, tc := range cases {
		answer.Store(tc.body)
		r, err := b.Exec(context.Background(), []string{"true"}, nil)
		if err != nil || r.FailureClass != tc.fc || r.Retryable != tc.retry {
			t.Errorf("%v: %+v %v", tc.body, r, err)
		}
	}
}

func TestRetryPolicyDelay(t *testing.T) {
	p := RetryPolicy{Attempts: 5, Base: 100 * time.Millisecond, Max: time.Second, MaxRetryAfter: 30 * time.Second}
	for attempt, ceil := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 400 * time.Millisecond, 5: time.Second, 9: time.Second} {
		for i := 0; i < 50; i++ {
			if d := p.Delay(nil, attempt); d < 0 || d > ceil {
				t.Fatalf("attempt %d: %v over %v", attempt, d, ceil)
			}
		}
	}
	ra := &Error{Kind: KindRateLimit, Status: 429, RetryAfter: 7 * time.Second}
	if d := p.Delay(ra, 1); d != 7*time.Second {
		t.Fatalf("retry-after: %v", d)
	}
	ra.RetryAfter = time.Hour
	if d := p.Delay(ra, 1); d != 30*time.Second {
		t.Fatalf("capped: %v", d)
	}
	if !p.ShouldRetry(&Error{Kind: KindConnection}, 4) || p.ShouldRetry(&Error{Kind: KindConnection}, 5) {
		t.Fatal("attempts")
	}
	if p.ShouldRetry(&Error{Kind: KindAPI, Status: 400}, 1) || NoRetry.ShouldRetry(&Error{Kind: KindConnection}, 1) {
		t.Fatal("non-retryable")
	}
	if DefaultRetryPolicy().Attempts != 3 || (RetryPolicy{}).attempts() != 1 {
		t.Fatal("defaults")
	}
	// Retry helper: stops on success, on a non-retryable error, and at the limit.
	n := 0
	err := Retry(context.Background(), p, func(context.Context) error {
		n++
		if n < 3 {
			return &Error{Kind: KindConnection}
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("%v %d", err, n)
	}
	n = 0
	_ = Retry(context.Background(), p, func(context.Context) error { n++; return errors.New("bug") })
	if n != 1 {
		t.Fatal(n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n = 0
	err = Retry(ctx, p, func(context.Context) error { n++; return &Error{Kind: KindConnection} })
	if n != 1 || !errors.Is(err, ErrConnection) {
		t.Fatalf("cancelled: %d %v", n, err)
	}
}

func TestClientRetriesWhatIsSafe(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	_, c := newFake(t, func(r fakeReq) fakeAns {
		n := calls.Add(1)
		if n == 1 {
			return fakeAns{status: 429, body: errBody("rate_limited", "slow", nil), headers: map[string]string{"Retry-After": "4"}}
		}
		if n == 2 {
			return fakeAns{status: 503, body: errBody("no_capacity", "full", nil)}
		}
		return fakeAns{body: map[string]any{"org": "o"}}
	})
	resetSlept()
	if _, err := c.Me(ctx); err != nil || calls.Load() != 3 {
		t.Fatalf("GET: %v after %d", err, calls.Load())
	}
	if s := sleeps(); len(s) != 2 || s[0] < 4*time.Second {
		t.Fatalf("waits %v", s)
	}
	// An unkeyed POST (exec) is not resent after a 5xx, but is after a 429.
	calls.Store(1)
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1"}}
	if _, err := b.Exec(ctx, []string{"x"}, nil); !errors.Is(err, ErrAPI) || calls.Load() != 2 {
		t.Fatalf("exec retried a 503: %v %d", err, calls.Load())
	}
	calls.Store(0)
	if _, err := b.Exec(ctx, []string{"x"}, nil); err == nil || calls.Load() != 2 {
		t.Fatalf("exec 429 then 503: %v %d", err, calls.Load())
	}
	// Non-retryable: once.
	_, c2 := newFake(t, func(fakeReq) fakeAns {
		calls.Add(1)
		return fakeAns{status: 402, body: errBody("payment_required", "pay", nil)}
	})
	calls.Store(0)
	if _, err := c2.Me(ctx); !errors.Is(err, ErrPaymentRequired) || calls.Load() != 1 {
		t.Fatalf("402: %v %d", err, calls.Load())
	}
	// NoRetry: once, even a 503.
	_, c3 := newFake(t, func(fakeReq) fakeAns {
		calls.Add(1)
		return fakeAns{status: 503, body: errBody("no_capacity", "full", nil)}
	}, func(o *CloudOptions) { o.Retry = &NoRetry })
	calls.Store(0)
	if _, err := c3.Me(ctx); err == nil || calls.Load() != 1 {
		t.Fatalf("NoRetry: %v %d", err, calls.Load())
	}
}

func TestIdempotencyKeyReusedOnRetry(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	f, c := newFake(t, func(r fakeReq) fakeAns {
		if calls.Add(1) == 1 {
			return fakeAns{status: 503, body: errBody("host_failed", "down", nil)}
		}
		return fakeAns{status: 201, body: map[string]any{"sandbox": box("sbx_9")}}
	})
	b, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Template: "python", LeaseS: 300})
	if err != nil || b.ID() != "sbx_9" {
		t.Fatalf("%v %v", b, err)
	}
	rs := f.reqs("POST", "/v1/sandboxes")
	if len(rs) != 2 || rs[0].Key == "" || rs[0].Key != rs[1].Key || len(rs[0].Key) != 36 {
		t.Fatalf("keys %q %q", rs[0].Key, rs[len(rs)-1].Key)
	}
	if rs[0].Body["lease_s"] != 300.0 {
		t.Fatalf("lease: %v", rs[0].Body)
	}
	// The caller's key is sent as given; another create gets a new one.
	calls.Store(1)
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{IdempotencyKey: "my-key-1"}); err != nil {
		t.Fatal(err)
	}
	calls.Store(1)
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rs = f.reqs("POST", "/v1/sandboxes")
	if rs[2].Key != "my-key-1" || rs[3].Key == "" || rs[3].Key == rs[0].Key {
		t.Fatalf("%q %q", rs[2].Key, rs[3].Key)
	}
	// 409 idempotency_in_progress is retried (after its retry_after_s).
	calls.Store(0)
	resetSlept()
	_, c2 := newFake(t, func(r fakeReq) fakeAns {
		if calls.Add(1) == 1 {
			return fakeAns{status: 409, body: errBody("idempotency_in_progress", "wait", map[string]any{"retry_after_s": 2})}
		}
		return fakeAns{status: 201, body: map[string]any{"sandbox": box("sbx_2")}}
	})
	if _, err := c2.Sandboxes.Create(ctx, CloudCreateOptions{}); err != nil || calls.Load() != 2 {
		t.Fatalf("in progress: %v %d", err, calls.Load())
	}
	if s := sleeps(); len(s) != 1 || s[0] < 2*time.Second {
		t.Fatalf("waits %v", s)
	}
	// Bad keys and lease options are refused before any request.
	n := len(f.reqs("POST", "/"))
	for _, o := range []CloudCreateOptions{{IdempotencyKey: "has space"}, {IdempotencyKey: strings.Repeat("k", 256)},
		{LeaseS: 30}, {LeaseS: 90000}, {LeaseS: 600, TTLSeconds: 600}} {
		if _, err := c.Sandboxes.Create(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if len(f.reqs("POST", "/")) != n {
		t.Fatal("refused create sent a request")
	}
	if keyedRoute("POST", "/v1/sandboxes/sbx_1/exec") || !keyedRoute("POST", "/v1/sandboxes/sbx_1/grade?x=1") || keyedRoute("GET", "/v1/sandboxes") {
		t.Fatal("keyedRoute")
	}
}

func TestCreateMany(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(t, func(r fakeReq) fakeAns {
		if r.Path == "/v1/sandboxes/batch" {
			return fakeAns{body: map[string]any{
				"sandboxes": []any{box("sbx_a"), box("sbx_b")},
				"failed":    []any{map[string]any{"index": 2, "code": "running_limit", "message": "past the limit", "failure_class": "quota", "retryable": false}},
				"requested": 3, "created": 2, "run_id": "run_1",
			}}
		}
		return fakeAns{status: 404, body: errBody("not_found", "no", nil)}
	})
	b, err := c.Sandboxes.CreateMany(ctx, CloudBatchOptions{
		CloudCreateOptions: CloudCreateOptions{Template: "python", LeaseS: 600, Labels: map[string]string{"team": "rl"}, IdempotencyKey: "batch-1"},
		Count:              3, RunID: "run_1", Prefix: "roll", PartialOK: Ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Created != 2 || b.Requested != 3 || b.RunID != "run_1" || len(b.Failed) != 1 || *b.Failed[0].Index != 2 || b.Failed[0].FailureClass != FailureQuota {
		t.Fatalf("%+v", b)
	}
	if !reflect.DeepEqual(b.IDs(), []string{"sbx_a", "sbx_b"}) || b.Sandboxes[0].c != c {
		t.Fatal(b.IDs())
	}
	if e := b.Failed[0].Err(); !strings.Contains(e.Error(), "past the limit") {
		t.Fatal(e)
	}
	r := f.reqs("POST", "/v1/sandboxes/batch")[0]
	want := map[string]any{"count": 3.0, "template": "python", "lease_s": 600.0, "labels": map[string]any{"team": "rl"}, "run_id": "run_1", "prefix": "roll", "partial_ok": true}
	if !reflect.DeepEqual(r.Body, want) || r.Key != "batch-1" {
		t.Fatalf("%v %q", r.Body, r.Key)
	}
	for _, o := range []CloudBatchOptions{{Count: 0}, {Count: 1001}, {Count: 2, Prefix: "-bad"}, {Count: 2, RunID: "bad id"},
		{Count: 2, CloudCreateOptions: CloudCreateOptions{Name: "x"}}, {Count: 2, CloudCreateOptions: CloudCreateOptions{LeaseS: 600, TTLSeconds: 600}}} {
		if _, err := c.Sandboxes.CreateMany(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

func TestForkWith(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(t, func(r fakeReq) fakeAns {
		if r.Body["n"] == 5.0 {
			return fakeAns{status: 503, body: errBody("no_capacity", "host full", map[string]any{"requested": 5, "created": 0, "failure_class": "infra", "retryable": true})}
		}
		return fakeAns{status: 201, body: map[string]any{
			"parent": "sbx_p", "sandboxes": []any{box("sbx_c1"), box("sbx_c2")}, "requested": 3, "created": 2,
			"failed": []any{map[string]any{"index": 2, "code": "no_capacity", "message": "host full", "failure_class": "infra", "retryable": true}},
		}}
	}, func(o *CloudOptions) { o.Retry = &NoRetry })
	p := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_p"}}
	r, err := p.ForkWith(ctx, CloudForkOptions{N: 3, Labels: map[string]string{"a": "b"}, RunID: "run_1", LeaseS: 120, Prefix: "kid", PartialOK: Ptr(true), IdempotencyKey: "fk"})
	if err != nil || r.Created != 2 || len(r.Failed) != 1 || r.Failed[0].Code != "no_capacity" {
		t.Fatalf("%+v %v", r, err)
	}
	q := f.reqs("POST", "/v1/sandboxes/sbx_p/fork")[0]
	want := map[string]any{"n": 3.0, "labels": map[string]any{"a": "b"}, "run_id": "run_1", "lease_s": 120.0, "prefix": "kid", "partial_ok": true}
	if !reflect.DeepEqual(q.Body, want) || q.Key != "fk" {
		t.Fatalf("%v %q", q.Body, q.Key)
	}
	_, err = p.ForkWith(ctx, CloudForkOptions{N: 5})
	if e, _ := AsError(err); e == nil || e.FailureClass != FailureInfra || !e.Retryable {
		t.Fatalf("nothing made: %v", err)
	}
	// A plain Fork over 100 stays all or nothing.
	if _, err := p.Fork(ctx, 250); err != nil {
		t.Fatal(err)
	}
	if q := f.reqs("POST", "/v1/sandboxes/sbx_p/fork"); q[len(q)-1].Body["partial_ok"] != false || q[len(q)-1].Key == "" {
		t.Fatalf("%v", q[len(q)-1].Body)
	}
	for _, o := range []CloudForkOptions{{N: -1}, {N: 1001}, {LeaseS: 59}, {RunID: "!"}, {Prefix: strings.Repeat("p", 60)}, {Labels: map[string]string{"bad key!": "v"}}} {
		if _, err := p.ForkWith(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

func TestHeartbeatAndDrain(t *testing.T) {
	ctx := context.Background()
	var drains atomic.Int32
	f, c := newFake(t, func(r fakeReq) fakeAns {
		switch r.Path {
		case "/v1/sandboxes/heartbeat":
			return fakeAns{body: map[string]any{"extended": 7, "at": 1}}
		case "/v1/sandboxes/drain":
			switch drains.Add(1) {
			case 1:
				return fakeAns{body: map[string]any{"deleted": 1000, "ids": []string{"sbx_1"}, "failed": []any{}, "more": true}}
			case 2:
				return fakeAns{body: map[string]any{"deleted": 2, "ids": []string{"sbx_2", "sbx_3"},
					"failed": []any{map[string]any{"id": "sbx_4", "code": "host_failed", "message": "x", "failure_class": "infra", "retryable": true}}, "more": false}}
			}
		}
		return fakeAns{status: 500}
	})
	n, err := c.Sandboxes.Heartbeat(ctx, CloudSelector{RunID: "run_1", Labels: map[string]string{"k": "v"}}, 300)
	if err != nil || n != 7 {
		t.Fatal(n, err)
	}
	hb := f.reqs("POST", "/v1/sandboxes/heartbeat")[0].Body
	if !reflect.DeepEqual(hb, map[string]any{"run_id": "run_1", "labels": map[string]any{"k": "v"}, "extend_s": 300.0}) {
		t.Fatal(hb)
	}
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_9"}}
	if n, err := b.Heartbeat(ctx, 0); err != nil || n != 7 {
		t.Fatal(n, err)
	}
	if hb := f.reqs("POST", "/v1/sandboxes/heartbeat")[1].Body; !reflect.DeepEqual(hb, map[string]any{"ids": []any{"sbx_9"}}) {
		t.Fatal(hb)
	}
	d, err := c.Sandboxes.Drain(ctx, CloudSelector{RunID: "run_1"})
	if err != nil || d.Deleted != 1002 || len(d.IDs) != 3 || len(d.Failed) != 1 || d.Failed[0].ID != "sbx_4" || drains.Load() != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	for _, sel := range []CloudSelector{{}, {RunID: "bad id"}, {IDs: []string{"no/slash"}}, {IDs: make([]string, 1001)}} {
		if _, err := c.Sandboxes.Drain(ctx, sel); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", sel, err)
		}
	}
	if _, err := c.Sandboxes.Heartbeat(ctx, CloudSelector{RunID: "r"}, 10); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	// A drain call that deletes none stops the loop even if it says more.
	drains.Store(0)
	_, c2 := newFake(t, func(fakeReq) fakeAns {
		drains.Add(1)
		return fakeAns{body: map[string]any{"deleted": 0, "ids": []string{}, "failed": []any{}, "more": true}}
	})
	if _, err := c2.Sandboxes.Drain(ctx, CloudSelector{IDs: []string{"sbx_1"}}); err != nil || drains.Load() != 1 {
		t.Fatal(err, drains.Load())
	}
}

func TestGrade(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(t, func(r fakeReq) fakeAns {
		return fakeAns{body: map[string]any{
			"reward": 0.5, "metrics": map[string]any{"passed": 3}, "reward_from": "json", "exit": 0, "stdout": "{\"reward\":0.5}",
			"stderr": "", "failure_class": "ok", "retryable": false, "ms": 1200, "grader_id": "sbx_g", "kept": false,
			"sandbox_id": "sbx_1", "run_id": "run_1", "recorded": true,
		}}
	})
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1"}}
	g, err := b.Grade(ctx, []string{"pytest", "-q", "/grader/t.py"}, &CloudGradeOptions{
		Files: map[string][]byte{"/grader/t.py": []byte("assert 1")}, TimeoutS: 60, Env: map[string]string{"B": "2", "A": "1"},
		Keep: true, RunID: "run_1", Rollout: &CloudRolloutInfo{Step: Ptr(3), Group: "7", Index: Ptr(1), Meta: map[string]any{"t": "x"}},
		IdempotencyKey: "g-1",
	})
	if err != nil || *g.Reward != 0.5 || g.Metrics["passed"] != 3 || *g.ExitCode != 0 || g.GraderID != "sbx_g" || !*g.Recorded || g.FailureClass != FailureOK {
		t.Fatalf("%+v %v", g, err)
	}
	r := f.reqs("POST", "/v1/sandboxes/sbx_1/grade")[0]
	want := map[string]any{
		"cmd": []any{"pytest", "-q", "/grader/t.py"}, "files": map[string]any{"/grader/t.py": base64.StdEncoding.EncodeToString([]byte("assert 1"))},
		"timeout_s": 60.0, "env": []any{"A=1", "B=2"}, "keep": true, "run_id": "run_1",
		"rollout": map[string]any{"step": 3.0, "group": "7", "index": 1.0, "meta": map[string]any{"t": "x"}},
	}
	if !reflect.DeepEqual(r.Body, want) || r.Key != "g-1" {
		t.Fatalf("%v %q", r.Body, r.Key)
	}
	if _, err := c.Sandboxes.Grade(ctx, "sbx_1", []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	if k := f.reqs("POST", "/v1/sandboxes/sbx_1/grade")[1].Key; k == "" || k == "g-1" {
		t.Fatalf("default key %q", k)
	}
	big := map[string][]byte{}
	for i := 0; i < 65; i++ {
		big[fmt.Sprintf("/f%d", i)] = nil
	}
	for _, o := range []*CloudGradeOptions{{Files: big}, {Files: map[string][]byte{"rel": nil}}, {Files: map[string][]byte{"/a/../b": nil}},
		{Files: map[string][]byte{"/a": make([]byte, CloudGradeMaxBytes+1)}}, {TimeoutS: 91}, {Env: map[string]string{"1x": "y"}},
		{RunID: "bad id"}, {Rollout: &CloudRolloutInfo{Group: strings.Repeat("g", 201)}}} {
		if _, err := b.Grade(ctx, []string{"x"}, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, err := b.Grade(ctx, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := c.Sandboxes.Grade(ctx, "bad/id", []string{"x"}, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if (&GradeError{}).Error() != "grade: the grader did not finish" {
		t.Fatal("GradeError")
	}
}

// fleet is a fake kosa8 Cloud that forks, deletes and drains sandboxes and
// tracks which are alive.
type fleet struct {
	mu       sync.Mutex
	next     int
	live     map[string]bool
	forkFail func(n int) *fakeAns // optional: refuse a fork
	grade    func(r fakeReq) fakeAns
	runs     map[string][]any
}

func newFleet() *fleet { return &fleet{live: map[string]bool{}, runs: map[string][]any{}} }

func (fl *fleet) alive() []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var out []string
	for id, ok := range fl.live {
		if ok && id != "sbx_base" {
			out = append(out, id)
		}
	}
	return out
}

func (fl *fleet) answer(r fakeReq) fakeAns {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.Path, "/fork"):
		n := int(r.Body["n"].(float64))
		if fl.forkFail != nil {
			if a := fl.forkFail(n); a != nil {
				return *a
			}
		}
		var kids []any
		for i := 0; i < n; i++ {
			fl.next++
			id := fmt.Sprintf("sbx_k%d", fl.next)
			fl.live[id] = true
			kids = append(kids, box(id))
		}
		return fakeAns{status: 201, body: map[string]any{"sandboxes": kids, "failed": []any{}, "requested": n, "created": n}}
	case r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/sandboxes/"):
		id := strings.TrimPrefix(r.Path, "/v1/sandboxes/")
		if !fl.live[id] {
			return fakeAns{status: 404, body: errBody("not_found", "gone", nil)}
		}
		fl.live[id] = false
		return fakeAns{body: map[string]any{"ok": true}}
	case r.Path == "/v1/sandboxes/drain":
		var ids []string
		for _, x := range r.Body["ids"].([]any) {
			id := x.(string)
			if fl.live[id] {
				fl.live[id] = false
				ids = append(ids, id)
			}
		}
		return fakeAns{body: map[string]any{"deleted": len(ids), "ids": ids, "failed": []any{}, "more": false}}
	case strings.HasSuffix(r.Path, "/grade") && fl.grade != nil:
		fl.mu.Unlock()
		a := fl.grade(r)
		fl.mu.Lock()
		return a
	case r.Method == "POST" && strings.HasSuffix(r.Path, "/rollouts"):
		id := strings.Split(r.Path, "/")[4]
		rows := r.Body["rollouts"].([]any)
		fl.runs[id] = append(fl.runs[id], rows...)
		return fakeAns{body: map[string]any{"recorded": len(rows)}}
	}
	return fakeAns{status: 404, body: errBody("not_found", "no route "+r.Path, nil)}
}

func TestForkMap(t *testing.T) {
	ctx := context.Background()
	fl := newFleet()
	_, c := newFake(t, fl.answer)
	base := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_base"}}
	var attempts sync.Map
	var inflight, peak atomic.Int32
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	res, err := ForkMap(ctx, base, items, func(ctx context.Context, child *CloudSandbox, item int) (string, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		v, _ := attempts.LoadOrStore(item, new(atomic.Int32))
		a := v.(*atomic.Int32).Add(1)
		switch item {
		case 3: // fails once on infra, then works
			if a == 1 {
				return "", &Error{Kind: KindAPI, Status: 502, Code: "host_failed", Message: "host down"}
			}
		case 5:
			return "", errors.New("my bug")
		case 7:
			panic("boom")
		}
		return fmt.Sprintf("%d@%s", item, child.ID()), nil
	}, &ForkMapOptions{Concurrency: 3, RunID: "run_1", LeaseS: 120, Labels: map[string]string{"job": "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 3 {
		t.Fatalf("concurrency %d", peak.Load())
	}
	for i, r := range res {
		if r.Item != i {
			t.Fatalf("order: %d at %d", r.Item, i)
		}
		switch i {
		case 3:
			if !r.OK() || r.Attempts != 2 {
				t.Fatalf("retry: %+v", r)
			}
		case 5:
			if r.OK() || r.FailureClass != FailureError || r.Attempts != 1 {
				t.Fatalf("bug: %+v", r)
			}
		case 7:
			if r.Err == nil || !strings.Contains(r.Err.Error(), "panic: boom") {
				t.Fatalf("panic: %+v", r)
			}
		default:
			if !r.OK() || !strings.HasPrefix(r.Value, fmt.Sprint(i)+"@sbx_k") || r.SandboxID == "" || r.FailureClass != FailureOK {
				t.Fatalf("%d: %+v", i, r)
			}
		}
	}
	if a := fl.alive(); len(a) != 0 {
		t.Fatalf("leaked %v", a)
	}

	// Timeouts delete the child; a cancelled map deletes everything.
	fl2 := newFleet()
	_, c2 := newFake(t, fl2.answer)
	base2 := &CloudSandbox{c: c2, Info: CloudSandboxInfo{ID: "sbx_base"}}
	res2, err := ForkMap(ctx, base2, []int{1}, func(ctx context.Context, _ *CloudSandbox, _ int) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, &ForkMapOptions{Timeout: 20 * time.Millisecond})
	if err != nil || res2[0].FailureClass != FailureTimeout || res2[0].Attempts != 1 {
		t.Fatalf("%+v %v", res2, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	started := make(chan struct{}, 10)
	go func() { <-started; cancel() }()
	res3, err := ForkMap(cctx, base2, []int{1, 2, 3, 4, 5, 6}, func(ctx context.Context, _ *CloudSandbox, _ int) (int, error) {
		started <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	}, &ForkMapOptions{Concurrency: 2})
	if !errors.Is(err, context.Canceled) || len(res3) != 6 {
		t.Fatalf("%v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if a := fl2.alive(); len(a) != 0 {
		t.Fatalf("leaked after cancel %v", a)
	}

	// A fork that fails for good is each item's error.
	fl3 := newFleet()
	fl3.forkFail = func(int) *fakeAns {
		return &fakeAns{status: 409, body: errBody("secrets", "has secrets", map[string]any{"failure_class": "client", "retryable": false})}
	}
	_, c3 := newFake(t, fl3.answer)
	res4, err := ForkMap(ctx, &CloudSandbox{c: c3, Info: CloudSandboxInfo{ID: "sbx_base"}}, []int{1, 2}, func(context.Context, *CloudSandbox, int) (int, error) { return 1, nil }, nil)
	if err != nil || res4[0].FailureClass != FailureClient || res4[1].Err == nil {
		t.Fatalf("%+v %v", res4, err)
	}
	if _, err := ForkMap[int, int](ctx, nil, nil, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := ForkMap(ctx, base, []int{1}, func(context.Context, *CloudSandbox, int) (int, error) { return 0, nil }, &ForkMapOptions{Concurrency: -1}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if r, err := ForkMap(ctx, base, []int{}, func(context.Context, *CloudSandbox, int) (int, error) { return 0, nil }, nil); err != nil || len(r) != 0 {
		t.Fatal(err)
	}
}

func TestChildPoolCloseFallsBackToDeletes(t *testing.T) {
	ctx := context.Background()
	fl := newFleet()
	var deletes atomic.Int32
	_, c := newFake(t, func(r fakeReq) fakeAns {
		if r.Path == "/v1/sandboxes/drain" {
			return fakeAns{status: 400, body: errBody("invalid_selector", "no", nil)}
		}
		if r.Method == "DELETE" {
			deletes.Add(1)
		}
		return fl.answer(r)
	})
	p := newChildPool(c, &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_base"}}, nil, "", 0)
	p.prefork(ctx, 3)
	if len(p.liveIDs()) != 3 {
		t.Fatal(p.liveIDs())
	}
	k, _ := p.take(ctx)
	p.forget(k)
	p.close(ctx)
	if deletes.Load() != 2 || len(fl.alive()) != 1 {
		t.Fatalf("deletes %d alive %v", deletes.Load(), fl.alive())
	}
	if _, err := p.take(ctx); !errors.Is(err, ErrCancelled) {
		t.Fatal(err)
	}
	if err := p.adopt(ctx, []*CloudSandbox{{c: c, Info: CloudSandboxInfo{ID: "sbx_late"}}}); !errors.Is(err, ErrCancelled) {
		t.Fatal(err)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var batches []int
	f, c := newFake(t, func(r fakeReq) fakeAns {
		switch {
		case r.Method == "POST" && r.Path == "/v1/rl/runs":
			return fakeAns{status: 201, body: map[string]any{"run": map[string]any{"id": "run_abc", "name": r.Body["name"], "status": "running"}}}
		case r.Method == "POST" && r.Path == "/v1/rl/runs/run_abc/rollouts":
			n := len(r.Body["rollouts"].([]any))
			mu.Lock()
			batches = append(batches, n)
			mu.Unlock()
			return fakeAns{body: map[string]any{"recorded": n}}
		case r.Path == "/v1/sandboxes/heartbeat":
			return fakeAns{body: map[string]any{"extended": 2}}
		case r.Path == "/v1/sandboxes/drain":
			return fakeAns{body: map[string]any{"deleted": 2, "ids": []string{"sbx_1", "sbx_2"}, "failed": []any{}, "more": false}}
		case r.Method == "PATCH":
			return fakeAns{body: map[string]any{"run": map[string]any{"id": "run_abc", "status": r.Body["status"]}}}
		case r.Method == "GET" && r.Path == "/v1/rl/runs/run_abc":
			return fakeAns{body: map[string]any{"run": map[string]any{"id": "run_abc"}, "summary": map[string]any{"rollouts": 1201}, "recommendations": []any{}}}
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/rl/runs/run_abc/rollouts"):
			return fakeAns{body: map[string]any{"rollouts": []any{map[string]any{"id": "ro_1", "source": "api"}}, "next_cursor": "c2"}}
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/rl/runs/run_abc/export"):
			return fakeAns{raw: []byte("{\"id\":\"ro_1\"}\n{\"id\":\"ro_2\"}\n")}
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/rl/runs"):
			return fakeAns{body: map[string]any{"runs": []any{map[string]any{"id": "run_abc"}}, "next_cursor": nil}}
		}
		return fakeAns{status: 404, body: errBody("not_found", r.Path, nil)}
	})
	run, err := c.StartRun(ctx, CloudRunOptions{Name: "grpo", Project: "p", Config: map[string]any{"lr": 1e-6}, LeaseS: 120,
		HeartbeatInterval: 10 * time.Millisecond, FlushEvery: 500, Logf: func(string, ...any) {}})
	if err != nil || run.ID != "run_abc" || run.LeaseS != 120 {
		t.Fatal(run, err)
	}
	if b := f.reqs("POST", "/v1/rl/runs")[0].Body; !reflect.DeepEqual(b, map[string]any{"name": "grpo", "project": "p", "config": map[string]any{"lr": 1e-6}}) {
		t.Fatal(b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for run.Beats() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if run.Beats() < 2 {
		t.Fatal("no heartbeats")
	}
	if hb := f.reqs("POST", "/v1/sandboxes/heartbeat")[0].Body; !reflect.DeepEqual(hb, map[string]any{"run_id": "run_abc"}) {
		t.Fatal(hb)
	}
	rows := make([]RolloutRow, 1201)
	for i := range rows {
		rows[i] = RolloutRow{Step: Ptr(0), Index: Ptr(i), Reward: Ptr(1.0)}
	}
	if err := run.Record(ctx, rows[:499]...); err != nil || run.Pending() > 499 {
		t.Fatal(err)
	}
	if err := run.Record(ctx, rows[499:]...); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(ctx, "finished"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	total := 0
	for _, n := range batches {
		total += n
		if n > MaxRolloutsPerCall {
			t.Fatalf("batch of %d", n)
		}
	}
	mu.Unlock()
	if total != 1201 || run.Recorded() != 1201 || run.Pending() != 0 {
		t.Fatalf("recorded %d %v", total, batches)
	}
	if d := run.Drained(); d == nil || d.Deleted != 2 {
		t.Fatal(d)
	}
	if p := f.reqs("PATCH", "/v1/rl/runs/run_abc"); len(p) != 1 || p[0].Body["status"] != "finished" || run.Info["status"] != "finished" {
		t.Fatal(p)
	}
	if err := run.Finish(ctx, "finished"); err != nil || len(f.reqs("PATCH", "/")) != 1 {
		t.Fatal("second finish did something")
	}
	if err := run.Finish(ctx, "done"); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	s, err := run.Summary(ctx)
	if err != nil || s["summary"].(map[string]any)["rollouts"] != 1201.0 {
		t.Fatal(s, err)
	}
	pg, err := run.Rollouts(ctx, RolloutQuery{Step: Ptr(2), FailureClass: FailureInfra, MinReward: Ptr(0.5), MaxReward: Ptr(1.0), SandboxID: "sbx_1", ID: "ro_1", Limit: 50, Cursor: "c1"})
	if err != nil || len(pg.Rollouts) != 1 || *pg.NextCursor != "c2" {
		t.Fatal(pg, err)
	}
	q := f.reqs("GET", "/v1/rl/runs/run_abc/rollouts")[0].Path
	for _, part := range []string{"step=2", "failure_class=infra", "min_reward=0.5", "max_reward=1", "sandbox_id=sbx_1", "id=ro_1", "limit=50", "cursor=c1"} {
		if !strings.Contains(q, part) {
			t.Fatalf("%s lacks %s", q, part)
		}
	}
	ex, err := run.Export(ctx)
	if err != nil || strings.Count(string(ex), "\n") != 2 || !strings.Contains(f.reqs("GET", "/v1/rl/runs/run_abc/export")[0].Path, "format=jsonl") {
		t.Fatal(string(ex), err)
	}
	if _, err := run.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListRuns(ctx, CloudRunListOptions{Project: "p", Status: "running", Limit: 5})
	if err != nil || len(list["runs"].([]any)) != 1 {
		t.Fatal(list, err)
	}
	if lp := f.reqs("GET", "/v1/rl/runs?")[0].Path; !strings.Contains(lp, "project=p") || !strings.Contains(lp, "limit=5") || !strings.Contains(lp, "status=running") {
		t.Fatal(lp)
	}
	if _, err := run.Update(ctx, "", ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}

	// Attach: no create; bad options are refused.
	n := len(f.reqs("POST", "/v1/rl/runs"))
	r2, err := c.AttachRun(ctx, "run_abc", CloudRunOptions{KeepSandboxes: true, Logf: func(string, ...any) {}})
	if err != nil || r2.ID != "run_abc" || len(f.reqs("POST", "/v1/rl/runs")) != n || r2.LeaseS != DefaultLeaseS {
		t.Fatal(r2, err)
	}
	drainsBefore := len(f.reqs("POST", "/v1/sandboxes/drain"))
	if err := r2.Finish(ctx, "failed"); err != nil || len(f.reqs("POST", "/v1/sandboxes/drain")) != drainsBefore || r2.Drained() != nil {
		t.Fatal("KeepSandboxes drained", err)
	}
	for _, o := range []CloudRunOptions{{}, {Name: "x", LeaseS: 30}, {Name: "x", FlushEvery: 501}, {RunID: "bad id"}} {
		if _, err := c.StartRun(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

func TestRunFlushKeepsRowsOnError(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	fail.Store(true)
	var logged atomic.Int32
	_, c := newFake(t, func(r fakeReq) fakeAns {
		switch {
		case r.Path == "/v1/rl/runs":
			return fakeAns{status: 201, body: map[string]any{"run": map[string]any{"id": "run_x"}}}
		case strings.HasSuffix(r.Path, "/rollouts"):
			if fail.Load() {
				return fakeAns{status: 400, body: errBody("invalid_rollout", "bad", nil)}
			}
			return fakeAns{body: map[string]any{"recorded": len(r.Body["rollouts"].([]any))}}
		case r.Path == "/v1/sandboxes/heartbeat":
			return fakeAns{status: 400, body: errBody("invalid_selector", "no", nil)}
		}
		return fakeAns{body: map[string]any{"deleted": 0, "failed": []any{}, "more": false, "run": map[string]any{}}}
	})
	run, err := c.StartRun(ctx, CloudRunOptions{Name: "x", FlushEvery: 2, HeartbeatInterval: 5 * time.Millisecond,
		Logf: func(string, ...any) { logged.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Record(ctx, RolloutRow{}, RolloutRow{}, RolloutRow{}); err == nil || run.Pending() != 3 {
		t.Fatalf("%v %d", err, run.Pending())
	}
	deadline := time.Now().Add(2 * time.Second)
	for logged.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if run.LastError() == nil || logged.Load() < 2 {
		t.Fatal("background errors not reported")
	}
	fail.Store(false)
	if err := run.Finish(ctx, "finished"); err != nil || run.Pending() != 0 || run.Recorded() != 3 {
		t.Fatalf("%v %d %d", err, run.Pending(), run.Recorded())
	}
	// A finish whose steps fail reports them all.
	fail.Store(true)
	run2, _ := c.StartRun(ctx, CloudRunOptions{Name: "y", Logf: func(string, ...any) {}})
	_ = run2.Record(ctx, RolloutRow{})
	if err := run2.Finish(ctx, "failed"); err == nil || !strings.Contains(err.Error(), "recording rollouts") {
		t.Fatal(err)
	}
}

type task struct {
	ID    string
	Tests string
}

func TestMapRolloutsWithSealedGrading(t *testing.T) {
	ctx := context.Background()
	fl := newFleet()
	var gradeCalls sync.Map // sandbox -> count
	var gradeKeys sync.Map
	fl.grade = func(r fakeReq) fakeAns {
		sb := strings.Split(r.Path, "/")[3]
		v, _ := gradeCalls.LoadOrStore(sb, new(atomic.Int32))
		n := v.(*atomic.Int32).Add(1)
		if _, dup := gradeKeys.LoadOrStore(r.Key, true); dup {
			return fakeAns{status: 500, body: errBody("internal", "key reused across grades", nil)}
		}
		files := r.Body["files"].(map[string]any)
		src, _ := base64.StdEncoding.DecodeString(files["/grader/test.py"].(string))
		rollout := r.Body["rollout"].(map[string]any)
		switch string(src) {
		case "flaky": // the grader's host fails once: a 200 with no reward, retryable
			if n == 1 {
				return fakeAns{body: map[string]any{"reward": nil, "exit": nil, "failure_class": "infra", "retryable": true, "grader_id": "sbx_g", "error": "host_failed", "message": "gone"}}
			}
		case "never":
			return fakeAns{body: map[string]any{"reward": nil, "exit": nil, "failure_class": "infra", "retryable": true, "grader_id": "sbx_g", "error": "host_failed", "recorded": true}}
		case "fails":
			return fakeAns{body: map[string]any{"reward": 0.0, "exit": 1, "failure_class": "exit_nonzero", "retryable": false, "grader_id": "sbx_g", "recorded": true, "stdout": "F"}}
		}
		if r.Body["run_id"] != "run_m" || rollout["step"] == nil {
			return fakeAns{status: 400, body: errBody("invalid_rollout", "no run", nil)}
		}
		return fakeAns{body: map[string]any{"reward": 1.0, "metrics": map[string]any{"passed": 2}, "exit": 0, "failure_class": "ok", "grader_id": "sbx_g", "recorded": true, "stdout": "ok"}}
	}
	_, c := newFake(t, func(r fakeReq) fakeAns {
		switch {
		case r.Path == "/v1/rl/runs":
			return fakeAns{status: 201, body: map[string]any{"run": map[string]any{"id": "run_m"}}}
		case r.Method == "PATCH":
			return fakeAns{body: map[string]any{"run": map[string]any{"id": "run_m"}}}
		case r.Path == "/v1/sandboxes/heartbeat":
			return fakeAns{body: map[string]any{"extended": 0}}
		}
		if r.Path == "/v1/sandboxes/drain" && r.Body["run_id"] != nil {
			return fakeAns{body: map[string]any{"deleted": 0, "ids": []string{}, "failed": []any{}, "more": false}}
		}
		if strings.HasSuffix(r.Path, "/fork") && (r.Body["run_id"] != "run_m" || r.Body["lease_s"] != 300.0) {
			return fakeAns{status: 400, body: errBody("invalid_run_id", "fork without the run", nil)}
		}
		return fl.answer(r)
	})
	run, err := c.StartRun(ctx, CloudRunOptions{Name: "m", LeaseS: 300, FlushEvery: 500, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	base := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_base"}}
	tasks := []task{{"t0", "pass"}, {"t1", "fails"}, {"t2", "flaky"}, {"t3", "never"}, {"t4", "pass"}}
	res, err := MapRollouts(ctx, run, base, tasks, func(ctx context.Context, sb *CloudSandbox, tk task) (any, error) {
		if tk.ID == "t4" {
			return nil, errors.New("agent crashed")
		}
		return RolloutOutput{Tokens: Ptr(42), Meta: map[string]any{"agent": "v1"}}, nil
	}, &RunMapOptions[task]{
		GradeCmd: []string{"python", "/grader/test.py"}, GroupSize: 2, Concurrency: 4,
		Files: func(tk task) map[string][]byte { return map[string][]byte{"/grader/test.py": []byte(tk.Tests)} },
		Meta:  func(tk task, _ any) map[string]any { return map[string]any{"task": tk.ID} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 10 {
		t.Fatal(len(res))
	}
	for i, r := range res {
		if r.Group != i/2 || r.Index != i%2 || *r.Step != 0 || r.Meta["task"] != tasks[i/2].ID {
			t.Fatalf("%d: %+v", i, r)
		}
		switch tasks[r.Group].ID {
		case "t0":
			if !r.OK() || *r.Reward != 1 || r.Metrics["passed"] != 2 || !r.Recorded || *r.Tokens != 42 || r.GraderID != "sbx_g" {
				t.Fatalf("pass: %+v", r)
			}
		case "t1":
			if !r.OK() || *r.Reward != 0 || r.FailureClass != FailureExitNonzero || r.Masked {
				t.Fatalf("fails: %+v", r)
			}
		case "t2":
			if !r.OK() || *r.Reward != 1 {
				t.Fatalf("flaky: %+v", r)
			}
		case "t3":
			if !r.Masked || r.Reward != nil || r.FailureClass != FailureInfra || r.Err == nil {
				t.Fatalf("never: %+v", r)
			}
		case "t4":
			if !r.Masked || r.FailureClass != FailureError || r.Attempts != 1 || r.Recorded {
				t.Fatalf("crash: %+v", r)
			}
		}
	}
	rs := Rollouts[task](res)
	if m, ok := rs.MeanReward(); !ok || m != 4.0/6.0 {
		t.Fatal(m)
	}
	if rs.ByClass()[FailureOK] != 4 || len(rs.Groups()) != 5 || len(rs.Groups()[0]) != 2 || rs.Mask()[6] || rs.Rewards()[6] != nil {
		t.Fatal(rs.ByClass())
	}
	if a := fl.alive(); len(a) != 0 {
		t.Fatalf("leaked %v", a)
	}
	// Only the rollouts the grader did not record are recorded by the SDK.
	if err := run.Finish(ctx, "finished"); err != nil {
		t.Fatal(err)
	}
	fl.mu.Lock()
	rows := fl.runs["run_m"]
	fl.mu.Unlock()
	if len(rows) != 2 {
		t.Fatalf("recorded %d rows", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["failure_class"] != "client" || row["meta"].(map[string]any)["sdk_failure_class"] != "error" || row["meta"].(map[string]any)["masked"] != true {
		t.Fatal(row)
	}
	if _, err := MapRollouts(ctx, run, base, tasks, func(context.Context, *CloudSandbox, task) (any, error) { return 1, nil }, nil); !errors.Is(err, ErrUsage) {
		t.Fatal("map on a finished run:", err)
	}
}

func TestMapRolloutsRewardsAndMake(t *testing.T) {
	ctx := context.Background()
	fl := newFleet()
	var made atomic.Int32
	_, c := newFake(t, func(r fakeReq) fakeAns {
		switch {
		case r.Path == "/v1/rl/runs":
			return fakeAns{status: 201, body: map[string]any{"run": map[string]any{"id": "run_r"}}}
		case r.Method == "PATCH", r.Path == "/v1/sandboxes/heartbeat":
			return fakeAns{body: map[string]any{"run": map[string]any{}}}
		case r.Path == "/v1/sandboxes" && r.Method == "POST":
			id := fmt.Sprintf("sbx_m%d", made.Add(1))
			fl.mu.Lock()
			fl.live[id] = true
			fl.mu.Unlock()
			return fakeAns{status: 201, body: map[string]any{"sandbox": box(id)}}
		case r.Path == "/v1/sandboxes/drain" && r.Body["run_id"] != nil:
			return fakeAns{body: map[string]any{"deleted": 0, "failed": []any{}, "more": false}}
		}
		return fl.answer(r)
	})
	run, _ := c.StartRun(ctx, CloudRunOptions{Name: "r", Logf: func(string, ...any) {}})
	defer run.Finish(ctx, "finished")
	mk := func(ctx context.Context, n int) (*CloudSandbox, error) {
		return c.Sandboxes.Create(ctx, CloudCreateOptions{Template: "python", LeaseS: run.LeaseS})
	}
	var infraOnce atomic.Bool
	res, err := MapRollouts(ctx, run, nil, []int{1, 2, 3, 4, 5}, func(ctx context.Context, sb *CloudSandbox, n int) (any, error) {
		switch n {
		case 1:
			return 0.25, nil
		case 2:
			return map[string]any{"reward": 0.75, "tokens": 9, "cost_usd": 0.01, "meta": map[string]any{"m": 1}}, nil
		case 3:
			if infraOnce.CompareAndSwap(false, true) {
				return nil, &Error{Kind: KindConnection, Message: "lost"}
			}
			return 1, nil
		case 4:
			return "no reward", nil
		}
		time.Sleep(200 * time.Millisecond)
		return 1.0, nil
	}, &RunMapOptions[int]{Make: mk, Step: Ptr(9), Timeout: 50 * time.Millisecond, Mask: []FailureClass{FailureInfra, FailureTimeout}})
	if err != nil {
		t.Fatal(err)
	}
	if *res[0].Reward != 0.25 || *res[1].Reward != 0.75 || *res[1].Tokens != 9 || *res[1].CostUSD != 0.01 || res[1].Meta["m"] != 1 {
		t.Fatalf("%+v %+v", res[0], res[1])
	}
	if *res[2].Reward != 1 || res[2].Attempts != 2 {
		t.Fatalf("retried: %+v", res[2])
	}
	if res[3].FailureClass != FailureError || res[3].Masked || !errors.Is(res[3].Err, errNoReward) {
		t.Fatalf("no reward is not masked by this mask: %+v", res[3])
	}
	if res[4].FailureClass != FailureTimeout || !res[4].Masked || *res[4].Step != 9 {
		t.Fatalf("timeout: %+v", res[4])
	}
	time.Sleep(250 * time.Millisecond)
	if a := fl.alive(); len(a) != 0 {
		t.Fatalf("leaked %v", a)
	}

	// Reward= gives the reward from the sandbox; a bad return is an error.
	sres, err := MapRollouts(ctx, run, &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_base"}}, []string{"a", "b"},
		func(ctx context.Context, sb *CloudSandbox, s string) (any, error) { return s, nil },
		&RunMapOptions[string]{Reward: func(ctx context.Context, sb *CloudSandbox, s string, out any) (any, error) {
			if s == "b" {
				return "nope", nil
			}
			return &RolloutOutput{Reward: Ptr(0.5), Metrics: map[string]float64{"x": 1}}, nil
		}})
	if err != nil || *sres[0].Reward != 0.5 || sres[0].Metrics["x"] != 1 || sres[1].Err == nil || !sres[1].Masked {
		t.Fatalf("%+v %+v %v", sres[0], sres[1], err)
	}
	if _, err := MapRollouts(ctx, run, nil, []int{1}, func(context.Context, *CloudSandbox, int) (any, error) { return 1, nil }, nil); !errors.Is(err, ErrUsage) {
		t.Fatal("no base and no Make:", err)
	}
	if _, err := MapRollouts(ctx, run, nil, []int{1}, func(context.Context, *CloudSandbox, int) (any, error) { return 1, nil }, &RunMapOptions[int]{Make: mk, GroupSize: -1}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if r, err := MapRollouts(ctx, run, nil, []int{}, func(context.Context, *CloudSandbox, int) (any, error) { return 1, nil }, &RunMapOptions[int]{Make: mk}); err != nil || len(r) != 0 {
		t.Fatal(err)
	}
}

func TestRolloutRowAndMeta(t *testing.T) {
	r := RolloutResult[int]{Group: 3, Index: 1, Step: Ptr(2), FailureClass: FailureError, Masked: true, Err: errors.New(strings.Repeat("e", 3000)),
		Attempts: 2, Stdout: strings.Repeat("o", 5000) + "END", Duration: 1500 * time.Millisecond, Meta: map[string]any{"big": strings.Repeat("x", 9000), "keep": 1}}
	row := r.Row()
	if row.Group != "3" || *row.Index != 1 || row.FailureClass != FailureClient || row.DurationMs != 1500 || len(row.StdoutTail) != RolloutStdoutTail || !strings.HasSuffix(row.StdoutTail, "END") {
		t.Fatalf("%+v", row)
	}
	if row.Meta["big"] != nil || row.Meta["keep"] != 1 || row.Meta["truncated"] != true || len(row.Meta["error"].(string)) != 2000 || row.Meta["attempts"] != 2 {
		t.Fatalf("%v", row.Meta)
	}
	b, _ := json.Marshal(row)
	if !strings.Contains(string(b), `"reward":null`) {
		t.Fatal(string(b))
	}
	if (RolloutResult[int]{FailureClass: FailureOK}).Row().Meta != nil {
		t.Fatal("empty meta sent")
	}
	if m, ok := (Rollouts[int]{}).MeanReward(); ok || m != 0 {
		t.Fatal("empty mean")
	}
}

func TestOrgTemplates(t *testing.T) {
	ctx := context.Background()
	var gets atomic.Int32
	f, c := newFake(t, func(r fakeReq) fakeAns {
		switch {
		case r.Method == "POST" && r.Path == "/v1/templates":
			return fakeAns{status: 202, body: map[string]any{"template": map[string]any{"id": "tpl_abcdefgh", "name": r.Body["name"], "state": "queued", "source": "image"}}}
		case r.Method == "POST" && r.Path == "/v1/templates/import":
			return fakeAns{status: 202, body: map[string]any{"import_id": "imp_1", "requested": 2, "queued": 1, "builds_at_once": 2,
				"templates": []any{map[string]any{"id": "tpl_1", "name": "a", "image": "img@sha256:00"}},
				"failed":    []any{map[string]any{"index": 1, "name": "b", "code": "plan_limit", "message": "snapshots"}}}}
		case r.Method == "GET" && r.Path == "/v1/templates":
			return fakeAns{body: map[string]any{"templates": []any{map[string]any{"key": "python"}}, "org_templates": []any{map[string]any{"id": "tpl_1", "name": "a", "state": "ready"}}}}
		case r.Method == "GET" && r.Path == "/v1/templates/swe-1":
			state := "building"
			if gets.Add(1) >= 3 {
				state = "ready"
			}
			return fakeAns{body: map[string]any{"template": map[string]any{"id": "tpl_2", "name": "swe-1", "state": state, "log": "step 1", "snapshot_id": "snap_1"}}}
		case r.Method == "GET" && r.Path == "/v1/templates/broken":
			return fakeAns{body: map[string]any{"template": map[string]any{"id": "tpl_3", "name": "broken", "state": "failed", "last_error": "pull failed"}}}
		case r.Method == "DELETE":
			return fakeAns{body: map[string]any{"ok": true, "id": "tpl_2"}}
		}
		return fakeAns{status: 404, body: errBody("not_found", "no", nil)}
	})
	tpl, err := c.OrgTemplates.Build(ctx, CloudTemplateBuild{Name: "swe-1", Image: "ghcr.io/x/y@sha256:ab", BuildArgs: map[string]string{"A": "1"},
		SetupCmd: []string{"pip install -e ."}, Size: "4x8", Region: "us-ash1"})
	if err != nil || tpl.ID != "tpl_abcdefgh" || tpl.State != "queued" || tpl.Ready() {
		t.Fatal(tpl, err)
	}
	want := map[string]any{"name": "swe-1", "image": "ghcr.io/x/y@sha256:ab", "build_args": map[string]any{"A": "1"}, "setup_cmd": "pip install -e .", "size": "4x8", "region": "us-ash1"}
	if b := f.reqs("POST", "/v1/templates")[0].Body; !reflect.DeepEqual(b, want) {
		t.Fatal(b)
	}
	if _, err := c.OrgTemplates.Build(ctx, CloudTemplateBuild{Name: "df", Dockerfile: "FROM python:3.12", SetupCmd: []string{"a", "b"}, CPUs: 2, MemMB: 4096}); err != nil {
		t.Fatal(err)
	}
	if b := f.reqs("POST", "/v1/templates")[1].Body; b["dockerfile"] != "FROM python:3.12" || b["cpus"] != 2.0 || len(b["setup_cmd"].([]any)) != 2 {
		t.Fatal(b)
	}
	for _, b := range []CloudTemplateBuild{{Name: "Bad"}, {Name: "x"}, {Name: "x", Image: "i", Dockerfile: "d"}, {Name: "x", Image: "i", Size: "3x3"},
		{Name: "x", Image: "i", CPUs: 2}, {Name: "x", Dockerfile: strings.Repeat("d", 65*1024)}} {
		if _, err := c.OrgTemplates.Build(ctx, b); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", b.Name, err)
		}
	}
	imp, err := c.OrgTemplates.Import(ctx, []CloudTemplateImage{{"a", "img@sha256:00"}, {"b", "img2"}}, &CloudTemplateImportOptions{Size: "2x4", SetupCmd: []string{"true"}, Region: "r", PartialOK: true})
	if err != nil || imp.Queued != 1 || imp.Templates[0].ID != "tpl_1" || imp.Failed[0].Code != "plan_limit" {
		t.Fatal(imp, err)
	}
	if b := f.reqs("POST", "/v1/templates/import")[0].Body; b["partial_ok"] != true || len(b["images"].([]any)) != 2 || b["size"] != "2x4" {
		t.Fatal(b)
	}
	if _, err := c.OrgTemplates.Import(ctx, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := c.OrgTemplates.Import(ctx, []CloudTemplateImage{{"UPPER", "i"}}, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	list, err := c.OrgTemplates.List(ctx)
	if err != nil || len(list) != 1 || !list[0].Ready() {
		t.Fatal(list, err)
	}
	if old, err := c.Templates(ctx); err != nil || old[0].Key != "python" {
		t.Fatal("Cloud.Templates still works:", old, err)
	}
	resetSlept()
	got, err := c.OrgTemplates.WaitReady(ctx, "swe-1", time.Millisecond)
	if err != nil || !got.Ready() || got.Log != "step 1" || *got.SnapshotID != "snap_1" || len(sleeps()) != 2 {
		t.Fatal(got, err, sleeps())
	}
	if _, err := c.OrgTemplates.WaitReady(ctx, "broken", 0); err == nil || !strings.Contains(err.Error(), "pull failed") {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	gets.Store(-100)
	if _, err := c.OrgTemplates.WaitReady(cctx, "swe-1", 0); err == nil {
		t.Fatal("cancelled wait")
	}
	if err := c.OrgTemplates.Delete(ctx, "swe-1"); err != nil {
		t.Fatal(err)
	}
	if f.reqs("DELETE", "/v1/templates/swe-1") == nil {
		t.Fatal("no delete")
	}
	if err := c.OrgTemplates.Delete(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := c.OrgTemplates.Get(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestCapabilityTokens(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(t, func(r fakeReq) fakeAns {
		switch r.Method {
		case "POST":
			return fakeAns{status: 201, body: map[string]any{"token": map[string]any{"id": "tok_1", "prefix": "k8c_tok_ab", "scopes": r.Body["scopes"],
				"state": "active", "expires_at": 99, "secret": "k8c_tok_secret"}}}
		case "GET":
			return fakeAns{body: map[string]any{"tokens": []any{map[string]any{"id": "tok_1", "state": "active"}}}}
		case "DELETE":
			return fakeAns{body: map[string]any{"token": map[string]any{"id": "tok_1", "state": "revoked"}}}
		}
		return fakeAns{status: 405}
	})
	tok, err := c.Tokens.Create(ctx, CloudTokenOptions{Scopes: []string{ScopeSandboxesRead, ScopeSandboxesWrite}, SandboxIDs: []string{"sbx_1"},
		Labels: map[string]string{"user": "u1"}, TTLS: 600, Name: "student"})
	if err != nil || tok.Secret != "k8c_tok_secret" || tok.State != "active" || len(tok.Scopes) != 2 {
		t.Fatal(tok, err)
	}
	want := map[string]any{"scopes": []any{"sandboxes:read", "sandboxes:write"}, "sandbox_ids": []any{"sbx_1"}, "labels": map[string]any{"user": "u1"}, "ttl_s": 600.0, "name": "student"}
	if b := f.reqs("POST", "/v1/tokens")[0].Body; !reflect.DeepEqual(b, want) {
		t.Fatal(b)
	}
	list, err := c.Tokens.List(ctx, true)
	if err != nil || len(list) != 1 || f.reqs("GET", "/v1/tokens?all=1") == nil {
		t.Fatal(list, err)
	}
	rv, err := c.Tokens.Revoke(ctx, "tok_1")
	if err != nil || rv.State != "revoked" || f.reqs("DELETE", "/v1/tokens/tok_1") == nil {
		t.Fatal(rv, err)
	}
	for _, o := range []CloudTokenOptions{{}, {Scopes: []string{"admin"}}, {Scopes: []string{"sandboxes:read", "sandboxes:read"}},
		{Scopes: []string{"sandboxes:read"}, TTLS: 30}, {Scopes: []string{"sandboxes:read"}, SandboxIDs: make([]string, 101)},
		{Scopes: []string{"sandboxes:read"}, Name: strings.Repeat("n", 65)}, {Scopes: []string{"sandboxes:read"}, Labels: map[string]string{"bad key!": ""}}} {
		if _, err := c.Tokens.Create(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, err := c.Tokens.Revoke(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	// A capability token works as the client's key.
	tc, err := NewCloud(CloudOptions{APIKey: "k8c_tok_abc", BaseURL: "https://api.example.com", Getenv: func(string) string { return "" }})
	if err != nil || tc == nil {
		t.Fatal(err)
	}
	if _, err := NewCloud(CloudOptions{APIKey: "sk_other", Getenv: func(string) string { return "" }}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestNewIdempotencyKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		k := NewIdempotencyKey()
		if len(k) != 36 || k[14] != '4' || seen[k] || checkIdempotencyKey(k) != nil {
			t.Fatal(k)
		}
		seen[k] = true
	}
}
