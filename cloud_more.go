package kosa8

// More of the kosa8 Cloud API: browsers, the AI gateway, hosted jobs,
// benchmarks, recordings, embeds, custom preview domains, the org's egress
// default, Burst Passes, API keys, webhooks, audit, limits and pricing,
// checkpoints and lineage, RL alerts and comparisons. Answers the SDK does
// not model field by field are Info (the API's JSON as sent).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var esc = url.PathEscape

// query encodes q as "?a=b", or "" when q is empty.
func query(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// ---- browsers ----

// BrowserPricePerHourUSD is a cloud browser's price per browser-hour, billed
// per second (CloudBrowserList.PricePerHourUSD is the live figure).
const BrowserPricePerHourUSD = 0.12

// CloudBrowserInfo is a headless browser as the API reports it.
type CloudBrowserInfo struct {
	ID          string  `json:"id"` // brw_...
	Name        *string `json:"name"`
	SandboxID   *string `json:"sandbox_id"`
	State       string  `json:"state"` // open | closed
	TTLS        int     `json:"ttl_s"`
	CreatedAt   any     `json:"created_at"`
	ExpiresAt   any     `json:"expires_at"`
	ClosedAt    any     `json:"closed_at"`
	CloseReason *string `json:"close_reason"`
	Seconds     float64 `json:"seconds"` // seconds open: what is billed
	// CDPURL is wss://.../v1/browsers/{id}/cdp?token=..., valid 10 minutes;
	// only on create and get while open. It is a credential.
	CDPURL          string `json:"cdp_url,omitempty"`
	CDPURLExpiresAt any    `json:"cdp_url_expires_at,omitempty"`
}

// CloudBrowserOptions configures CloudBrowsers.Create.
type CloudBrowserOptions struct {
	TTLS      int    // 30..3600 s, default 300: it closes on its own then
	SandboxID string // tie it to a sandbox
	Name      string
}

// CloudBrowserPageOptions configures CloudBrowser.Page.
type CloudBrowserPageOptions struct {
	Screenshot bool
	Text       bool
	FullPage   bool
}

// CloudBrowserPage is what loading a URL answered.
type CloudBrowserPage struct {
	URL                 string `json:"url"`
	Title               string `json:"title"`
	Text                string `json:"text,omitempty"`
	ScreenshotPNGBase64 string `json:"screenshot_png_base64,omitempty"`
}

// CloudBrowserList is GET /v1/browsers as sent.
type CloudBrowserList struct {
	Browsers        []CloudBrowserInfo `json:"browsers"`
	Enabled         bool               `json:"enabled"`
	Limit           int                `json:"limit"`
	PricePerHourUSD float64            `json:"price_per_hour_usd"`
}

// CloudBrowsers are the org's headless Chromium browsers, driven over CDP
// (Playwright's connectOverCDP, chromedp, Puppeteer). $0.12 per
// browser-hour, billed per second.
type CloudBrowsers struct{ c *Cloud }

// CloudBrowser is one browser. Info is its state as of the last call.
type CloudBrowser struct {
	c    *Cloud
	Info CloudBrowserInfo
}

// ID is the browser's id (brw_...).
func (b *CloudBrowser) ID() string { return b.Info.ID }

// CDPURL is the wss:// URL a CDP client opens while the browser is open
// ("" from a list, or once closed; Refresh gets a fresh one).
func (b *CloudBrowser) CDPURL() string { return b.Info.CDPURL }

// String never includes the CDP URL (it carries a token).
func (b *CloudBrowser) String() string {
	return fmt.Sprintf("kosa8.CloudBrowser(%s, %s)", b.Info.ID, b.Info.State)
}

// Create opens a browser.
func (s *CloudBrowsers) Create(ctx context.Context, o CloudBrowserOptions) (*CloudBrowser, error) {
	body := map[string]any{}
	if o.TTLS != 0 {
		if o.TTLS < 30 || o.TTLS > 3600 {
			return nil, usage("TTLS must be 30..3600, not %d", o.TTLS)
		}
		body["ttl_s"] = o.TTLS
	}
	if o.SandboxID != "" {
		body["sandbox_id"] = o.SandboxID
	}
	if o.Name != "" {
		body["name"] = o.Name
	}
	var r struct {
		Browser CloudBrowserInfo `json:"browser"`
	}
	if err := s.c.request(ctx, "POST", "/v1/browsers", body, &r); err != nil {
		return nil, err
	}
	return &CloudBrowser{c: s.c, Info: r.Browser}, nil
}

// List is the list answer as sent; all includes closed browsers (the last
// 100). Items carry no CDP URL.
func (s *CloudBrowsers) List(ctx context.Context, all bool) (CloudBrowserList, error) {
	p := "/v1/browsers"
	if all {
		p += "?state=all"
	}
	var r CloudBrowserList
	return r, s.c.request(ctx, "GET", p, nil, &r)
}

// Get reads a browser, with a fresh CDP URL while it is open.
func (s *CloudBrowsers) Get(ctx context.Context, id string) (*CloudBrowser, error) {
	var r struct {
		Browser CloudBrowserInfo `json:"browser"`
	}
	if err := s.c.request(ctx, "GET", fmt.Sprintf("/v1/browsers/%s", esc(id)), nil, &r); err != nil {
		return nil, err
	}
	return &CloudBrowser{c: s.c, Info: r.Browser}, nil
}

// Delete closes a browser by id; billing stops.
func (s *CloudBrowsers) Delete(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", fmt.Sprintf("/v1/browsers/%s", esc(id)), nil, nil)
}

// CDPEndpoint is the browser's CDP WebSocket, GET /v1/browsers/{id}/cdp,
// with the token it needs: the CDP URL as the API answered it (refreshed
// when expired or missing). Open it with any CDP client.
func (b *CloudBrowser) CDPEndpoint(ctx context.Context) (string, error) {
	if b.Info.CDPURL == "" {
		if err := b.Refresh(ctx); err != nil {
			return "", err
		}
	}
	if b.Info.CDPURL == "" {
		return "", usage("browser %s is %s: it has no CDP endpoint", b.Info.ID, b.Info.State)
	}
	if !strings.Contains(b.Info.CDPURL, fmt.Sprintf("/v1/browsers/%s/cdp", esc(b.Info.ID))) {
		return "", &Error{Kind: KindAPI, Message: "the API sent an unexpected cdp_url"}
	}
	return b.Info.CDPURL, nil
}

// Refresh reads the browser again: its state and, while open, a fresh CDP URL.
func (b *CloudBrowser) Refresh(ctx context.Context) error {
	n, err := (&CloudBrowsers{c: b.c}).Get(ctx, b.Info.ID)
	if err != nil {
		return err
	}
	b.Info = n.Info
	return nil
}

// Page loads url in the browser and answers its title and, if asked, its
// text and a PNG screenshot (base64).
func (b *CloudBrowser) Page(ctx context.Context, pageURL string, o *CloudBrowserPageOptions) (CloudBrowserPage, error) {
	if pageURL == "" {
		return CloudBrowserPage{}, usage("Page needs a url")
	}
	body := map[string]any{"url": pageURL}
	if o != nil {
		body["screenshot"] = o.Screenshot
		body["text"] = o.Text
		if o.FullPage {
			body["full_page"] = true
		}
	}
	var r struct {
		Page CloudBrowserPage `json:"page"`
	}
	err := b.c.request(ctx, "POST", fmt.Sprintf("/v1/browsers/%s/page", esc(b.Info.ID)), body, &r)
	return r.Page, err
}

// Screenshot loads url and returns a PNG of it (the viewport, or the whole
// page with fullPage).
func (b *CloudBrowser) Screenshot(ctx context.Context, pageURL string, fullPage bool) ([]byte, error) {
	p, err := b.Page(ctx, pageURL, &CloudBrowserPageOptions{Screenshot: true, FullPage: fullPage})
	if err != nil {
		return nil, err
	}
	if p.ScreenshotPNGBase64 == "" {
		return nil, &Error{Kind: KindAPI, Message: "the API sent no screenshot"}
	}
	return base64.StdEncoding.DecodeString(p.ScreenshotPNGBase64)
}

// Close closes the browser; billing stops.
func (b *CloudBrowser) Close(ctx context.Context) error {
	if err := (&CloudBrowsers{c: b.c}).Delete(ctx, b.Info.ID); err != nil {
		return err
	}
	b.Info.State, b.Info.CDPURL = "closed", ""
	return nil
}

// ---- AI gateway ----

// CloudAISettingsUpdate changes the AI gateway's settings; nil fields are
// left as they are.
type CloudAISettingsUpdate struct {
	Enabled      *bool
	AllSandboxes *bool // every sandbox uses the gateway, not only those created with it
	CacheTTLS    *int
	RatePerMin   *int
}

// CloudAIKey is a stored provider key: a hint, never the key.
type CloudAIKey struct {
	Provider  string `json:"provider"`
	Hint      string `json:"hint"`
	CreatedAt any    `json:"created_at"`
	UpdatedAt any    `json:"updated_at"`
}

// CloudAI is the org's AI gateway: provider keys kept server-side, caching,
// a rate limit and usage. A sandbox created with the gateway gets
// OPENAI_BASE_URL, ANTHROPIC_BASE_URL, KOSA8_AI_GATEWAY_URL and
// KOSA8_AI_TOKEN; outside a sandbox an API key works the same way.
type CloudAI struct{ c *Cloud }

// Settings are the gateway's settings, base URL and configured providers.
func (a *CloudAI) Settings(ctx context.Context) (Info, error) {
	var out Info
	return out, a.c.request(ctx, "GET", "/v1/ai/settings", nil, &out)
}

// UpdateSettings changes the given settings only.
func (a *CloudAI) UpdateSettings(ctx context.Context, u CloudAISettingsUpdate) (Info, error) {
	body := map[string]any{}
	if u.Enabled != nil {
		body["enabled"] = *u.Enabled
	}
	if u.AllSandboxes != nil {
		body["all_sandboxes"] = *u.AllSandboxes
	}
	if u.CacheTTLS != nil {
		body["cache_ttl_s"] = *u.CacheTTLS
	}
	if u.RatePerMin != nil {
		body["rate_per_min"] = *u.RatePerMin
	}
	if len(body) == 0 {
		return nil, usage("UpdateSettings needs at least one setting to change")
	}
	var out Info
	return out, a.c.request(ctx, "PATCH", "/v1/ai/settings", body, &out)
}

// Keys lists the stored provider keys (hints only).
func (a *CloudAI) Keys(ctx context.Context) ([]CloudAIKey, error) {
	var r struct {
		Keys []CloudAIKey `json:"keys"`
	}
	return r.Keys, a.c.request(ctx, "GET", "/v1/ai/keys", nil, &r)
}

// SetKey stores a provider's API key (openai, anthropic, ...). It is sealed,
// never sent back and never put in a sandbox.
func (a *CloudAI) SetKey(ctx context.Context, provider, key string) (Info, error) {
	if provider == "" || key == "" {
		return nil, usage("SetKey needs a provider and a key")
	}
	var out Info
	return out, a.c.request(ctx, "PUT", fmt.Sprintf("/v1/ai/keys/%s", esc(provider)), map[string]any{"key": key}, &out)
}

// DeleteKey removes a provider key.
func (a *CloudAI) DeleteKey(ctx context.Context, provider string) error {
	return a.c.request(ctx, "DELETE", fmt.Sprintf("/v1/ai/keys/%s", esc(provider)), nil, nil)
}

var monthRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// Usage is a month's (YYYY-MM; "" this one) model spend: totals, by
// provider and model, by sandbox, and recent calls.
func (a *CloudAI) Usage(ctx context.Context, month string) (Info, error) {
	p := "/v1/ai/usage"
	if month != "" {
		if !monthRE.MatchString(month) {
			return nil, usage("month must be YYYY-MM, not %q", month)
		}
		p += "?month=" + month
	}
	var out Info
	return out, a.c.request(ctx, "GET", p, nil, &out)
}

// GatewayURL is the gateway's base URL for a provider,
// {BaseURL}/v1/ai/gw/{provider}/: point an OpenAI or Anthropic client at it
// (base URL) with a kosa8 API key (or a sandbox's KOSA8_AI_TOKEN) as its
// API key. Some clients want the provider's version prefix after it
// (".../v1/ai/gw/openai/v1").
func (a *CloudAI) GatewayURL(provider string) string {
	return a.c.baseURL + fmt.Sprintf("/v1/ai/gw/%s/", esc(provider))
}

// Call sends one model call through the gateway in the provider's own
// request shape, e.g. Call(ctx, "openai", "v1/chat/completions", body) or
// Call(ctx, "anthropic", "v1/messages", body), and decodes the answer into
// out. It is not retried unless the cloud refused it before sending it on
// (429, 503 database_busy).
func (a *CloudAI) Call(ctx context.Context, provider, path string, body, out any) error {
	path = strings.TrimLeft(path, "/")
	if provider == "" || path == "" {
		return usage("Call needs a provider and a provider API path")
	}
	return a.c.request(ctx, "POST", fmt.Sprintf("/v1/ai/gw/%s/%s", esc(provider), path), body, out)
}

// ---- hosted jobs ----

// JobDoneStates are the job statuses after which nothing more runs.
var JobDoneStates = []string{"done", "failed", "cancelled", "budget_reached"}

// CloudJobCmd is a solver or grader command.
type CloudJobCmd struct {
	Cmd      []string // run as given; one element runs under sh -c
	TimeoutS int      // seconds; a solver past it is recorded as timeout
	Env      map[string]string
	// Files (grader only) are hidden files, written only into the sealed
	// copy (no network) the grader runs in; at most 1 MiB.
	Files map[string][]byte
}

// CloudJobTask is one task: its id (unique in the job; the rollout's
// group), its input and its files.
type CloudJobTask struct {
	ID    string
	Input any               // any JSON, at most 32 KiB: $KOSA8_TASK_INPUT
	Files map[string][]byte // written into the task's sandbox before the solver runs
}

// CloudJobOptions is a hosted job: tasks, a solver and a sealed grader.
type CloudJobOptions struct {
	Name  string
	Tasks []CloudJobTask // 1..10000; ids unique
	// TasksJSONL is the tasks as JSONL (files already base64), instead of Tasks.
	TasksJSONL string
	Solver     CloudJobCmd
	Grader     CloudJobCmd
	// Exactly one of Template, Image, Snapshot (or Benchmark).
	Template  string
	Image     string
	Snapshot  string
	Benchmark string
	Size      string
	Project   string
	// Concurrency is tasks in flight at once (default 16).
	Concurrency int
	// RetriesOnInfra: infra failures retried this many times, then recorded masked.
	RetriesOnInfra *int
	LeaseS         int     // each task sandbox's lease, 120..86400 s
	BudgetUSD      float64 // stop with budget_reached once the list cost reaches this
	// IdempotencyKey (default a new UUID) makes a retried submit start one job.
	IdempotencyKey string
}

// CloudJob is a job as kosa8 Cloud answers it (Info carries every field).
type CloudJob struct {
	ID     string `json:"id"` // job_...
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Info   Info   `json:"-"`
}

// Done reports whether the job has finished.
func (j CloudJob) Done() bool {
	for _, s := range JobDoneStates {
		if j.Status == s {
			return true
		}
	}
	return false
}

func jobOf(m Info) CloudJob {
	j := Info{}
	if inner, ok := m["job"].(map[string]any); ok {
		for k, v := range inner {
			j[k] = v
		}
	}
	if id, ok := m["job_id"].(string); ok && j["id"] == nil {
		j["id"] = id
	}
	if id, ok := m["run_id"].(string); ok && j["run_id"] == nil {
		j["run_id"] = id
	}
	out := CloudJob{Info: j}
	out.ID, _ = j["id"].(string)
	out.RunID, _ = j["run_id"].(string)
	out.Status, _ = j["status"].(string)
	return out
}

func encodeFiles(files map[string][]byte) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for p, b := range files {
		if !strings.HasPrefix(p, "/") {
			return nil, usage("file paths are absolute (in the sandbox), not %q", p)
		}
		out[p] = base64.StdEncoding.EncodeToString(b)
	}
	return out, nil
}

