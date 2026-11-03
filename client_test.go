package kosa8

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockDaemon serves the native API on a unix socket, as kosa8d does.
type mockDaemon struct {
	sock   string
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "METHOD /path"
	seen   []*http.Request
	bodies []string
	srv    *http.Server
}

func newMock(t *testing.T) *mockDaemon {
	t.Helper()
	base := "/tmp" // short: unix socket paths are limited to ~104 bytes
	if runtime.GOOS == "windows" {
		base = "" // %TEMP%; Windows 10+ has AF_UNIX
	}
	dir, err := os.MkdirTemp(base, "k8go")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m := &mockDaemon{sock: filepath.Join(dir, "k.sock"), routes: map[string]http.HandlerFunc{}}
	ln, err := net.Listen("unix", m.sock)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("no unix sockets here: %v", err)
		}
		t.Fatal(err)
	}
	m.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.seen = append(m.seen, r)
		m.bodies = append(m.bodies, string(b))
		h := m.routes[r.Method+" "+r.URL.Path]
		m.mu.Unlock()
		if h == nil {
			sendJSON(w, 404, map[string]any{"error": "no route " + r.Method + " " + r.URL.Path})
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		h(w, r)
	})}
	go m.srv.Serve(ln)
	t.Cleanup(func() { m.srv.Close() })
	return m
}

func (m *mockDaemon) on(method, path string, h http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[method+" "+path] = h
}

func (m *mockDaemon) last() (*http.Request, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[len(m.seen)-1], m.bodies[len(m.bodies)-1]
}

