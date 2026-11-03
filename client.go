// Package kosa8 is the Go SDK for kosa8: microVM sandboxes for AI agents.
//
// It talks to the local kosa8 daemon over its unix socket
// (~/.kosa8/run/kosa8.sock), or to a remote worker via KOSA8_HOST and
// KOSA8_TOKEN, using only the standard library. Method for method it matches
// @kosa8/sdk (TypeScript) and kosa8 (Python).
//
//	k, _ := kosa8.New(kosa8.Options{})
//	sb, _ := k.CreateSandbox(ctx, kosa8.CreateOptions{Name: "demo", Egress: "deny"})
//	_, _ = k.PullIntoSandbox(ctx, "demo", "alpine:latest")
//	res, _ := k.Run(ctx, "demo", "alpine:latest", []string{"echo", "hi"}, nil)
//	fmt.Print(res.Stdout())
package kosa8

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Version of this SDK.
const Version = "0.1.2"

// Options configures a Client. The zero value talks to the local daemon.
type Options struct {
	SocketPath string // default ~/.kosa8/run/kosa8.sock
	Host       string // a remote worker; default $KOSA8_HOST
	Token      string // bearer token for Host; default $KOSA8_TOKEN when Host came from the env
	Insecure   bool   // allow plain http to a non-loopback worker
	Actor      string // human | agent | tool (default tool)
	AgentToken string // default $KOSA8_AGENT_TOKEN
	Headers    map[string]string
	HTTPClient *http.Client // overrides the transport entirely
	// Checkpoints is where checkpoint stacks live. Default: the CLI's own
	// ~/.kosa8/checkpoints.json, so `kosa8 rewind` and this SDK share them.
	Checkpoints CheckpointStore
	Getenv      func(string) string // for tests; default os.Getenv
}

// Info is a JSON object the daemon returned (a sandbox, snapshot, container…).
type Info = map[string]any

// Client is a typed client for the kosa8 native API (/kosa8/v1).
type Client struct {
	t     *transport
	ckpts checkpointBook

	stdinMu   sync.Mutex
	stdinSent map[[2]string]bool
}

// New returns a Client.
func New(o Options) (*Client, error) {
	t, err := newTransport(o)
	if err != nil {
		return nil, err
	}
	store := o.Checkpoints
	if store == nil {
		store = NewFileCheckpointStore("")
	}
	return &Client{t: t, ckpts: checkpointBook{store}, stdinSent: map[[2]string]bool{}}, nil
}

// Endpoint reports where this client sends requests.
func (c *Client) Endpoint() Endpoint { return c.t.ep }

func enc(s string) string { return url.PathEscape(s) }

func sbPath(id string, rest ...string) string {
	p := "/kosa8/v1/sandboxes/" + enc(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// Call is per-request options: a signed approval for a policy that asks for one.
type Call struct{ Approval string }

func approval(c []Call) string {
	if len(c) > 0 {
		return c[0].Approval
	}
	return ""
}

// --- daemon -------------------------------------------------------------------

// Health is GET /kosa8/v1/health: daemon status and version.
func (c *Client) Health(ctx context.Context) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "GET", path: "/kosa8/v1/health"}, &out)
}

// Version is the daemon's version.
func (c *Client) Version(ctx context.Context) (string, error) {
	h, err := c.Health(ctx)
	if err != nil {
		return "", err
	}
	v, _ := h["version"].(string)
	return v, nil
}

// Pull is POST /kosa8/v1/pull: pull an image into the host engine.
func (c *Client) Pull(ctx context.Context, ref string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: "/kosa8/v1/pull", body: map[string]any{"ref": ref}, approval: approval(call)}, &out)
}

// --- sandboxes ----------------------------------------------------------------

// ListSandboxes is GET /kosa8/v1/sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]Info, error) {
	var out []Info
	return out, c.t.json(ctx, request{method: "GET", path: "/kosa8/v1/sandboxes"}, &out)
}

// GetSandbox finds a sandbox by id, then by name; nil when there is none.
func (c *Client) GetSandbox(ctx context.Context, idOrName string) (Info, error) {
	l, err := c.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"id", "name"} {
		for _, s := range l {
			if v, _ := s[key].(string); v == idOrName {
				return s, nil
			}
		}
	}
	return nil, nil
}