func jobCmd(c CloudJobCmd, what string, files bool) (map[string]any, error) {
	if len(c.Cmd) == 0 || c.Cmd[0] == "" {
		return nil, usage("%s needs Cmd", what)
	}
	out := map[string]any{}
	if len(c.Cmd) == 1 {
		out["cmd"] = c.Cmd[0]
	} else {
		out["cmd"] = c.Cmd
	}
	if c.TimeoutS != 0 {
		out["timeout_s"] = c.TimeoutS
	}
	if len(c.Env) > 0 {
		out["env"] = c.Env
	}
	if len(c.Files) > 0 {
		if !files {
			return nil, usage("%s takes no Files (put them in the task)", what)
		}
		f, err := encodeFiles(c.Files)
		if err != nil {
			return nil, err
		}
		out["files"] = f
	}
	return out, nil
}

func jobBody(o CloudJobOptions) (map[string]any, error) {
	if o.Name == "" {
		return nil, usage("a job needs a Name")
	}
	n := 0
	for _, s := range []string{o.Template, o.Image, o.Snapshot, o.Benchmark} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return nil, usage("give exactly one of Template, Image, Snapshot or Benchmark")
	}
	if o.Solver.TimeoutS == 0 {
		return nil, usage("Solver needs TimeoutS (seconds; a solver past it is recorded as timeout)")
	}
	solver, err := jobCmd(o.Solver, "Solver", false)
	if err != nil {
		return nil, err
	}
	grader, err := jobCmd(o.Grader, "Grader", true)
	if err != nil && o.Benchmark == "" {
		return nil, err
	}
	var tasks []map[string]any
	switch {
	case o.TasksJSONL != "" && len(o.Tasks) > 0:
		return nil, usage("give Tasks or TasksJSONL, not both")
	case o.TasksJSONL != "":
		for i, line := range strings.Split(o.TasksJSONL, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var t map[string]any
			if err := json.Unmarshal([]byte(line), &t); err != nil {
				return nil, usage("tasks line %d: not JSON: %v", i+1, err)
			}
			tasks = append(tasks, t)
		}
	default:
		seen := map[string]bool{}
		for i, t := range o.Tasks {
			if t.ID == "" {
				return nil, usage("Tasks[%d] needs an ID", i)
			}
			if seen[t.ID] {
				return nil, usage("task id %q appears twice", t.ID)
			}
			seen[t.ID] = true
			row := map[string]any{"id": t.ID}
			if t.Input != nil {
				row["input"] = t.Input
			}
			if len(t.Files) > 0 {
				f, err := encodeFiles(t.Files)
				if err != nil {
					return nil, err
				}
				row["files"] = f
			}
			tasks = append(tasks, row)
		}
	}
	if o.Benchmark == "" && (len(tasks) < 1 || len(tasks) > 10000) {
		return nil, usage("a job has 1..10000 tasks, not %d", len(tasks))
	}
	body := map[string]any{"name": o.Name, "solver": solver}
	if tasks != nil {
		body["tasks"] = tasks
	}
	if grader != nil {
		body["grader"] = grader
	}
	for k, v := range map[string]string{"template": o.Template, "image": o.Image, "snapshot": o.Snapshot,
		"benchmark": o.Benchmark, "size": o.Size, "project": o.Project} {
		if v != "" {
			body[k] = v
		}
	}
	if o.Concurrency != 0 {
		body["concurrency"] = o.Concurrency
	}
	if o.RetriesOnInfra != nil {
		body["retries_on_infra"] = *o.RetriesOnInfra
	}
	if o.LeaseS != 0 {
		body["lease_s"] = o.LeaseS
	}
	if o.BudgetUSD != 0 {
		body["budget_usd"] = o.BudgetUSD
	}
	return body, nil
}