func (m *mockDaemon) client(t *testing.T, store CheckpointStore) *Client {
	t.Helper()
	if store == nil {
		store = &MemoryCheckpointStore{}
	}
	c, err := New(Options{SocketPath: m.sock, Checkpoints: store, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sendJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func frame(stream, data string) map[string]any {
	return map[string]any{"stream": stream, "data": base64.StdEncoding.EncodeToString([]byte(data))}
}

func sendFrames(w http.ResponseWriter, frames ...map[string]any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	fl, _ := w.(http.Flusher)
	for _, f := range frames {
		b, _ := json.Marshal(f)
		w.Write(append(b, '\n'))
		if fl != nil {
			fl.Flush()
		}
	}
}

var ctx = context.Background()

func TestHealthAndHeaders(t *testing.T) {
	m := newMock(t)
	m.on("GET", "/kosa8/v1/health", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"status": "ok", "version": "0.12.0"})
	})
	c, err := New(Options{SocketPath: m.sock, Actor: "agent", AgentToken: "at-1", Checkpoints: &MemoryCheckpointStore{}, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Version(ctx)
	if err != nil || v != "0.12.0" {
		t.Fatalf("version = %q, %v", v, err)
	}
	r, _ := m.last()
	if r.Header.Get(ActorHeader) != "agent" || r.Header.Get(AgentTokenHeader) != "at-1" {
		t.Fatalf("headers %v", r.Header)
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatal("bearer token must not go over the local socket")
	}
}

func TestCreateSandboxBodyAndEgress(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 201, map[string]any{"sandbox": map[string]any{"id": "demo"}})
	})
	c := m.client(t, nil)
	_, err := c.CreateSandbox(ctx, CreateOptions{Name: "demo", CPUs: 2, EgressAllow: []string{" pypi.org", "github.com"}, Labels: map[string]string{"team": "a"}, Approval: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	r, body := m.last()
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	if got["egress"] != "allow:pypi.org,github.com" || got["cpus"].(float64) != 2 || got["name"] != "demo" {
		t.Fatalf("body %s", body)
	}
	if _, ok := got["mem_mb"]; ok {
		t.Fatal("zero fields must be omitted")
	}
	if r.Header.Get(ApprovalHeader) != "sig" {
		t.Fatal("approval header missing")
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{Egress: "any", EgressAllow: []string{"x"}}); !errors.Is(err, &Error{Kind: KindUsage}) {
		t.Fatalf("want usage error, got %v", err)
	}
}

func TestTypedErrors(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/x/up", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 403, map[string]any{"error": "policy needs approval", "remediation": "sign it", "approval": map[string]any{"why": "egress any"}})
	})
	m.on("POST", "/kosa8/v1/sandboxes/x/down", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 409, map[string]any{"error": "already stopped"})
	})
	c := m.client(t, nil)
	_, err := c.StartSandbox(ctx, "x")
	e, ok := AsError(err)
	if !ok || e.Kind != KindApprovalRequired || e.Approval["why"] != "egress any" || e.Message != "policy needs approval (sign it)" {
		t.Fatalf("got %#v", err)
	}
	if !errors.Is(err, ErrPolicy) || !errors.Is(err, ErrAPI) || errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is classification wrong")
	}
	_, err = c.StopSandbox(ctx, "x")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	_, err = c.SandboxEgress(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestConnectionError(t *testing.T) {
	c, _ := New(Options{SocketPath: filepath.Join(os.TempDir(), "k8go-nope.sock"), Checkpoints: &MemoryCheckpointStore{}, Getenv: func(string) string { return "" }})
	_, err := c.Health(ctx)
	if !errors.Is(err, ErrConnection) || !strings.Contains(err.Error(), "kosa8 daemon start") {
		t.Fatalf("got %v", err)
	}
}

func TestRunStreams(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/sb/run", func(w http.ResponseWriter, r *http.Request) {
		sendFrames(w, frame("status", "pulling"), frame("stdout", "hel"), frame("stderr", "warn"), frame("stdout", "lo\n"),
			map[string]any{"done": true, "ok": true, "exit": 3})
	})
	c := m.client(t, nil)
	var live []string
	res, err := c.Run(ctx, "sb", "alpine", []string{"echo"}, &RunOptions{Env: []string{"A=1"}, OnStdout: func(b []byte) { live = append(live, string(b)) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout() != "hello\n" || res.Stderr() != "warn" || res.ExitCode != 3 || res.OK() || strings.Join(live, "|") != "hel|lo\n" {
		t.Fatalf("res %+v live %v", res, live)
	}
	_, body := m.last()
	if !strings.Contains(body, `"env":["A=1"]`) {
		t.Fatalf("body %s", body)
	}
}

func TestStreamErrorAndTruncation(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/sb/exec", func(w http.ResponseWriter, r *http.Request) {
		sendFrames(w, frame("stdout", "partial"), map[string]any{"done": true, "error": "container gone"})
	})
	m.on("POST", "/kosa8/v1/sandboxes/sb/containers/c1/attach", func(w http.ResponseWriter, r *http.Request) {
		sendFrames(w, frame("stdout", "cut"))
	})
	c := m.client(t, nil)
	_, err := c.Exec(ctx, "sb", "c1", []string{"ls"}, nil)
	e, _ := AsError(err)
	if e == nil || e.Kind != KindStream || e.Stdout != "partial" || e.Message != "container gone" {
		t.Fatalf("got %#v", err)
	}
	_, err = c.Attach(ctx, "sb", "c1", nil)
	e, _ = AsError(err)
	if e == nil || e.Kind != KindStream || !strings.Contains(e.Message, "no final frame") || e.Stdout != "cut" {
		t.Fatalf("got %#v", err)
	}
}

func TestStdinChunked(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/sb/containers/c%2F1/stdin", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sendJSON(w, 200, map[string]any{"bytes": len(b)})
	})
	m.on("POST", "/kosa8/v1/sandboxes/sb/containers/c/1/stdin", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sendJSON(w, 200, map[string]any{"bytes": len(b)})
	})
	c := m.client(t, nil)
	pr, pw := io.Pipe()
	go func() { pw.Write([]byte("abc")); pw.Write([]byte("de")); pw.Close() }()
	out, err := c.WriteStdin(ctx, "sb", "c/1", pr)
	if err != nil || out["bytes"].(float64) != 5 {
		t.Fatalf("out %v err %v", out, err)
	}
	r, _ := m.last()
	if r.Header.Get("Content-Type") != "application/octet-stream" || len(r.TransferEncoding) == 0 || r.TransferEncoding[0] != "chunked" {
		t.Fatalf("want chunked octet-stream, got %v %v", r.Header, r.TransferEncoding)
	}
	if !strings.Contains(r.RequestURI, "c%2F1") {
		t.Fatalf("container id must be escaped: %s", r.RequestURI)
	}
}

