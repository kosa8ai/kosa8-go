package kosa8

// RL runs: what a training run did, recorded on kosa8 Cloud and shown on
// app.kosa8.com (RL runs). StartRun creates a run, leases its sandboxes and
// heartbeats them; Record buffers rollouts and sends them in batches of at
// most 500; MapRollouts runs one training step with sealed grading; Finish
// records what is left, drains the run's sandboxes and marks it done.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// RL run limits.
const (
	MaxRolloutsPerCall = 500  // rows per POST /v1/rl/runs/{id}/rollouts
	DefaultLeaseS      = 600  // a run's sandboxes outlive the last heartbeat by this
	RolloutStdoutTail  = 4096 // stdout kept on a recorded rollout
	rolloutMetaMax     = 8192
)

// DefaultMask are the failure classes masked by default: the sandbox or the
// platform failed, not the policy, so the rollout must not train as reward
// 0. FailureError is your own solve failing (a harness bug). Add
// FailureTimeout to mask timeouts too.
var DefaultMask = []FailureClass{FailureInfra, FailureQuota, FailureError}

// CloudRunOptions describe a run to start (or, with RunID, to attach to).
type CloudRunOptions struct {
	Name    string
	Project string
	Config  map[string]any
	// RunID attaches to an existing run (to keep recording after a restart)
	// instead of creating one.
	RunID string
	// LeaseS is how long the run's sandboxes outlive the last heartbeat
	// (default 600 s). Heartbeats go every HeartbeatInterval (default a
	// third of it), and also flush the buffered rollouts.
	LeaseS            int
	HeartbeatInterval time.Duration
	// KeepSandboxes leaves the run's sandboxes alive at Finish (default: it
	// drains them).
	KeepSandboxes bool
	// FlushEvery sends the buffer once it holds this many rows (1..500,
	// default 100).
	FlushEvery int
	// Logf reports background heartbeat and recording failures; default
	// log.Printf. func(string, ...any) {} silences them.
	Logf func(format string, args ...any)
}

// CloudRun is one RL training run on kosa8 Cloud.
//
//	run, err := cloud.StartRun(ctx, kosa8.CloudRunOptions{Name: "grpo-1"})
//	defer run.Finish(ctx, "finished")
//	results, err := kosa8.MapRollouts(ctx, run, base, tasks, solve, &kosa8.RunMapOptions[Task]{GradeCmd: ...})
//
// Its sandboxes are the ones labelled kosa8.run=<ID>: give RunID and
// LeaseS (run.ID, run.LeaseS) to any sandbox you make for it, as
// MapRollouts does.
type CloudRun struct {
	ID     string
	Name   string
	LeaseS int
	// Info is the run as the API last answered it.
	Info Info

	c          *Cloud
	o          CloudRunOptions
	mu         sync.Mutex
	buf        []RolloutRow
	flushMu    sync.Mutex
	recorded   int
	step       int
	open       bool
	stop       chan struct{}
	done       chan struct{}
	beats      int
	lastErr    error
	drained    *CloudDrainResult
	finishOnce sync.Once
}

// StartRun creates the run (or attaches to o.RunID) and starts heartbeating
// its sandboxes. Call Finish when done (defer it), so the rollouts left are
// recorded and the sandboxes deleted; if the process dies instead, the
// heartbeats stop and kosa8 Cloud deletes them LeaseS later.
func (c *Cloud) StartRun(ctx context.Context, o CloudRunOptions) (*CloudRun, error) {
	if o.Name == "" && o.RunID == "" {
		return nil, usage("give the run a Name, or RunID to attach to one")
	}
	if o.LeaseS == 0 {
		o.LeaseS = DefaultLeaseS
	}
	if o.LeaseS < CloudMinLeaseS || o.LeaseS > CloudMaxLeaseS {
		return nil, usage("LeaseS must be %d..%d seconds, not %d", CloudMinLeaseS, CloudMaxLeaseS, o.LeaseS)
	}
	if o.FlushEvery == 0 {
		o.FlushEvery = 100
	}
	if o.FlushEvery < 1 || o.FlushEvery > MaxRolloutsPerCall {
		return nil, usage("FlushEvery must be 1..%d, not %d", MaxRolloutsPerCall, o.FlushEvery)
	}
	if err := checkRunID(o.RunID); err != nil {
		return nil, err
	}
	if o.Logf == nil {
		o.Logf = log.New(os.Stderr, "", log.LstdFlags).Printf
	}
	r := &CloudRun{ID: o.RunID, Name: o.Name, LeaseS: o.LeaseS, c: c, o: o, Info: Info{}}
	if r.ID == "" {
		body := map[string]any{"name": o.Name}
		if o.Project != "" {
			body["project"] = o.Project
		}
		if len(o.Config) > 0 {
			body["config"] = o.Config
		}
		var a struct {
			Run Info `json:"run"`
		}
		if err := c.request(ctx, "POST", "/v1/rl/runs", body, &a); err != nil {
			return nil, err
		}
		r.Info = a.Run
		r.ID, _ = a.Run["id"].(string)
		if r.ID == "" {
			return nil, &Error{Kind: KindAPI, Message: "kosa8 Cloud answered no run id"}
		}
	}
	r.open = true
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	every := o.HeartbeatInterval
	if every <= 0 {
		every = time.Duration(o.LeaseS) * time.Second / 3
	}
	go r.loop(context.WithoutCancel(ctx), every)
	return r, nil
}