// CloudJobs are hosted jobs: kosa8 runs every task's rollout itself (each
// in its own sandbox, graded sealed) and records them as an RL run.
type CloudJobs struct{ c *Cloud }

// Submit starts a job. It carries an Idempotency-Key, so a retry after a
// dropped connection starts the job once.
func (s *CloudJobs) Submit(ctx context.Context, o CloudJobOptions) (CloudJob, error) {
	body, err := jobBody(o)
	if err != nil {
		return CloudJob{}, err
	}
	var r Info
	if err := s.c.requestKeyed(ctx, "POST", "/v1/jobs", body, &r, o.IdempotencyKey); err != nil {
		return CloudJob{}, err
	}
	return jobOf(r), nil
}

// CloudJobEstimateOptions are Submit's options plus a pilot sample.
type CloudJobEstimateOptions struct {
	CloudJobOptions
	Sample      int    // start a pilot job of the first n tasks (1..50)
	SampleJobID string // estimate from that finished pilot job's durations
}

// Estimate is what a job would use: sandbox_hours and usd_list as
// {low, high} and included_hours_left.
func (s *CloudJobs) Estimate(ctx context.Context, o CloudJobEstimateOptions) (Info, error) {
	if o.Sample != 0 && o.SampleJobID != "" {
		return nil, usage("give Sample or SampleJobID, not both")
	}
	body, err := jobBody(o.CloudJobOptions)
	if err != nil {
		return nil, err
	}
	if o.Sample != 0 {
		body["sample"] = o.Sample
	}
	if o.SampleJobID != "" {
		body["sample_job_id"] = o.SampleJobID
	}
	var out Info
	return out, s.c.request(ctx, "POST", "/v1/jobs/estimate", body, &out)
}