// CreateOptions describes a new sandbox. Zero fields take the daemon's defaults.
type CreateOptions struct {
	Name   string
	CPUs   int
	MemMB  int
	DiskGB int
	Mounts []string // host:guest[:ro]; host must be absolute
	Egress string   // deny | any | fakenet | allow:host,host
	// EgressAllow limits the network to these hosts (sets Egress to allow:…).
	EgressAllow []string
	// Capture records the sandbox's network traffic as pcap segments under
	// Capture.Path, a file on the host (pcapng). nil records nothing.
	Capture  *CaptureOptions
	Labels   map[string]string
	Broker   any // the credential-broker config, passed through
	Approval string
}

// CaptureOptions turn on packet capture for a sandbox (daemon 0.9.36 writes one pcapng file at Path).
type CaptureOptions struct {
	Path     string // host file the capture is written to (required)
	MaxBytes int64  // per segment; 0 is the daemon's default
	MaxFiles int    // segments kept; 0 is the daemon's default
}

// mountBody turns "host:guest[:ro]" into the objects the daemon takes.
func mountBody(ms []string) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		parts := strings.Split(m, ":")
		ro := false
		if len(parts) == 3 && parts[2] == "ro" {
			ro, parts = true, parts[:2]
		} else if len(parts) == 3 && parts[2] == "rw" {
			parts = parts[:2]
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, usage("mount %q: want host:guest or host:guest:ro", m)
		}
		if !filepath.IsAbs(parts[0]) || !strings.HasPrefix(parts[1], "/") {
			return nil, usage("mount %q: both paths must be absolute (use filepath.Abs for the host side)", m)
		}
		out = append(out, map[string]any{"host_path": parts[0], "guest_path": parts[1], "read_only": ro})
	}
	return out, nil
}

// CreateSandbox is POST /kosa8/v1/sandboxes: boot a microVM sandbox.
func (c *Client) CreateSandbox(ctx context.Context, o CreateOptions) (Info, error) {
	body := map[string]any{}
	if o.Name != "" {
		body["name"] = o.Name
	}
	if o.CPUs > 0 {
		body["cpus"] = o.CPUs
	}
	if o.MemMB > 0 {
		body["mem_mb"] = o.MemMB
	}
	if o.DiskGB > 0 {
		body["disk_gb"] = o.DiskGB
	}
	if len(o.Mounts) > 0 {
		mb, err := mountBody(o.Mounts)
		if err != nil {
			return nil, err
		}
		body["mounts"] = mb
	}
	switch {
	case len(o.EgressAllow) > 0:
		if o.Egress != "" && o.Egress != "deny" {
			return nil, usage("EgressAllow already limits the network to the hosts it lists; do not also set Egress")
		}
		hosts := make([]string, len(o.EgressAllow))
		for i, h := range o.EgressAllow {
			hosts[i] = strings.TrimSpace(h)
		}
		body["egress"] = "allow:" + strings.Join(hosts, ",")
	case o.Egress != "":
		body["egress"] = o.Egress
	}
	if o.Capture != nil {
		if o.Capture.Path == "" {
			return nil, usage("Capture.Path is required: the host file the capture is written to")
		}
		cb := map[string]any{"path": o.Capture.Path}
		if o.Capture.MaxBytes > 0 {
			cb["max_bytes"] = o.Capture.MaxBytes
		}
		if o.Capture.MaxFiles > 0 {
			cb["max_files"] = o.Capture.MaxFiles
		}
		body["capture"] = cb
	}
	if len(o.Labels) > 0 {
		body["labels"] = o.Labels
	}
	if o.Broker != nil {
		body["broker"] = o.Broker
	}
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: "/kosa8/v1/sandboxes", body: body, approval: o.Approval}, &out)
}

// StartSandbox is POST …/up: boot a stopped sandbox from its own disk.
func (c *Client) StartSandbox(ctx context.Context, sandbox string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "up"), approval: approval(call)}, &out)
}

