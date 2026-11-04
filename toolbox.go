package kosa8

// The sandbox toolbox: run code, files and git, shell sessions, terminals,
// previews, lifecycle and templates. Every method maps to one daemon route
// (see https://docs.kosa8.com/guides/native-api/), as kosa8 (Python) does.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Ptr returns a pointer to v, for the optional fields of LifecycleOptions:
// kosa8.Ptr(30), kosa8.Ptr(true).
func Ptr[T any](v T) *T { return &v }

func secs(d time.Duration) float64 { return d.Seconds() }

// --- code ---------------------------------------------------------------------

// Artifact is a file a code run produced: today, a chart as a PNG.
type Artifact struct {
	Type string // e.g. image/png
	Name string
	Data []byte // decoded from base64
}

// CodeResult is the outcome of RunCode. A non-zero ExitCode is not an error.
type CodeResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	Language   string
	Image      string
	Artifacts  []Artifact
	DurationMS int64
	TimedOut   bool
	Truncated  bool
}

// OK is true when the snippet exited 0 and was not stopped by its timeout.
func (r CodeResult) OK() bool { return r.ExitCode == 0 && !r.TimedOut }

// CodeOptions for RunCode.
type CodeOptions struct {
	Language string            // python (default), javascript or typescript
	Image    string            // default: the sandbox template's image, else the language's
	Env      map[string]string // environment for the snippet
	Timeout  time.Duration     // the daemon stops the snippet after this; 0 is its default
	Approval string
}

// RunCode is POST …/code: run a Python, JavaScript or TypeScript snippet.
// It starts in /workspace. Charts a Python snippet draws with matplotlib come
// back in Artifacts as PNG bytes.
func (c *Client) RunCode(ctx context.Context, sandbox, code string, opts *CodeOptions) (CodeResult, error) {
	var o CodeOptions
	if opts != nil {
		o = *opts
	}
	if o.Language == "" {
		o.Language = "python"
	}
	body := map[string]any{"language": o.Language, "code": code}
	if o.Image != "" {
		body["image"] = o.Image
	}
	if len(o.Env) > 0 {
		body["env"] = o.Env
	}
	if o.Timeout > 0 {
		body["timeout_s"] = secs(o.Timeout)
	}
	var raw struct {
		Language  string `json:"language"`
		Image     string `json:"image"`
		Exit      int    `json:"exit"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Artifacts []struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"artifacts"`
		DurationMS int64 `json:"duration_ms"`
		TimedOut   bool  `json:"timed_out"`
		Truncated  bool  `json:"truncated"`
	}
	r := request{method: "POST", path: sbPath(sandbox, "code"), body: body, approval: o.Approval}
	if err := c.t.json(ctx, r, &raw); err != nil {
		return CodeResult{}, err
	}
	res := CodeResult{ExitCode: raw.Exit, Stdout: raw.Stdout, Stderr: raw.Stderr, Language: raw.Language, Image: raw.Image,
		DurationMS: raw.DurationMS, TimedOut: raw.TimedOut, Truncated: raw.Truncated}
	for _, a := range raw.Artifacts {
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return res, &Error{Kind: KindAPI, Message: fmt.Sprintf("artifact %s: bad base64: %v", a.Name, err), Method: r.method, Path: r.path, Err: err}
		}
		res.Artifacts = append(res.Artifacts, Artifact{Type: a.Type, Name: a.Name, Data: data})
	}
	return res, nil
}

// --- files --------------------------------------------------------------------

// FileInfo describes one entry in a sandbox's workspace.
type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"` // file | dir | symlink | other
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"` // permission bits
	ModTime string `json:"mod_time"`
	Link    string `json:"link,omitempty"` // a symlink's target
}

// IsDir reports whether the entry is a folder.
func (f FileInfo) IsDir() bool { return f.Type == "dir" }

func pathQuery(p string) url.Values { return url.Values{"path": {p}} }

// ListFiles is GET …/files: a folder in the workspace (or the one entry, for a
// file). Paths are as a container sees them (/workspace/src) or relative to
// the workspace (src); "" is /workspace.
func (c *Client) ListFiles(ctx context.Context, sandbox, p string) ([]FileInfo, error) {
	if p == "" {
		p = "/workspace"
	}
	var out struct {
		Files []FileInfo `json:"files"`
	}
	if err := c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "files"), query: pathQuery(p)}, &out); err != nil {
		return nil, err
	}
	if out.Files == nil {
		out.Files = []FileInfo{}
	}
	return out.Files, nil
}