// Get reads a job: status, counts, by_failure_class, progress, cost_usd, eta_s.
func (s *CloudJobs) Get(ctx context.Context, id string) (CloudJob, error) {
	var r Info
	if err := s.c.request(ctx, "GET", fmt.Sprintf("/v1/jobs/%s", esc(id)), nil, &r); err != nil {
		return CloudJob{}, err
	}
	return jobOf(r), nil
}

// CloudJobListOptions filters CloudJobs.List.
type CloudJobListOptions struct {
	Status, Project, Cursor string
	Limit                   int
}

// List is the org's jobs, newest first.
func (s *CloudJobs) List(ctx context.Context, o CloudJobListOptions) ([]CloudJob, error) {
	q := url.Values{}
	if o.Status != "" {
		q.Set("status", o.Status)
	}
	if o.Project != "" {
		q.Set("project", o.Project)
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit != 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	var r struct {
		Jobs []Info `json:"jobs"`
	}
	if err := s.c.request(ctx, "GET", "/v1/jobs"+query(q), nil, &r); err != nil {
		return nil, err
	}
	out := make([]CloudJob, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		out = append(out, jobOf(Info{"job": map[string]any(j)}))
	}
	return out, nil
}

// Cancel stops starting rollouts and drains the job's sandboxes.
func (s *CloudJobs) Cancel(ctx context.Context, id string) (CloudJob, error) {
	var r Info
	if err := s.c.request(ctx, "POST", fmt.Sprintf("/v1/jobs/%s/cancel", esc(id)), map[string]any{}, &r); err != nil {
		return CloudJob{}, err
	}
	return jobOf(r), nil
}

// Wait polls every interval (0 means 5 s) until the job finishes or ctx
// ends; progress (may be nil) sees every poll.
func (s *CloudJobs) Wait(ctx context.Context, id string, interval time.Duration, progress func(CloudJob)) (CloudJob, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		j, err := s.Get(ctx, id)
		if err != nil {
			return j, err
		}
		if progress != nil {
			progress(j)
		}
		if j.Done() {
			return j, nil
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return j, err
		}
	}
}

// ---- benchmarks ----

// CloudBenchmarks are the ready-made benchmarks (SWE-bench and the like):
// instances with their environments, built ahead with Prepare.
type CloudBenchmarks struct{ c *Cloud }

// List is the benchmarks, with their source and revision.
func (s *CloudBenchmarks) List(ctx context.Context) ([]Info, error) {
	var r struct {
		Benchmarks []Info `json:"benchmarks"`
	}
	return r.Benchmarks, s.c.request(ctx, "GET", "/v1/benchmarks", nil, &r)
}

// Get is one benchmark.
func (s *CloudBenchmarks) Get(ctx context.Context, name string) (Info, error) {
	var r struct {
		Benchmark Info `json:"benchmark"`
	}
	return r.Benchmark, s.c.request(ctx, "GET", fmt.Sprintf("/v1/benchmarks/%s", esc(name)), nil, &r)
}

// CloudBenchmarkInstanceQuery filters CloudBenchmarks.Instances.
type CloudBenchmarkInstanceQuery struct {
	State, Cursor string
	Limit         int
}

// CloudBenchmarkInstances is a page of instances.
type CloudBenchmarkInstances struct {
	Instances  []Info  `json:"instances"`
	NextCursor *string `json:"next_cursor"`
}

// Instances is a page of a benchmark's instances, in the dataset's order,
// with their environments' state.
func (s *CloudBenchmarks) Instances(ctx context.Context, name string, q CloudBenchmarkInstanceQuery) (CloudBenchmarkInstances, error) {
	v := url.Values{}
	if q.State != "" {
		v.Set("state", q.State)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var r CloudBenchmarkInstances
	return r, s.c.request(ctx, "GET", fmt.Sprintf("/v1/benchmarks/%s/instances", esc(name))+query(v), nil, &r)
}

// Instance is one instance: the solver's input and its environment's state
// and ETA (never the hidden tests).
func (s *CloudBenchmarks) Instance(ctx context.Context, name, id string) (Info, error) {
	var r struct {
		Instance Info `json:"instance"`
	}
	return r.Instance, s.c.request(ctx, "GET", fmt.Sprintf("/v1/benchmarks/%s/instances/%s", esc(name), esc(id)), nil, &r)
}

// Prepare builds instances' environments ahead of use (at most 100 a call);
// seed picks a sample when instances is empty (0: none sent).
func (s *CloudBenchmarks) Prepare(ctx context.Context, name string, instances []string, seed int) (Info, error) {
	body := map[string]any{}
	if len(instances) > 0 {
		if len(instances) > 100 {
			return nil, usage("Prepare takes at most 100 instances a call, not %d", len(instances))
		}
		body["instances"] = instances
	}
	if seed != 0 {
		body["seed"] = seed
	}
	var out Info
	return out, s.c.request(ctx, "POST", fmt.Sprintf("/v1/benchmarks/%s/prepare", esc(name)), body, &out)
}

// ---- recordings ----

// CloudRecording is a desktop session recording (MP4), made when a sandbox
// created with recording on is deleted.
type CloudRecording struct {
	ID          string  `json:"id"` // rec_...
	SandboxID   string  `json:"sandbox_id"`
	State       string  `json:"state"` // saving | ready | failed
	Bytes       *int64  `json:"bytes"`
	Error       *string `json:"error"`
	ContentType string  `json:"content_type,omitempty"`
	CreatedAt   int64   `json:"created_at"`
	ExpiresAt   int64   `json:"expires_at"` // deleted then, by the plan's retention
}

// CloudRecordingList is the org's recordings.
type CloudRecordingList struct {
	Recordings    []CloudRecording `json:"recordings"`
	Configured    bool             `json:"configured"`
	RetentionDays *int             `json:"retention_days"`
}

// CloudRecordingLink is a signed download URL for a recording.
type CloudRecordingLink struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
	Bytes     *int64 `json:"bytes"`
}