// AttachRun is StartRun for an existing run.
func (c *Cloud) AttachRun(ctx context.Context, runID string, o CloudRunOptions) (*CloudRun, error) {
	o.RunID = runID
	return c.StartRun(ctx, o)
}

func (r *CloudRun) loop(ctx context.Context, every time.Duration) {
	defer close(r.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *CloudRun) tick(ctx context.Context) {
	if _, err := r.c.Sandboxes.Heartbeat(ctx, CloudSelector{RunID: r.ID}, 0); err != nil {
		r.setErr(err)
		r.o.Logf("kosa8: heartbeat of run %s failed: %v", r.ID, err)
	} else {
		r.mu.Lock()
		r.beats++
		r.mu.Unlock()
	}
	if _, err := r.Flush(ctx); err != nil {
		r.setErr(err)
		r.o.Logf("kosa8: recording rollouts of run %s failed, will retry: %v", r.ID, err)
	}
}

func (r *CloudRun) setErr(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
}

// Beats is how many heartbeats went through; LastError the last background
// failure.
func (r *CloudRun) Beats() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beats
}

// LastError is the last error a background heartbeat or flush met.
func (r *CloudRun) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// Drained is what Finish's drain answered, if it drained.
func (r *CloudRun) Drained() *CloudDrainResult { return r.drained }

func (r *CloudRun) path(suffix string) string { return "/v1/rl/runs/" + url.PathEscape(r.ID) + suffix }

// RolloutRow is one rollout as POST /v1/rl/runs/{id}/rollouts takes it.
type RolloutRow struct {
	Step         *int               `json:"step,omitempty"`
	Group        string             `json:"group,omitempty"`
	Index        *int               `json:"index,omitempty"`
	SandboxID    string             `json:"sandbox_id,omitempty"`
	Reward       *float64           `json:"reward"`
	FailureClass FailureClass       `json:"failure_class,omitempty"` // default: from Exit
	Exit         *int               `json:"exit,omitempty"`
	DurationMs   int64              `json:"duration_ms,omitempty"`
	CostUSD      *float64           `json:"cost_usd,omitempty"`
	Tokens       *int               `json:"tokens,omitempty"`
	Metrics      map[string]float64 `json:"metrics,omitempty"` // at most 32
	Meta         map[string]any     `json:"meta,omitempty"`    // at most 8 KiB as JSON
	StdoutTail   string             `json:"stdout_tail,omitempty"`
}

// Record buffers rollouts. They are sent in batches (FlushEvery, at most
// 500 a call), at each heartbeat, and at Finish. It sends at once when the
// buffer is full, returning that send's error (the rows stay buffered).
func (r *CloudRun) Record(ctx context.Context, rows ...RolloutRow) error {
	r.mu.Lock()
	r.buf = append(r.buf, rows...)
	full := len(r.buf) >= r.o.FlushEvery
	r.mu.Unlock()
	if full {
		_, err := r.Flush(ctx)
		return err
	}
	return nil
}

// Pending is how many rows are buffered, not yet sent.
func (r *CloudRun) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

// Recorded is how many rows this CloudRun has sent.
func (r *CloudRun) Recorded() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorded
}

