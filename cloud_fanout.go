package kosa8

// Fan-out for RL rollouts and evals: many sandboxes at once (CreateMany,
// ForkWith), leases so a crashed trainer leaks none (Heartbeat), a drain to
// clean up a run (Drain), and a sealed grader (Grade).

import (
	"context"
	"encoding/base64"
	"net/url"
	"regexp"
	"strings"
)

// Fan-out limits the Cloud API checks; the SDK checks them first.
const (
	CloudMaxBatch       = 1000 // sandboxes per CreateMany or ForkWith call
	CloudMinLeaseS      = 60
	CloudMaxLeaseS      = 86400
	CloudGradeMaxFiles  = 64
	CloudGradeMaxBytes  = 8 << 20
	CloudGradeMaxTimeS  = 90
	CloudMaxDrainRounds = 100 // drain calls at most (1000 sandboxes each)
	// RunLabel is the label kosa8 Cloud puts on every sandbox made for a run
	// (RunID): kosa8.run=<run id>. Heartbeat, Drain and the RL run's usage
	// select by it.
	RunLabel = "kosa8.run"
)

var (
	runIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	prefixRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,58}$`)
	sbxIDRE  = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func leaseBody(body map[string]any, lease, ttl int, deleteAfter int64) error {
	if lease == 0 {
		return nil
	}
	if lease < CloudMinLeaseS || lease > CloudMaxLeaseS {
		return usage("LeaseS must be %d..%d seconds, not %d", CloudMinLeaseS, CloudMaxLeaseS, lease)
	}
	if ttl != 0 || deleteAfter != 0 {
		return usage("set LeaseS or TTLSeconds/DeleteAfter, not both: a lease is a TTL that heartbeats move forward")
	}
	body["lease_s"] = lease
	return nil
}

func checkRunID(id string) error {
	if id != "" && !runIDRE.MatchString(id) {
		return usage("RunID %q: 1..128 of letters, digits, '_', '.', ':' or '-', starting with a letter or digit", id)
	}
	return nil
}

// fanoutBody adds the fields a batch and a fork share.
func fanoutBody(body map[string]any, labels map[string]string, runID string, lease int, prefix string, partialOK *bool) error {
	if len(labels) > 0 {
		if err := checkLabels(labels); err != nil {
			return err
		}
		body["labels"] = labels
	}
	if err := checkRunID(runID); err != nil {
		return err
	}
	if runID != "" {
		body["run_id"] = runID
	}
	if err := leaseBody(body, lease, 0, 0); err != nil {
		return err
	}
	if prefix != "" {
		if !prefixRE.MatchString(prefix) {
			return usage("Prefix %q: 1..59 of letters, digits, '.', '_' or '-', starting with a letter or digit", prefix)
		}
		body["prefix"] = prefix
	}
	if partialOK != nil {
		body["partial_ok"] = *partialOK
	}
	return nil
}

// CloudFanoutFailure is one sandbox a batch, fork or drain could not make
// (or delete): Index for a batch or fork, ID for a drain.
type CloudFanoutFailure struct {
	Index        *int         `json:"index"`
	ID           string       `json:"id,omitempty"`
	Code         string       `json:"code"`
	Message      string       `json:"message"`
	FailureClass FailureClass `json:"failure_class"`
	Retryable    bool         `json:"retryable"`
}

// Err is the failure as an *Error, with its class.
func (f CloudFanoutFailure) Err() error {
	msg := f.Message
	if msg == "" {
		msg = f.Code
	}
	if msg == "" {
		msg = "not created"
	}
	return &Error{Kind: KindAPI, Message: msg, Code: f.Code, FailureClass: f.FailureClass, Retryable: f.Retryable}
}

// CloudSandboxBatch is what a CreateMany or ForkWith made: the sandboxes in
// index order and the ones that could not be made.
type CloudSandboxBatch struct {
	Sandboxes []*CloudSandbox
	Failed    []CloudFanoutFailure
	Requested int
	Created   int
	RunID     string
}

// IDs are the made sandboxes' ids.
func (b *CloudSandboxBatch) IDs() []string {
	out := make([]string, len(b.Sandboxes))
	for i, s := range b.Sandboxes {
		out[i] = s.ID()
	}
	return out
}

type batchAnswer struct {
	Sandboxes []CloudSandboxInfo   `json:"sandboxes"`
	Failed    []CloudFanoutFailure `json:"failed"`
	Requested int                  `json:"requested"`
	Created   *int                 `json:"created"`
	RunID     string               `json:"run_id"`
}

func (a batchAnswer) batch(c *Cloud, n int) *CloudSandboxBatch {
	out := &CloudSandboxBatch{Failed: a.Failed, Requested: a.Requested, RunID: a.RunID}
	if out.Requested == 0 {
		out.Requested = n
	}
	for _, i := range a.Sandboxes {
		out.Sandboxes = append(out.Sandboxes, &CloudSandbox{c: c, Info: i})
	}
	out.Created = len(out.Sandboxes)
	if a.Created != nil {
		out.Created = *a.Created
	}
	if out.Failed == nil {
		out.Failed = []CloudFanoutFailure{}
	}
	return out
}

// CloudBatchOptions are a CreateMany: every create field of
// CloudCreateOptions applies to each sandbox (Labels on every one; Name is
// not allowed, use Prefix).
type CloudBatchOptions struct {
	CloudCreateOptions
	// Count is how many (1..1000).
	Count int
	// RunID labels every sandbox kosa8.run=<RunID>, which Heartbeat, Drain
	// and RL runs select by.
	RunID string
	// Prefix names them <Prefix>-0001, <Prefix>-0002, ...
	Prefix string
	// PartialOK: true (the API's default) makes what fits and lists the rest
	// in Failed; false is all or nothing.
	PartialOK *bool
}

// CreateMany creates up to 1000 sandboxes in one call, paced by kosa8 (16 at
// a time), each held to the rules of a Create. Past the plan's running limit
// it makes what fits and lists the rest in Failed (code running_limit,
// FailureClass quota), unless PartialOK is false. Nothing made is an error.
// The call carries an Idempotency-Key reused on every retry, so a retry
// never doubles the fleet. Unless NoWait, it waits for sized sandboxes that
// are still booting.
func (s *CloudSandboxes) CreateMany(ctx context.Context, o CloudBatchOptions) (*CloudSandboxBatch, error) {
	if o.Count < 1 || o.Count > CloudMaxBatch {
		return nil, usage("Count must be 1..%d, not %d", CloudMaxBatch, o.Count)
	}
	if o.Name != "" {
		return nil, usage("a batch names its sandboxes with Prefix (<prefix>-0001, ...), not Name")
	}
	body := map[string]any{"count": o.Count}
	cr := o.CloudCreateOptions
	if cr.Template != "" {
		body["template"] = cr.Template
	}
	if cr.Region != "" {
		body["region"] = cr.Region
	}
	if err := checkSleepAfter(cr.SleepAfterS); err != nil {
		return nil, err
	}
	if cr.SleepAfterS != nil {
		body["sleep_after_s"] = *cr.SleepAfterS
	}
	if err := lifetimeBody(body, cr.TTLSeconds, cr.DeleteAfter); err != nil {
		return nil, err
	}
	if cr.LeaseS != 0 && (cr.TTLSeconds != 0 || cr.DeleteAfter != 0) {
		return nil, usage("set LeaseS or TTLSeconds/DeleteAfter, not both: a lease is a TTL that heartbeats move forward")
	}
	if cr.AcceptPrice != nil {
		body["accept_price"] = *cr.AcceptPrice
	}
	labels := cr.Labels
	cr.Labels = nil
	if err := sizedBody(body, cr); err != nil {
		return nil, err
	}
	if err := fanoutBody(body, labels, o.RunID, cr.LeaseS, o.Prefix, o.PartialOK); err != nil {
		return nil, err
	}
	var a batchAnswer
	if err := s.c.requestKeyed(ctx, "POST", "/v1/sandboxes/batch", body, &a, cr.IdempotencyKey); err != nil {
		return nil, err
	}
	out := a.batch(s.c, o.Count)
	if !cr.NoWait {
		for _, b := range out.Sandboxes {
			if b.Info.State == "creating" {
				if err := b.WaitRunning(ctx, 0); err != nil {
					return out, err
				}
			}
		}
	}
	return out, nil
}

// CloudForkOptions are a ForkWith.
type CloudForkOptions struct {
	N      int               // copies, 1..1000 (0 means 1); past 100 kosa8 makes them 100 at a time
	Labels map[string]string // on every copy
	RunID  string            // labels every copy kosa8.run=<RunID>
	LeaseS int               // leases every copy (60..86400 s); see CloudCreateOptions.LeaseS
	Prefix string            // names them <Prefix>-0001, ...
	// PartialOK: true makes what fits and lists the rest in Failed; false is
	// all or nothing. The API's default is true when N is over 100 or a
	// fan-out field is given, else false.
	PartialOK *bool
	// IdempotencyKey (default a new UUID, renewed after a 5xx) is reused on
	// every retry; yours is never changed.
	IdempotencyKey string
}

// ForkWith makes live copies of the sandbox as it is now, on its host, with
// labels, a run, a lease and names. Each is held to the rules of a create.
// With PartialOK, copies that could not be made (no room on the host, the
// running limit) are in Failed; a fork that made none is an error (its
// Body has requested, created and failed). A sandbox with secrets cannot be
// forked (409 secrets). May answer 501 on hosts without the data plane.
func (b *CloudSandbox) ForkWith(ctx context.Context, o CloudForkOptions) (*CloudSandboxBatch, error) {
	n := o.N
	if n == 0 {
		n = 1
	}
	if n < 1 || n > CloudMaxBatch {
		return nil, usage("a fork makes 1..%d copies, not %d", CloudMaxBatch, n)
	}
	body := map[string]any{"n": n}
	if err := fanoutBody(body, o.Labels, o.RunID, o.LeaseS, o.Prefix, o.PartialOK); err != nil {
		return nil, err
	}
	var a batchAnswer
	if err := b.c.requestKeyed(ctx, "POST", b.path("/fork"), body, &a, o.IdempotencyKey); err != nil {
		return nil, err
	}
	return a.batch(b.c, n), nil
}

// CloudSelector picks sandboxes for Heartbeat and Drain: by run, by labels
// (every one must match), by ids, or a combination (all must hold).
type CloudSelector struct {
	RunID  string
	Labels map[string]string
	IDs    []string
}

func (sel CloudSelector) body() (map[string]any, error) {
	body := map[string]any{}
	if err := checkRunID(sel.RunID); err != nil {
		return nil, err
	}
	if sel.RunID != "" {
		body["run_id"] = sel.RunID
	}
	if len(sel.Labels) > 0 {
		if err := checkLabels(sel.Labels); err != nil {
			return nil, err
		}
		body["labels"] = sel.Labels
	}
	if len(sel.IDs) > 0 {
		if len(sel.IDs) > CloudMaxBatch {
			return nil, usage("at most %d ids a call, not %d", CloudMaxBatch, len(sel.IDs))
		}
		for _, id := range sel.IDs {
			if !sbxIDRE.MatchString(id) {
				return nil, usage("%q is not a sandbox id", id)
			}
		}
		body["ids"] = sel.IDs
	}
	if len(body) == 0 {
		return nil, usage("give RunID, Labels or IDs: which sandboxes")
	}
	return body, nil
}

// Heartbeat pushes back the leases of the matching leased sandboxes to now
// plus extendS (0: each one's own lease_s) and returns how many moved. A
// lapsed lease is not brought back. Send it every lease / 3 or so; a
// CloudRun does it in the background.
func (s *CloudSandboxes) Heartbeat(ctx context.Context, sel CloudSelector, extendS int) (int, error) {
	body, err := sel.body()
	if err != nil {
		return 0, err
	}
	if extendS != 0 {
		if extendS < CloudMinLeaseS || extendS > CloudMaxLeaseS {
			return 0, usage("extendS must be %d..%d seconds, not %d", CloudMinLeaseS, CloudMaxLeaseS, extendS)
		}
		body["extend_s"] = extendS
	}
	var r struct {
		Extended int `json:"extended"`
	}
	err = s.c.request(ctx, "POST", "/v1/sandboxes/heartbeat", body, &r)
	return r.Extended, err
}

// Heartbeat pushes back this sandbox's lease; see CloudSandboxes.Heartbeat.
func (b *CloudSandbox) Heartbeat(ctx context.Context, extendS int) (int, error) {
	return b.c.Sandboxes.Heartbeat(ctx, CloudSelector{IDs: []string{b.ID()}}, extendS)
}

// CloudDrainResult is what a Drain deleted, and what it could not.
type CloudDrainResult struct {
	Deleted int
	IDs     []string
	Failed  []CloudFanoutFailure
}

// Drain deletes every matching sandbox of the org. kosa8 Cloud deletes up to
// 1000 a call; Drain calls again while it says there are more (and stops if
// a call deleted none).
func (s *CloudSandboxes) Drain(ctx context.Context, sel CloudSelector) (CloudDrainResult, error) {
	body, err := sel.body()
	out := CloudDrainResult{IDs: []string{}, Failed: []CloudFanoutFailure{}}
	if err != nil {
		return out, err
	}
	for i := 0; i < CloudMaxDrainRounds; i++ {
		var r struct {
			Deleted int                  `json:"deleted"`
			IDs     []string             `json:"ids"`
			Failed  []CloudFanoutFailure `json:"failed"`
			More    bool                 `json:"more"`
		}
		if err := s.c.request(ctx, "POST", "/v1/sandboxes/drain", body, &r); err != nil {
			return out, err
		}
		out.Deleted += r.Deleted
		out.IDs = append(out.IDs, r.IDs...)
		out.Failed = append(out.Failed, r.Failed...)
		if !r.More || r.Deleted == 0 {
			break
		}
	}
	return out, nil
}

// CloudRolloutInfo says which rollout a grade is, when it is recorded on an
// RL run.
type CloudRolloutInfo struct {
	Step  *int           `json:"step,omitempty"`
	Group string         `json:"group,omitempty"`
	Index *int           `json:"index,omitempty"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// CloudGradeOptions are the hidden files and limits of a Grade.