// StopSandbox is POST …/down: stop without deleting (the disk is kept).
func (c *Client) StopSandbox(ctx context.Context, sandbox string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "down"), approval: approval(call)}, &out)
}

// DeleteOptions for DeleteSandbox.
type DeleteOptions struct {
	Wipe            bool // overwrite the disks, not just unlink them
	KeepCheckpoints bool // keep this sandbox's checkpoint snapshots
	Approval        string
}

// DeleteSandbox is DELETE /kosa8/v1/sandboxes/{id}. Its checkpoints (and their
// snapshots) go too unless KeepCheckpoints; the answer carries checkpoints_dropped.
func (c *Client) DeleteSandbox(ctx context.Context, sandbox string, o DeleteOptions) (Info, error) {
	q := url.Values{}
	if o.Wipe {
		q.Set("wipe", "1")
	}
	out := Info{}
	if err := c.t.json(ctx, request{method: "DELETE", path: sbPath(sandbox), query: q, approval: o.Approval}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = Info{}
	}
	dropped := 0
	if !o.KeepCheckpoints {
		if ids, err := c.ckpts.forget(sandbox); err == nil { // bookkeeping debris is not a failed delete
			for _, id := range ids {
				_, _ = c.DeleteSnapshot(ctx, id, SnapshotDeleteOptions{Wipe: o.Wipe, Approval: o.Approval})
			}
			dropped = len(ids)
		}
	}
	out["checkpoints_dropped"] = dropped
	return out, nil
}

// PullIntoSandbox is POST …/pull: pull an image into the sandbox's private engine.
func (c *Client) PullIntoSandbox(ctx context.Context, sandbox, ref string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "pull"), body: map[string]any{"ref": ref}, approval: approval(call)}, &out)
}

// SandboxImages is GET …/images.
func (c *Client) SandboxImages(ctx context.Context, sandbox string) ([]string, error) {
	var out []string
	return out, c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "images")}, &out)
}

// SandboxContainers is GET …/containers.
func (c *Client) SandboxContainers(ctx context.Context, sandbox string) ([]Info, error) {
	var out []Info
	return out, c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "containers")}, &out)
}

// SandboxEgress is GET …/egress: the policy the host enforces and what it decided.
func (c *Client) SandboxEgress(ctx context.Context, sandbox string) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "egress")}, &out)
}

// --- processes ----------------------------------------------------------------

// ExecResult is the outcome of a run, exec or attach.
type ExecResult struct {
	ExitCode    int
	StdoutBytes []byte
	StderrBytes []byte
}

func (r ExecResult) Stdout() string { return string(r.StdoutBytes) }
func (r ExecResult) Stderr() string { return string(r.StderrBytes) }
func (r ExecResult) OK() bool       { return r.ExitCode == 0 }

// RunOptions for Run, Exec and Attach. Callbacks see output as it arrives.
type RunOptions struct {
	Env      []string // KEY=VALUE
	OnStdout func([]byte)
	OnStderr func([]byte)
	OnStatus func([]byte) // pull/progress lines
	Timeout  time.Duration
	Approval string
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

func ro(o *RunOptions) RunOptions {
	if o == nil {
		return RunOptions{}
	}
	return *o
}

// Run is POST …/run: run a container to completion in the sandbox's engine.
// The image must already be in the sandbox (PullIntoSandbox).
func (c *Client) Run(ctx context.Context, sandbox, image string, args []string, opts *RunOptions) (ExecResult, error) {
	o := ro(opts)
	body := map[string]any{"image": image, "args": nonNil(args)}
	if o.Env != nil {
		body["env"] = o.Env
	}
	ctx, cancel := withTimeout(ctx, o.Timeout)
	defer cancel()
	return c.collect(ctx, request{method: "POST", path: sbPath(sandbox, "run"), body: body, approval: o.Approval}, o)
}

// StartContainer is POST …/start: start a detached container; returns its id.
func (c *Client) StartContainer(ctx context.Context, sandbox, image string, args []string, call ...Call) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "start"), body: map[string]any{"image": image, "args": nonNil(args)}, approval: approval(call)}, &out)
	return out.ID, err
}

