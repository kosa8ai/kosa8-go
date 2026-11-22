package kosa8

// More of kosa8 Cloud: sized creates (in cloud.go), background commands,
// durable snapshots, previews, terminals, streamed files and presigned
// links, egress, secrets, schedules, the code interpreter, metrics, logs,
// labels and Pipelines.
//
// Hosts that lack a feature answer 501 (not_available / not_supported),
// which is errors.Is(err, ErrUnsupported). Cloud.Capabilities tells you
// what the org and its hosts can do before you try.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// envList is a map as K=V strings, sorted by key.
func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

var (
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sbxNameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	snapIDRE   = regexp.MustCompile(`^snap_[A-Za-z0-9]{8,40}$`)
	secretRE   = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,63}$`)
)

// Limits on a create's env and labels, checked before any request.
const (
	CloudMaxEnv        = 64
	CloudMaxEnvValue   = 8 << 10
	CloudMaxLabels     = 16
	CloudMaxLabelValue = 256
)

func checkEnv(env map[string]string) error {
	if len(env) > CloudMaxEnv {
		return usage("at most %d environment variables, not %d", CloudMaxEnv, len(env))
	}
	for k, v := range env {
		if !envNameRE.MatchString(k) {
			return usage("environment variable name %q: letters, digits and _, not starting with a digit", k)
		}
		if len(v) > CloudMaxEnvValue {
			return usage("environment variable %s is longer than 8 KiB", k)
		}
	}
	return nil
}

func checkLabels(labels map[string]string) error {
	if len(labels) > CloudMaxLabels {
		return usage("at most %d labels, not %d", CloudMaxLabels, len(labels))
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return usage("label key %q: 1..63 of letters, digits, '.', '_', '/' or '-'", k)
		}
		if len(v) > CloudMaxLabelValue {
			return usage("label %s is longer than %d characters", k, CloudMaxLabelValue)
		}
	}
	return nil
}

// sizedBody adds the sized-create, env, labels and snapshot fields of o.
func sizedBody(body map[string]any, o CloudCreateOptions) error {
	if o.Size != "" {
		ok := false
		for _, s := range CloudSizes {
			ok = ok || s == o.Size
		}
		if !ok {
			return usage("Size must be one of %s, not %q", strings.Join(CloudSizes, ", "), o.Size)
		}
		body["size"] = o.Size
	}
	if (o.CPUs == 0) != (o.MemMB == 0) {
		return usage("set CPUs and MemMB together")
	}
	if o.CPUs != 0 {
		if o.Size != "" {
			return usage("set Size or CPUs and MemMB, not both")
		}
		body["cpus"] = o.CPUs
		body["mem_mb"] = o.MemMB
	}
	if o.Image != "" {
		body["image"] = o.Image
	}
	if o.DiskGB != 0 {
		if o.DiskGB < 8 || o.DiskGB > 200 {
			return usage("DiskGB must be 8..200, not %d", o.DiskGB)
		}
		body["disk_gb"] = o.DiskGB
	}
	if o.Persistent {
		body["persistent"] = true
	}
	if o.Docker {
		body["docker"] = true
	}
	switch o.CPU {
	case "":
	case "shared", "dedicated":
		body["cpu"] = o.CPU
	default:
		return usage(`CPU must be "shared" or "dedicated", not %q`, o.CPU)
	}
	if o.Name != "" {
		if !sbxNameRE.MatchString(o.Name) {
			return usage("Name %q: 1..64 of letters, digits, '.', '_' or '-', starting with a letter or digit", o.Name)
		}
		body["name"] = o.Name
	}
	if len(o.Labels) > 0 {
		if err := checkLabels(o.Labels); err != nil {
			return err
		}
		body["labels"] = o.Labels
	}
	if len(o.Env) > 0 {
		if err := checkEnv(o.Env); err != nil {
			return err
		}
		body["env"] = o.Env
	}
	if len(o.Secrets) > 0 {
		for _, n := range o.Secrets {
			if err := checkSecretName(n); err != nil {
				return err
			}
		}
		body["secrets"] = o.Secrets
	}
	if o.Egress != nil {
		body["egress"] = egressBody(*o.Egress)
	}
	if o.Snapshot != "" {
		if !snapIDRE.MatchString(o.Snapshot) {
			return usage("Snapshot must be a snapshot id (snap_...), not %q", o.Snapshot)
		}
		if o.Template != "" || o.Size != "" || o.CPUs != 0 || o.Image != "" || o.DiskGB != 0 ||
			o.Docker || o.Persistent || o.CPU != "" || o.Region != "" {
			return usage("a create from a snapshot takes the snapshot's template, size, image, disk and region: set only Env, Name, Labels, a lifetime or SleepAfterS with it")
		}
		body["snapshot"] = o.Snapshot
	}
	return nil
}

// WaitRunning polls the sandbox every interval (0 means 1 s) until it is no
// longer "creating", then returns; a state other than running is an error.
// It stops when ctx is done.
func (b *CloudSandbox) WaitRunning(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	for b.Info.State == "creating" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if err := b.Refresh(ctx); err != nil {
			return err
		}
	}
	if b.Info.State != "running" {
		msg := ""
		if b.Info.LastError != nil {
			msg = ": " + *b.Info.LastError
		}
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("sandbox %s is %s, not running%s", b.Info.ID, b.Info.State, msg)}
	}
	return nil
}

// Capabilities reports what the org and its hosts can do (GET
// /v1/capabilities), for feature detection.
func (c *Cloud) Capabilities(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/capabilities", nil, &out)
}

// ---- background commands ----

// CloudBackgroundExec is a command started with ExecBackground.
type CloudBackgroundExec struct {
	ID              string  `json:"id"` // bg_...
	SandboxID       string  `json:"sandbox_id"`
	State           string  `json:"state"` // running | exited
	PID             *int    `json:"pid"`
	ExitCode        *int    `json:"exit"`
	OutputPath      string  `json:"output_path"` // stdout and stderr, in the sandbox
	ExitPath        string  `json:"exit_path"`
	Output          *string `json:"output"` // the output so far (its tail)
	OutputBytes     *int64  `json:"output_bytes"`
	OutputTruncated bool    `json:"output_truncated"`
}

// Done reports whether the command has exited.
func (e CloudBackgroundExec) Done() bool { return e.State == "exited" || e.ExitCode != nil }

// ExecBackground starts argv and returns at once while it keeps running
// (no 90 s limit). Poll it with BackgroundExec or WaitBackground.
func (b *CloudSandbox) ExecBackground(ctx context.Context, argv []string, o *CloudExecOptions) (CloudBackgroundExec, error) {
	if len(argv) == 0 {
		return CloudBackgroundExec{}, usage("ExecBackground needs a command")
	}
	body := map[string]any{"cmd": argv, "background": true}
	if o != nil {
		if o.Image != "" {
			body["image"] = o.Image
		}
		if len(o.Env) > 0 {
			body["env"] = envList(o.Env)
		}
	}
	var r struct {
		Exec CloudBackgroundExec `json:"exec"`
	}
	err := b.c.request(ctx, "POST", b.path("/exec"), body, &r)
	return r.Exec, err
}

// BackgroundExec reads a background command: running or exited, its exit
// code and its output so far.
func (b *CloudSandbox) BackgroundExec(ctx context.Context, execID string) (CloudBackgroundExec, error) {
	if execID == "" {
		return CloudBackgroundExec{}, usage("BackgroundExec needs an exec id (bg_...)")
	}
	var r struct {
		Exec CloudBackgroundExec `json:"exec"`
	}
	err := b.c.request(ctx, "GET", b.path("/exec/"+url.PathEscape(execID)), nil, &r)
	return r.Exec, err
}

// WaitBackground polls a background command every interval (0 means 2 s)
// until it exits or ctx is done.
func (b *CloudSandbox) WaitBackground(ctx context.Context, execID string, interval time.Duration) (CloudBackgroundExec, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		e, err := b.BackgroundExec(ctx, execID)
		if err != nil || e.Done() {
			return e, err
		}
		select {
		case <-ctx.Done():
			return e, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ---- snapshots ----

// CloudSnapshot is a durable snapshot of a sandbox: memory, processes and
// disk, kept until deleted whatever happens to the sandbox.
type CloudSnapshot struct {
	ID              string  `json:"id"` // snap_...
	Name            *string `json:"name"`
	State           string  `json:"state"`
	SourceSandboxID string  `json:"source_sandbox_id"`
	Template        string  `json:"template"`
	CPUs            int     `json:"cpus"`
	MemMB           int     `json:"mem_mb"`
	Image           *string `json:"image"`
	DiskGB          *int    `json:"disk_gb"`
	Persistent      bool    `json:"persistent"`
	Bytes           int64   `json:"bytes"`
	Region          *string `json:"region"`
	CreatedAt       int64   `json:"created_at"` // Unix ms
	DeletedAt       *int64  `json:"deleted_at"`
	LastError       *string `json:"last_error"`
}

// Snapshot saves the sandbox as a durable snapshot (name may be ""). Start
// new sandboxes from it with CloudCreateOptions.Snapshot.
func (b *CloudSandbox) Snapshot(ctx context.Context, name string) (CloudSnapshot, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	var r struct {
		Snapshot CloudSnapshot `json:"snapshot"`
	}
	err := b.c.request(ctx, "POST", b.path("/snapshots"), body, &r)
	return r.Snapshot, err
}

// CloudSnapshots lists and deletes the org's snapshots.
type CloudSnapshots struct{ c *Cloud }

// List returns the org's snapshots, newest first; sandboxID "" for all.
func (s *CloudSnapshots) List(ctx context.Context, sandboxID string) ([]CloudSnapshot, error) {
	p := "/v1/snapshots"
	if sandboxID != "" {
		p += "?" + url.Values{"sandbox_id": {sandboxID}}.Encode()
	}
	var r struct {
		Snapshots []CloudSnapshot `json:"snapshots"`
	}
	if err := s.c.request(ctx, "GET", p, nil, &r); err != nil {
		return nil, err
	}
	if r.Snapshots == nil {
		r.Snapshots = []CloudSnapshot{}
	}
	return r.Snapshots, nil
}

// Get returns one snapshot.
func (s *CloudSnapshots) Get(ctx context.Context, id string) (CloudSnapshot, error) {
	var r struct {
		Snapshot CloudSnapshot `json:"snapshot"`
	}
	err := s.c.request(ctx, "GET", "/v1/snapshots/"+url.PathEscape(id), nil, &r)
	return r.Snapshot, err
}

// Delete deletes a snapshot; sandboxes made from it are not touched.
func (s *CloudSnapshots) Delete(ctx context.Context, id string) error {
	return s.c.request(ctx, "DELETE", "/v1/snapshots/"+url.PathEscape(id), nil, nil)
}

// ---- previews ----

// CloudPreview is a port of the sandbox at its own https URL.
type CloudPreview struct {
	Port        int    `json:"port"`
	Public      bool   `json:"public"`
	URL         string `json:"url"` // a private preview's URL carries its signed token
	Host        string `json:"host"`
	Token       string `json:"token"`
	ExpiresAt   string `json:"expires_at"`
	CreatedAt   int64  `json:"created_at"`
	Wake        bool   `json:"wake"` // a request wakes a sleeping sandbox
	WakePerHour int    `json:"wake_per_hour"`
}

// CloudPreviewOptions are the settings for a new preview.
type CloudPreviewOptions struct {
	Public      bool // no signed link needed
	ExpiresInS  int  // a private link's lifetime; 0 the server's default
	Wake        *bool
	WakePerHour int
}

// CloudPreviews opens a sandbox's ports at https URLs.
type CloudPreviews struct{ b *CloudSandbox }

// Previews returns the sandbox's preview operations.
func (b *CloudSandbox) Previews() *CloudPreviews { return &CloudPreviews{b: b} }

func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return usage("a port is 1..65535, not %d", port)
	}
	return nil
}

type previewEnvelope struct {
	Preview CloudPreview `json:"preview"`
}

// Create opens port at its own URL: private (a signed link) unless Public.
func (p *CloudPreviews) Create(ctx context.Context, port int, o *CloudPreviewOptions) (CloudPreview, error) {
	if err := checkPort(port); err != nil {
		return CloudPreview{}, err
	}
	body := map[string]any{"port": port}
	if o != nil {
		if o.Public {
			body["public"] = true
		}
		if o.ExpiresInS != 0 {
			body["expires_in_s"] = o.ExpiresInS
		}
		if o.Wake != nil {
			body["wake"] = *o.Wake
		}
		if o.WakePerHour != 0 {
			body["wake_per_hour"] = o.WakePerHour
		}
	}
	var r previewEnvelope
	err := p.b.c.request(ctx, "POST", p.b.path("/previews"), body, &r)
	return r.Preview, err
}

// List returns the sandbox's previews (without tokens).
func (p *CloudPreviews) List(ctx context.Context) ([]CloudPreview, error) {
	var r struct {
		Previews []CloudPreview `json:"previews"`
	}
	if err := p.b.c.request(ctx, "GET", p.b.path("/previews"), nil, &r); err != nil {
		return nil, err
	}
	if r.Previews == nil {
		r.Previews = []CloudPreview{}
	}
	return r.Previews, nil
}

// Link makes a new signed link for a private preview; expiresInS 0 takes
// the server's default.
func (p *CloudPreviews) Link(ctx context.Context, port, expiresInS int) (CloudPreview, error) {
	if err := checkPort(port); err != nil {
		return CloudPreview{}, err
	}
	body := map[string]any{}
	if expiresInS != 0 {
		body["expires_in_s"] = expiresInS
	}
	var r previewEnvelope
	err := p.b.c.request(ctx, "POST", p.b.path("/previews/"+strconv.Itoa(port)+"/link"), body, &r)
	return r.Preview, err
}

// Rotate moves the preview to a new host name, so every old link stops
// working.
func (p *CloudPreviews) Rotate(ctx context.Context, port int) (CloudPreview, error) {
	if err := checkPort(port); err != nil {
		return CloudPreview{}, err
	}
	var r previewEnvelope
	err := p.b.c.request(ctx, "POST", p.b.path("/previews/"+strconv.Itoa(port)+"/rotate"), map[string]any{}, &r)
	return r.Preview, err
}

// Delete closes the preview.
func (p *CloudPreviews) Delete(ctx context.Context, port int) error {
	if err := checkPort(port); err != nil {
		return err
	}
	return p.b.c.request(ctx, "DELETE", p.b.path("/previews/"+strconv.Itoa(port)), nil, nil)
}

// ---- terminal ----

// CloudTerminal is a short-lived wss:// URL for an interactive terminal
// (PTY). The URL carries its own token: treat it as a secret.
type CloudTerminal struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
	Protocol  string `json:"protocol"`
}

// CloudTerminalOptions size the terminal and pick its command (default a
// login shell).
type CloudTerminalOptions struct {
	Cols, Rows int
	Command    []string
}

// Terminal returns a wss:// URL for an interactive terminal in the sandbox.
func (b *CloudSandbox) Terminal(ctx context.Context, o *CloudTerminalOptions) (CloudTerminal, error) {
	body := map[string]any{}
	if o != nil {
		if o.Cols != 0 {
			body["cols"] = o.Cols
		}
		if o.Rows != 0 {
			body["rows"] = o.Rows
		}
		if len(o.Command) > 0 {
			body["command"] = o.Command
		}
	}
	var r struct {
		Terminal CloudTerminal `json:"terminal"`
	}
	err := b.c.request(ctx, "POST", b.path("/terminal"), body, &r)
	return r.Terminal, err
}

// ---- streamed files and links ----

// stream sends one request and hands back the answer for the caller to
// read; an HTTP error answer becomes a typed *Error. The request's timeout
// does not apply; ctx bounds it.
func (c *Cloud) stream(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, usage("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", UserAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, c.unreachable(method, path, err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, withHeaders(cloudError(resp.StatusCode, decodeBody(b), method, path), resp.Header)
	}
	return resp, nil
}

// Download streams a file of any size into w and returns the bytes copied.
func (f *CloudFiles) Download(ctx context.Context, path string, w io.Writer) (int64, error) {
	if err := needPath(path); err != nil {
		return 0, err
	}
	resp, err := f.b.c.stream(ctx, "GET", f.filePath("/raw", path, nil), "", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// Upload streams r into a file of any size, replacing it.
func (f *CloudFiles) Upload(ctx context.Context, path string, r io.Reader) (CloudFileInfo, error) {
	if err := needPath(path); err != nil {
		return CloudFileInfo{}, err
	}
	p := f.filePath("/raw", path, nil)
	resp, err := f.b.c.stream(ctx, "PUT", p, "application/octet-stream", r)
	if err != nil {
		return CloudFileInfo{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		File CloudFileInfo `json:"file"`
	}
	return out.File, decodeCloud(b, &out, "PUT", p)
}

// CloudFileLink is a presigned URL for one download (GET) or upload (PUT)
// of one path, straight to the host. Treat the URL as a secret.
type CloudFileLink struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	ExpiresAt int64  `json:"expires_at"`
}

// Link makes a presigned URL; method is "GET" (default) or "PUT", ttlS 0
// the server's default.
func (f *CloudFiles) Link(ctx context.Context, path, method string, ttlS int) (CloudFileLink, error) {
	if err := needPath(path); err != nil {
		return CloudFileLink{}, err
	}
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)
	if method != "GET" && method != "PUT" {
		return CloudFileLink{}, usage(`a file link's method is "GET" or "PUT", not %q`, method)
	}
	body := map[string]any{"path": path, "method": method}
	if ttlS != 0 {
		body["ttl_s"] = ttlS
	}
	var r struct {
		Link CloudFileLink `json:"link"`
	}
	err := f.b.c.request(ctx, "POST", f.b.path("/files/link"), body, &r)
	return r.Link, err
}