// StatFile is GET …/files/stat: one entry, not following a symlink.
func (c *Client) StatFile(ctx context.Context, sandbox, p string) (FileInfo, error) {
	var out FileInfo
	return out, c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "files", "stat"), query: pathQuery(p)}, &out)
}

// ReadFileTo is GET …/files/content: stream a file's bytes into w. A body cut
// short of the Content-Length the daemon announced is a KindStream error.
func (c *Client) ReadFileTo(ctx context.Context, sandbox, p string, w io.Writer) (int64, error) {
	r := request{method: "GET", path: sbPath(sandbox, "files", "content"), query: pathQuery(p)}
	resp, err := c.t.do(ctx, r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return 0, errorFromResponse(resp.StatusCode, decodeBody(b), r.method, r.path)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		return n, &Error{Kind: KindStream, Message: fmt.Sprintf("%s: read broke after %d bytes: %v", p, n, err), Method: r.method, Path: r.path, Err: err}
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, &Error{Kind: KindStream, Message: fmt.Sprintf("%s: got %d of %d bytes", p, n, resp.ContentLength), Method: r.method, Path: r.path}
	}
	return n, nil
}

// ReadFile is GET …/files/content: a file's bytes.
func (c *Client) ReadFile(ctx context.Context, sandbox, p string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := c.ReadFileTo(ctx, sandbox, p, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteOptions for WriteFile.
type WriteOptions struct {
	Mode     string // octal permission bits, e.g. "0755"; default the daemon's
	Approval string
}

// WriteFile is PUT …/files/content: replace a file with what r yields, making
// its folders. A *bytes.Reader, *bytes.Buffer or *strings.Reader is sent with
// its length; any other reader is sent chunked and spooled by the daemon.
func (c *Client) WriteFile(ctx context.Context, sandbox, p string, r io.Reader, opts *WriteOptions) (FileInfo, error) {
	var o WriteOptions
	if opts != nil {
		o = *opts
	}
	if r == nil {
		r = bytes.NewReader(nil)
	}
	q := pathQuery(p)
	if o.Mode != "" {
		q.Set("mode", o.Mode)
	}
	var out FileInfo
	return out, c.t.json(ctx, request{method: "PUT", path: sbPath(sandbox, "files", "content"), query: q, rawBody: r, approval: o.Approval}, &out)
}

// WriteFileBytes is WriteFile with the content in hand.
func (c *Client) WriteFileBytes(ctx context.Context, sandbox, p string, data []byte, opts *WriteOptions) (FileInfo, error) {
	return c.WriteFile(ctx, sandbox, p, bytes.NewReader(data), opts)
}

// DeleteFileOptions for DeleteFile.
type DeleteFileOptions struct {
	Recursive bool // remove a folder and everything in it
	Approval  string
}

// DeleteFile is DELETE …/files: a file, or with Recursive a folder.
func (c *Client) DeleteFile(ctx context.Context, sandbox, p string, opts *DeleteFileOptions) (Info, error) {
	var o DeleteFileOptions
	if opts != nil {
		o = *opts
	}
	q := pathQuery(p)
	if o.Recursive {
		q.Set("recursive", "1")
	}
	var out Info
	return out, c.t.json(ctx, request{method: "DELETE", path: sbPath(sandbox, "files"), query: q, approval: o.Approval}, &out)
}

// MakeDir is POST …/files/mkdir: a folder and its parents.
func (c *Client) MakeDir(ctx context.Context, sandbox, p string, call ...Call) (FileInfo, error) {
	var out FileInfo
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "files", "mkdir"), body: map[string]any{"path": p}, approval: approval(call)}, &out)
}

// MoveFile is POST …/files/move: rename or move an entry within the workspace.
func (c *Client) MoveFile(ctx context.Context, sandbox, from, to string, call ...Call) (FileInfo, error) {
	var out FileInfo
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "files", "move"), body: map[string]any{"from": from, "to": to}, approval: approval(call)}, &out)
}

// --- git ----------------------------------------------------------------------