// Flush sends every buffered row now, 500 a call, and returns how many were
// recorded. On an error the unsent rows stay buffered. (Recording carries
// no Idempotency-Key, so a call is not resent after a 5xx; one whose answer
// was lost and that the next flush sends again may be recorded twice.)
func (r *CloudRun) Flush(ctx context.Context) (int, error) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	sent := 0
	for {
		r.mu.Lock()
		n := min(len(r.buf), MaxRolloutsPerCall)
		chunk := append([]RolloutRow(nil), r.buf[:n]...)
		r.buf = r.buf[n:]
		r.mu.Unlock()
		if len(chunk) == 0 {
			return sent, nil
		}
		var a struct {
			Recorded *int `json:"recorded"`
		}
		if err := r.c.request(ctx, "POST", r.path("/rollouts"), map[string]any{"rollouts": chunk}, &a); err != nil {
			r.mu.Lock()
			r.buf = append(chunk, r.buf...)
			r.mu.Unlock()
			return sent, err
		}
		got := len(chunk)
		if a.Recorded != nil {
			got = *a.Recorded
		}
		sent += got
		r.mu.Lock()
		r.recorded += got
		r.mu.Unlock()
	}
}

// Finish records what is buffered, stops the heartbeats, drains the run's
// sandboxes (unless KeepSandboxes) and marks the run status ("finished" or
// "failed"). Safe to call twice; only the first does anything.
func (r *CloudRun) Finish(ctx context.Context, status string) error {
	if status != "finished" && status != "failed" {
		return usage(`status is "finished" or "failed", not %q`, status)
	}
	var errs []error
	r.finishOnce.Do(func() {
		close(r.stop)
		<-r.done
		r.mu.Lock()
		r.open = false
		r.mu.Unlock()
		if _, err := r.Flush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("recording rollouts: %w", err))
		}
		if !r.o.KeepSandboxes {
			d, err := r.c.Sandboxes.Drain(ctx, CloudSelector{RunID: r.ID})
			r.drained = &d
			if err != nil {
				errs = append(errs, fmt.Errorf("draining: %w", err))
			}
		}
		if _, err := r.Update(ctx, status, ""); err != nil {
			errs = append(errs, fmt.Errorf("marking the run %s: %w", status, err))
		}
	})
	return errors.Join(errs...)
}

// Update renames the run or sets its status (running, finished, failed).
func (r *CloudRun) Update(ctx context.Context, status, name string) (Info, error) {
	body := map[string]any{}
	if status != "" {
		body["status"] = status
	}
	if name != "" {
		body["name"] = name
	}
	if len(body) == 0 {
		return nil, usage("give status or name")
	}
	var a struct {
		Run Info `json:"run"`
	}
	if err := r.c.request(ctx, "PATCH", r.path(""), body, &a); err != nil {
		return nil, err
	}
	if a.Run != nil {
		r.Info = a.Run
	}
	return r.Info, nil
}

// Summary is the run with its summary (rewards, failure classes, live
// sandboxes, cost) and recommendations: {"run", "summary", "recommendations"}.
func (r *CloudRun) Summary(ctx context.Context) (Info, error) {
	var out Info
	if err := r.c.request(ctx, "GET", r.path(""), nil, &out); err != nil {
		return nil, err
	}
	if run, ok := out["run"].(map[string]any); ok {
		r.Info = run
	}
	return out, nil
}

// RolloutQuery filters Rollouts; zero values are not sent.
type RolloutQuery struct {
	Step         *int
	FailureClass FailureClass
	MinReward    *float64
	MaxReward    *float64
	SandboxID    string
	ID           string
	Limit        int
	Cursor       string
}

// RolloutPage is one page of recorded rollouts. Each has source "grade"
// (recorded by the sealed grader) or "api" (by Record).
type RolloutPage struct {
	Rollouts   []Info  `json:"rollouts"`
	NextCursor *string `json:"next_cursor"`
}