// ---- egress ----

// CloudEgress is what a sandbox may reach.
type CloudEgress struct {
	Internet bool     `json:"internet"`
	Allow    []string `json:"allow"`   // hosts or CIDRs
	Deny     []string `json:"deny"`    // hosts or CIDRs
	Presets  []string `json:"presets"` // see Cloud.EgressPresets
	Source   string   `json:"source,omitempty"`
	Enforced string   `json:"enforced,omitempty"`
}

type egressEnvelope struct {
	Egress CloudEgress `json:"egress"`
}

func egressBody(e CloudEgress) map[string]any {
	body := map[string]any{"internet": e.Internet}
	for k, v := range map[string][]string{"allow": e.Allow, "deny": e.Deny, "presets": e.Presets} {
		if v == nil {
			v = []string{}
		}
		body[k] = v
	}
	return body
}

// Egress returns the policy the sandbox runs with.
func (b *CloudSandbox) Egress(ctx context.Context) (CloudEgress, error) {
	var r egressEnvelope
	err := b.c.request(ctx, "GET", b.path("/egress"), nil, &r)
	return r.Egress, err
}

// SetEgress changes what the sandbox may reach, at once (a sleeping one
// when it wakes). Source and Enforced are ignored.
func (b *CloudSandbox) SetEgress(ctx context.Context, e CloudEgress) (CloudEgress, error) {
	var r egressEnvelope
	err := b.c.request(ctx, "PUT", b.path("/egress"), egressBody(e), &r)
	return r.Egress, err
}