// Exec is POST …/exec: run a command in a running container of the sandbox.
func (c *Client) Exec(ctx context.Context, sandbox, container string, args []string, opts *RunOptions) (ExecResult, error) {
	o := ro(opts)
	if len(o.Env) > 0 {
		// The daemon's exec takes no environment; dropping it silently would
		// run the command without the variables the caller asked for.
		return ExecResult{}, usage("Exec does not take Env: the container keeps the environment it was started with. " +
			"Set it at StartInteractive (InteractiveOptions.Env), or wrap the command: []string{\"env\", \"K=V\", \"cmd\"}")
	}
	ctx, cancel := withTimeout(ctx, o.Timeout)
	defer cancel()
	return c.collect(ctx, c.execReq(sandbox, container, args, o.Approval), o)
}

func (c *Client) execReq(sandbox, container string, args []string, appr string) request {
	return request{method: "POST", path: sbPath(sandbox, "exec"), body: map[string]any{"container": container, "args": nonNil(args)}, approval: appr}
}

// ExecFrames is the raw frame stream of an exec, for callers that process
// output as it arrives. Return an error from fn to stop.
//
// A final frame that carries an error is passed to fn and then returned as a
// KindStream error, as Exec would return it.
func (c *Client) ExecFrames(ctx context.Context, sandbox, container string, args []string, fn func(Frame) error, call ...Call) error {
	r := c.execReq(sandbox, container, args, approval(call))
	var final error
	err := c.t.frames(ctx, r, func(f Frame) error {
		if f.Done && f.Error != "" {
			final = &Error{Kind: KindStream, Message: f.Error, DaemonMessage: f.Error, Method: r.method, Path: r.path}
		}
		return fn(f)
	})
	if err != nil {
		return err
	}
	return final
}

// InteractiveOptions for StartInteractive.
type InteractiveOptions struct {
	Env      []string
	TTY      bool
	Approval string
}

// StartInteractive is POST …/start-interactive: start a container with stdin held open.
func (c *Client) StartInteractive(ctx context.Context, sandbox, image string, args []string, o InteractiveOptions) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "start-interactive"),
		body: map[string]any{"image": image, "args": nonNil(args), "env": o.Env, "tty": o.TTY}, approval: o.Approval}, &out)
	return out.ID, err
}

// WriteStdin is POST …/containers/{cid}/stdin: stream r into the container's stdin.
//
// r is the container's whole stdin: the daemon closes stdin when r is
// exhausted, so call this once per container and pass a reader that yields
// over time (an io.Pipe) to feed input as you go. A second call returns a
// KindUsage error instead of reporting success for input the process would
// never see.
func (c *Client) WriteStdin(ctx context.Context, sandbox, container string, r io.Reader, call ...Call) (Info, error) {
	key := [2]string{sandbox, container}
	c.stdinMu.Lock()
	if c.stdinSent[key] {
		c.stdinMu.Unlock()
		return nil, usage("stdin of container %s in %s was already sent and is closed: the daemon treats one WriteStdin "+
			"body as the whole of stdin. Pass an io.Pipe reader to stream input", container, sandbox)
	}
	c.stdinSent[key] = true
	c.stdinMu.Unlock()
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "containers", enc(container), "stdin"), rawBody: r, approval: approval(call)}, &out)
}