// GitOptions are the fields of a git operation; each op reads the ones it
// needs. Zero fields are left out.
type GitOptions struct {
	Path        string   // the repository, in the workspace
	URL         string   // clone (http or https)
	Branch      string   // clone, checkout, push, pull
	Depth       int      // clone
	Remote      string   // push, pull; default origin
	Create      bool     // checkout -b
	Files       []string // add; default everything
	Message     string   // commit
	AuthorName  string   // commit
	AuthorEmail string   // commit
	N           int      // log; default 20
	Staged      bool     // diff --cached
	Username    string   // with Token, for an http(s) remote
	Token       string   // sent as an Authorization header, redacted from output
	Image       string   // the image git runs in
	// NoStage makes GitCommit commit only what is already staged. The daemon never sees it.
	NoStage  bool
	Approval string
}

// GitStatus is the parsed answer to a status op.
type GitStatus struct {
	Branch   string          `json:"branch"`
	Upstream string          `json:"upstream,omitempty"`
	Ahead    int             `json:"ahead"`
	Behind   int             `json:"behind"`
	Clean    bool            `json:"clean"`
	Files    []GitFileStatus `json:"files"`
}

// GitFileStatus is one changed file: Index and Worktree are porcelain codes (M, A, D, ?…).
type GitFileStatus struct {
	Path     string `json:"path"`
	From     string `json:"from,omitempty"` // a rename's old path
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}