// Rollouts returns one page of the run's recorded rollouts.
func (r *CloudRun) Rollouts(ctx context.Context, q RolloutQuery) (RolloutPage, error) {
	v := url.Values{}
	if q.Step != nil {
		v.Set("step", strconv.Itoa(*q.Step))
	}
	if q.FailureClass != "" {
		v.Set("failure_class", string(q.FailureClass))
	}
	if q.MinReward != nil {
		v.Set("min_reward", strconv.FormatFloat(*q.MinReward, 'g', -1, 64))
	}
	if q.MaxReward != nil {
		v.Set("max_reward", strconv.FormatFloat(*q.MaxReward, 'g', -1, 64))
	}
	if q.SandboxID != "" {
		v.Set("sandbox_id", q.SandboxID)
	}
	if q.ID != "" {
		v.Set("id", q.ID)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	p := r.path("/rollouts")
	if len(v) > 0 {
		p += "?" + v.Encode()
	}
	var out RolloutPage
	err := r.c.request(ctx, "GET", p, nil, &out)
	return out, err
}

// Export returns every rollout of the run as JSON lines, oldest first.
func (r *CloudRun) Export(ctx context.Context) ([]byte, error) {
	return r.c.do(ctx, "GET", r.path("/export?format=jsonl"), "", nil, "application/x-ndjson")
}

// Drain deletes the run's sandboxes now (the run stays open).
func (r *CloudRun) Drain(ctx context.Context) (CloudDrainResult, error) {
	return r.c.Sandboxes.Drain(ctx, CloudSelector{RunID: r.ID})
}

// CloudRunListOptions filter ListRuns.
type CloudRunListOptions struct {
	Project, Status, Name string
	Limit                 int
	Cursor                string
}

// ListRuns returns the org's runs, newest first, each with a short summary:
// {"runs": [...], "next_cursor": ...}.
func (c *Cloud) ListRuns(ctx context.Context, o CloudRunListOptions) (Info, error) {
	v := url.Values{}
	for k, s := range map[string]string{"project": o.Project, "status": o.Status, "name": o.Name, "cursor": o.Cursor} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if o.Limit > 0 {
		v.Set("limit", strconv.Itoa(o.Limit))
	}
	p := "/v1/rl/runs"
	if len(v) > 0 {
		p += "?" + v.Encode()
	}
	var out Info
	err := c.request(ctx, "GET", p, nil, &out)
	return out, err
}

// --- rollouts ----------------------------------------------------------------

// RolloutOutput is what a solve may return to give the reward itself (when
// there is no GradeCmd) and the rollout's tokens, cost and meta. A plain
// float64 or int works too, as does a map with "reward".
type RolloutOutput struct {
	Reward  *float64
	Metrics map[string]float64
	Tokens  *int
	CostUSD *float64
	Meta    map[string]any
	Value   any // anything else to keep on the result
}

// RolloutResult is one rollout. Masked means: leave it out of the gradient
// (the failure was not the policy's); then Reward is nil, never 0.
type RolloutResult[T any] struct {
	Task         T
	Group        int // which task (its position in tasks)
	Index        int // which sample of its group
	Step         *int
	Reward       *float64
	FailureClass FailureClass
	Masked       bool
	Metrics      map[string]float64
	Output       any // what solve returned
	ExitCode     *int
	Stdout       string
	Stderr       string
	Err          error
	Attempts     int
	Duration     time.Duration
	SandboxID    string
	GraderID     string
	Tokens       *int
	CostUSD      *float64
	// Recorded: kosa8 Cloud already recorded it (the grader does, given a
	// run), so it is not recorded again.
	Recorded bool
	Meta     map[string]any
}

// OK reports whether it trains: it ran (ok or exit_nonzero) and is not masked.
func (r RolloutResult[T]) OK() bool {
	return (r.FailureClass == FailureOK || r.FailureClass == FailureExitNonzero) && !r.Masked
}

// Row is the rollout as Record takes it. The SDK's own "error" class (your
// solve failed) is recorded as "client", with the error in meta.
func (r RolloutResult[T]) Row() RolloutRow {
	meta := map[string]any{}
	for k, v := range r.Meta {
		meta[k] = v
	}
	if r.Masked {
		meta["masked"] = true
	}
	if r.Err != nil {
		s := r.Err.Error()
		if len(s) > 2000 {
			s = s[:2000]
		}
		meta["error"] = s
	}
	if r.Attempts > 1 {
		meta["attempts"] = r.Attempts
	}
	fc := r.FailureClass
	if fc == FailureError {
		fc = FailureClient
		meta["sdk_failure_class"] = "error"
	}
	idx := r.Index
	row := RolloutRow{
		Step: r.Step, Group: strconv.Itoa(r.Group), Index: &idx, SandboxID: r.SandboxID, Reward: r.Reward,
		FailureClass: fc, Exit: r.ExitCode, DurationMs: r.Duration.Milliseconds(), Tokens: r.Tokens, CostUSD: r.CostUSD,
		Metrics: r.Metrics,
	}
	if len(meta) > 0 {
		row.Meta = fitMeta(meta)
	}
	if s := r.Stdout; len(s) > RolloutStdoutTail {
		row.StdoutTail = s[len(s)-RolloutStdoutTail:]
	} else {
		row.StdoutTail = s
	}
	return row
}

// fitMeta drops the largest string values until meta fits in 8 KiB as JSON.
func fitMeta(meta map[string]any) map[string]any {
	for {
		b, err := json.Marshal(meta)
		if err == nil && len(b) <= rolloutMetaMax {
			return meta
		}
		big, size := "", -1
		for k, v := range meta {
			vb, _ := json.Marshal(v)
			if len(vb) > size {
				big, size = k, len(vb)
			}
		}
		if big == "" {
			return nil
		}
		delete(meta, big)
		meta["truncated"] = true
	}
}

// Rollouts are a map's results, in task order (group_size per task).
type Rollouts[T any] []RolloutResult[T]

// Rewards are each reward, nil where masked.
func (rs Rollouts[T]) Rewards() []*float64 {
	out := make([]*float64, len(rs))
	for i, r := range rs {
		out[i] = r.Reward
	}
	return out
}

// Mask is true where the rollout should train (not masked).
func (rs Rollouts[T]) Mask() []bool {
	out := make([]bool, len(rs))
	for i, r := range rs {
		out[i] = !r.Masked
	}
	return out
}

// MeanReward is the mean of the rewards that are set; ok false if none.
func (rs Rollouts[T]) MeanReward() (float64, bool) {
	sum, n := 0.0, 0
	for _, r := range rs {
		if r.Reward != nil {
			sum += *r.Reward
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// ByClass counts the rollouts of each failure class.
func (rs Rollouts[T]) ByClass() map[FailureClass]int {
	out := map[FailureClass]int{}
	for _, r := range rs {
		out[r.FailureClass]++
	}
	return out
}

// Groups are the rollouts of each task together, for group-relative
// advantages (GRPO).
func (rs Rollouts[T]) Groups() [][]RolloutResult[T] {
	by := map[int][]RolloutResult[T]{}
	for _, r := range rs {
		by[r.Group] = append(by[r.Group], r)
	}
	keys := make([]int, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([][]RolloutResult[T], len(keys))
	for i, k := range keys {
		out[i] = by[k]
	}
	return out
}

// RunMapOptions configure MapRollouts.
type RunMapOptions[T any] struct {
	// GradeCmd grades each solved sandbox in a sealed fork (hidden files,
	// no network) and gives the reward. Without it, Reward or solve's own
	// return gives it.
	GradeCmd []string
	// Files are the hidden grader files for a task, e.g.
	// {"/grader/test.py": ...}.
	Files        func(task T) map[string][]byte
	GradeTimeout int               // seconds, 1..90
	GradeEnv     map[string]string // NAME=value for the grader
	// Reward gives the reward when there is no GradeCmd: a float64, an
	// int, a RolloutOutput or a map with "reward".
	Reward func(ctx context.Context, sb *CloudSandbox, task T, output any) (any, error)
	// Concurrency rollouts at once (default 16); GroupSize samples per task
	// (GRPO's G, default 1).
	Concurrency int
	GroupSize   int
	// Step defaults to this run's count of maps.
	Step *int
	// Policy retries infra failures and rate limits on a fresh sandbox, with
	// backoff and jitter (a failed grade is retried on its own first).
	// Default DefaultRetryPolicy() (3 attempts).
	Policy *RetryPolicy
	// Timeout caps solve, per attempt.
	Timeout time.Duration
	// Mask: failure classes masked (Reward nil, Masked true). Default
	// DefaultMask (infra, quota, error).
	Mask []FailureClass
	// Keep leaves each solved sandbox alive (still leased) instead of
	// deleting it.
	Keep bool
	// Meta adds to each rollout's meta from the task and solve's output.
	Meta func(task T, output any) map[string]any
	// Make creates a rollout's sandbox instead of forking base (base nil).
	Make func(ctx context.Context, task T) (*CloudSandbox, error)
}

var errNoReward = errors.New("no reward: set GradeCmd (a sealed grader) or Reward, or return a number or RolloutOutput from solve")

// fromOutput takes the reward, metrics, tokens, cost and meta from what
// solve (or Reward) returned; true if it gave a reward.
func fromOutput[T any](res *RolloutResult[T], out any) bool {
	switch v := out.(type) {
	case nil, bool:
		return false
	case float64:
		res.Reward = Ptr(v)
		return true
	case float32:
		res.Reward = Ptr(float64(v))
		return true
	case int:
		res.Reward = Ptr(float64(v))
		return true
	case int64:
		res.Reward = Ptr(float64(v))
		return true
	case RolloutOutput:
		return fromOutput(res, &v)
	case *RolloutOutput:
		if v == nil {
			return false
		}
		if v.Tokens != nil {
			res.Tokens = v.Tokens
		}
		if v.CostUSD != nil {
			res.CostUSD = v.CostUSD
		}
		for k, m := range v.Meta {
			res.Meta[k] = m
		}
		if v.Reward != nil {
			res.Reward = Ptr(*v.Reward)
			if v.Metrics != nil {
				res.Metrics = v.Metrics
			}
			return true
		}
	case map[string]any:
		if t, ok := v["tokens"].(int); ok {
			res.Tokens = &t
		}
		if m, ok := v["meta"].(map[string]any); ok {
			for k, x := range m {
				res.Meta[k] = x
			}
		}
		if f, ok := toFloat(v["cost_usd"]); ok {
			res.CostUSD = &f
		}
		if f, ok := toFloat(v["reward"]); ok {
			res.Reward = &f
			if m, ok := v["metrics"].(map[string]float64); ok {
				res.Metrics = m
			}
			return true
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// MapRollouts is one training step on run: solve(ctx, sandbox, task) for
// every task (GroupSize samples each), Concurrency at a time.
//
// For each rollout: fork base (labelled with the run and leased; or Make
// one), run solve in it, grade it in a sealed fork with GradeCmd and the
// task's hidden Files, record it on the run, delete the sandbox. Infra
// failures and rate limits are retried on a fresh sandbox (a grade that did
// not finish is retried alone, with a new Idempotency-Key). What still
// fails in Mask comes back Masked with Reward nil, never 0. Results are in
// task order. The error is only for bad options or ctx ending; every
// sandbox it made is deleted before it returns (unless Keep).
func MapRollouts[T any](ctx context.Context, run *CloudRun, base *CloudSandbox, tasks []T, solve func(ctx context.Context, sb *CloudSandbox, task T) (any, error), o *RunMapOptions[T]) (Rollouts[T], error) {
	if run == nil || solve == nil {
		return nil, usage("MapRollouts needs a run and a solve function")
	}
	run.mu.Lock()
	open := run.open
	run.mu.Unlock()
	if !open {
		return nil, usage("the run is finished: start a new one with StartRun")
	}
	if o == nil {
		o = &RunMapOptions[T]{}
	}
	if (base == nil) == (o.Make == nil) {
		return nil, usage("give a base sandbox to fork, or Make to create each sandbox")
	}
	group := o.GroupSize
	if group == 0 {
		group = 1
	}
	conc := o.Concurrency
	if conc == 0 {
		conc = 16
	}
	if group < 1 || conc < 1 {
		return nil, usage("GroupSize and Concurrency must be at least 1")
	}
	mask := o.Mask
	if mask == nil {
		mask = DefaultMask
	}
	masked := map[FailureClass]bool{}
	for _, m := range mask {
		masked[m] = true
	}
	policy := DefaultRetryPolicy()
	if o.Policy != nil {
		policy = *o.Policy
	}
	run.mu.Lock()
	step := run.step
	run.step++
	run.mu.Unlock()
	if o.Step != nil {
		step = *o.Step
	}
	n := len(tasks) * group
	out := make(Rollouts[T], n)
	if n == 0 {
		return out, nil
	}
	pool := newChildPool(run.c, base, nil, run.ID, run.LeaseS)
	defer pool.close(ctx)
	if base != nil {
		pool.prefork(ctx, min(n, conc))
	}

	runPool(ctx, n, conc, func(i int) {
		ti, gi := i/group, i%group
		task := tasks[ti]
		res := &out[i]
		*res = RolloutResult[T]{Task: task, Group: ti, Index: gi, Step: Ptr(step), Meta: map[string]any{}}
		t0 := time.Now()
		var output any
		for attempt := 1; ; attempt++ {
			res.Attempts = attempt
			if ctx.Err() != nil {
				res.FailureClass, res.Err = FailureError, ErrCancelled
				break
			}
			var child *CloudSandbox
			var err error
			if o.Make != nil {
				child, err = o.Make(ctx, task)
				if child != nil {
					if aerr := pool.adopt(ctx, []*CloudSandbox{child}); aerr != nil {
						child, err = nil, aerr
					}
				}
			} else {
				child, err = pool.take(ctx)
			}
			if err == nil {
				err = rolloutAttempt(ctx, run, pool, child, task, solve, o, res, policy, &output)
			}
			if child != nil {
				if o.Keep && res.FailureClass != FailureInfra {
					pool.forget(child)
				} else {
					pool.release(ctx, child)
				}
			}
			if err == nil {
				break
			}
			fc, retryable := ClassifyFailure(err)
			res.FailureClass, res.Err, res.Reward = fc, err, nil
			if !retryable || attempt >= policy.attempts() {
				break
			}
			if sleepCtx(ctx, policy.Delay(err, attempt)) != nil {
				break
			}
		}
		res.Duration = time.Since(t0)
		if o.Meta != nil {
			for k, v := range o.Meta(task, output) {
				res.Meta[k] = v
			}
		}
		res.Masked = masked[res.FailureClass]
		if res.Masked {
			res.Reward = nil
		}
		if !res.Recorded {
			_ = run.Record(ctx, res.Row()) // recording never fails a rollout
		}
	})
	return out, ctx.Err()
}

// rolloutAttempt runs solve in child and grades it; an error means the
// attempt failed (and may be retried).
func rolloutAttempt[T any](ctx context.Context, run *CloudRun, pool *childPool, child *CloudSandbox, task T,
	solve func(ctx context.Context, sb *CloudSandbox, task T) (any, error), o *RunMapOptions[T], res *RolloutResult[T],
	policy RetryPolicy, output *any) error {
	res.SandboxID = child.ID()
	out, err := callWithTimeout(ctx, o.Timeout, func(ctx context.Context) (any, error) { return solve(ctx, child, task) },
		func() { pool.release(ctx, child) })
	if err != nil {
		return err
	}
	*output = out
	res.Output = out
	res.FailureClass, res.Err = FailureOK, nil
	gave := fromOutput(res, out)
	switch {
	case len(o.GradeCmd) > 0:
		// The solved state is still there: retry the grader alone, not solve.
		var files map[string][]byte
		if o.Files != nil {
			files = o.Files(task)
		}
		idx := res.Index
		info := &CloudRolloutInfo{Step: res.Step, Group: strconv.Itoa(res.Group), Index: &idx}
		if len(res.Meta) > 0 {
			info.Meta = res.Meta
		}
		var g CloudGradeResult
		gerr := Retry(ctx, policy, func(ctx context.Context) error {
			var e error
			g, e = child.Grade(ctx, o.GradeCmd, &CloudGradeOptions{Files: files, TimeoutS: o.GradeTimeout, Env: o.GradeEnv, RunID: run.ID, Rollout: info})
			if e == nil && g.Reward == nil && g.Retryable {
				return &GradeError{Result: g} // did not finish: grade again, with a new key
			}
			return e
		})
		var ge *GradeError
		if gerr != nil && !errors.As(gerr, &ge) {
			return gerr
		}
		applyGrade(res, g)
	case o.Reward != nil:
		r, err := o.Reward(ctx, child, task, out)
		if err != nil {
			return err
		}
		if !fromOutput(res, r) {
			return errors.New("Reward must return a number, a RolloutOutput or a map with \"reward\"")
		}
	case !gave:
		return errNoReward
	}
	return nil
}

func applyGrade[T any](res *RolloutResult[T], g CloudGradeResult) {
	res.Reward = g.Reward
	res.Metrics = g.Metrics
	res.ExitCode = g.ExitCode
	res.Stdout, res.Stderr = g.Stdout, g.Stderr
	res.FailureClass = g.FailureClass
	if res.FailureClass == "" {
		res.FailureClass, _ = execFailure(g.ExitCode)
	}
	res.GraderID = g.GraderID
	if g.Reward == nil && g.Error != "" {
		msg := "grader: " + g.Error
		if g.Message != nil && *g.Message != "" {
			msg += ": " + *g.Message
		}
		res.Err = errors.New(msg)
	}
	// kosa8 Cloud recorded it (source "grade") when given a run: never
	// twice. Recorded false (recording failed there) leaves it to Record.
	res.Recorded = g.Recorded != nil && *g.Recorded
}