// Attach is POST …/containers/{cid}/attach: follow a container's output until it exits.
func (c *Client) Attach(ctx context.Context, sandbox, container string, opts *RunOptions) (ExecResult, error) {
	o := ro(opts)
	ctx, cancel := withTimeout(ctx, o.Timeout)
	defer cancel()
	return c.collect(ctx, request{method: "POST", path: sbPath(sandbox, "containers", enc(container), "attach"), approval: o.Approval}, o)
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

var errDone = fmt.Errorf("done")

func (c *Client) collect(ctx context.Context, r request, o RunOptions) (ExecResult, error) {
	var out, errb []byte
	var res ExecResult
	var final error
	done := false
	err := c.t.frames(ctx, r, func(f Frame) error {
		if f.Done {
			done = true
			if f.Error != "" {
				final = &Error{Kind: KindStream, Message: f.Error, DaemonMessage: f.Error, Stdout: string(out), Stderr: string(errb), Method: r.method, Path: r.path}
			} else {
				res = ExecResult{StdoutBytes: out, StderrBytes: errb}
				if f.Exit != nil {
					res.ExitCode = *f.Exit
				}
			}
			return errDone
		}
		if len(f.Data) == 0 {
			return nil
		}
		switch f.Stream {
		case "stderr":
			errb = append(errb, f.Data...)
			if o.OnStderr != nil {
				o.OnStderr(f.Data)
			}
		case "status":
			if o.OnStatus != nil {
				o.OnStatus(f.Data)
			}
		default:
			out = append(out, f.Data...)
			if o.OnStdout != nil {
				o.OnStdout(f.Data)
			}
		}
		return nil
	})
	if err != nil && err != errDone {
		if e, ok := AsError(err); ok && (e.Kind == KindStream || e.Kind == KindTimeout) && e.Stdout == "" && e.Stderr == "" {
			e.Stdout, e.Stderr = string(out), string(errb)
		}
		return ExecResult{StdoutBytes: out, StderrBytes: errb}, err
	}
	if final != nil {
		return ExecResult{StdoutBytes: out, StderrBytes: errb}, final
	}
	if !done {
		return ExecResult{StdoutBytes: out, StderrBytes: errb}, &Error{Kind: KindStream, Message: "stream ended unexpectedly (no final frame)", Stdout: string(out), Stderr: string(errb), Method: r.method, Path: r.path}
	}
	return res, nil
}

// --- snapshots and forks ------------------------------------------------------

// SnapshotOptions for Snapshot.
type SnapshotOptions struct {
	ID       string // default: the daemon picks one
	Encrypt  bool
	Approval string
}

// Snapshot is POST …/snapshot: capture memory, processes and disks.
func (c *Client) Snapshot(ctx context.Context, sandbox string, o SnapshotOptions) (Info, error) {
	body := map[string]any{}
	if o.ID != "" {
		body["id"] = o.ID
	}
	if o.Encrypt {
		body["encrypt"] = true
	}
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "snapshot"), body: body, approval: o.Approval}, &out)
}

// ListSnapshots is GET /kosa8/v1/snapshots.
func (c *Client) ListSnapshots(ctx context.Context) ([]Info, error) {
	var out []Info
	return out, c.t.json(ctx, request{method: "GET", path: "/kosa8/v1/snapshots"}, &out)
}

// RestoreOptions for RestoreSnapshot.
type RestoreOptions struct {
	Name          string
	Label         string
	AllowTampered bool
	Approval      string
}

// RestoreSnapshot is POST /kosa8/v1/snapshots/{id}/restore: boot a sandbox from
// a snapshot with its processes resumed. The answer has sandbox and restore_ms.
func (c *Client) RestoreSnapshot(ctx context.Context, snapshotID string, o RestoreOptions) (Info, error) {
	body := map[string]any{}
	if o.Name != "" {
		body["name"] = o.Name
	}
	if o.Label != "" {
		body["label"] = o.Label
	}
	if o.AllowTampered {
		body["allow_tampered"] = true
	}
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: "/kosa8/v1/snapshots/" + enc(snapshotID) + "/restore", body: body, approval: o.Approval}, &out)
}

// SnapshotDeleteOptions for DeleteSnapshot.
type SnapshotDeleteOptions struct {
	Wipe, Force bool
	Approval    string
}

// DeleteSnapshot is DELETE /kosa8/v1/snapshots/{id}.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshotID string, o SnapshotDeleteOptions) (Info, error) {
	q := url.Values{}
	if o.Wipe {
		q.Set("wipe", "1")
	}
	if o.Force {
		q.Set("force", "1")
	}
	var out Info
	return out, c.t.json(ctx, request{method: "DELETE", path: "/kosa8/v1/snapshots/" + enc(snapshotID), query: q, approval: o.Approval}, &out)
}

// ForkOptions for Fork.
type ForkOptions struct {
	Prefix   string
	Label    string
	Approval string
}

// ForkResult is the answer to a fork.
type ForkResult struct {
	Forks    []Info `json:"forks"`
	Snapshot any    `json:"snapshot"`
	ForkMS   int64  `json:"fork_ms"`
	Count    int    `json:"count"`
}

