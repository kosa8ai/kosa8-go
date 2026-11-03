package kosa8

// kosa8 Cloud: the same sandboxes, run on kosa8's hosts (or your own, with
// bring-your-own-cloud) instead of the local daemon.
//
//	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{}) // KOSA8_API_KEY from the environment
//	box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "python"})
//	r, _ := box.Exec(ctx, []string{"python3", "-c", "print(6 * 7)"}, nil)
//	fmt.Print(r.Stdout) // 42
//	_ = box.Delete(ctx)
//
// Authentication is an API key (k8c_live_...) made in the cloud dashboard.
// Errors are the SDK's usual *Error; each carries the cloud's stable Code
// (for example payment_required, quota_exceeded).

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
	"strings"
	"time"
)

// DefaultCloudURL is the kosa8 Cloud control plane.
const DefaultCloudURL = "https://api.kosa8.cloud"

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

func (c *Cloud) request(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return usage("encode request: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return usage("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Kind: KindTimeout, Message: fmt.Sprintf("kosa8 Cloud did not answer %s %s in time", method, path), Method: method, Path: path, Err: err}
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		return &Error{Kind: KindConnection, Message: fmt.Sprintf("could not reach kosa8 Cloud at %s: %v", c.baseURL, err), Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Kind: KindConnection, Message: fmt.Sprintf("read %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	if resp.StatusCode >= 400 {
		return cloudError(resp.StatusCode, decodeBody(b), method, path)
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("decode %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	return nil
}

// cloudError reads the cloud's {error: code, message}: the sentence becomes
// the message and the code is kept.
func cloudError(status int, body any, method, path string) *Error {
	msgBody := body
	code := ""
	if m, ok := body.(map[string]any); ok {
		code, _ = m["error"].(string)
		msg, _ := m["message"].(string)
		if msg == "" {
			msg = code
		}
		msgBody = map[string]any{"error": msg}
	}
	e := errorFromResponse(status, msgBody, method, path)
	e.Code = code
	e.Body = body
	return e
}

// CloudSandboxInfo is a cloud sandbox as the control plane reports it.
type CloudSandboxInfo struct {
	ID         string  `json:"id"`
	State      string  `json:"state"` // creating | running | sleeping | stopped | deleted | error
	Template   string  `json:"template"`
	CPUs       float64 `json:"cpus"`
	MemMB      int     `json:"mem_mb"`
	Source     *string `json:"source"` // pool | restore | cold
	Region     *string `json:"region"`
	OwnHost    bool    `json:"own_host"`
	CreatedAt  int64   `json:"created_at"`
	StartedAt  *int64  `json:"started_at"`
	StoppedAt  *int64  `json:"stopped_at"`
	DeletedAt  *int64  `json:"deleted_at"`
	LastError  *string `json:"last_error"`
	RunningS   int64   `json:"running_s"`
	AsleepS    int64   `json:"asleep_s"`
	SleepBytes int64   `json:"sleep_bytes"`
	CostUSD    float64 `json:"cost_usd"`
}

// CloudCreateOptions are the settings for a new cloud sandbox; zero values
// take the org's defaults.
type CloudCreateOptions struct {
	Template string // base, python, node, ...
	Region   string // such as eu-hel1
	// SleepAfterS is idle seconds before the sandbox sleeps; Ptr(0) never.
	SleepAfterS *int
}

// CloudSandboxes creates and finds cloud sandboxes.
type CloudSandboxes struct{ c *Cloud }

type sandboxEnvelope struct {
	Sandbox CloudSandboxInfo `json:"sandbox"`
}

// Create starts a sandbox; it returns once the sandbox is running.
func (s *CloudSandboxes) Create(ctx context.Context, o CloudCreateOptions) (*CloudSandbox, error) {
	body := map[string]any{}
	if o.Template != "" {
		body["template"] = o.Template
	}
	if o.Region != "" {
		body["region"] = o.Region
	}
	if o.SleepAfterS != nil {
		body["sleep_after_s"] = *o.SleepAfterS
	}
	var r sandboxEnvelope
	if err := s.c.request(ctx, "POST", "/v1/sandboxes", body, &r); err != nil {
		return nil, err
	}
	return &CloudSandbox{c: s.c, Info: r.Sandbox}, nil
}

// List returns the org's sandboxes; with all, deleted ones too.
func (s *CloudSandboxes) List(ctx context.Context, all bool) ([]*CloudSandbox, error) {
	p := "/v1/sandboxes"
	if all {
		p += "?all=1"
	}
	var r struct {
		Sandboxes []CloudSandboxInfo `json:"sandboxes"`
	}
	if err := s.c.request(ctx, "GET", p, nil, &r); err != nil {
		return nil, err
	}
	out := make([]*CloudSandbox, 0, len(r.Sandboxes))
	for _, i := range r.Sandboxes {
		out = append(out, &CloudSandbox{c: s.c, Info: i})
	}
	return out, nil
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
