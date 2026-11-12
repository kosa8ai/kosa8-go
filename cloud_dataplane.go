package kosa8

// The kosa8 Cloud data plane: files, checkpoints, rewind, fork and ports.
//
// Every method here needs the host's data plane; on hosts without it the API
// answers 501 (not_available / not_supported), returned as an error matching
// ErrUnsupported. Not every kosa8 Cloud host has it yet.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"unicode/utf8"
)

// CloudMaxFileBytes is the largest file the Cloud API reads or writes (8 MiB).
const CloudMaxFileBytes = 8 << 20

// CloudFileInfo describes a file or folder in a cloud sandbox.
type CloudFileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"` // file | dir
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
}

// CloudFiles reads and writes a cloud sandbox's files. A sleeping sandbox
// wakes first. Only /workspace persists between execs.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
type CloudFiles struct{ b *CloudSandbox }

// Files returns the sandbox's file operations.
func (b *CloudSandbox) Files() *CloudFiles { return &CloudFiles{b: b} }

func (f *CloudFiles) filePath(suffix, path string, extra url.Values) string {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	if path != "" {
		q.Set("path", path)
	}
	p := f.b.path("/files" + suffix)
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p
}

func needPath(path string) error {
	if path == "" {
		return usage("a file path is needed")
	}
	return nil
}

// Read returns a file's bytes (at most 8 MiB).
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) Read(ctx context.Context, path string) ([]byte, error) {
	if err := needPath(path); err != nil {
		return nil, err
	}
	b, err := f.b.c.do(ctx, "GET", f.filePath("", path, nil), "", nil, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// ReadText returns a file as a string.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) ReadText(ctx context.Context, path string) (string, error) {
	b, err := f.Read(ctx, path)
	return string(b), err
}

// Write creates or replaces a file with data (at most 8 MiB); larger data
// is refused before any request.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) Write(ctx context.Context, path string, data []byte) (CloudFileInfo, error) {
	if err := needPath(path); err != nil {
		return CloudFileInfo{}, err
	}
	if len(data) > CloudMaxFileBytes {
		return CloudFileInfo{}, usage("a kosa8 Cloud file is at most 8 MiB; this is %d bytes", len(data))
	}
	p := f.filePath("", path, nil)
	b, err := f.b.c.do(ctx, "PUT", p, "application/octet-stream", bytes.NewReader(data), "application/json")
	if err != nil {
		return CloudFileInfo{}, err
	}
	var r struct {
		File CloudFileInfo `json:"file"`
	}
	return r.File, decodeCloud(b, &r, "PUT", p)
}

// WriteString is Write with a string.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) WriteString(ctx context.Context, path, s string) (CloudFileInfo, error) {
	return f.Write(ctx, path, []byte(s))
}

// List lists a folder; "" lists /workspace.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) List(ctx context.Context, path string) ([]CloudFileInfo, error) {
	if path == "" {
		path = "/workspace"
	}
	var r struct {
		Files []CloudFileInfo `json:"files"`
	}
	if err := f.b.c.request(ctx, "GET", f.filePath("/list", path, nil), nil, &r); err != nil {
		return nil, err
	}
	if r.Files == nil {
		r.Files = []CloudFileInfo{}
	}
	return r.Files, nil
}

// Mkdir makes a folder and its parents.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) Mkdir(ctx context.Context, path string) (CloudFileInfo, error) {
	if err := needPath(path); err != nil {
		return CloudFileInfo{}, err
	}
	var r struct {
		File CloudFileInfo `json:"file"`
	}
	err := f.b.c.request(ctx, "POST", f.b.path("/files/mkdir"), map[string]any{"path": path}, &r)
	return r.File, err
}

// Delete removes a file; a folder needs recursive. It returns the path
// removed.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (f *CloudFiles) Delete(ctx context.Context, path string, recursive bool) (string, error) {
	if err := needPath(path); err != nil {
		return "", err
	}
	var extra url.Values
	if recursive {
		extra = url.Values{"recursive": {"1"}}
	}
	var r struct {
		Removed string `json:"removed"`
	}
	err := f.b.c.request(ctx, "DELETE", f.filePath("", path, extra), nil, &r)
	return r.Removed, err
}

func decodeCloud(b []byte, out any, method, path string) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("decode %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	return nil
}

