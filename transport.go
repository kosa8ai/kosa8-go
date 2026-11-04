package kosa8

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// goos is runtime.GOOS; a variable so tests can check the Windows hint.
var goos = runtime.GOOS

// Header names the daemon reads.
const (
	ApprovalHeader   = "X-Kosa8-Approval"
	ActorHeader      = "X-Kosa8-Actor"
	AgentTokenHeader = "X-Kosa8-Agent-Token"
)

// DefaultSocketPath is $HOME/.kosa8/run/kosa8.sock (pkg/paths).
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kosa8", "run", "kosa8.sock")
}

// DefaultWorkerFile is $HOME/.kosa8/worker.json (%USERPROFILE% on Windows):
// the connection `kosa8 wsl setup` and `kosa8 worker join` save, which the CLI
// uses whenever KOSA8_HOST is unset.
func DefaultWorkerFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kosa8", "worker.json")
}

// savedWorker is the part of worker.json that matters; other fields are ignored.
type savedWorker struct {
	Host     string `json:"host"`
	Token    string `json:"token"`
	Insecure bool   `json:"insecure"`
}

// loadSavedWorker reads a saved connection; nil when the file is missing,
// unreadable, malformed or has no host.
func loadSavedWorker(path string) *savedWorker {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var w savedWorker
	if json.Unmarshal(b, &w) != nil || strings.TrimSpace(w.Host) == "" {
		return nil
	}
	return &w
}

// Endpoint is where requests go: a unix socket or a remote worker.
type Endpoint struct {
	Unix  string   // socket path, when local
	URL   *url.URL // remote worker, when not
	Token string
}

// Target describes the endpoint for humans.
func (e Endpoint) Target() string {
	if e.URL == nil {
		return "unix://" + e.Unix
	}
	return e.URL.Scheme + "://" + e.URL.Host
}

func isLoopback(host string) bool {
	h := strings.Trim(host, "[]")
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

// Resolve picks the endpoint for o as the CLI does: an explicit Host or a
// non-blank KOSA8_HOST first; else, unless SocketPath is set, the connection
// saved in the worker file (Options.WorkerFile, default DefaultWorkerFile;
// NoWorkerFile skips it), with Token or else the saved token; else the unix
// socket. A saved token is never sent to a host from an option or the env.
func Resolve(o Options) (Endpoint, error) {
	ep, _, err := resolve(o)
	return ep, err
}

// resolve also reports whether the endpoint is the default unix socket.
func resolve(o Options) (Endpoint, bool, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	noHost := strings.TrimSpace(o.Host) == "" && strings.TrimSpace(getenv("KOSA8_HOST")) == ""
	if noHost && o.SocketPath == "" && !o.NoWorkerFile {
		file := o.WorkerFile
		if file == "" {
			file = DefaultWorkerFile()
		}
		if w := loadSavedWorker(file); w != nil {
			token := o.Token
			if token == "" {
				token = w.Token
			}
			ep, err := parseHost(w.Host, token, o.Insecure || w.Insecure, getenv)
			return ep, false, err
		}
	}
	ep, err := ResolveEndpoint(o.Host, o.SocketPath, o.Token, o.Insecure, getenv)
	return ep, err == nil && noHost && o.SocketPath == "", err
}

// ResolveEndpoint follows the CLI's KOSA8_HOST rules, without the saved
// worker file (see Resolve): no host means the local socket; https://,
// http://, tcp://, unix:// or a bare host:port select a worker; plain http to a non-loopback host is refused unless insecure (or
// KOSA8_INSECURE=1). KOSA8_TOKEN is used only for a host that came from KOSA8_HOST.
func ResolveEndpoint(host, socketPath, token string, insecure bool, getenv func(string) string) (Endpoint, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fromEnv := strings.TrimSpace(getenv("KOSA8_HOST"))
	explicit := host != ""
	raw := host
	if !explicit {
		raw = fromEnv
	}
	if strings.TrimSpace(raw) == "" {
		if socketPath == "" {
			socketPath = DefaultSocketPath()
		}
		return Endpoint{Unix: socketPath}, nil
	}
	if token == "" && !explicit {
		token = getenv("KOSA8_TOKEN")
	}
	return parseHost(raw, token, insecure, getenv)
}

// parseHost reads a KOSA8_HOST spelling into an endpoint carrying token.
func parseHost(raw, token string, insecure bool, getenv func(string) string) (Endpoint, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	h := raw
	if i := strings.Index(h, "://"); i > 0 {
		h = strings.ToLower(h[:i]) + h[i:]
	}
	switch {
	case strings.HasPrefix(h, "unix://"):
		return Endpoint{Unix: strings.TrimPrefix(h, "unix://")}, nil
	case strings.HasPrefix(h, "tcp://"):
		h = "http://" + strings.TrimPrefix(h, "tcp://")
	case strings.HasPrefix(h, "http://"), strings.HasPrefix(h, "https://"):
	case strings.Contains(h, "://"):
		return Endpoint{}, usage(`KOSA8_HOST scheme "%s" is not supported; use http://, https://, tcp:// or unix://`, h[:strings.Index(h, "://")])
	default:
		h = "http://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Hostname() == "" {
		return Endpoint{}, usage("KOSA8_HOST %q names no host", raw)
	}
	if u.Scheme == "http" && !insecure && getenv("KOSA8_INSECURE") != "1" && !isLoopback(u.Hostname()) {
		return Endpoint{}, usage("refusing to talk to %s over plain http: the token and every request would cross the network unencrypted. "+
			"Use https://%s (a worker behind a TLS-terminating tunnel or proxy), or set Insecure / KOSA8_INSECURE=1 if this link is already private and trusted", u.Host, u.Host)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return Endpoint{URL: u, Token: token}, nil
}

// Frame is one NDJSON frame of a run, exec or attach stream. Data is decoded from base64.
type Frame struct {
	Stream string `json:"stream,omitempty"` // stdout | stderr | status
	Data   []byte `json:"data,omitempty"`
	Done   bool   `json:"done,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Error  string `json:"error,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	ID     string `json:"id,omitempty"`
	Image  string `json:"image,omitempty"` // a template build's final frame
}

type transport struct {
	ep          Endpoint
	defaultSock bool // ep is the default unix socket
	hc          *http.Client
	actor       string
	agentToken  string
	headers     map[string]string
}

type request struct {
	method   string
	path     string
	query    url.Values
	body     any       // JSON-encoded
	rawBody  io.Reader // sent as-is (chunked when length unknown)
	approval string
}

func newTransport(o Options) (*transport, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	ep, defaultSock, err := resolve(o)
	if err != nil {
		return nil, err
	}
	actor := o.Actor
	if actor == "" {
		actor = "tool"
	}
	if actor != "human" && actor != "agent" && actor != "tool" {
		return nil, usage("actor must be human, agent or tool, not %q", actor)
	}
	agent := o.AgentToken
	if agent == "" {
		agent = getenv("KOSA8_AGENT_TOKEN")
	}
	hc := o.HTTPClient
	if hc == nil {
		tr := &http.Transport{DisableCompression: true}
		if ep.URL == nil {
			sock := ep.Unix
			tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}
		}
		hc = &http.Client{Transport: tr}
	}
	return &transport{ep: ep, defaultSock: defaultSock, hc: hc, actor: actor, agentToken: agent, headers: o.Headers}, nil
}

func (t *transport) do(ctx context.Context, r request) (*http.Response, error) {
	var base string
	if t.ep.URL == nil {
		base = "http://kosa8"
	} else {
		base = t.ep.URL.Scheme + "://" + t.ep.URL.Host + t.ep.URL.Path
	}
	full := base + r.path
	if len(r.query) > 0 {
		full += "?" + r.query.Encode()
	}
	var body io.Reader
	var ctype string
	switch {
	case r.rawBody != nil:
		body, ctype = r.rawBody, "application/octet-stream"
	case r.body != nil:
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, usage("encode request: %v", err)
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, full, body)
	if err != nil {
		return nil, usage("build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(ActorHeader, t.actor)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if a := strings.TrimSpace(r.approval); a != "" {
		req.Header.Set(ApprovalHeader, a)
	}
	if t.agentToken != "" {
		req.Header.Set(AgentTokenHeader, t.agentToken)
	}
	if t.ep.URL != nil && t.ep.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.ep.Token)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Kind: KindTimeout, Message: fmt.Sprintf("%s %s timed out talking to %s", r.method, r.path, t.ep.Target()), Method: r.method, Path: r.path, Err: err}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hint := ""
		var op *net.OpError
		if errors.Is(err, os.ErrNotExist) || (errors.As(err, &op) && op.Op == "dial") || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such file") {
			hint = " (is kosa8 installed and its daemon running? start it with `kosa8 daemon start`)"
		}
		if t.defaultSock && goos == "windows" {
			hint += " (on Windows the engine runs in WSL2: run `kosa8 wsl setup`, or set KOSA8_HOST)"
		}
		return nil, &Error{Kind: KindConnection, Message: fmt.Sprintf("kosa8 daemon unreachable at %s: %v%s", t.ep.Target(), err, hint), Method: r.method, Path: r.path, Err: err}
	}
	return resp, nil
}