// CloudRecordings are the org's desktop recordings.
type CloudRecordings struct{ c *Cloud }

// List is the recordings, newest first; sandboxID ("" all) and limit
// (1..200; 0 the default) narrow it.
func (s *CloudRecordings) List(ctx context.Context, sandboxID string, limit int) (CloudRecordingList, error) {
	q := url.Values{}
	if sandboxID != "" {
		q.Set("sandbox_id", sandboxID)
	}
	if limit != 0 {
		if limit < 1 || limit > 200 {
			return CloudRecordingList{}, usage("limit must be 1..200, not %d", limit)
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	var r CloudRecordingList
	return r, s.c.request(ctx, "GET", "/v1/recordings"+query(q), nil, &r)
}

// Link is a signed download URL for the recording's MP4 (ttlS 60..86400; 0
// the default, 900).
func (s *CloudRecordings) Link(ctx context.Context, id string, ttlS int) (CloudRecordingLink, error) {
	body := map[string]any{}
	if ttlS != 0 {
		if ttlS < 60 || ttlS > 86400 {
			return CloudRecordingLink{}, usage("ttlS must be 60..86400, not %d", ttlS)
		}
		body["ttl_s"] = ttlS
	}
	var r struct {
		Link CloudRecordingLink `json:"link"`
	}
	err := s.c.request(ctx, "POST", fmt.Sprintf("/v1/recordings/%s/link", esc(id)), body, &r)
	return r.Link, err
}

// Delete deletes a recording now.
func (s *CloudRecordings) Delete(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", fmt.Sprintf("/v1/recordings/%s", esc(id)), nil, nil)
}

// Recordings lists the sandbox's recordings.
func (b *CloudSandbox) Recordings(ctx context.Context) ([]CloudRecording, error) {
	var r struct {
		Recordings []CloudRecording `json:"recordings"`
	}
	return r.Recordings, b.c.request(ctx, "GET", fmt.Sprintf("/v1/sandboxes/%s/recordings", b.eid()), nil, &r)
}

// ---- embeds ----

// CloudEmbed is a signed, expiring iframe URL for a sandbox's terminal or desktop.
type CloudEmbed struct {
	ID        string   `json:"id"` // emb_...
	SandboxID string   `json:"sandbox_id"`
	Mode      string   `json:"mode"` // view | interact
	UI        string   `json:"ui"`   // terminal | desktop
	Origins   []string `json:"origins"`
	Command   []string `json:"command"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt int64    `json:"expires_at"`
	Revoked   bool     `json:"revoked"`
	// URL (create only) is the credential: give it only to pages you trust.
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"` // create only: an <iframe> tag
}

// CloudEmbedOptions configures CloudEmbeds.Create.
type CloudEmbedOptions struct {
	Origins []string // 1..10 origins that may frame it, e.g. https://app.example.com
	Mode    string   // view (default) | interact
	UI      string   // terminal (default) | desktop
	TTLS    int      // 60..604800 s (default 3600)
	Command []string // terminal only: the program to show
}

// CloudEmbeds are a sandbox's embeds.
type CloudEmbeds struct{ b *CloudSandbox }

// Embeds are the sandbox's embeds.
func (b *CloudSandbox) Embeds() *CloudEmbeds { return &CloudEmbeds{b: b} }

// Create makes an embed URL.
func (e *CloudEmbeds) Create(ctx context.Context, o CloudEmbedOptions) (CloudEmbed, error) {
	if len(o.Origins) < 1 || len(o.Origins) > 10 {
		return CloudEmbed{}, usage("an embed takes 1..10 Origins")
	}
	body := map[string]any{"origins": o.Origins}
	if o.Mode != "" {
		body["mode"] = o.Mode
	}
	if o.UI != "" {
		body["ui"] = o.UI
	}
	if o.TTLS != 0 {
		if o.TTLS < 60 || o.TTLS > 604800 {
			return CloudEmbed{}, usage("TTLS must be 60..604800, not %d", o.TTLS)
		}
		body["ttl_s"] = o.TTLS
	}
	if len(o.Command) > 0 {
		body["command"] = o.Command
	}
	var r struct {
		Embed CloudEmbed `json:"embed"`
	}
	err := e.b.c.request(ctx, "POST", fmt.Sprintf("/v1/sandboxes/%s/embed", e.b.eid()), body, &r)
	return r.Embed, err
}

// List is the sandbox's live embeds (no URLs).
func (e *CloudEmbeds) List(ctx context.Context) ([]CloudEmbed, error) {
	var r struct {
		Embeds []CloudEmbed `json:"embeds"`
	}
	return r.Embeds, e.b.c.request(ctx, "GET", fmt.Sprintf("/v1/sandboxes/%s/embeds", e.b.eid()), nil, &r)
}

// Revoke ends an embed's URL before it expires.
func (e *CloudEmbeds) Revoke(ctx context.Context, embedID string) error {
	return e.b.c.request(ctx, "DELETE", fmt.Sprintf("/v1/sandboxes/%s/embeds/%s", e.b.eid(), esc(embedID)), nil, nil)
}

// ---- custom preview domains ----

// CloudDNSRecord is a record to make at your DNS provider.
type CloudDNSRecord struct {
	Type    string `json:"type"` // CNAME | TXT
	Name    string `json:"name"`
	Value   string `json:"value"`
	Purpose string `json:"purpose"` // routing | ownership | certificate
}

// CloudPreviewDomain is a preview served at your own hostname.
type CloudPreviewDomain struct {
	Hostname     string           `json:"hostname"`
	URL          string           `json:"url"`
	SandboxID    string           `json:"sandbox_id"`
	Port         int              `json:"port"`
	PreviewID    string           `json:"preview_id"`
	Status       string           `json:"status"` // pending | active | failed
	SSLStatus    *string          `json:"ssl_status"`
	Records      []CloudDNSRecord `json:"records"`
	Error        *string          `json:"error"`
	Instructions string           `json:"instructions,omitempty"`
	CreatedAt    int64            `json:"created_at"`
	CheckedAt    *int64           `json:"checked_at"`
}

// CloudPreviewDomainList is a preview's custom domains.
type CloudPreviewDomainList struct {
	Domains     []CloudPreviewDomain `json:"domains"`
	Configured  bool                 `json:"configured"`
	CNAMETarget *string              `json:"cname_target"`
}

// CloudPreviewDomains are one preview's custom domains (at most 5 a
// preview, 50 an org). 501 not_available where custom hostnames are off.
type CloudPreviewDomains struct {
	c    *Cloud
	list string                       // the domains' path
	item func(hostname string) string // one domain's path
}

// Domains are the custom domains of the preview on port.
func (p *CloudPreviews) Domains(port int) *CloudPreviewDomains {
	id := p.b.eid()
	return &CloudPreviewDomains{c: p.b.c,
		list: fmt.Sprintf("/v1/sandboxes/%s/previews/%d/domains", id, port),
		item: func(h string) string { return fmt.Sprintf("/v1/sandboxes/%s/previews/%d/domains/%s", id, port, esc(h)) }}
}

// PreviewDomains are the custom domains of a preview by its id
// ({sandbox_id}:{port}, CloudPreview.ID).
func (c *Cloud) PreviewDomains(previewID string) *CloudPreviewDomains {
	id := esc(previewID)
	return &CloudPreviewDomains{c: c,
		list: fmt.Sprintf("/v1/previews/%s/domains", id),
		item: func(h string) string { return fmt.Sprintf("/v1/previews/%s/domains/%s", id, esc(h)) }}
}

// List is the preview's custom domains.
func (d *CloudPreviewDomains) List(ctx context.Context) (CloudPreviewDomainList, error) {
	var r CloudPreviewDomainList
	return r, d.c.request(ctx, "GET", d.list, nil, &r)
}

// Add serves the preview at hostname; make the records it answers at your
// DNS provider. sslMethod is "", "http" or "txt".
func (d *CloudPreviewDomains) Add(ctx context.Context, hostname, sslMethod string) (CloudPreviewDomain, error) {
	body := map[string]any{"hostname": hostname}
	if sslMethod != "" {
		body["ssl_method"] = sslMethod
	}
	var r struct {
		Domain CloudPreviewDomain `json:"domain"`
	}
	err := d.c.request(ctx, "POST", d.list, body, &r)
	return r.Domain, err
}

// Get is a domain's status, asked of the certificate provider again.
func (d *CloudPreviewDomains) Get(ctx context.Context, hostname string) (CloudPreviewDomain, error) {
	var r struct {
		Domain CloudPreviewDomain `json:"domain"`
	}
	err := d.c.request(ctx, "GET", d.item(hostname), nil, &r)
	return r.Domain, err
}

// Remove stops serving the preview at hostname.
func (d *CloudPreviewDomains) Remove(ctx context.Context, hostname string) error {
	return d.c.request(ctx, "DELETE", d.item(hostname), nil, nil)
}

// ---- background exec stop ----

// StopBackground stops a background command and what it started (SIGTERM,
// then SIGKILL after 5 s). It answers the state: stopping, or exited when it
// had already ended; BackgroundExec then reports its exit code.
func (b *CloudSandbox) StopBackground(ctx context.Context, execID string) (string, error) {
	if execID == "" {
		return "", usage("StopBackground needs an exec id (bg_...)")
	}
	var r struct {
		Exec struct {
			State string `json:"state"`
		} `json:"exec"`
	}
	if err := b.c.request(ctx, "POST", fmt.Sprintf("/v1/sandboxes/%s/exec/%s/stop", b.eid(), esc(execID)), map[string]any{}, &r); err != nil {
		return "", err
	}
	if r.Exec.State == "" {
		r.Exec.State = "stopping"
	}
	return r.Exec.State, nil
}

// ---- org egress, Burst Passes ----

// OrgEgress is the org's egress default ({default, enforce}); nil when there
// is none (open internet).
func (c *Cloud) OrgEgress(ctx context.Context) (Info, error) {
	var r struct {
		Egress Info `json:"egress"`
	}
	return r.Egress, c.request(ctx, "GET", "/v1/org/egress", nil, &r)
}

// SetOrgEgress sets the org's egress default (an admin key); nil clears it.
// With enforce it is a ceiling sandboxes cannot widen. It is pushed at once
// to running and sleeping sandboxes.
func (c *Cloud) SetOrgEgress(ctx context.Context, policy *CloudEgress, enforce bool) (Info, error) {
	body := map[string]any{"default": nil}
	if policy != nil {
		body["default"] = egressBody(*policy)
		body["enforce"] = enforce
	}
	var out Info
	return out, c.request(ctx, "PUT", "/v1/org/egress", body, &out)
}

// Burst is the Burst Pass status: the pass (nil when none is sold here),
// the passes held and those running.
func (c *Cloud) Burst(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/billing/burst", nil, &out)
}

// ActivateBurst activates quantity passes (an owner's admin key). Holding
// fewer, the answer is a checkout url for the rest.
func (c *Cloud) ActivateBurst(ctx context.Context, quantity int) (Info, error) {
	if quantity < 1 {
		return nil, usage("quantity must be at least 1")
	}
	var out Info
	return out, c.request(ctx, "POST", "/v1/billing/burst", map[string]any{"quantity": quantity}, &out)
}

// ---- usage, limits, pricing ----

// Pricing is the public rate card.
func (c *Cloud) Pricing(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/pricing", nil, &out)
}

// Limits is the running-sandbox limit: active, max_running, reached, and
// the last request for more.
func (c *Cloud) Limits(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/limits", nil, &out)
}

// RequestLimit asks kosa8 for a higher running-sandbox limit (once a day);
// kosa8 answers the owner by email.
func (c *Cloud) RequestLimit(ctx context.Context, maxRunning int, reason string) (Info, error) {
	if maxRunning < 1 {
		return nil, usage("maxRunning must be at least 1")
	}
	body := map[string]any{"max_running": maxRunning}
	if reason != "" {
		body["reason"] = reason
	}
	var out Info
	return out, c.request(ctx, "POST", "/v1/limits/request", body, &out)
}

// UsageInsights is daily spend, top sandboxes, credit left and projected
// month-end spend.
func (c *Cloud) UsageInsights(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/usage/insights", nil, &out)
}

// ---- checkpoints, lineage, images ----

// Checkpoints are the checkpoints of every running or sleeping sandbox (up
// to 25), asked of each host; none is woken.
func (c *Cloud) Checkpoints(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/checkpoints", nil, &out)
}

// Lineage is the sandboxes with their fork parents (nodes, roots, forked),
// for drawing the fork tree.
func (c *Cloud) Lineage(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/lineage", nil, &out)
}

// CloudImage is an image behind a template.
type CloudImage struct {
	Template string  `json:"template"`
	Image    string  `json:"image"`
	Tag      *string `json:"tag"`
	Digest   *string `json:"digest"`
}

// Images are the images behind the templates this org's hosts offer.
func (c *Cloud) Images(ctx context.Context) ([]CloudImage, error) {
	var r struct {
		Images []CloudImage `json:"images"`
	}
	return r.Images, c.request(ctx, "GET", "/v1/images", nil, &r)
}

// ---- function invocations ----

// Invocations are a function's recent calls, newest first (limit 1..100;
// 0 the default, 20).
func (f *CloudFunctions) Invocations(ctx context.Context, name string, limit int) ([]Info, error) {
	p := fmt.Sprintf("/v1/functions/%s/invocations", esc(name))
	if limit != 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var r struct {
		Invocations []Info `json:"invocations"`
	}
	return r.Invocations, f.c.request(ctx, "GET", p, nil, &r)
}

// ---- API keys ----

// CloudAPIKey is an API key; Secret is only in the answer that made it.
type CloudAPIKey struct {
	ID          string   `json:"id"`
	Name        *string  `json:"name"`
	Prefix      string   `json:"prefix"`
	Scopes      []string `json:"scopes"`
	CreatedAt   any      `json:"created_at"`
	LastUsedAt  any      `json:"last_used_at"`
	RevokedAt   any      `json:"revoked_at"`
	ExpiresAt   any      `json:"expires_at"`
	MemberEmail *string  `json:"member_email"`
	CreatedBy   *string  `json:"created_by"`
	Secret      string   `json:"secret,omitempty"`
}

// GoString keeps %#v from printing the secret.
func (k CloudAPIKey) GoString() string {
	return fmt.Sprintf("kosa8.CloudAPIKey(%s, %s)", k.ID, k.Prefix)
}

// CloudAPIKeys are the org's API keys (a keys:manage key).
type CloudAPIKeys struct{ c *Cloud }

// List is the keys (never secrets).
func (s *CloudAPIKeys) List(ctx context.Context) ([]CloudAPIKey, error) {
	var r struct {
		Keys []CloudAPIKey `json:"api_keys"`
	}
	return r.Keys, s.c.request(ctx, "GET", "/v1/api-keys", nil, &r)
}

// Create makes a key; its Secret is shown only here. scopes nil means the
// server's default; expiresInS 0 means it never expires.
func (s *CloudAPIKeys) Create(ctx context.Context, name string, scopes []string, expiresInS int) (CloudAPIKey, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if scopes != nil {
		body["scopes"] = scopes
	}
	if expiresInS != 0 {
		body["expires_in_s"] = expiresInS
	}
	var r struct {
		Key CloudAPIKey `json:"api_key"`
	}
	err := s.c.request(ctx, "POST", "/v1/api-keys", body, &r)
	return r.Key, err
}

// Revoke revokes a key now.
func (s *CloudAPIKeys) Revoke(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", fmt.Sprintf("/v1/api-keys/%s", esc(id)), nil, nil)
}

// Rotate makes a new key with the same name and scopes; the old one keeps
// working graceS more seconds (0: it stops now).
func (s *CloudAPIKeys) Rotate(ctx context.Context, id string, graceS int) (CloudAPIKey, error) {
	var r struct {
		Key CloudAPIKey `json:"api_key"`
	}
	err := s.c.request(ctx, "POST", fmt.Sprintf("/v1/api-keys/%s/rotate", esc(id)), map[string]any{"grace_s": graceS}, &r)
	return r.Key, err
}

// ---- webhooks ----

// CloudWebhook is a webhook endpoint; Secret (whsec_...) only on create.
type CloudWebhook struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Description *string  `json:"description"`
	Secret      string   `json:"secret,omitempty"`
}

// GoString keeps %#v from printing the secret.
func (w CloudWebhook) GoString() string {
	return fmt.Sprintf("kosa8.CloudWebhook(%s, %s)", w.ID, w.URL)
}

// CloudWebhooks are the org's webhook endpoints.
type CloudWebhooks struct{ c *Cloud }

// List is the endpoints and the events one can subscribe to.
func (s *CloudWebhooks) List(ctx context.Context) ([]CloudWebhook, []string, error) {
	var r struct {
		Webhooks []CloudWebhook `json:"webhooks"`
		Events   []string       `json:"events"`
	}
	err := s.c.request(ctx, "GET", "/v1/webhooks", nil, &r)
	return r.Webhooks, r.Events, err
}

// Create adds an endpoint; its signing secret is shown only here.
func (s *CloudWebhooks) Create(ctx context.Context, endpoint string, events []string, description string) (CloudWebhook, error) {
	if endpoint == "" {
		return CloudWebhook{}, usage("Create needs the endpoint's URL")
	}
	body := map[string]any{"url": endpoint}
	if len(events) > 0 {
		body["events"] = events
	}
	if description != "" {
		body["description"] = description
	}
	var r struct {
		Webhook CloudWebhook `json:"webhook"`
	}
	err := s.c.request(ctx, "POST", "/v1/webhooks", body, &r)
	return r.Webhook, err
}

// Delete stops sending to an endpoint, retries included.
func (s *CloudWebhooks) Delete(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", fmt.Sprintf("/v1/webhooks/%s", esc(id)), nil, nil)
}

// Deliveries are an endpoint's recent deliveries (limit 0: the default).
func (s *CloudWebhooks) Deliveries(ctx context.Context, id string, limit int) ([]Info, error) {
	p := fmt.Sprintf("/v1/webhooks/%s/deliveries", esc(id))
	if limit != 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var r struct {
		Deliveries []Info `json:"deliveries"`
	}
	return r.Deliveries, s.c.request(ctx, "GET", p, nil, &r)
}

// Test sends a webhook.test event now and answers the delivery.
func (s *CloudWebhooks) Test(ctx context.Context, id string) (Info, error) {
	var r struct {
		Delivery Info `json:"delivery"`
	}
	return r.Delivery, s.c.request(ctx, "POST", fmt.Sprintf("/v1/webhooks/%s/test", esc(id)), map[string]any{}, &r)
}

// ---- audit ----

// CloudAuditQuery filters CloudAudit.List.
type CloudAuditQuery struct {
	SandboxID, Actor, Action string
	Since, Until             int64 // Unix ms; 0 none
	BeforeSeq                int64 // page: the previous page's NextBeforeSeq
	Limit                    int
}

// CloudAuditPage is a page of audit events, newest first.
type CloudAuditPage struct {
	Events        []Info `json:"events"`
	NextBeforeSeq *int64 `json:"next_before_seq"`
}

// CloudAudit is the org's tamper-evident audit log (an audit:read key).
type CloudAudit struct{ c *Cloud }

// List is a page of audit events, newest first.
func (a *CloudAudit) List(ctx context.Context, q CloudAuditQuery) (CloudAuditPage, error) {
	v := url.Values{}
	for k, s := range map[string]string{"sandbox_id": q.SandboxID, "actor": q.Actor, "action": q.Action} {
		if s != "" {
			v.Set(k, s)
		}
	}
	for k, n := range map[string]int64{"since": q.Since, "until": q.Until, "before_seq": q.BeforeSeq, "limit": int64(q.Limit)} {
		if n != 0 {
			v.Set(k, strconv.FormatInt(n, 10))
		}
	}
	var r CloudAuditPage
	return r, a.c.request(ctx, "GET", "/v1/audit"+query(v), nil, &r)
}

// Verify recomputes the org's hash chain and says where it first breaks, if anywhere.
func (a *CloudAudit) Verify(ctx context.Context) (Info, error) {
	var out Info
	return out, a.c.request(ctx, "GET", "/v1/audit/verify", nil, &out)
}

// Export is every audit event as NDJSON, oldest first, with hashes.
func (a *CloudAudit) Export(ctx context.Context) ([]byte, error) {
	return a.c.do(ctx, "GET", "/v1/audit/export", "", nil, "application/x-ndjson")
}

// ExportEvents is Export decoded, one Info per line.
func (a *CloudAudit) ExportEvents(ctx context.Context) ([]Info, error) {
	b, err := a.Export(ctx)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m Info
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, &Error{Kind: KindAPI, Message: "decode audit export: " + err.Error(), Err: err}
		}
		out = append(out, m)
	}
	return out, nil
}