// CloudCheckpoint is a saved state of a cloud sandbox: memory, processes
// and disk.
type CloudCheckpoint struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Auto      bool    `json:"auto"` // saved automatically before a rewind
	CreatedAt *string `json:"created_at"`
	Bytes     int64   `json:"bytes"`
}

// Checkpoint saves the sandbox as a checkpoint; label may be "" and is at
// most 200 characters. The newest 10 are kept.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (b *CloudSandbox) Checkpoint(ctx context.Context, label string) (CloudCheckpoint, error) {
	if n := utf8.RuneCountInString(label); n > CloudMaxLabelLen {
		return CloudCheckpoint{}, usage("a checkpoint label is at most %d characters, not %d", CloudMaxLabelLen, n)
	}
	body := map[string]any{}
	if label != "" {
		body["label"] = label
	}
	var r struct {
		Checkpoint CloudCheckpoint `json:"checkpoint"`
	}
	err := b.c.request(ctx, "POST", b.path("/checkpoints"), body, &r)
	return r.Checkpoint, err
}

// Checkpoints lists the sandbox's checkpoints, oldest first. It does not
// wake a sleeping sandbox.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (b *CloudSandbox) Checkpoints(ctx context.Context) ([]CloudCheckpoint, error) {
	var r struct {
		Checkpoints []CloudCheckpoint `json:"checkpoints"`
	}
	if err := b.c.request(ctx, "GET", b.path("/checkpoints"), nil, &r); err != nil {
		return nil, err
	}
	if r.Checkpoints == nil {
		r.Checkpoints = []CloudCheckpoint{}
	}
	return r.Checkpoints, nil
}

// CloudRewindResult is a finished rewind.
type CloudRewindResult struct {
	Checkpoint CloudCheckpoint `json:"checkpoint"` // the one rewound to
	Backup     CloudCheckpoint `json:"backup"`     // the sandbox as it was just before
	Ms         *int            `json:"ms"`
}

// Rewind takes the sandbox back to a checkpoint and refreshes Info. The
// sandbox as it was is saved first as an automatic checkpoint (Backup).
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (b *CloudSandbox) Rewind(ctx context.Context, checkpointID string) (CloudRewindResult, error) {
	if checkpointID == "" {
		return CloudRewindResult{}, usage("Rewind needs a checkpoint id")
	}
	var r struct {
		CloudRewindResult
		Sandbox *CloudSandboxInfo `json:"sandbox"`
	}
	if err := b.c.request(ctx, "POST", b.path("/rewind"), map[string]any{"checkpoint_id": checkpointID}, &r); err != nil {
		return CloudRewindResult{}, err
	}
	if r.Sandbox != nil {
		b.Info = *r.Sandbox
	}
	return r.CloudRewindResult, nil
}

// Fork makes n copies (1..100; 0 means 1) of the sandbox as it is now, on
// its host, all or nothing, and returns them.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (b *CloudSandbox) Fork(ctx context.Context, n int) ([]*CloudSandbox, error) {
	if n == 0 {
		n = 1
	}
	if n < 1 || n > CloudMaxForks {
		return nil, usage("Fork makes 1..%d copies, not %d", CloudMaxForks, n)
	}
	var r struct {
		Sandboxes []CloudSandboxInfo `json:"sandboxes"`
	}
	if err := b.c.request(ctx, "POST", b.path("/fork"), map[string]any{"n": n}, &r); err != nil {
		return nil, err
	}
	out := make([]*CloudSandbox, 0, len(r.Sandboxes))
	for _, i := range r.Sandboxes {
		out = append(out, &CloudSandbox{c: b.c, Info: i})
	}
	return out, nil
}

// CloudPort is a TCP port listening in one of the sandbox's containers.
type CloudPort struct {
	Port      int    `json:"port"`
	Address   string `json:"address"`
	Container string `json:"container"`
	Loopback  bool   `json:"loopback"`
}

// Ports lists the TCP ports listening in the sandbox's running containers.
//
// Needs the host's data plane; on hosts without it the API answers 501
// (not_available / not_supported), returned as ErrUnsupported. Not every
// kosa8 Cloud host has it yet.
func (b *CloudSandbox) Ports(ctx context.Context) ([]CloudPort, error) {
	var r struct {
		Ports []CloudPort `json:"ports"`
	}
	if err := b.c.request(ctx, "GET", b.path("/ports"), nil, &r); err != nil {
		return nil, err
	}
	if r.Ports == nil {
		r.Ports = []CloudPort{}
	}
	return r.Ports, nil
}