type CloudGradeOptions struct {
	// Files are written only in the sealed copy: {"/grader/test.py": ...};
	// at most 64, 8 MiB in all. The graded sandbox never sees them.
	Files    map[string][]byte
	TimeoutS int               // 1..90 (0: the API's default)
	Env      map[string]string // sent as NAME=value
	Keep     bool              // keep the copy (with a 1 h lease) to look at
	// RunID records the grade as a rollout of this RL run (Recorded says it
	// did), with Rollout's step, group, index and meta.
	RunID   string
	Rollout *CloudRolloutInfo
	// IdempotencyKey (default a new UUID) is reused on every retry, so a
	// retried grade answers the first grade's result and records nothing
	// twice.
	IdempotencyKey string
}

// CloudGradeResult is a grade's reward and what the grader printed.
type CloudGradeResult struct {
	// Reward is the last stdout line that is JSON with a numeric "reward",
	// else 1.0 on exit 0 and 0.0 otherwise; nil when the command did not
	// finish (FailureClass timeout or infra), so mask it rather than score 0.
	Reward       *float64           `json:"reward"`
	Metrics      map[string]float64 `json:"metrics"`
	RewardFrom   *string            `json:"reward_from"` // json | exit
	ExitCode     *int               `json:"exit"`
	Stdout       string             `json:"stdout"` // the last 64 KiB
	Stderr       string             `json:"stderr"`
	FailureClass FailureClass       `json:"failure_class"`
	Retryable    bool               `json:"retryable"`
	Ms           int64              `json:"ms"`
	GraderID     string             `json:"grader_id"`
	Kept         bool               `json:"kept"`
	SandboxID    string             `json:"sandbox_id"`
	RunID        string             `json:"run_id"`
	Recorded     *bool              `json:"recorded"`
	Error        string             `json:"error"`
	Message      *string            `json:"message"`
}