// GitCommit is one entry of a log op.
type GitCommit struct {
	Hash        string `json:"hash"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Date        string `json:"date"`
	Subject     string `json:"subject"`
}

// GitBranch is one entry of a branches op.
type GitBranch struct {
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// GitResult is the outcome of a git operation. A git failure is OK == false,
// not an error; Status, Log or Branches are filled for those ops.
type GitResult struct {
	Op       string      `json:"op"`
	Path     string      `json:"path"`
	OK       bool        `json:"ok"`
	ExitCode int         `json:"exit"`
	Stdout   string      `json:"stdout"`
	Stderr   string      `json:"stderr"`
	Status   *GitStatus  `json:"status,omitempty"`
	Log      []GitCommit `json:"log,omitempty"`
	Branches []GitBranch `json:"branches,omitempty"`
}

func gitBody(op string, o GitOptions) map[string]any {
	b := map[string]any{"op": op}
	set := func(k, v string) {
		if v != "" {
			b[k] = v
		}
	}
	set("path", o.Path)
	set("url", o.URL)
	set("branch", o.Branch)
	set("remote", o.Remote)
	set("message", o.Message)
	set("author_name", o.AuthorName)
	set("author_email", o.AuthorEmail)
	set("username", o.Username)
	set("token", o.Token)
	set("image", o.Image)
	if o.Depth > 0 {
		b["depth"] = o.Depth
	}
	if o.N > 0 {
		b["n"] = o.N
	}
	if o.Create {
		b["create"] = true
	}
	if o.Staged {
		b["staged"] = true
	}
	if len(o.Files) > 0 {
		b["files"] = o.Files
	}
	return b
}

// Git is POST …/git: op is clone, status, add, commit, push, pull, checkout,
// branches, log or diff.
func (c *Client) Git(ctx context.Context, sandbox, op string, opts *GitOptions) (GitResult, error) {
	var o GitOptions
	if opts != nil {
		o = *opts
	}
	out := GitResult{Op: op}
	err := c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "git"), body: gitBody(op, o), approval: o.Approval}, &out)
	return out, err
}

func gopts(o *GitOptions) GitOptions {
	if o == nil {
		return GitOptions{}
	}
	return *o
}

// GitClone clones repoURL into p (default: a folder named after the repository).
func (c *Client) GitClone(ctx context.Context, sandbox, repoURL, p string, opts *GitOptions) (GitResult, error) {
	o := gopts(opts)
	o.URL, o.Path = repoURL, p
	return c.Git(ctx, sandbox, "clone", &o)
}

// GitStatus is the status of the repository at p; the parsed form is in Status.
func (c *Client) GitStatus(ctx context.Context, sandbox, p string, opts *GitOptions) (GitResult, error) {
	o := gopts(opts)
	o.Path = p
	return c.Git(ctx, sandbox, "status", &o)
}

// GitCommit stages (opts.Files, or everything) and commits the repository at
// p. A failed add is returned as is, without committing. Set opts.NoStage to
// commit only what is already staged.
func (c *Client) GitCommit(ctx context.Context, sandbox, p, message string, opts *GitOptions) (GitResult, error) {
	o := gopts(opts)
	o.Path = p
	if !o.NoStage {
		r, err := c.Git(ctx, sandbox, "add", &GitOptions{Path: p, Files: o.Files, Image: o.Image, Approval: o.Approval})
		if err != nil || !r.OK {
			return r, err
		}
	}
	o.Message, o.Files = message, nil
	return c.Git(ctx, sandbox, "commit", &o)
}

// --- sessions -----------------------------------------------------------------

// Session is a shell that keeps cd and export between commands.
type Session struct {
	Name   string `json:"name"`
	ID     string `json:"id"` // its container
	Image  string `json:"image"`
	Status string `json:"status"`
}

// SessionOptions for CreateSession.
type SessionOptions struct {
	Image    string
	Env      map[string]string
	Approval string
}

// CreateSession is POST …/sessions. name "" lets the daemon choose.
func (c *Client) CreateSession(ctx context.Context, sandbox, name string, opts *SessionOptions) (Session, error) {
	var o SessionOptions
	if opts != nil {
		o = *opts
	}
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if o.Image != "" {
		body["image"] = o.Image
	}
	if len(o.Env) > 0 {
		body["env"] = o.Env
	}
	var out Session
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "sessions"), body: body, approval: o.Approval}, &out)
}

// ListSessions is GET …/sessions.
func (c *Client) ListSessions(ctx context.Context, sandbox string) ([]Session, error) {
	out := []Session{}
	if err := c.t.json(ctx, request{method: "GET", path: sbPath(sandbox, "sessions")}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Session{}
	}
	return out, nil
}

// ShellResult is the outcome of one command in a session. Cwd is where the
// shell is now. A non-zero ExitCode is not an error.
type ShellResult struct {
	ExitCode   int    `json:"exit"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Cwd        string `json:"cwd"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
}

// OK is true when the command exited 0 and was not stopped by its timeout.
func (r ShellResult) OK() bool { return r.ExitCode == 0 && !r.TimedOut }

// ShellOptions for SessionExec and Shell.
type ShellOptions struct {
	Timeout  time.Duration // the daemon stops the command after this; 0 is its default
	Session  string        // Shell only: the session to use; default "main"
	Approval string
}

// SessionExec is POST …/sessions/{name}/exec: run one command line in the session.
func (c *Client) SessionExec(ctx context.Context, sandbox, session, command string, opts *ShellOptions) (ShellResult, error) {
	var o ShellOptions
	if opts != nil {
		o = *opts
	}
	body := map[string]any{"command": command}
	if o.Timeout > 0 {
		body["timeout_s"] = secs(o.Timeout)
	}
	var out ShellResult
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "sessions", enc(session), "exec"), body: body, approval: o.Approval}, &out)
}

// Shell runs a command in a session (opts.Session, default "main"), making the
// session on first use.
func (c *Client) Shell(ctx context.Context, sandbox, command string, opts *ShellOptions) (ShellResult, error) {
	var o ShellOptions
	if opts != nil {
		o = *opts
	}
	if o.Session == "" {
		o.Session = "main"
	}
	res, err := c.SessionExec(ctx, sandbox, o.Session, command, &o)
	e, ok := AsError(err)
	if err == nil || !ok || e.Kind != KindNotFound || !strings.Contains(e.DaemonMessage+" "+e.Message, "no such session") {
		return res, err
	}
	if _, err := c.CreateSession(ctx, sandbox, o.Session, &SessionOptions{Approval: o.Approval}); err != nil {
		return ShellResult{}, err
	}
	return c.SessionExec(ctx, sandbox, o.Session, command, &o)
}

// DeleteSession is DELETE …/sessions/{name}: stop the shell and its container.
func (c *Client) DeleteSession(ctx context.Context, sandbox, session string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "DELETE", path: sbPath(sandbox, "sessions", enc(session)), approval: approval(call)}, &out)
}

// --- terminals ----------------------------------------------------------------

// PTYOptions for StartPTY.
type PTYOptions struct {
	Image      string   // default: the sandbox template's image, else the daemon's
	Command    []string // default: a shell
	Cols, Rows int      // initial size; both or neither
	Env        map[string]string
	Approval   string
}

// StartPTY is POST …/pty: a container with a terminal; it returns the
// container id. Connect to TTYPath(sandbox, id) with any WebSocket client:
// binary messages are the terminal, text messages JSON control.
func (c *Client) StartPTY(ctx context.Context, sandbox string, opts *PTYOptions) (string, error) {
	var o PTYOptions
	if opts != nil {
		o = *opts
	}
	body := map[string]any{}
	if o.Image != "" {
		body["image"] = o.Image
	}
	if len(o.Command) > 0 {
		body["command"] = o.Command
	}
	if o.Cols > 0 && o.Rows > 0 {
		body["cols"], body["rows"] = o.Cols, o.Rows
	}
	if len(o.Env) > 0 {
		body["env"] = o.Env
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "pty"), body: body, approval: o.Approval}, &out)
	return out.ID, err
}

// TTYPath is the WebSocket route of a terminal StartPTY started.
func TTYPath(sandbox, container string) string {
	return sbPath(sandbox, "containers", enc(container), "tty")
}

// ResizePTY is POST …/containers/{cid}/resize.
func (c *Client) ResizePTY(ctx context.Context, sandbox, container string, cols, rows int, call ...Call) error {
	if cols < 1 || rows < 1 {
		return usage("cols and rows must both be positive, not %dx%d", cols, rows)
	}
	return c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "containers", enc(container), "resize"),
		body: map[string]any{"cols": cols, "rows": rows}, approval: approval(call)}, nil)
}

// --- previews -----------------------------------------------------------------

// Preview is a signed link to a web server in a sandbox.
type Preview struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"` // RFC 3339
	Container string `json:"container"`
	Port      int    `json:"port"`
	Listen    string `json:"listen"` // the daemon's preview listener
}

