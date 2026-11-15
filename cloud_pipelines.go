package kosa8

// kosa8 Pipelines: CI on kosa8 Cloud. A run reads the repository's
// .kosa8/pipeline.yml (or runs its .github/workflows through act), each job
// in a sandbox of its own.
//
//	run, _ := cloud.Pipelines.Run(ctx, kosa8.CloudPipelineRunOptions{RepoURL: "https://github.com/me/app", Ref: "main"})
//	run, _ = cloud.Pipelines.Wait(ctx, run.ID, 0)
//	fmt.Println(run.State) // success

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"time"
)

// CloudPipelineJob is one job of a run.
type CloudPipelineJob struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind"` // native | act
	State        string          `json:"state"`
	Size         *string         `json:"size"`
	Needs        []string        `json:"needs"`
	SandboxID    *string         `json:"sandbox_id"`
	ExitCode     *int            `json:"exit"`
	Error        *string         `json:"error"`
	Steps        json.RawMessage `json:"steps,omitempty"`
	LogBytes     int64           `json:"log_bytes"`
	LogTruncated bool            `json:"log_truncated"`
	StartedAt    *string         `json:"started_at"`
	FinishedAt   *string         `json:"finished_at"`
	DurationMs   *int64          `json:"duration_ms"`
}

// CloudPipelineRun is one pipeline run. State is queued, running, or one of
// the finished states success, failure, cancelled, skipped, error.
type CloudPipelineRun struct {
	ID         string             `json:"id"`
	RepoURL    string             `json:"repo_url"`
	Ref        string             `json:"ref"`
	SHA        *string            `json:"sha"`
	Branch     *string            `json:"branch"`
	Event      string             `json:"event"`
	Workflow   *string            `json:"workflow"`
	Job        *string            `json:"job"`
	Mode       *string            `json:"mode"` // native | act
	State      string             `json:"state"`
	Error      *string            `json:"error"`
	Warnings   []string           `json:"warnings"`
	Size       *string            `json:"size"`
	Secrets    []string           `json:"secrets"`
	HookID     *string            `json:"hook_id"`
	TimeoutS   int                `json:"timeout_s"`
	CreatedAt  string             `json:"created_at"`
	StartedAt  *string            `json:"started_at"`
	FinishedAt *string            `json:"finished_at"`
	DurationMs *int64             `json:"duration_ms"`
	Jobs       []CloudPipelineJob `json:"jobs"`
}

// Done reports whether the run has finished.
func (r CloudPipelineRun) Done() bool {
	switch r.State {
	case "success", "failure", "cancelled", "skipped", "error":
		return true
	}
	return false
}

// CloudPipelineRunOptions start a run.
type CloudPipelineRunOptions struct {
	RepoURL  string            // required: an https git URL
	Ref      string            // branch, tag or commit (default the repository's default branch)
	Event    string            // push | pull_request | workflow_dispatch | schedule
	Workflow string            // one workflow file (act mode)
	Job      string            // one job
	Inputs   map[string]string // workflow_dispatch inputs
	Size     string            // job sandbox size, such as 4x8
	// Secrets are kosa8 secret names marked Pipelines; each job gets it as
	// an environment variable, upper-cased with - as _.
	Secrets      []string
	TokenSecret  string // a secret holding a token to clone a private repository
	StatusSecret string // a secret holding a token to post commit statuses
	TimeoutS     int
}