// GradeError is a grade that answered but whose command did not finish on
// a retryable failure (the host failed); ClassifyFailure reads its class.
type GradeError struct{ Result CloudGradeResult }

func (e *GradeError) Error() string {
	msg := e.Result.Error
	if e.Result.Message != nil && *e.Result.Message != "" {
		msg = *e.Result.Message
	}
	if msg == "" {
		msg = "the grader did not finish"
	}
	return "grade: " + msg
}

func gradeBody(cmd []string, o *CloudGradeOptions) (map[string]any, string, error) {
	if len(cmd) == 0 || len(cmd) > 256 {
		return nil, "", usage("Grade needs a command of 1..256 arguments")
	}
	body := map[string]any{"cmd": cmd}
	if o == nil {
		return body, "", nil
	}
	if len(o.Files) > 0 {
		if len(o.Files) > CloudGradeMaxFiles {
			return nil, "", usage("at most %d grader files, not %d", CloudGradeMaxFiles, len(o.Files))
		}
		total := 0
		files := map[string]string{}
		for p, data := range o.Files {
			if !strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") || containsDotDot(p) {
				return nil, "", usage("grader file path %q must be absolute, without ..", p)
			}
			total += len(data)
			if total > CloudGradeMaxBytes {
				return nil, "", usage("grader files add up to more than %d MiB", CloudGradeMaxBytes>>20)
			}
			files[p] = base64.StdEncoding.EncodeToString(data)
		}
		body["files"] = files
	}
	if o.TimeoutS != 0 {
		if o.TimeoutS < 1 || o.TimeoutS > CloudGradeMaxTimeS {
			return nil, "", usage("TimeoutS must be 1..%d, not %d", CloudGradeMaxTimeS, o.TimeoutS)
		}
		body["timeout_s"] = o.TimeoutS
	}
	if len(o.Env) > 0 {
		for k := range o.Env {
			if !envKeyRE.MatchString(k) {
				return nil, "", usage("env name %q is not a NAME", k)
			}
		}
		body["env"] = envList(o.Env)
	}
	if o.Keep {
		body["keep"] = true
	}
	if err := checkRunID(o.RunID); err != nil {
		return nil, "", err
	}
	if o.RunID != "" {
		body["run_id"] = o.RunID
	}
	if o.Rollout != nil {
		if len(o.Rollout.Group) > 200 {
			return nil, "", usage("Rollout.Group is at most 200 characters")
		}
		body["rollout"] = o.Rollout
	}
	return body, o.IdempotencyKey, nil
}

func containsDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// Grade grades this sandbox's state with hidden tests, in a sealed copy:
// kosa8 Cloud forks one copy, takes all network away from it, writes
// o.Files there, runs cmd, reads the reward and deletes the copy (unless
// Keep). A command that does not finish answers a result with Reward nil
// and its FailureClass, not an error. 501: the host cannot seal the copy,
// so nothing was run.
func (b *CloudSandbox) Grade(ctx context.Context, cmd []string, o *CloudGradeOptions) (CloudGradeResult, error) {
	body, key, err := gradeBody(cmd, o)
	if err != nil {
		return CloudGradeResult{}, err
	}
	var r CloudGradeResult
	err = b.c.requestKeyed(ctx, "POST", b.path("/grade"), body, &r, key)
	return r, err
}

// Grade grades the sandbox with this id; see CloudSandbox.Grade.
func (s *CloudSandboxes) Grade(ctx context.Context, id string, cmd []string, o *CloudGradeOptions) (CloudGradeResult, error) {
	if !sbxIDRE.MatchString(id) {
		return CloudGradeResult{}, usage("%q is not a sandbox id", id)
	}
	box := &CloudSandbox{c: s.c, Info: CloudSandboxInfo{ID: id}}
	return box.Grade(ctx, cmd, o)
}

// quietDelete deletes a sandbox, ignoring one that is gone already or an API
// that will not answer (its lease or TTL then deletes it).
func quietDelete(ctx context.Context, b *CloudSandbox) {
	_ = b.c.request(ctx, "DELETE", "/v1/sandboxes/"+url.PathEscape(b.ID()), nil, nil)
}