// PreviewOptions for CreatePreview. With neither Session nor Container the
// sandbox's one running container is used.
type PreviewOptions struct {
	Session   string
	Container string
	ExpiresIn time.Duration // 0 is the daemon's default
	Approval  string
}

// CreatePreview is POST …/previews: a link to port in the sandbox that expires.
func (c *Client) CreatePreview(ctx context.Context, sandbox string, port int, opts *PreviewOptions) (Preview, error) {
	var o PreviewOptions
	if opts != nil {
		o = *opts
	}
	body := map[string]any{"port": port}
	if o.Session != "" {
		body["session"] = o.Session
	}
	if o.Container != "" {
		body["container"] = o.Container
	}
	if o.ExpiresIn > 0 {
		body["expires_in_s"] = secs(o.ExpiresIn)
	}
	var out Preview
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "previews"), body: body, approval: o.Approval}, &out)
}

// RevokePreviews is POST /kosa8/v1/previews/rotate: end every preview link at once.
func (c *Client) RevokePreviews(ctx context.Context, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: "/kosa8/v1/previews/rotate", approval: approval(call)}, &out)
}

// --- lifecycle ----------------------------------------------------------------

// LifecycleOptions for SetLifecycle. nil fields keep their value; 0 (or
// false) turns one off. Use kosa8.Ptr: LifecycleOptions{AutoStopMin: kosa8.Ptr(30)}.
type LifecycleOptions struct {
	AutoStopMin   *int  // stop after this many idle minutes
	AutoDeleteMin *int  // delete once stopped this long
	TTLMin        *int  // stop this long after creation
	Ephemeral     *bool // delete instead of stopping
	Approval      string
}

// SetLifecycle is POST …/lifecycle: change auto-stop, auto-delete, TTL or
// ephemeral. The answer is the sandbox.
func (c *Client) SetLifecycle(ctx context.Context, sandbox string, o LifecycleOptions) (Info, error) {
	body := map[string]any{}
	if o.AutoStopMin != nil {
		body["auto_stop_min"] = *o.AutoStopMin
	}
	if o.AutoDeleteMin != nil {
		body["auto_delete_min"] = *o.AutoDeleteMin
	}
	if o.TTLMin != nil {
		body["ttl_min"] = *o.TTLMin
	}
	if o.Ephemeral != nil {
		body["ephemeral"] = *o.Ephemeral
	}
	var out Info
	return out, c.t.json(ctx, request{method: "POST", path: sbPath(sandbox, "lifecycle"), body: body, approval: o.Approval}, &out)
}

// --- templates ----------------------------------------------------------------

// Template is a built image new sandboxes can start with (CreateOptions.Template).
type Template struct {
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	SizeBytes  int64     `json:"size_bytes"`
	Created    time.Time `json:"created"`
	Dockerfile string    `json:"dockerfile,omitempty"`
}