func TestForkAndExplore(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/p/pull", func(w http.ResponseWriter, r *http.Request) { sendJSON(w, 200, map[string]any{}) })
	m.on("POST", "/kosa8/v1/sandboxes/p/fork", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"forks": []any{map[string]any{"id": "e-1"}, map[string]any{"id": "e-2"}, map[string]any{"id": "e-3"}}, "fork_ms": 412, "count": 3})
	})
	var flaky int32
	run := func(delay time.Duration, exit int, out string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			sendFrames(w, frame("stdout", out), map[string]any{"done": true, "exit": exit})
		}
	}
	m.on("POST", "/kosa8/v1/sandboxes/e-1/run", run(1500*time.Millisecond, 0, "slow"))
	m.on("POST", "/kosa8/v1/sandboxes/e-2/run", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&flaky, 1) == 1 { // first try: engine still settling
			sendFrames(w, map[string]any{"done": true, "error": "connection refused"})
			return
		}
		run(0, 0, "fast")(w, r)
	})
	m.on("POST", "/kosa8/v1/sandboxes/e-3/run", run(0, 1, "fail"))
	var deleted []string
	var dmu sync.Mutex
	for _, n := range []string{"e-1", "e-2", "e-3"} {
		n := n
		m.on("DELETE", "/kosa8/v1/sandboxes/"+n, func(w http.ResponseWriter, r *http.Request) {
			dmu.Lock()
			deleted = append(deleted, n)
			dmu.Unlock()
			sendJSON(w, 200, map[string]any{})
		})
	}
	c := m.client(t, nil)
	fr, err := c.Fork(ctx, "p", 3, ForkOptions{Prefix: "e"})
	if err != nil || fr.Count != 3 || fr.ForkMS != 412 || len(fr.Forks) != 3 {
		t.Fatalf("fork %+v %v", fr, err)
	}
	res, err := c.Explore(ctx, "p", []string{"a", "b", "c"}, ExploreOptions{Prefix: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner != "e-2" || res.Removed != 2 || res.Branches[1].Output != "fast" || res.Branches[2].Exit != 1 {
		t.Fatalf("explore %+v", res)
	}
	if atomic.LoadInt32(&flaky) != 2 {
		t.Fatal("transient failure was not retried")
	}
	if _, err := c.Explore(ctx, "p", []string{"only"}, ExploreOptions{}); err == nil {
		t.Fatal("one variant must be refused")
	}
}

func checkpointMock(t *testing.T) (*mockDaemon, *[]string) {
	m := newMock(t)
	var log []string
	var mu sync.Mutex
	add := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	m.on("POST", "/kosa8/v1/sandboxes/sb/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		add("snap " + b["id"].(string))
		sendJSON(w, 200, map[string]any{"id": b["id"]})
	})
	m.on("DELETE", "/kosa8/v1/sandboxes/sb", func(w http.ResponseWriter, r *http.Request) {
		add("del sb")
		sendJSON(w, 200, map[string]any{"deleted": "sb"})
	})
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("sb-ckpt-%d", i)
		m.on("DELETE", "/kosa8/v1/snapshots/"+id, func(w http.ResponseWriter, r *http.Request) {
			add("dsnap " + id)
			sendJSON(w, 200, map[string]any{})
		})
		m.on("POST", "/kosa8/v1/snapshots/"+id+"/restore", func(w http.ResponseWriter, r *http.Request) {
			add("restore " + id)
			sendJSON(w, 200, map[string]any{"sandbox": map[string]any{"id": "sb"}, "restore_ms": 897})
		})
	}
	return m, &log
}

func TestCheckpointRewind(t *testing.T) {
	m, log := checkpointMock(t)
	c := m.client(t, nil)
	for _, l := range []string{"base", "deps", "broken"} {
		if _, err := c.Checkpoint(ctx, "sb", l); err != nil {
			t.Fatal(err)
		}
	}
	rw, err := c.Rewind(ctx, "sb", "DEPS")
	if err != nil {
		t.Fatal(err)
	}
	if rw.Checkpoint.ID != "sb-ckpt-2" || len(rw.Dropped) != 1 || rw.Dropped[0] != "sb-ckpt-3" || rw.RestoreMS.(float64) != 897 {
		t.Fatalf("rewind %+v", rw)
	}
	got := strings.Join(*log, ",")
	if !strings.Contains(got, "del sb,restore sb-ckpt-2,dsnap sb-ckpt-3") {
		t.Fatalf("order %s", got)
	}
	l, _ := c.ListCheckpoints("sb")
	if len(l) != 2 {
		t.Fatalf("left %v", l)
	}
	// A new checkpoint keeps counting up, past ids that were dropped.
	cr, _ := c.Checkpoint(ctx, "sb", "")
	if cr.Checkpoint.ID != "sb-ckpt-3" {
		t.Fatalf("next id %s", cr.Checkpoint.ID)
	}
	if _, err := c.Rewind(ctx, "sb", "nope"); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("want checkpoint error, got %v", err)
	}
	if _, err := c.Checkpoint(ctx, "../etc", ""); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("bad name accepted: %v", err)
	}
}