// ---- attack scorecards ----

// AttackScorecards are the agent attack scorecards the org uploaded, newest
// first (limit 0: the default).
func (c *Cloud) AttackScorecards(ctx context.Context, limit int) ([]Info, error) {
	p := "/v1/attack/scorecards"
	if limit != 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var r struct {
		Scorecards []Info `json:"scorecards"`
	}
	return r.Scorecards, c.request(ctx, "GET", p, nil, &r)
}

// AttackScorecard is one scorecard, with the signed file as uploaded.
func (c *Cloud) AttackScorecard(ctx context.Context, id string) (Info, error) {
	var r struct {
		Scorecard Info `json:"scorecard"`
	}
	return r.Scorecard, c.request(ctx, "GET", fmt.Sprintf("/v1/attack/scorecards/%s", esc(id)), nil, &r)
}

// UploadAttackScorecard uploads the signed scorecard `kosa8 attack run`
// wrote (its JSON); the signature is checked before it is stored.
func (c *Cloud) UploadAttackScorecard(ctx context.Context, signed json.RawMessage) (Info, error) {
	if !json.Valid(signed) {
		return nil, usage("the signed scorecard is not JSON")
	}
	var r struct {
		Scorecard Info `json:"scorecard"`
	}
	return r.Scorecard, c.request(ctx, "POST", "/v1/attack/scorecards", map[string]any{"signed": signed}, &r)
}