// ListTemplates is GET /kosa8/v1/templates.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	out := []Template{}
	if err := c.t.json(ctx, request{method: "GET", path: "/kosa8/v1/templates"}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Template{}
	}
	return out, nil
}

// DeleteTemplate is DELETE /kosa8/v1/templates/{name}. Sandboxes made from it keep their copy.
func (c *Client) DeleteTemplate(ctx context.Context, name string, call ...Call) (Info, error) {
	var out Info
	return out, c.t.json(ctx, request{method: "DELETE", path: "/kosa8/v1/templates/" + enc(name), approval: approval(call)}, &out)
}

// BuildOptions for BuildTemplate.
type BuildOptions struct {
	Dockerfile string            // relative to the context; default Dockerfile
	BuildArgs  map[string]string // --build-arg K=V
	Egress     string            // the build sandbox's network; default the daemon's (any)
	Replace    bool              // rebuild a template that exists
	OnOutput   func([]byte)      // build progress as it arrives
	Approval   string
}

// BuildResult is what a template build made.
type BuildResult struct {
	Image string // the template's image reference
}

// BuildTemplate is POST /kosa8/v1/templates: build contextDir's Dockerfile in
// a throwaway sandbox and keep the image as the template name. The context is
// sent as a tar, leaving out what .dockerignore lists (by path prefix, as
// `kosa8 build` does). A build that fails is a KindStream error.
func (c *Client) BuildTemplate(ctx context.Context, name, contextDir string, opts *BuildOptions) (BuildResult, error) {
	var o BuildOptions
	if opts != nil {
		o = *opts
	}
	if name == "" {
		return BuildResult{}, usage("a template needs a name")
	}
	if contextDir == "" {
		contextDir = "."
	}
	if o.Dockerfile == "" {
		o.Dockerfile = "Dockerfile"
	}
	ctxTar, err := tarContext(contextDir, o.Dockerfile)
	if err != nil {
		return BuildResult{}, err
	}
	q := url.Values{"name": {name}, "dockerfile": {o.Dockerfile}}
	keys := make([]string, 0, len(o.BuildArgs))
	for k := range o.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q.Add("buildarg", k+"="+o.BuildArgs[k])
	}
	if o.Egress != "" {
		q.Set("egress", o.Egress)
	}
	if o.Replace {
		q.Set("replace", "1")
	}
	r := request{method: "POST", path: "/kosa8/v1/templates", query: q, rawBody: bytes.NewReader(ctxTar), approval: o.Approval}
	var res BuildResult
	var final error
	done := false
	err = c.t.frames(ctx, r, func(f Frame) error {
		if f.Done {
			done = true
			if f.Error != "" {
				final = &Error{Kind: KindStream, Message: f.Error, DaemonMessage: f.Error, Method: r.method, Path: r.path}
			} else {
				res.Image = f.Image
			}
			return errDone
		}
		if o.OnOutput != nil && len(f.Data) > 0 {
			o.OnOutput(f.Data)
		}
		return nil
	})
	if err != nil && err != errDone {
		return BuildResult{}, err
	}
	if final != nil {
		return BuildResult{}, final
	}
	if !done {
		return BuildResult{}, &Error{Kind: KindStream, Message: "template build stream ended unexpectedly (no final frame)", Method: r.method, Path: r.path}
	}
	return res, nil
}