// ResetEgress puts the sandbox back on the organization's default policy.
func (b *CloudSandbox) ResetEgress(ctx context.Context) (CloudEgress, error) {
	var r egressEnvelope
	err := b.c.request(ctx, "DELETE", b.path("/egress"), nil, &r)
	return r.Egress, err
}

// EgressPresets returns the named host lists an egress policy can use.
func (c *Cloud) EgressPresets(ctx context.Context) (Info, error) {
	var r struct {
		Presets Info `json:"presets"`
	}
	err := c.request(ctx, "GET", "/v1/egress/presets", nil, &r)
	return r.Presets, err
}

// ---- secrets ----

// CloudSecret is a stored secret, without its value.
type CloudSecret struct {
	Name       string            `json:"name"`
	Upstream   string            `json:"upstream"`
	Header     string            `json:"header"`
	Scheme     string            `json:"scheme"`
	Methods    []string          `json:"methods"`
	Paths      []string          `json:"paths"`
	Env        map[string]string `json:"env"`
	Pipelines  bool              `json:"pipelines"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
	LastUsedAt *int64            `json:"last_used_at"`
}

// CloudSecretSpec is a secret to store. Sandboxes hold only a placeholder;
// the real value is added on the way out to Upstream.
type CloudSecretSpec struct {
	Value    string // required; never logged or returned
	Upstream string // the https origin it may be sent to
	Header   string // default Authorization
	Scheme   string // such as Bearer
	Methods  []string
	Paths    []string
	// Env is what the sandbox is given: variable names to values using
	// {{placeholder}} (the stand-in for the value) and {{url}} (the broker's
	// address); default NAME_API_KEY and NAME_BASE_URL.
	Env map[string]string
	// Pipelines lets Pipelines runs use the secret by name.
	Pipelines bool
}

// GoString keeps %#v from printing the value.
func (s CloudSecretSpec) GoString() string {
	return fmt.Sprintf("kosa8.CloudSecretSpec{Upstream:%q, Value:<redacted>}", s.Upstream)
}

// CloudSecrets stores the org's secrets.
type CloudSecrets struct{ c *Cloud }

func checkSecretName(name string) error {
	if !secretRE.MatchString(name) {
		return usage("secret name %q: 1..32 of lowercase letters, digits and -", name)
	}
	return nil
}

// Set stores (or replaces) a secret and reports whether it replaced one.
func (s *CloudSecrets) Set(ctx context.Context, name string, spec CloudSecretSpec) (CloudSecret, bool, error) {
	if err := checkSecretName(name); err != nil {
		return CloudSecret{}, false, err
	}
	if spec.Value == "" {
		return CloudSecret{}, false, usage("a secret needs a Value")
	}
	body := map[string]any{"value": spec.Value}
	if spec.Upstream != "" {
		body["upstream"] = spec.Upstream
	}
	if spec.Header != "" {
		body["header"] = spec.Header
	}
	if spec.Scheme != "" {
		body["scheme"] = spec.Scheme
	}
	if len(spec.Methods) > 0 {
		body["methods"] = spec.Methods
	}
	if len(spec.Paths) > 0 {
		body["paths"] = spec.Paths
	}
	if len(spec.Env) > 0 {
		body["env"] = spec.Env
	}
	if spec.Pipelines {
		body["pipelines"] = true
	}
	var r struct {
		Secret   CloudSecret `json:"secret"`
		Replaced bool        `json:"replaced"`
	}
	err := s.c.request(ctx, "PUT", "/v1/secrets/"+url.PathEscape(name), body, &r)
	return r.Secret, r.Replaced, err
}

// List returns the org's secrets, without values.
func (s *CloudSecrets) List(ctx context.Context) ([]CloudSecret, error) {
	var r struct {
		Secrets []CloudSecret `json:"secrets"`
	}
	if err := s.c.request(ctx, "GET", "/v1/secrets", nil, &r); err != nil {
		return nil, err
	}
	if r.Secrets == nil {
		r.Secrets = []CloudSecret{}
	}
	return r.Secrets, nil
}

// Delete deletes a secret and returns the sandboxes still holding it (they
// keep it until they are deleted).
func (s *CloudSecrets) Delete(ctx context.Context, name string) ([]string, error) {
	if err := checkSecretName(name); err != nil {
		return nil, err
	}
	var r struct {
		StillHeldBy []string `json:"still_held_by"`
	}
	err := s.c.request(ctx, "DELETE", "/v1/secrets/"+url.PathEscape(name), nil, &r)
	return r.StillHeldBy, err
}

// Secrets returns the names of the secrets the sandbox holds.
func (b *CloudSandbox) Secrets(ctx context.Context) ([]string, error) {
	var r struct {
		Secrets []string `json:"secrets"`
	}
	if err := b.c.request(ctx, "GET", b.path("/secrets"), nil, &r); err != nil {
		return nil, err
	}
	if r.Secrets == nil {
		r.Secrets = []string{}
	}
	return r.Secrets, nil
}

// ---- schedules ----

// CloudScheduleRun is one run of a schedule.
type CloudScheduleRun struct {
	At       string  `json:"at,omitempty"`
	Status   string  `json:"status"`
	ExitCode *int    `json:"exit"`
	Error    *string `json:"error"`
	Output   *string `json:"output"`
	Ms       *int    `json:"ms"`
}

// CloudSchedule runs a command in the sandbox on a cron schedule (UTC).
type CloudSchedule struct {
	ID        string            `json:"id"`
	SandboxID string            `json:"sandbox_id"`
	Name      *string           `json:"name"`
	Cron      string            `json:"cron"`
	Cmd       []string          `json:"cmd"`
	EnvNames  []string          `json:"env_names"`
	Wake      bool              `json:"wake"`
	NextRunAt *string           `json:"next_run_at"`
	Runs      int               `json:"runs"`
	CreatedAt string            `json:"created_at"`
	LastRun   *CloudScheduleRun `json:"last_run"`
}

// CloudScheduleSpec is a schedule to create.
type CloudScheduleSpec struct {
	Cron string   // five fields, UTC, e.g. "0 * * * *"
	Cmd  []string // argv
	Env  map[string]string
	Name string
	Wake *bool // wake a sleeping sandbox to run it (default the server's)
}

// CloudSchedules runs commands in a sandbox on a schedule.
type CloudSchedules struct{ b *CloudSandbox }

// Schedules returns the sandbox's schedule operations.
func (b *CloudSandbox) Schedules() *CloudSchedules { return &CloudSchedules{b: b} }

// Create adds a schedule.
func (s *CloudSchedules) Create(ctx context.Context, spec CloudScheduleSpec) (CloudSchedule, error) {
	if spec.Cron == "" || len(spec.Cmd) == 0 {
		return CloudSchedule{}, usage("a schedule needs Cron and Cmd")
	}
	body := map[string]any{"cron": spec.Cron, "cmd": spec.Cmd}
	if len(spec.Env) > 0 {
		body["env"] = envList(spec.Env)
	}
	if spec.Name != "" {
		body["name"] = spec.Name
	}
	if spec.Wake != nil {
		body["wake"] = *spec.Wake
	}
	var r struct {
		Schedule CloudSchedule `json:"schedule"`
	}
	err := s.b.c.request(ctx, "POST", s.b.path("/schedules"), body, &r)
	return r.Schedule, err
}

// List returns the sandbox's schedules, each with its last run.
func (s *CloudSchedules) List(ctx context.Context) ([]CloudSchedule, error) {
	var r struct {
		Schedules []CloudSchedule `json:"schedules"`
	}
	if err := s.b.c.request(ctx, "GET", s.b.path("/schedules"), nil, &r); err != nil {
		return nil, err
	}
	if r.Schedules == nil {
		r.Schedules = []CloudSchedule{}
	}
	return r.Schedules, nil
}

// Delete removes a schedule.
func (s *CloudSchedules) Delete(ctx context.Context, id string) error {
	return s.b.c.request(ctx, "DELETE", s.b.path("/schedules/"+url.PathEscape(id)), nil, nil)
}

// Run runs a schedule now and returns the run.
func (s *CloudSchedules) Run(ctx context.Context, id string) (CloudScheduleRun, error) {
	var r struct {
		Run CloudScheduleRun `json:"run"`
	}
	err := s.b.c.request(ctx, "POST", s.b.path("/schedules/"+url.PathEscape(id)+"/run"), map[string]any{}, &r)
	return r.Run, err
}

// ---- code interpreter ----

// CloudCodeOutput is one rich result of a cell: text, an image (base64 PNG
// in Data) and so on, by Type.
type CloudCodeOutput struct {
	Type string `json:"type"`
	Data any    `json:"data"`
	Name string `json:"name,omitempty"`
}

// CloudCodeError is the exception a cell raised.
type CloudCodeError struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Traceback string `json:"traceback"`
}

// CloudCodeResult is a finished cell.
type CloudCodeResult struct {
	Context        string            `json:"context"`
	ExecutionCount int               `json:"execution_count"`
	Results        []CloudCodeOutput `json:"results"`
	Stdout         string            `json:"stdout"`
	Stderr         string            `json:"stderr"`
	Error          *CloudCodeError   `json:"error"`
	Truncated      bool              `json:"truncated"`
	WokeMs         *int              `json:"woke_ms"`
}

// CloudCodeOptions pick the context and the time limit of a cell.
type CloudCodeOptions struct {
	Context  string  // "" the default context
	TimeoutS float64 // 0 the server's default
}

// RunCode runs a Python cell in a stateful context: globals persist
// between calls and charts come back as images. A cell that raised is
// Error != nil, not a Go error.
func (b *CloudSandbox) RunCode(ctx context.Context, code string, o *CloudCodeOptions) (CloudCodeResult, error) {
	if code == "" {
		return CloudCodeResult{}, usage("RunCode needs code")
	}
	body := map[string]any{"code": code, "language": "python"}
	if o != nil {
		if o.Context != "" {
			body["context"] = o.Context
		}
		if o.TimeoutS != 0 {
			body["timeout_s"] = o.TimeoutS
		}
	}
	var r CloudCodeResult
	err := b.c.request(ctx, "POST", b.path("/code"), body, &r)
	return r, err
}

// CloudCodeContext is a code interpreter context.
type CloudCodeContext struct {
	Name           string `json:"name"`
	ExecutionCount int    `json:"execution_count"`
}

// CodeContexts lists the sandbox's code contexts.
func (b *CloudSandbox) CodeContexts(ctx context.Context) ([]CloudCodeContext, error) {
	var r struct {
		Contexts []CloudCodeContext `json:"contexts"`
	}
	if err := b.c.request(ctx, "GET", b.path("/code/contexts"), nil, &r); err != nil {
		return nil, err
	}
	if r.Contexts == nil {
		r.Contexts = []CloudCodeContext{}
	}
	return r.Contexts, nil
}

// CreateCodeContext makes a new context; name "" lets the server pick one.
func (b *CloudSandbox) CreateCodeContext(ctx context.Context, name string) (CloudCodeContext, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	var r struct {
		Context CloudCodeContext `json:"context"`
	}
	err := b.c.request(ctx, "POST", b.path("/code/contexts"), body, &r)
	return r.Context, err
}

// ResetCodeContext clears a context's globals.
func (b *CloudSandbox) ResetCodeContext(ctx context.Context, name string) error {
	return b.c.request(ctx, "POST", b.path("/code/contexts/"+url.PathEscape(name)+"/reset"), map[string]any{}, nil)
}

// DeleteCodeContext deletes a context.
func (b *CloudSandbox) DeleteCodeContext(ctx context.Context, name string) error {
	return b.c.request(ctx, "DELETE", b.path("/code/contexts/"+url.PathEscape(name)), nil, nil)
}

// ---- metrics, logs, labels ----

// CloudNet is network use since boot.
type CloudNet struct {
	OutFrames int64 `json:"out_frames"`
	OutBytes  int64 `json:"out_bytes"`
	InFrames  int64 `json:"in_frames"`
	InBytes   int64 `json:"in_bytes"`
}

// CloudMetrics are a sandbox's CPU, memory, disk and network use, read on
// the host.
type CloudMetrics struct {
	ID           string   `json:"id"`
	State        string   `json:"state"`
	CPUs         int      `json:"cpus"`
	MemMB        int      `json:"mem_mb"`
	DiskGB       int      `json:"disk_gb"`
	CPUPct       *float64 `json:"cpu_pct"`
	CPUSeconds   *float64 `json:"cpu_seconds"`
	MemBytes     *int64   `json:"mem_bytes"`
	MemPct       *float64 `json:"mem_pct"`
	DiskBytes    int64    `json:"disk_bytes"`
	Net          CloudNet `json:"net"`
	EgressDenied int64    `json:"egress_denied"`
	SleepBytes   int64    `json:"sleep_bytes"`
	At           string   `json:"at"`
}

// Metrics reads the sandbox's resource use.
func (b *CloudSandbox) Metrics(ctx context.Context) (CloudMetrics, error) {
	var r struct {
		Metrics CloudMetrics `json:"metrics"`
	}
	err := b.c.request(ctx, "GET", b.path("/metrics"), nil, &r)
	return r.Metrics, err
}

// CloudLogEntry is one command the sandbox ran.
type CloudLogEntry struct {
	At         string   `json:"at"`
	Cmd        []string `json:"cmd"`
	ExitCode   *int     `json:"exit"`
	Ms         int      `json:"ms"`
	Stream     bool     `json:"stream"`
	Direct     bool     `json:"direct"`
	StdoutTail string   `json:"stdout_tail"`
	StderrTail string   `json:"stderr_tail"`
	Error      string   `json:"error"`
	OOM        bool     `json:"oom"`
}

// Logs returns the sandbox's last commands (limit 0 the server's default),
// with exit codes and output tails.
func (b *CloudSandbox) Logs(ctx context.Context, limit int) ([]CloudLogEntry, error) {
	p := b.path("/logs")
	if limit > 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var r struct {
		Entries []CloudLogEntry `json:"entries"`
	}
	if err := b.c.request(ctx, "GET", p, nil, &r); err != nil {
		return nil, err
	}
	if r.Entries == nil {
		r.Entries = []CloudLogEntry{}
	}
	return r.Entries, nil
}

// SetLabels replaces the sandbox's labels (nil or empty clears them) and
// refreshes Info.Labels.
func (b *CloudSandbox) SetLabels(ctx context.Context, labels map[string]string) (map[string]string, error) {
	if err := checkLabels(labels); err != nil {
		return nil, err
	}
	var body any = map[string]any{"labels": labels}
	if len(labels) == 0 {
		body = map[string]any{"labels": nil}
	}
	var r struct {
		Labels map[string]string `json:"labels"`
	}
	if err := b.c.request(ctx, "PUT", b.path("/labels"), body, &r); err != nil {
		return nil, err
	}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	b.Info.Labels = r.Labels
	return r.Labels, nil
}