// Fork is POST …/fork: n live copies of a running sandbox.
func (c *Client) Fork(ctx context.Context, sandbox string, n int, o ForkOptions) (ForkResult, error) {
	if n < 1 {
		n = 1
	}
	body := map[string]any{"n": n}
	if o.Prefix != "" {
		body["prefix"] = o.Prefix
	}
	if o.Label != "" {
		body["label"] = o.Label
	}
	var out ForkResult
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "fork"), body: body, approval: o.Approval}, &out)
}

// --- explore: best-of-N -------------------------------------------------------

// ExploreOptions for Explore.
type ExploreOptions struct {
	Image    string        // default alpine:latest
	KeepAll  bool          // keep every branch
	Keep     string        // keep this branch instead of the fastest success
	Timeout  time.Duration // per branch; default 120s
	Prefix   string
	NoPull   bool // the image is already in the sandbox
	Approval string
}

// ExploreBranch is one variant's outcome.
type ExploreBranch struct {
	Name       string
	Variant    string
	Exit       int
	Output     string
	Error      string // set when the branch could not run
	DurationMS int64
}

// ExploreResult is what Explore kept and dropped.
type ExploreResult struct {
	Parent   string
	Branches []ExploreBranch
	Winner   string // "" when no branch succeeded
	Removed  int
}

var transient = regexp.MustCompile(`connection refused|EOF|connection reset|broken pipe|no such file or directory|stream ended|ECONNREFUSED|ECONNRESET|unreachable`)

// Explore is best-of-N, as `kosa8 explore`: it forks the sandbox once per
// variant, runs each (sh -c VARIANT in Image) concurrently in its own branch,
// keeps the fastest successful branch and deletes the rest.
func (c *Client) Explore(ctx context.Context, sandbox string, variants []string, o ExploreOptions) (ExploreResult, error) {
	if len(variants) < 2 {
		return ExploreResult{}, usage("give at least two variants; with one there is nothing to compare")
	}
	if o.Image == "" {
		o.Image = "alpine:latest"
	}
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	if !o.NoPull {
		if _, err := c.PullIntoSandbox(ctx, sandbox, o.Image, Call{o.Approval}); err != nil {
			return ExploreResult{}, err
		}
	}
	if o.Prefix == "" {
		o.Prefix = fmt.Sprintf("%s-explore-%d", sandbox, time.Now().Unix()%100000)
	}
	forked, err := c.Fork(ctx, sandbox, len(variants), ForkOptions{Prefix: o.Prefix, Approval: o.Approval})
	if err != nil {
		return ExploreResult{}, err
	}
	var names []string
	for _, f := range forked.Forks {
		if id, ok := f["id"].(string); ok {
			names = append(names, id)
		}
	}
	if len(names) < len(variants) {
		return ExploreResult{}, usage("forked %d branches for %d variants; see ListSandboxes for what was created", len(names), len(variants))
	}
	branches := make([]ExploreBranch, len(variants))
	var wg sync.WaitGroup
	for i := range variants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branches[i] = c.runBranch(ctx, names[i], o.Image, variants[i], o.Timeout, o.Approval)
		}(i)
	}
	wg.Wait()
	res := ExploreResult{Parent: sandbox, Branches: branches}
	var best int64 = -1
	for _, b := range branches {
		if b.Error == "" && b.Exit == 0 && (best < 0 || b.DurationMS < best) {
			res.Winner, best = b.Name, b.DurationMS
		}
	}
	if o.Keep != "" {
		found := false
		for _, n := range names {
			found = found || n == o.Keep
		}
		if !found {
			return res, usage("keep %s is not one of the branches (%s); all of them were kept", o.Keep, strings.Join(names, ", "))
		}
		res.Winner = o.Keep
	}
	if !o.KeepAll {
		for _, b := range branches {
			if b.Name != res.Winner {
				if c.t.json(ctx, request{method: "DELETE", path: sbPath(b.Name), approval: o.Approval}, nil) == nil {
					res.Removed++
				}
			}
		}
	}
	return res, nil
}