// dockerignore reads dir/.dockerignore as cleaned path prefixes.
func dockerignore(dir string) ([]string, error) {
	f, err := os.Open(filepath.Join(dir, ".dockerignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		p := path.Clean(strings.TrimPrefix(filepath.ToSlash(l), "/"))
		if p != "." {
			out = append(out, p)
		}
	}
	return out, sc.Err()
}

// tarContext packs a build context the way `kosa8 build` does.
func tarContext(dir, dockerfile string) ([]byte, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, usage("build context %s: %v", dir, err)
	}
	if !st.IsDir() {
		return nil, usage("build context %s is not a folder", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err != nil {
		return nil, usage("no %s in %s", dockerfile, dir)
	}
	ignore, err := dockerignore(dir)
	if err != nil {
		return nil, usage("read %s: %v", filepath.Join(dir, ".dockerignore"), err)
	}
	skip := func(rel string) bool {
		for _, p := range ignore {
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = rel
		if d.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return nil, usage("pack build context %s: %v", dir, err)
	}
	return buf.Bytes(), nil
}

// --- bound handle -------------------------------------------------------------

func (s *Sandbox) RunCode(ctx context.Context, code string, o *CodeOptions) (CodeResult, error) {
	return s.c.RunCode(ctx, s.ID, code, o)
}
func (s *Sandbox) ListFiles(ctx context.Context, p string) ([]FileInfo, error) {
	return s.c.ListFiles(ctx, s.ID, p)
}
func (s *Sandbox) StatFile(ctx context.Context, p string) (FileInfo, error) {
	return s.c.StatFile(ctx, s.ID, p)
}
func (s *Sandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return s.c.ReadFile(ctx, s.ID, p)
}
func (s *Sandbox) ReadFileTo(ctx context.Context, p string, w io.Writer) (int64, error) {
	return s.c.ReadFileTo(ctx, s.ID, p, w)
}
func (s *Sandbox) WriteFile(ctx context.Context, p string, r io.Reader, o *WriteOptions) (FileInfo, error) {
	return s.c.WriteFile(ctx, s.ID, p, r, o)
}
func (s *Sandbox) WriteFileBytes(ctx context.Context, p string, data []byte, o *WriteOptions) (FileInfo, error) {
	return s.c.WriteFileBytes(ctx, s.ID, p, data, o)
}
func (s *Sandbox) DeleteFile(ctx context.Context, p string, o *DeleteFileOptions) (Info, error) {
	return s.c.DeleteFile(ctx, s.ID, p, o)
}
func (s *Sandbox) MakeDir(ctx context.Context, p string, call ...Call) (FileInfo, error) {
	return s.c.MakeDir(ctx, s.ID, p, call...)
}
func (s *Sandbox) MoveFile(ctx context.Context, from, to string, call ...Call) (FileInfo, error) {
	return s.c.MoveFile(ctx, s.ID, from, to, call...)
}
func (s *Sandbox) Git(ctx context.Context, op string, o *GitOptions) (GitResult, error) {
	return s.c.Git(ctx, s.ID, op, o)
}
func (s *Sandbox) GitClone(ctx context.Context, repoURL, p string, o *GitOptions) (GitResult, error) {
	return s.c.GitClone(ctx, s.ID, repoURL, p, o)
}
func (s *Sandbox) GitStatus(ctx context.Context, p string, o *GitOptions) (GitResult, error) {
	return s.c.GitStatus(ctx, s.ID, p, o)
}
func (s *Sandbox) GitCommit(ctx context.Context, p, message string, o *GitOptions) (GitResult, error) {
	return s.c.GitCommit(ctx, s.ID, p, message, o)
}
func (s *Sandbox) CreateSession(ctx context.Context, name string, o *SessionOptions) (Session, error) {
	return s.c.CreateSession(ctx, s.ID, name, o)
}
func (s *Sandbox) Sessions(ctx context.Context) ([]Session, error) {
	return s.c.ListSessions(ctx, s.ID)
}
func (s *Sandbox) SessionExec(ctx context.Context, session, command string, o *ShellOptions) (ShellResult, error) {
	return s.c.SessionExec(ctx, s.ID, session, command, o)
}
func (s *Sandbox) Shell(ctx context.Context, command string, o *ShellOptions) (ShellResult, error) {
	return s.c.Shell(ctx, s.ID, command, o)
}
func (s *Sandbox) DeleteSession(ctx context.Context, session string, call ...Call) (Info, error) {
	return s.c.DeleteSession(ctx, s.ID, session, call...)
}
func (s *Sandbox) StartPTY(ctx context.Context, o *PTYOptions) (string, error) {
	return s.c.StartPTY(ctx, s.ID, o)
}
func (s *Sandbox) ResizePTY(ctx context.Context, container string, cols, rows int, call ...Call) error {
	return s.c.ResizePTY(ctx, s.ID, container, cols, rows, call...)
}
func (s *Sandbox) CreatePreview(ctx context.Context, port int, o *PreviewOptions) (Preview, error) {
	return s.c.CreatePreview(ctx, s.ID, port, o)
}
func (s *Sandbox) SetLifecycle(ctx context.Context, o LifecycleOptions) (Info, error) {
	return s.c.SetLifecycle(ctx, s.ID, o)
}