// CloudPipelineLog is one job's log text since the cursor asked for.
type CloudPipelineLog struct {
	Job       string `json:"job"`
	State     string `json:"state"`
	Text      string `json:"text"`
	Next      int    `json:"next"` // pass as After to read only newer text
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// CloudPipelineLogs are a run's logs by job, secrets replaced with ***.
type CloudPipelineLogs struct {
	RunID string             `json:"run_id"`
	State string             `json:"state"`
	Done  bool               `json:"done"`
	Logs  []CloudPipelineLog `json:"logs"`
}

// CloudPipelineArtifact is a file a job saved.
type CloudPipelineArtifact struct {
	Job   string `json:"job"`
	Idx   int    `json:"idx"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	URL   string `json:"url"`
}

// CloudPipelineHook is a webhook (and optional cron schedule) that starts
// runs.
type CloudPipelineHook struct {
	ID             string   `json:"id"`
	Name           *string  `json:"name"`
	Provider       string   `json:"provider"` // github | gitlab
	RepoURL        string   `json:"repo_url"`
	URL            string   `json:"url"`    // give this to the git host
	Secret         string   `json:"secret"` // the webhook secret, only in the create's answer
	Size           *string  `json:"size"`
	Secrets        []string `json:"secrets"`
	TokenSecret    *string  `json:"token_secret"`
	StatusSecret   *string  `json:"status_secret"`
	Workflow       *string  `json:"workflow"`
	TimeoutS       *int     `json:"timeout_s"`
	Cron           *string  `json:"cron"`
	CronRef        *string  `json:"cron_ref"`
	NextRunAt      *string  `json:"next_run_at"`
	Deliveries     int      `json:"deliveries"`
	LastDeliveryAt *string  `json:"last_delivery_at"`
	CreatedAt      string   `json:"created_at"`
}

// CloudPipelineHookOptions make a hook.
type CloudPipelineHookOptions struct {
	RepoURL      string // required
	Provider     string // github (default) | gitlab
	Name         string
	Secret       string // the webhook secret; default a random one, returned once
	Size         string
	Secrets      []string
	TokenSecret  string
	StatusSecret string
	Workflow     string
	TimeoutS     int
	Cron         string // also run on this UTC cron schedule
	CronRef      string // the ref a scheduled run builds
}

// CloudPipelines runs CI on kosa8 Cloud.
type CloudPipelines struct{ c *Cloud }

func runPath(id, suffix string) string {
	return "/v1/pipelines/runs/" + url.PathEscape(id) + suffix
}

func setStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

type runEnvelope struct {
	Run CloudPipelineRun `json:"run"`
}

// Run starts a pipeline run.
func (p *CloudPipelines) Run(ctx context.Context, o CloudPipelineRunOptions) (CloudPipelineRun, error) {
	if o.RepoURL == "" {
		return CloudPipelineRun{}, usage("a pipeline run needs RepoURL")
	}
	body := map[string]any{"repo_url": o.RepoURL}
	setStr(body, "ref", o.Ref)
	setStr(body, "event", o.Event)
	setStr(body, "workflow", o.Workflow)
	setStr(body, "job", o.Job)
	setStr(body, "size", o.Size)
	setStr(body, "token_secret", o.TokenSecret)
	setStr(body, "status_secret", o.StatusSecret)
	if len(o.Inputs) > 0 {
		body["inputs"] = o.Inputs
	}
	if len(o.Secrets) > 0 {
		body["secrets"] = o.Secrets
	}
	if o.TimeoutS != 0 {
		body["timeout_s"] = o.TimeoutS
	}
	var r runEnvelope
	err := p.c.request(ctx, "POST", "/v1/pipelines/runs", body, &r)
	return r.Run, err
}

// Runs lists runs, newest first; state "" for all, limit 0 the server's
// default.
func (p *CloudPipelines) Runs(ctx context.Context, state string, limit int) ([]CloudPipelineRun, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/pipelines/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var r struct {
		Runs []CloudPipelineRun `json:"runs"`
	}
	if err := p.c.request(ctx, "GET", path, nil, &r); err != nil {
		return nil, err
	}
	if r.Runs == nil {
		r.Runs = []CloudPipelineRun{}
	}
	return r.Runs, nil
}

// Get returns one run with its jobs.
func (p *CloudPipelines) Get(ctx context.Context, id string) (CloudPipelineRun, error) {
	var r runEnvelope
	err := p.c.request(ctx, "GET", runPath(id, ""), nil, &r)
	return r.Run, err
}

// Wait polls a run every interval (0 means 5 s) until it finishes or ctx
// is done.
func (p *CloudPipelines) Wait(ctx context.Context, id string, interval time.Duration) (CloudPipelineRun, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		r, err := p.Get(ctx, id)
		if err != nil || r.Done() {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Logs returns a run's logs: job "" for every job, after < 0 for all text
// (else only text after a previous answer's Next).
func (p *CloudPipelines) Logs(ctx context.Context, id, job string, after int) (CloudPipelineLogs, error) {
	q := url.Values{}
	if job != "" {
		q.Set("job", job)
	}
	if after >= 0 {
		q.Set("after", strconv.Itoa(after))
	}
	path := runPath(id, "/logs")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var r CloudPipelineLogs
	err := p.c.request(ctx, "GET", path, nil, &r)
	return r, err
}

// Cancel cancels a run.
func (p *CloudPipelines) Cancel(ctx context.Context, id string) (CloudPipelineRun, error) {
	var r runEnvelope
	err := p.c.request(ctx, "POST", runPath(id, "/cancel"), map[string]any{}, &r)
	return r.Run, err
}

// Artifacts lists the files a run's jobs saved.
func (p *CloudPipelines) Artifacts(ctx context.Context, id string) ([]CloudPipelineArtifact, error) {
	var r struct {
		Artifacts []CloudPipelineArtifact `json:"artifacts"`
	}
	if err := p.c.request(ctx, "GET", runPath(id, "/artifacts"), nil, &r); err != nil {
		return nil, err
	}
	if r.Artifacts == nil {
		r.Artifacts = []CloudPipelineArtifact{}
	}
	return r.Artifacts, nil
}

// DownloadArtifact streams one artifact (a .tar.gz) into w.
func (p *CloudPipelines) DownloadArtifact(ctx context.Context, id, job string, idx int, w io.Writer) (int64, error) {
	resp, err := p.c.stream(ctx, "GET", runPath(id, "/artifacts/"+url.PathEscape(job)+"/"+strconv.Itoa(idx)), "", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// CreateHook makes a webhook URL for a repository's pushes and pull
// requests, and optionally a cron schedule. Hook.Secret is only in this
// answer: give it, and Hook.URL, to the git host.
func (p *CloudPipelines) CreateHook(ctx context.Context, o CloudPipelineHookOptions) (CloudPipelineHook, error) {
	if o.RepoURL == "" {
		return CloudPipelineHook{}, usage("a pipeline hook needs RepoURL")
	}
	body := map[string]any{"repo_url": o.RepoURL}
	setStr(body, "provider", o.Provider)
	setStr(body, "name", o.Name)
	setStr(body, "secret", o.Secret)
	setStr(body, "size", o.Size)
	setStr(body, "token_secret", o.TokenSecret)
	setStr(body, "status_secret", o.StatusSecret)
	setStr(body, "workflow", o.Workflow)
	setStr(body, "cron", o.Cron)
	setStr(body, "cron_ref", o.CronRef)
	if len(o.Secrets) > 0 {
		body["secrets"] = o.Secrets
	}
	if o.TimeoutS != 0 {
		body["timeout_s"] = o.TimeoutS
	}
	var r struct {
		Hook CloudPipelineHook `json:"hook"`
	}
	err := p.c.request(ctx, "POST", "/v1/pipelines/hooks", body, &r)
	return r.Hook, err
}

// Hooks lists the org's hooks (without their secrets).
func (p *CloudPipelines) Hooks(ctx context.Context) ([]CloudPipelineHook, error) {
	var r struct {
		Hooks []CloudPipelineHook `json:"hooks"`
	}
	if err := p.c.request(ctx, "GET", "/v1/pipelines/hooks", nil, &r); err != nil {
		return nil, err
	}
	if r.Hooks == nil {
		r.Hooks = []CloudPipelineHook{}
	}
	return r.Hooks, nil
}

// DeleteHook deletes a hook.
func (p *CloudPipelines) DeleteHook(ctx context.Context, id string) error {
	return p.c.request(ctx, "DELETE", "/v1/pipelines/hooks/"+url.PathEscape(id), nil, nil)
}