// ---- RL: compare and alerts ----

// CompareRuns puts two to four RL runs side by side: reward by step and a summary each.
func (c *Cloud) CompareRuns(ctx context.Context, runIDs ...string) (Info, error) {
	if len(runIDs) < 2 || len(runIDs) > 4 {
		return nil, usage("CompareRuns takes 2..4 run ids, not %d", len(runIDs))
	}
	var out Info
	return out, c.request(ctx, "GET", "/v1/rl/compare?ids="+url.QueryEscape(strings.Join(runIDs, ",")), nil, &out)
}

// CloudRLAlert is an alert rule on RL runs.
type CloudRLAlert struct {
	ID          string   `json:"id"`
	RunID       *string  `json:"run_id"` // nil: every run
	Scope       string   `json:"scope"`
	Kind        string   `json:"kind"`
	Threshold   float64  `json:"threshold"`
	Window      *int     `json:"window"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	LastFiredAt *int64   `json:"last_fired_at"`
	LastValue   *float64 `json:"last_value"`
	LastRunID   *string  `json:"last_run_id"`
}

// CloudRLAlertSpec is a new alert rule.
type CloudRLAlertSpec struct {
	RunID     string // "" every run
	Kind      string
	Threshold float64
	Window    int // 0: the kind's default
}

// CloudRLAlertUpdate changes a rule; nil fields are left as they are.
type CloudRLAlertUpdate struct {
	Threshold *float64
	Window    *int
	Enabled   *bool
}

// CloudRLAlerts are alert rules on RL runs (they fire the rl.alert webhook event).
type CloudRLAlerts struct{ c *Cloud }

// List is the rules (those of runID and the org-wide ones; "" all) and the
// kinds one can make, as sent.
func (s *CloudRLAlerts) List(ctx context.Context, runID string) ([]CloudRLAlert, Info, error) {
	p := "/v1/rl/alerts"
	if runID != "" {
		p += "?run_id=" + url.QueryEscape(runID)
	}
	var r struct {
		Alerts []CloudRLAlert `json:"alerts"`
		Kinds  Info           `json:"kinds"`
	}
	err := s.c.request(ctx, "GET", p, nil, &r)
	return r.Alerts, r.Kinds, err
}

// Create adds a rule.
func (s *CloudRLAlerts) Create(ctx context.Context, spec CloudRLAlertSpec) (CloudRLAlert, error) {
	if spec.Kind == "" {
		return CloudRLAlert{}, usage("an alert needs a Kind")
	}
	body := map[string]any{"kind": spec.Kind, "threshold": spec.Threshold}
	if spec.RunID != "" {
		body["run_id"] = spec.RunID
	}
	if spec.Window != 0 {
		body["window"] = spec.Window
	}
	var r struct {
		Alert CloudRLAlert `json:"alert"`
	}
	err := s.c.request(ctx, "POST", "/v1/rl/alerts", body, &r)
	return r.Alert, err
}

// Update changes, pauses (Enabled false) or resumes a rule.
func (s *CloudRLAlerts) Update(ctx context.Context, id string, u CloudRLAlertUpdate) (CloudRLAlert, error) {
	body := map[string]any{}
	if u.Threshold != nil {
		body["threshold"] = *u.Threshold
	}
	if u.Window != nil {
		body["window"] = *u.Window
	}
	if u.Enabled != nil {
		body["enabled"] = *u.Enabled
	}
	if len(body) == 0 {
		return CloudRLAlert{}, usage("Update needs at least one field to change")
	}
	var r struct {
		Alert CloudRLAlert `json:"alert"`
	}
	err := s.c.request(ctx, "PATCH", fmt.Sprintf("/v1/rl/alerts/%s", esc(id)), body, &r)
	return r.Alert, err
}

// Delete deletes a rule.
func (s *CloudRLAlerts) Delete(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", fmt.Sprintf("/v1/rl/alerts/%s", esc(id)), nil, nil)
}
