package kosa8

// kosa8 Cloud: the same sandboxes, run on kosa8's hosts (or your own, with
// bring-your-own-cloud) instead of the local daemon.
//
//	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{}) // KOSA8_API_KEY from the environment
//	box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "python", TTLSeconds: 3600})
//	r, _ := box.Exec(ctx, []string{"python3", "-c", "print(6 * 7)"}, nil)
//	fmt.Print(r.Stdout) // 42
//	_ = box.Delete(ctx)
//
// Authentication is an API key (k8c_live_...) made in the cloud dashboard.
// Errors are the SDK's usual *Error; each carries the cloud's stable Code
// (for example payment_required, quota_exceeded); see ErrPaymentRequired,
// ErrOrgFrozen, ErrRateLimit, ErrExecTimeout and ErrUnsupported.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultCloudURL is the kosa8 Cloud control plane.
const DefaultCloudURL = "https://api.kosa8.com"

// CloudKeyPrefix starts every kosa8 Cloud API key.
const CloudKeyPrefix = "k8c_live_"

// CloudOptions configures a Cloud client.
type CloudOptions struct {
	APIKey     string              // default $KOSA8_API_KEY
	BaseURL    string              // default $KOSA8_CLOUD_URL, then DefaultCloudURL
	HTTPClient *http.Client        // default a plain http.Client
	Timeout    time.Duration       // per request; default 120 s
	Getenv     func(string) string // for tests; default os.Getenv
}

// Cloud is a client for the kosa8 Cloud control plane.
type Cloud struct {
	Sandboxes *CloudSandboxes
	Functions *CloudFunctions

	baseURL string
	key     string
	hc      *http.Client
	timeout time.Duration
}

func checkCloudBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", usage("kosa8 Cloud address is not a URL: %s", raw)
	}
	h := u.Hostname()
	local := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		// The API key travels in every request.
		return "", usage("kosa8 Cloud address must use https: %s", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// NewCloud returns a Cloud client. The key must start with k8c_live_ and the
// address must use https (plain http only to localhost).
func NewCloud(o CloudOptions) (*Cloud, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	key := o.APIKey
	if key == "" {
		key = getenv("KOSA8_API_KEY")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, usage("no kosa8 Cloud API key: pass CloudOptions.APIKey or set KOSA8_API_KEY")
	}
	if !strings.HasPrefix(key, CloudKeyPrefix) {
		return nil, usage("a kosa8 Cloud API key starts with %s", CloudKeyPrefix)
	}
	base := o.BaseURL
	if base == "" {
		base = getenv("KOSA8_CLOUD_URL")
	}
	if base == "" {
		base = DefaultCloudURL
	}
	base, err := checkCloudBase(base)
	if err != nil {
		return nil, err
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	c := &Cloud{baseURL: base, key: key, hc: hc, timeout: timeout}
	c.Sandboxes = &CloudSandboxes{c: c}
	c.Functions = &CloudFunctions{c: c}
	return c, nil
}

// BaseURL is the control-plane address in use.
func (c *Cloud) BaseURL() string { return c.baseURL }

// String never includes the API key.
func (c *Cloud) String() string { return fmt.Sprintf("kosa8.Cloud(%s)", c.baseURL) }

// GoString keeps %#v from printing the key.
func (c *Cloud) GoString() string { return c.String() }

// Me reports the org, the key in use and, for a member's key, the member.
func (c *Cloud) Me(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/me", nil, &out)
}

// Usage reports this month's usage, bill estimate and whether work is paused
// for payment.
func (c *Cloud) Usage(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/usage", nil, &out)
}

// Templates lists the templates the org can create sandboxes from now.
func (c *Cloud) Templates(ctx context.Context) ([]CloudTemplate, error) {
	var r struct {
		Templates []CloudTemplate `json:"templates"`
	}
	if err := c.request(ctx, "GET", "/v1/templates", nil, &r); err != nil {
		return nil, err
	}
	return r.Templates, nil
}

// CloudTemplate is a template a cloud sandbox can start from.
type CloudTemplate struct {
	Key     string   `json:"key"`
	CPUs    float64  `json:"cpus"`
	MemMB   int      `json:"mem_mb"`
	Image   string   `json:"image"`
	Regions []string `json:"regions"`
	Hosts   int      `json:"hosts"`
	Warm    int      `json:"warm"`
	OwnHost bool     `json:"own_host"`
}

// request sends a JSON body (or none) and decodes a JSON answer into out.
func (c *Cloud) request(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return usage("encode request: %v", err)
		}
		rd = bytes.NewReader(b)
		ct = "application/json"
	}
	b, err := c.do(ctx, method, path, ct, rd, "application/json")
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("decode %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	return nil
}

// do sends one request and returns the answer's body; an HTTP error answer
// becomes a typed *Error.
func (c *Cloud) do(ctx context.Context, method, path, contentType string, body io.Reader, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, usage("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", UserAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Kind: KindTimeout, Message: fmt.Sprintf("kosa8 Cloud did not answer %s %s in time", method, path), Method: method, Path: path, Err: err}
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, &Error{Kind: KindConnection, Message: fmt.Sprintf("could not reach kosa8 Cloud at %s: %v", c.baseURL, err), Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Kind: KindConnection, Message: fmt.Sprintf("read %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	if resp.StatusCode >= 400 {
		e := cloudError(resp.StatusCode, decodeBody(b), method, path)
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return nil, e
	}
	return b, nil
}

// parseRetryAfter reads a Retry-After header: seconds, or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

// execTimeoutHint is added to the cloud's own words when an exec is cut off.
const execTimeoutHint = "kosa8 Cloud cuts a buffered exec off after about 30 s; run long jobs in the background " +
	"(nohup cmd > /workspace/job.log 2>&1 &) and poll /workspace for the result"

// cloudError reads the cloud's {error: code, message, upgrade_url?,
// agent_message?}: the sentence becomes the message and the code is kept.
func cloudError(status int, body any, method, path string) *Error {
	msgBody := body
	code, upgrade, agent := "", "", ""
	if m, ok := body.(map[string]any); ok {
		code, _ = m["error"].(string)
		msg, _ := m["message"].(string)
		if msg == "" {
			msg = code
		}
		upgrade, _ = m["upgrade_url"].(string)
		agent, _ = m["agent_message"].(string)
		msgBody = map[string]any{"error": msg}
	}
	e := errorFromResponse(status, msgBody, method, path)
	e.Code = code
	e.Body = body
	e.UpgradeURL = upgrade
	e.AgentMessage = agent
	switch {
	case status == 423 || code == "org_frozen":
		e.Kind = KindOrgFrozen
	case isExecTimeout(status, code, path, e.Message):
		e.Kind = KindExecTimeout
		e.Message += " (" + execTimeoutHint + ")"
	}
	return e
}

func isExecTimeout(status int, code, path, msg string) bool {
	if status == 504 || code == "exec_timeout" {
		return true
	}
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return status == 502 && code == "host_failed" && strings.HasSuffix(p, "/exec") && strings.Contains(strings.ToLower(msg), "timed out")
}

// CloudSandboxInfo is a cloud sandbox as the control plane reports it (the
// OpenAPI Sandbox schema). Nullable fields are pointers.
type CloudSandboxInfo struct {
	ID string `json:"id"`
	// State: creating | running | paused | sleeping | stopped | error |
	// deleted | host_removed | host_lost
	State    string `json:"state"`
	Template string `json:"template"`
	// OriginItemVersion is the kosa8 Store version it was started (or
	// forked) from.
	OriginItemVersion *string `json:"origin_item_version"`
	CPUs              float64 `json:"cpus"`
	MemMB             int     `json:"mem_mb"`
	Source            *string `json:"source"`    // pool | restore | cold | fork
	ParentID          *string `json:"parent_id"` // the sandbox it was forked from
	Region            *string `json:"region"`
	OwnHost           bool    `json:"own_host"`
	CreatedAt         int64   `json:"created_at"` // Unix ms
	StartedAt         *int64  `json:"started_at"`
	StoppedAt         *int64  `json:"stopped_at"`
	DeletedAt         *int64  `json:"deleted_at"`
	DeleteAfter       *int64  `json:"delete_after"`  // Unix ms: deleted automatically then
	SleepAfterS       *int    `json:"sleep_after_s"` // idle seconds before it sleeps; 0 never
	LastError         *string `json:"last_error"`
	HostUnreachable   bool    `json:"host_unreachable"`
	RunningS          int64   `json:"running_s"`
	AsleepS           int64   `json:"asleep_s"`
	SleepBytes        int64   `json:"sleep_bytes"`
	CostUSD           float64 `json:"cost_usd"` // estimate so far
}

// Limits the Cloud API checks; the SDK checks them first.
const (
	CloudMaxSleepAfterS = 604800  // 7 days
	CloudMinTTLSeconds  = 60      // 1 minute
	CloudMaxTTLSeconds  = 7776000 // 90 days
	CloudMaxForks       = 100
	CloudMaxLabelLen    = 200
)

// CloudCreateOptions are the settings for a new cloud sandbox; zero values
// take the org's defaults and are not sent.
type CloudCreateOptions struct {
	// Template is a key from Cloud.Templates (base, python, node, ...) or a
	// kosa8 Store item: creator/name or creator/name@1.2.3.
	Template string
	Region   string // such as eu-hel1
	// SleepAfterS is idle seconds before the sandbox sleeps (0..604800);
	// Ptr(0) never.
	SleepAfterS *int
	// TTLSeconds deletes the sandbox this many seconds from now
	// (60..7776000). 0 means no lifetime.
	TTLSeconds int
	// DeleteAfter deletes the sandbox at this Unix time in milliseconds. 0
	// means none. Set TTLSeconds or DeleteAfter, not both.
	DeleteAfter int64
	// AcceptPrice is a paid Store item's current price in USD, which the
	// person agreed to. Without it a paid item answers 402
	// price_not_accepted; a different price answers 409 price_changed.
	AcceptPrice *float64
}

func checkSleepAfter(v *int) error {
	if v != nil && (*v < 0 || *v > CloudMaxSleepAfterS) {
		return usage("SleepAfterS must be 0..%d seconds, not %d", CloudMaxSleepAfterS, *v)
	}
	return nil
}

func lifetimeBody(body map[string]any, ttl int, deleteAfter int64) error {
	if ttl != 0 && deleteAfter != 0 {
		return usage("set TTLSeconds or DeleteAfter, not both")
	}
	if ttl != 0 {
		if ttl < CloudMinTTLSeconds || ttl > CloudMaxTTLSeconds {
			return usage("TTLSeconds must be %d..%d seconds, not %d", CloudMinTTLSeconds, CloudMaxTTLSeconds, ttl)
		}
		body["ttl_s"] = ttl
	}
	if deleteAfter < 0 {
		return usage("DeleteAfter must be a Unix time in milliseconds, not %d", deleteAfter)
	}
	if deleteAfter != 0 {
		body["delete_after"] = deleteAfter
	}
	return nil
}

// CloudSandboxes creates and finds cloud sandboxes.
type CloudSandboxes struct{ c *Cloud }

type sandboxEnvelope struct {
	Sandbox CloudSandboxInfo `json:"sandbox"`
}

// Create starts a sandbox; it returns once the sandbox is running. Bad
// TTLSeconds, DeleteAfter or SleepAfterS are refused before any request.
func (s *CloudSandboxes) Create(ctx context.Context, o CloudCreateOptions) (*CloudSandbox, error) {
	body := map[string]any{}
	if o.Template != "" {
		body["template"] = o.Template
	}
	if o.Region != "" {
		body["region"] = o.Region
	}
	if err := checkSleepAfter(o.SleepAfterS); err != nil {
		return nil, err
	}
	if o.SleepAfterS != nil {
		body["sleep_after_s"] = *o.SleepAfterS
	}
	if err := lifetimeBody(body, o.TTLSeconds, o.DeleteAfter); err != nil {
		return nil, err
	}
	if o.AcceptPrice != nil {
		body["accept_price"] = *o.AcceptPrice
	}
	var r sandboxEnvelope
	if err := s.c.request(ctx, "POST", "/v1/sandboxes", body, &r); err != nil {
		return nil, err
	}
	return &CloudSandbox{c: s.c, Info: r.Sandbox}, nil
}

// List returns the org's sandboxes; with all, deleted ones too. It returns
// the first page only once the Cloud API pages its answer; ListAll follows
// every page.
func (s *CloudSandboxes) List(ctx context.Context, all bool) ([]*CloudSandbox, error) {
	p, err := s.ListPage(ctx, CloudListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return p.Sandboxes, nil
}

// CloudListOptions select a page of sandboxes.
type CloudListOptions struct {
	All    bool   // include deleted sandboxes
	Limit  int    // page size; 0 the server's default
	Cursor string // a page's NextCursor; "" the first page
}

// CloudSandboxPage is one page of sandboxes.
type CloudSandboxPage struct {
	Sandboxes []*CloudSandbox
	// NextCursor asks for the next page; "" when this is the last. Servers
	// that do not page their answer yet never send one.
	NextCursor string
}

// ListPage returns one page of the org's sandboxes.
func (s *CloudSandboxes) ListPage(ctx context.Context, o CloudListOptions) (CloudSandboxPage, error) {
	if o.Limit < 0 {
		return CloudSandboxPage{}, usage("Limit must not be negative")
	}
	q := url.Values{}
	if o.All {
		q.Set("all", "1")
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	p := "/v1/sandboxes"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var r struct {
		Sandboxes  []CloudSandboxInfo `json:"sandboxes"`
		NextCursor *string            `json:"next_cursor"`
	}
	if err := s.c.request(ctx, "GET", p, nil, &r); err != nil {
		return CloudSandboxPage{}, err
	}
	out := CloudSandboxPage{Sandboxes: make([]*CloudSandbox, 0, len(r.Sandboxes))}
	for _, i := range r.Sandboxes {
		out.Sandboxes = append(out.Sandboxes, &CloudSandbox{c: s.c, Info: i})
	}
	if r.NextCursor != nil {
		out.NextCursor = *r.NextCursor
	}
	return out, nil
}

// ListAll follows NextCursor from o.Cursor until the last page and returns
// every sandbox. It stops if the server hands back a cursor it already gave.
func (s *CloudSandboxes) ListAll(ctx context.Context, o CloudListOptions) ([]*CloudSandbox, error) {
	var out []*CloudSandbox
	seen := map[string]bool{}
	for {
		p, err := s.ListPage(ctx, o)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Sandboxes...)
		if p.NextCursor == "" || seen[p.NextCursor] || p.NextCursor == o.Cursor {
			return out, nil
		}
		seen[p.NextCursor] = true
		o.Cursor = p.NextCursor
	}
}

// Get returns one sandbox by id.
func (s *CloudSandboxes) Get(ctx context.Context, id string) (*CloudSandbox, error) {
	var r sandboxEnvelope
	if err := s.c.request(ctx, "GET", "/v1/sandboxes/"+url.PathEscape(id), nil, &r); err != nil {
		return nil, err
	}
	return &CloudSandbox{c: s.c, Info: r.Sandbox}, nil
}

// CloudSandbox is one cloud sandbox. Info is its state as of the last call.
type CloudSandbox struct {
	Info CloudSandboxInfo
	c    *Cloud
}

// ID is the sandbox id.
func (b *CloudSandbox) ID() string { return b.Info.ID }

// State is the state as of the last call.
func (b *CloudSandbox) State() string { return b.Info.State }

func (b *CloudSandbox) String() string {
	return fmt.Sprintf("kosa8.CloudSandbox(%s, %s)", b.Info.ID, b.Info.State)
}

func (b *CloudSandbox) path(suffix string) string {
	return "/v1/sandboxes/" + url.PathEscape(b.Info.ID) + suffix
}

// CloudExecOptions are optional settings for CloudSandbox.Exec.
type CloudExecOptions struct {
	Image string            // default the template's own image
	Env   map[string]string // sent as K=V, sorted by key
}

// CloudExecResult is a finished cloud command.
type CloudExecResult struct {
	ExitCode *int // nil when the cloud reported no exit status
	Stdout   string
	Stderr   string
	WokeMs   *int // set when the sandbox was asleep and woke for this command
}

// OK reports whether the command exited 0.
func (r CloudExecResult) OK() bool { return r.ExitCode != nil && *r.ExitCode == 0 }

// Exec runs argv and waits for it. A sleeping sandbox wakes for it.
//
// Each exec runs in a fresh container; only /workspace persists between
// calls. The Cloud API cuts a buffered exec off after about 30 s (that is
// ErrExecTimeout): run long jobs in the background (nohup ... & writing to
// /workspace) and poll. The request body is capped at 64 KiB (413
// too_large). ExitCode is nil when the cloud reports no exit status, so
// check it (or use OK) before dereferencing.
func (b *CloudSandbox) Exec(ctx context.Context, argv []string, o *CloudExecOptions) (CloudExecResult, error) {
	if len(argv) == 0 {
		return CloudExecResult{}, usage("Exec needs a command")
	}
	body := map[string]any{"cmd": argv}
	if o != nil {
		if o.Image != "" {
			body["image"] = o.Image
		}
		if len(o.Env) > 0 {
			keys := make([]string, 0, len(o.Env))
			for k := range o.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			env := make([]string, 0, len(keys))
			for _, k := range keys {
				env = append(env, k+"="+o.Env[k])
			}
			body["env"] = env
		}
	}
	var r struct {
		Exit   *int   `json:"exit"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		WokeMs *int   `json:"woke_ms"`
	}
	if err := b.c.request(ctx, "POST", b.path("/exec"), body, &r); err != nil {
		return CloudExecResult{}, err
	}
	return CloudExecResult{ExitCode: r.Exit, Stdout: r.Stdout, Stderr: r.Stderr, WokeMs: r.WokeMs}, nil
}

// ExecShell runs a command line through sh -c (the string form of the
// Python and TypeScript exec).
func (b *CloudSandbox) ExecShell(ctx context.Context, cmd string, o *CloudExecOptions) (CloudExecResult, error) {
	return b.Exec(ctx, []string{"sh", "-c", cmd}, o)
}

func (b *CloudSandbox) update(ctx context.Context, method, suffix string) error {
	var r sandboxEnvelope
	if err := b.c.request(ctx, method, b.path(suffix), nil, &r); err != nil {
		return err
	}
	b.Info = r.Sandbox
	return nil
}

// Sleep snapshots the sandbox and frees its VM; it then costs only snapshot
// storage.
func (b *CloudSandbox) Sleep(ctx context.Context) error { return b.update(ctx, "POST", "/sleep") }

// Wake restores a sleeping sandbox, files and all.
func (b *CloudSandbox) Wake(ctx context.Context) error { return b.update(ctx, "POST", "/wake") }

// Refresh reloads Info.
func (b *CloudSandbox) Refresh(ctx context.Context) error { return b.update(ctx, "GET", "") }

// CloudUpdateOptions change a sandbox's lifetime or sleep timer. Zero values
// are not sent.
type CloudUpdateOptions struct {
	// TTLSeconds deletes the sandbox this many seconds from now (60..7776000).
	TTLSeconds int
	// DeleteAfter deletes the sandbox at this Unix time in milliseconds.
	DeleteAfter int64
	// ClearLifetime removes any lifetime, so the sandbox is never deleted
	// automatically (sends "ttl_s": null). Not with TTLSeconds or DeleteAfter.
	ClearLifetime bool
	// SleepAfterS is idle seconds before it sleeps (0..604800); Ptr(0) never.
	SleepAfterS *int
}

// Update changes the sandbox's lifetime or sleep timer (PATCH) and refreshes
// Info. It returns the server's notes, if any.
func (b *CloudSandbox) Update(ctx context.Context, o CloudUpdateOptions) ([]string, error) {
	body := map[string]any{}
	if o.ClearLifetime && (o.TTLSeconds != 0 || o.DeleteAfter != 0) {
		return nil, usage("ClearLifetime cannot be combined with TTLSeconds or DeleteAfter")
	}
	if err := lifetimeBody(body, o.TTLSeconds, o.DeleteAfter); err != nil {
		return nil, err
	}
	if o.ClearLifetime {
		body["ttl_s"] = nil
	}
	if err := checkSleepAfter(o.SleepAfterS); err != nil {
		return nil, err
	}
	if o.SleepAfterS != nil {
		body["sleep_after_s"] = *o.SleepAfterS
	}
	if len(body) == 0 {
		return nil, usage("Update needs TTLSeconds, DeleteAfter, ClearLifetime or SleepAfterS")
	}
	var r struct {
		Sandbox CloudSandboxInfo `json:"sandbox"`
		Notes   []string         `json:"notes"`
	}
	if err := b.c.request(ctx, "PATCH", b.path(""), body, &r); err != nil {
		return nil, err
	}
	b.Info = r.Sandbox
	return r.Notes, nil
}

// Delete deletes the sandbox.
func (b *CloudSandbox) Delete(ctx context.Context) error {
	return b.c.request(ctx, "DELETE", b.path(""), nil, nil)
}

// CloudFunctionSpec is a function to deploy. Extra fields go in Extra.
type CloudFunctionSpec struct {
	Runtime  string            `json:"runtime"` // python3.12 | node22
	Code     string            `json:"code"`
	Env      map[string]string `json:"env,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
	Web      string            `json:"web,omitempty"`      // off | public | key
	Schedule string            `json:"schedule,omitempty"` // cron
	Extra    map[string]any    `json:"-"`
}

func (s CloudFunctionSpec) body() map[string]any {
	m := map[string]any{}
	for k, v := range s.Extra {
		m[k] = v
	}
	m["runtime"] = s.Runtime
	m["code"] = s.Code
	if len(s.Env) > 0 {
		m["env"] = s.Env
	}
	if s.TimeoutS != 0 {
		m["timeout_s"] = s.TimeoutS
	}
	if s.Web != "" {
		m["web"] = s.Web
	}
	if s.Schedule != "" {
		m["schedule"] = s.Schedule
	}
	return m
}

// CloudInvocation identifies one function call.
type CloudInvocation struct {
	ID         string `json:"id"`
	Function   string `json:"function"`
	FunctionID string `json:"function_id"`
	Version    int    `json:"version"`
	Trigger    string `json:"trigger"` // api | web | batch | cron
}

// CloudFunctionError is what a handler raised.
type CloudFunctionError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// CloudTimings are an invocation's timings in milliseconds.
type CloudTimings struct {
	Start     *float64 `json:"start"`
	Run       *float64 `json:"run"`
	HostTotal *float64 `json:"host_total"`
	Total     float64  `json:"total"`
}

// CloudInvokeResult is one function call. A handler that raised is OK ==
// false with Error set, not a Go error.
type CloudInvokeResult struct {
	Invocation    *CloudInvocation    `json:"invocation"`
	OK            bool                `json:"ok"`
	Result        json.RawMessage     `json:"result,omitempty"`
	Error         *CloudFunctionError `json:"error,omitempty"`
	Logs          string              `json:"logs,omitempty"`
	LogsTruncated bool                `json:"logs_truncated,omitempty"`
	Source        *string             `json:"source,omitempty"` // pool | restore
	TimingsMs     *CloudTimings       `json:"timings_ms,omitempty"`
}

// DecodeResult unmarshals Result into v.
func (r CloudInvokeResult) DecodeResult(v any) error {
	if len(r.Result) == 0 {
		return usage("the invocation has no result")
	}
	return json.Unmarshal(r.Result, v)
}

// CloudMapResult is a Map batch; Results are in input order.
type CloudMapResult struct {
	Function  string              `json:"function"`
	Version   int                 `json:"version"`
	Count     int                 `json:"count"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	TotalMs   float64             `json:"total_ms"`
	Results   []CloudInvokeResult `json:"results"`
}

// CloudFunctions deploys and calls cloud functions.
type CloudFunctions struct{ c *Cloud }

func fnPath(name, suffix string) string { return "/v1/functions/" + url.PathEscape(name) + suffix }

// Deploy creates or replaces a function; every deploy bumps its version. It
// returns the control plane's answer ({"function": {...}}).
func (f *CloudFunctions) Deploy(ctx context.Context, name string, spec CloudFunctionSpec) (Info, error) {
	var out Info
	return out, f.c.request(ctx, "PUT", fnPath(name, ""), spec.body(), &out)
}

// List returns the org's functions.
func (f *CloudFunctions) List(ctx context.Context) ([]Info, error) {
	var r struct {
		Functions []Info `json:"functions"`
	}
	if err := f.c.request(ctx, "GET", "/v1/functions", nil, &r); err != nil {
		return nil, err
	}
	return r.Functions, nil
}

// Get returns one function ({"function": {...}}, with its code).
func (f *CloudFunctions) Get(ctx context.Context, name string) (Info, error) {
	var out Info
	return out, f.c.request(ctx, "GET", fnPath(name, ""), nil, &out)
}

// Delete deletes a function.
func (f *CloudFunctions) Delete(ctx context.Context, name string) error {
	return f.c.request(ctx, "DELETE", fnPath(name, ""), nil, nil)
}

// Invoke calls the function once with event (nil sends null).
func (f *CloudFunctions) Invoke(ctx context.Context, name string, event any) (CloudInvokeResult, error) {
	var out CloudInvokeResult
	err := f.c.request(ctx, "POST", fnPath(name, "/invoke"), map[string]any{"event": event}, &out)
	return out, err
}

// CloudMapOptions are optional settings for Map.
type CloudMapOptions struct {
	Concurrency int // 0 takes the cloud's default
}

// Map makes one call per event; results come back in input order.
func (f *CloudFunctions) Map(ctx context.Context, name string, events []any, o *CloudMapOptions) (CloudMapResult, error) {
	if events == nil {
		events = []any{}
	}
	body := map[string]any{"events": events}
	if o != nil && o.Concurrency != 0 {
		body["concurrency"] = o.Concurrency
	}
	var out CloudMapResult
	err := f.c.request(ctx, "POST", fnPath(name, "/map"), body, &out)
	return out, err
}