func (c *Client) runBranch(ctx context.Context, name, image, variant string, timeout time.Duration, appr string) ExploreBranch {
	start := time.Now()
	b := ExploreBranch{Name: name, Variant: variant}
	// A freshly restored branch can refuse the first connection while its
	// engine settles; retry connection-level failures briefly, as the CLI does.
	for attempt := 0; attempt < 4; attempt++ {
		res, err := c.Run(ctx, name, image, []string{"sh", "-c", variant}, &RunOptions{Timeout: timeout, Approval: appr})
		if err == nil {
			b.Exit, b.Error = res.ExitCode, ""
			b.Output = strings.TrimRight(res.Stdout()+res.Stderr(), "\n")
			break
		}
		b.Error = err.Error()
		e, _ := AsError(err)
		if e != nil && e.Kind == KindStream {
			b.Output = strings.TrimRight(e.Stdout+e.Stderr, "\n")
		}
		if (e != nil && e.Kind == KindTimeout) || ctx.Err() != nil || !transient.MatchString(b.Error) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	b.DurationMS = time.Since(start).Milliseconds()
	return b
}

// --- checkpoints: client-side over snapshots, as pkg/checkpoint ---------------

// CheckpointResult is what Checkpoint saved.
type CheckpointResult struct {
	Checkpoint Checkpoint
	Evicted    []string
	Snapshot   Info
}

// Checkpoint saves a restore point: a snapshot <sandbox>-ckpt-<n> on the
// sandbox's stack, in the same store `kosa8 checkpoint` uses. Past 10 the
// oldest is evicted, once its replacement exists.
func (c *Client) Checkpoint(ctx context.Context, sandbox, label string, call ...Call) (CheckpointResult, error) {
	id, evict, err := c.ckpts.nextID(sandbox)
	if err != nil {
		return CheckpointResult{}, err
	}
	// Two checkpoints taken at once (here or by the CLI) can pick the same
	// next id; the loser's snapshot is refused with 409. The winner records
	// its id only once its snapshot is done, so step past the taken id
	// rather than re-reading the store.
	var snap Info
	for attempt := 0; ; attempt++ {
		snap, err = c.Snapshot(ctx, sandbox, SnapshotOptions{ID: id, Approval: approval(call)})
		if err == nil {
			break
		}
		var ke *Error
		if attempt == 9 || !errors.As(err, &ke) || ke.Kind != KindConflict ||
			!strings.Contains(ke.DaemonMessage+ke.Message, "already exists") {
			return CheckpointResult{}, err
		}
		id = fmt.Sprintf("%s-ckpt-%d", sandbox, seqOf(id, sandbox)+1)
	}
	for _, old := range evict {
		_, _ = c.DeleteSnapshot(ctx, old, SnapshotDeleteOptions{Approval: approval(call)})
	}
	rec, err := c.ckpts.record(Checkpoint{ID: id, Sandbox: sandbox, Label: label}, evict)
	if err != nil {
		return CheckpointResult{}, err
	}
	return CheckpointResult{Checkpoint: rec, Evicted: evict, Snapshot: snap}, nil
}

// ListCheckpoints is a sandbox's restore points, oldest first.
func (c *Client) ListCheckpoints(sandbox string) ([]Checkpoint, error) { return c.ckpts.list(sandbox) }

// RewindResult is what Rewind restored.
type RewindResult struct {
	Sandbox    any
	Checkpoint Checkpoint
	Dropped    []string
	RestoreMS  any
}

// Rewind undoes: it puts the sandbox back to a checkpoint (to: an id or label;
// "" means the latest) under the same name, processes resumed. Checkpoints
// taken after the target are discarded. The current state is lost.
func (c *Client) Rewind(ctx context.Context, sandbox, to string, call ...Call) (RewindResult, error) {
	var target Checkpoint
	var err error
	if to != "" {
		target, err = c.ckpts.resolve(sandbox, to)
	} else {
		target, err = c.ckpts.latest(sandbox)
	}
	if err != nil {
		return RewindResult{}, err
	}
	// The sandbox must go first: the restored VM takes its name. A refusal
	// here leaves everything as it was, and is returned as is.
	if err := c.t.json(ctx, request{method: "DELETE", path: sbPath(sandbox), approval: approval(call)}, nil); err != nil {
		return RewindResult{}, err
	}
	restored, err := c.RestoreSnapshot(ctx, target.ID, RestoreOptions{Name: sandbox, Approval: approval(call)})
	if err != nil {
		return RewindResult{}, &Error{Kind: KindCheckpoint, Err: err, Message: fmt.Sprintf(
			"restore failed and %s no longer exists (it was deleted to free its name for the restore): %v — the checkpoint is intact; "+
				"recover with RestoreSnapshot(%q, RestoreOptions{Name: %q})", sandbox, err, target.ID, sandbox)}
	}
	dropped, err := c.ckpts.pop(sandbox, target.ID)
	if err == nil {
		for _, id := range dropped {
			_, _ = c.DeleteSnapshot(ctx, id, SnapshotDeleteOptions{Approval: approval(call)})
		}
	}
	return RewindResult{Sandbox: restored["sandbox"], Checkpoint: target, Dropped: dropped, RestoreMS: restored["restore_ms"]}, nil
}

// --- bound handle -------------------------------------------------------------

// Sandbox is the client's sandbox methods bound to one sandbox id.
type Sandbox struct {
	c  *Client
	ID string
}

// Sandbox returns a handle bound to one sandbox.
func (c *Client) Sandbox(id string) *Sandbox { return &Sandbox{c: c, ID: id} }

func (s *Sandbox) Info(ctx context.Context) (Info, error) { return s.c.GetSandbox(ctx, s.ID) }
func (s *Sandbox) Start(ctx context.Context, call ...Call) (Info, error) {
	return s.c.StartSandbox(ctx, s.ID, call...)
}
func (s *Sandbox) Stop(ctx context.Context, call ...Call) (Info, error) {
	return s.c.StopSandbox(ctx, s.ID, call...)
}
func (s *Sandbox) Delete(ctx context.Context, o DeleteOptions) (Info, error) {
	return s.c.DeleteSandbox(ctx, s.ID, o)
}
func (s *Sandbox) Pull(ctx context.Context, ref string, call ...Call) (Info, error) {
	return s.c.PullIntoSandbox(ctx, s.ID, ref, call...)
}
func (s *Sandbox) Run(ctx context.Context, image string, args []string, o *RunOptions) (ExecResult, error) {
	return s.c.Run(ctx, s.ID, image, args, o)
}
func (s *Sandbox) StartContainer(ctx context.Context, image string, args []string, call ...Call) (string, error) {
	return s.c.StartContainer(ctx, s.ID, image, args, call...)
}
func (s *Sandbox) Exec(ctx context.Context, container string, args []string, o *RunOptions) (ExecResult, error) {
	return s.c.Exec(ctx, s.ID, container, args, o)
}
func (s *Sandbox) Containers(ctx context.Context) ([]Info, error) {
	return s.c.SandboxContainers(ctx, s.ID)
}
func (s *Sandbox) Images(ctx context.Context) ([]string, error) { return s.c.SandboxImages(ctx, s.ID) }
func (s *Sandbox) Egress(ctx context.Context) (Info, error)     { return s.c.SandboxEgress(ctx, s.ID) }
func (s *Sandbox) Snapshot(ctx context.Context, o SnapshotOptions) (Info, error) {
	return s.c.Snapshot(ctx, s.ID, o)
}
func (s *Sandbox) Fork(ctx context.Context, n int, o ForkOptions) (ForkResult, error) {
	return s.c.Fork(ctx, s.ID, n, o)
}
func (s *Sandbox) Explore(ctx context.Context, variants []string, o ExploreOptions) (ExploreResult, error) {
	return s.c.Explore(ctx, s.ID, variants, o)
}
func (s *Sandbox) Checkpoint(ctx context.Context, label string, call ...Call) (CheckpointResult, error) {
	return s.c.Checkpoint(ctx, s.ID, label, call...)
}
func (s *Sandbox) Checkpoints() ([]Checkpoint, error) { return s.c.ListCheckpoints(s.ID) }
func (s *Sandbox) Rewind(ctx context.Context, to string, call ...Call) (RewindResult, error) {
	return s.c.Rewind(ctx, s.ID, to, call...)
}