func TestCheckpointEvictionAndDelete(t *testing.T) {
	m, log := checkpointMock(t)
	c := m.client(t, nil)
	for i := 0; i < 11; i++ {
		if _, err := c.Checkpoint(ctx, "sb", ""); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := c.ListCheckpoints("sb")
	if len(l) != MaxCheckpointsPerSandbox || l[0].ID != "sb-ckpt-2" || l[9].ID != "sb-ckpt-11" {
		t.Fatalf("stack %v", l)
	}
	if !strings.Contains(strings.Join(*log, ","), "snap sb-ckpt-11,dsnap sb-ckpt-1") {
		t.Fatal("eviction must happen after the replacement snapshot exists")
	}
	out, err := c.DeleteSandbox(ctx, "sb", DeleteOptions{Wipe: true})
	if err != nil || out["checkpoints_dropped"] != 10 || out["deleted"] != "sb" {
		t.Fatalf("delete %v %v", out, err)
	}
	if l, _ := c.ListCheckpoints("sb"); len(l) != 0 {
		t.Fatal("checkpoints not forgotten")
	}
}

func TestFileStoreSharedWithCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoints.json")
	// What the Go CLI writes: RFC 3339 with nanoseconds.
	os.WriteFile(path, []byte(`{"sb":[{"id":"sb-ckpt-4","sandbox":"sb","label":"cli","created":"2026-11-01T10:00:00.123456789Z"}]}`), 0o600)
	s := NewFileCheckpointStore(path)
	b := checkpointBook{s}
	id, _, err := b.nextID("sb")
	if err != nil || id != "sb-ckpt-5" {
		t.Fatalf("id %s %v", id, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.record(Checkpoint{ID: fmt.Sprintf("other-ckpt-%d", i), Sandbox: "other"}, nil)
		}(i)
	}
	wg.Wait()
	d, _ := s.Load()
	if len(d["other"]) != 8 || d["sb"][0].Label != "cli" || d["sb"][0].Created.Nanosecond() != 123456789 {
		t.Fatalf("store %+v", d)
	}
	st, _ := os.Stat(path)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 { // Windows has no unix modes
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("lock file missing: the CLI locks the same file")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"auto"`) {
		t.Fatal("auto must be omitted when false, as the CLI writes it")
	}
}

func TestResolveEndpoint(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	cases := []struct {
		host   string
		env    map[string]string
		want   string
		token  string
		errHas string
	}{
		{"", nil, "unix:///s.sock", "", ""},
		{"", map[string]string{"KOSA8_HOST": "https://w.example.com/", "KOSA8_TOKEN": "t"}, "https://w.example.com", "t", ""},
		{"tcp://127.0.0.1:7070", nil, "http://127.0.0.1:7070", "", ""},
		{"localhost:7070", nil, "http://localhost:7070", "", ""},
		{"unix:///other.sock", nil, "unix:///other.sock", "", ""},
		{"http://10.0.0.5:7070", nil, "", "", "plain http"},
		{"", map[string]string{"KOSA8_HOST": "http://10.0.0.5:7070", "KOSA8_INSECURE": "1"}, "http://10.0.0.5:7070", "", ""},
		{"ftp://x", nil, "", "", "not supported"},
		{"https://w.example.com", map[string]string{"KOSA8_TOKEN": "t"}, "https://w.example.com", "", ""}, // env token only for env host
	}
	for _, tc := range cases {
		ep, err := ResolveEndpoint(tc.host, "/s.sock", "", false, env(tc.env))
		if tc.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("%q: want error %q, got %v", tc.host, tc.errHas, err)
			}
			continue
		}
		if err != nil || ep.Target() != tc.want || ep.Token != tc.token {
			t.Errorf("%q %v: got %s token %q err %v", tc.host, tc.env, ep.Target(), ep.Token, err)
		}
	}
}

func TestRemoteBearer(t *testing.T) {
	var auth string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		sendJSON(w, 200, map[string]any{"version": "x"})
	})}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	c, err := New(Options{Host: "http://" + ln.Addr().String(), Token: "tok", Checkpoints: &MemoryCheckpointStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(ctx); err != nil || auth != "Bearer tok" {
		t.Fatalf("auth %q err %v", auth, err)
	}
}

func TestFixesMountsCaptureExecEnv(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) { sendJSON(w, 201, map[string]any{"ok": true}) })
	c := m.client(t, nil)
	ctx := context.Background()
	if _, err := c.CreateSandbox(ctx, CreateOptions{Mounts: []string{"/src:/work:ro", "/data:/data"}, Capture: &CaptureOptions{Path: "/tmp/pcap"}}); err != nil {
		t.Fatal(err)
	}
	_, body := m.last()
	var b struct {
		Mounts  []map[string]any `json:"mounts"`
		Capture map[string]any   `json:"capture"`
	}
	_ = json.Unmarshal([]byte(body), &b)
	if len(b.Mounts) != 2 || b.Mounts[0]["guest_path"] != "/work" || b.Mounts[0]["read_only"] != true || b.Mounts[1]["read_only"] != false {
		t.Fatalf("mounts body %s", body)
	}
	if b.Capture["path"] != "/tmp/pcap" {
		t.Fatalf("capture body %s", body)
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{Mounts: []string{"rel/dir:/work"}}); !errors.Is(err, ErrUsage) {
		t.Fatalf("relative mount: %v", err)
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{Mounts: []string{"nocolon"}}); !errors.Is(err, ErrUsage) {
		t.Fatalf("bad mount: %v", err)
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{Capture: &CaptureOptions{}}); !errors.Is(err, ErrUsage) {
		t.Fatalf("capture without path: %v", err)
	}
	if _, err := c.Exec(ctx, "s", "c", []string{"true"}, &RunOptions{Env: []string{"A=1"}}); !errors.Is(err, ErrUsage) {
		t.Fatalf("exec env: %v", err)
	}
}

func TestFixesStdinOnceAndExecFramesError(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes/s/containers/c/stdin", func(w http.ResponseWriter, r *http.Request) { sendJSON(w, 200, map[string]any{"ok": true}) })
	m.on("POST", "/kosa8/v1/sandboxes/s/exec", func(w http.ResponseWriter, r *http.Request) {
		sendFrames(w, map[string]any{"done": true, "error": "no such container"})
	})
	c := m.client(t, nil)
	ctx := context.Background()
	if _, err := c.WriteStdin(ctx, "s", "c", strings.NewReader("hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteStdin(ctx, "s", "c", strings.NewReader("again\n")); !errors.Is(err, ErrUsage) {
		t.Fatalf("second stdin: %v", err)
	}
	seen := 0
	err := c.ExecFrames(ctx, "s", "c", []string{"x"}, func(Frame) error { seen++; return nil })
	if !errors.Is(err, ErrStream) || seen != 1 {
		t.Fatalf("exec frames: err=%v seen=%d", err, seen)
	}
}

func TestFixesCheckpointCollisionStepsID(t *testing.T) {
	m := newMock(t)
	var ids []string
	var mu sync.Mutex
	m.on("POST", "/kosa8/v1/sandboxes/s/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		ids = append(ids, b.ID)
		first := len(ids) == 1
		mu.Unlock()
		if first {
			sendJSON(w, 409, map[string]any{"error": "snapshot already exists: s-ckpt-1"})
			return
		}
		sendJSON(w, 201, map[string]any{"id": b.ID})
	})
	m.on("POST", "/kosa8/v1/sandboxes/t/snapshot", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 409, map[string]any{"error": "sandbox busy"})
	})
	c := m.client(t, nil)
	res, err := c.Checkpoint(context.Background(), "s", "a")
	if err != nil || res.Checkpoint.ID != "s-ckpt-2" || strings.Join(ids, ",") != "s-ckpt-1,s-ckpt-2" {
		t.Fatalf("res=%+v err=%v ids=%v", res, err, ids)
	}
	if _, err := c.Checkpoint(context.Background(), "t", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("other conflict: %v", err)
	}
}