func decodeBody(b []byte) any {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	return string(b)
}

// json sends a request and decodes a JSON answer into out (which may be nil).
func (t *transport) json(ctx context.Context, r request, out any) error {
	resp, err := t.do(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Kind: KindConnection, Message: fmt.Sprintf("read %s %s: %v", r.method, r.path, err), Err: err}
	}
	if resp.StatusCode >= 400 {
		return errorFromResponse(resp.StatusCode, decodeBody(b), r.method, r.path)
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("decode %s %s: %v", r.method, r.path, err), Err: err}
	}
	return nil
}

type rawFrame struct {
	Stream string `json:"stream,omitempty"`
	Data   string `json:"data,omitempty"`
	Done   bool   `json:"done,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Error  string `json:"error,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	ID     string `json:"id,omitempty"`
	Image  string `json:"image,omitempty"` // a template build's final frame
}

// frames sends a request whose answer is NDJSON frames and calls fn for each.
// Returning a non-nil error from fn stops the stream.
func (t *transport) frames(ctx context.Context, r request, fn func(Frame) error) error {
	resp, err := t.do(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return errorFromResponse(resp.StatusCode, decodeBody(b), r.method, r.path)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rf rawFrame
		if err := json.Unmarshal(line, &rf); err != nil {
			return &Error{Kind: KindStream, Message: fmt.Sprintf("bad frame from %s: %v", r.path, err), Err: err}
		}
		f := Frame{Stream: rf.Stream, Done: rf.Done, OK: rf.OK, Error: rf.Error, Exit: rf.Exit, ID: rf.ID, Image: rf.Image}
		if rf.Data != "" {
			if f.Data, err = base64.StdEncoding.DecodeString(rf.Data); err != nil {
				return &Error{Kind: KindStream, Message: "bad frame data: " + err.Error(), Err: err}
			}
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Kind: KindTimeout, Message: fmt.Sprintf("%s %s timed out mid-stream", r.method, r.path), Err: err}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Kind: KindStream, Message: "stream broke: " + err.Error(), Err: err}
	}
	return nil
}
