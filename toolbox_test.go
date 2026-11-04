package kosa8

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const tbS = "/kosa8/v1/sandboxes/sb1"

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("body %q: %v", s, err)
	}
	return m
}

func TestRunCodeDecodesArtifacts(t *testing.T) {
	m := newMock(t)
	png := []byte("\x89PNG fake")
	m.on("POST", tbS+"/code", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"language": "python", "image": "python:3.12-slim", "exit": 0, "stdout": "hi\n", "stderr": "",
			"artifacts":   []map[string]any{{"type": "image/png", "name": "figure-1.png", "data": base64.StdEncoding.EncodeToString(png)}},
			"duration_ms": 12})
	})
	c := m.client(t, nil)
	res, err := c.Sandbox("sb1").RunCode(ctx, "print('hi')", &CodeOptions{Env: map[string]string{"A": "1"}, Timeout: 10 * time.Second, Approval: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.Stdout != "hi\n" || res.Image != "python:3.12-slim" || res.DurationMS != 12 {
		t.Fatalf("%+v", res)
	}
	if len(res.Artifacts) != 1 || !bytes.Equal(res.Artifacts[0].Data, png) || res.Artifacts[0].Name != "figure-1.png" {
		t.Fatalf("artifacts %+v", res.Artifacts)
	}
	r, body := m.last()
	want := map[string]any{"language": "python", "code": "print('hi')", "env": map[string]any{"A": "1"}, "timeout_s": 10.0}
	if got := jsonOf(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("body %v", got)
	}
	if r.Header.Get(ApprovalHeader) != "sig" {
		t.Fatal("approval header missing")
	}
}

func TestRunCodeTimedOutIsNotOK(t *testing.T) {
	m := newMock(t)
	m.on("POST", tbS+"/code", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"exit": 124, "stderr": "killed", "timed_out": true})
	})
	res, err := m.client(t, nil).RunCode(ctx, "sb1", "while True: pass", &CodeOptions{Language: "python"})
	if err != nil || res.OK() || !res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFilesListReadWriteDelete(t *testing.T) {
	m := newMock(t)
	m.on("GET", tbS+"/files", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"path": "/workspace", "files": []map[string]any{{"name": "a.txt", "type": "file", "size": 3, "mod_time": "t"}}})
	})
	m.on("GET", tbS+"/files/stat", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"name": "a.txt", "path": "/workspace/a.txt", "type": "file", "size": 5, "mode": 420})
	})
	m.on("GET", tbS+"/files/content", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(200)
		w.Write([]byte("hello"))
	})
	m.on("PUT", tbS+"/files/content", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sendJSON(w, 200, map[string]any{"path": "/workspace/b.txt", "type": "file", "size": len(b)})
	})
	m.on("DELETE", tbS+"/files", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"removed": r.URL.Query().Get("path")})
	})
	m.on("POST", tbS+"/files/mkdir", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"path": "/workspace/d", "type": "dir"})
	})
	m.on("POST", tbS+"/files/move", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"path": "/workspace/c.txt", "type": "file"})
	})
	sb := m.client(t, nil).Sandbox("sb1")

	l, err := sb.ListFiles(ctx, "")
	if err != nil || len(l) != 1 || l[0].Name != "a.txt" || l[0].ModTime != "t" {
		t.Fatalf("%v %v", l, err)
	}
	if r, _ := m.last(); r.URL.Query().Get("path") != "/workspace" {
		t.Fatalf("query %s", r.URL.RawQuery)
	}
	st, err := sb.StatFile(ctx, "a.txt")
	if err != nil || st.Size != 5 || st.Mode != 0o644 || st.IsDir() {
		t.Fatalf("%+v %v", st, err)
	}
	b, err := sb.ReadFile(ctx, "a.txt")
	if err != nil || string(b) != "hello" {
		t.Fatalf("%q %v", b, err)
	}
	fi, err := sb.WriteFileBytes(ctx, "b.txt", []byte("héllo"), &WriteOptions{Mode: "0644"})
	if err != nil || fi.Size != 6 {
		t.Fatalf("%+v %v", fi, err)
	}
	r, body := m.last()
	if body != "héllo" || r.URL.Query().Get("mode") != "0644" || r.ContentLength != 6 || r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("put %q %s len=%d", body, r.URL.RawQuery, r.ContentLength)
	}
	// An unknown-length reader goes chunked.
	pr, pw := io.Pipe()
	go func() { pw.Write([]byte("streamed")); pw.Close() }()
	if fi, err = sb.WriteFile(ctx, "s.txt", pr, nil); err != nil || fi.Size != 8 {
		t.Fatalf("%+v %v", fi, err)
	}
	if _, err := sb.DeleteFile(ctx, "d", &DeleteFileOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.last(); r.URL.Query().Get("recursive") != "1" || r.URL.Query().Get("path") != "d" {
		t.Fatalf("delete %s", r.URL.RawQuery)
	}
	if _, err := sb.DeleteFile(ctx, "x", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.last(); r.URL.Query().Has("recursive") {
		t.Fatalf("delete %s", r.URL.RawQuery)
	}
	if fi, err := sb.MakeDir(ctx, "d"); err != nil || !fi.IsDir() {
		t.Fatalf("%+v %v", fi, err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"path": "d"}) {
		t.Fatal(body)
	}
	if _, err := sb.MoveFile(ctx, "b.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"from": "b.txt", "to": "c.txt"}) {
		t.Fatal(body)
	}
}

func TestReadFileErrorsAreTyped(t *testing.T) {
	m := newMock(t)
	m.on("GET", tbS+"/files/content", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "short" {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(200)
			w.Write([]byte("abc"))
			return
		}
		sendJSON(w, 404, map[string]any{"error": "no such file", "remediation": "list the folder"})
	})
	c := m.client(t, nil)
	_, err := c.ReadFile(ctx, "sb1", "missing")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "list the folder") {
		t.Fatalf("%v", err)
	}
	_, err = c.ReadFile(ctx, "sb1", "short")
	if !errors.Is(err, ErrStream) {
		t.Fatalf("short read: %v", err)
	}
}

func TestGitCommitStagesFirst(t *testing.T) {
	m := newMock(t)
	var ops []map[string]any
	m.on("POST", tbS+"/git", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		ops = append(ops, req)
		switch req["op"] {
		case "status":
			sendJSON(w, 200, map[string]any{"op": "status", "ok": true, "exit": 0, "path": "/workspace/r",
				"status": map[string]any{"branch": "main", "clean": false, "files": []map[string]any{{"path": "a", "index": "?", "worktree": "?"}}}})
		case "add":
			ok := req["path"] != "/workspace/bad"
			sendJSON(w, 200, map[string]any{"op": "add", "ok": ok, "exit": map[bool]int{true: 0, false: 128}[ok], "stderr": "fatal"})
		default:
			sendJSON(w, 200, map[string]any{"op": req["op"], "ok": true, "exit": 0, "stdout": "[main abc] m"})
		}
	})
	sb := m.client(t, nil).Sandbox("sb1")

	res, err := sb.GitClone(ctx, "https://github.com/o/r", "/workspace/r", &GitOptions{Depth: 1, Token: "tok"})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	want := map[string]any{"op": "clone", "url": "https://github.com/o/r", "path": "/workspace/r", "depth": 1.0, "token": "tok"}
	if !reflect.DeepEqual(ops[0], want) {
		t.Fatalf("clone body %v", ops[0])
	}
	st, err := sb.GitStatus(ctx, "/workspace/r", nil)
	if err != nil || st.Status == nil || st.Status.Branch != "main" || len(st.Status.Files) != 1 || st.Status.Files[0].Index != "?" {
		t.Fatalf("%+v %v", st, err)
	}
	ops = nil
	res, err = sb.GitCommit(ctx, "/workspace/r", "m", &GitOptions{AuthorName: "A", AuthorEmail: "a@x"})
	if err != nil || !res.OK || res.Op != "commit" || len(ops) != 2 {
		t.Fatalf("%+v %v %v", res, err, ops)
	}
	if ops[0]["op"] != "add" || ops[1]["op"] != "commit" || ops[1]["message"] != "m" || ops[1]["author_name"] != "A" {
		t.Fatalf("ops %v", ops)
	}
	ops = nil
	res, err = sb.GitCommit(ctx, "/workspace/bad", "m", nil)
	if err != nil || res.OK || res.Op != "add" || res.ExitCode != 128 || len(ops) != 1 {
		t.Fatalf("failed add must stop the commit: %+v %v %v", res, err, ops)
	}
	ops = nil
	if _, err = sb.GitCommit(ctx, "/workspace/r", "m", &GitOptions{NoStage: true}); err != nil || len(ops) != 1 || ops[0]["op"] != "commit" {
		t.Fatalf("NoStage: %v %v", err, ops)
	}
	if _, ok := ops[0]["no_stage"]; ok {
		t.Fatal("NoStage must not be sent")
	}
}

func TestSessionsAndShell(t *testing.T) {
	m := newMock(t)
	created := false
	m.on("POST", tbS+"/sessions", func(w http.ResponseWriter, r *http.Request) {
		created = true
		sendJSON(w, 200, map[string]any{"name": "main", "id": "c1", "image": "alpine", "status": "running"})
	})
	m.on("GET", tbS+"/sessions", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, []map[string]any{{"name": "main", "id": "c1"}})
	})
	m.on("POST", tbS+"/sessions/main/exec", func(w http.ResponseWriter, r *http.Request) {
		if !created {
			sendJSON(w, 404, map[string]any{"error": "no such session: main", "remediation": "create it with POST …/sessions"})
			return
		}
		sendJSON(w, 200, map[string]any{"exit": 0, "stdout": "/tmp\n", "cwd": "/tmp", "duration_ms": 3})
	})
	m.on("DELETE", tbS+"/sessions/main", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"deleted": "main"})
	})
	sb := m.client(t, nil).Sandbox("sb1")

	res, err := sb.Shell(ctx, "cd /tmp && pwd", &ShellOptions{Timeout: 5 * time.Second})
	if err != nil || !res.OK() || res.Cwd != "/tmp" || !created {
		t.Fatalf("%+v %v", res, err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"command": "cd /tmp && pwd", "timeout_s": 5.0}) {
		t.Fatal(body)
	}
	l, err := sb.Sessions(ctx)
	if err != nil || len(l) != 1 || l[0].ID != "c1" {
		t.Fatalf("%v %v", l, err)
	}
	s, err := sb.CreateSession(ctx, "main", &SessionOptions{Image: "alpine", Env: map[string]string{"K": "V"}})
	if err != nil || s.Status != "running" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"name": "main", "image": "alpine", "env": map[string]any{"K": "V"}}) {
		t.Fatal(body)
	}
	if _, err := sb.DeleteSession(ctx, "main"); err != nil {
		t.Fatal(err)
	}
}

func TestShellDoesNotCreateOnOther404(t *testing.T) {
	m := newMock(t)
	calls := 0
	m.on("POST", tbS+"/sessions/main/exec", func(w http.ResponseWriter, r *http.Request) {
		calls++
		sendJSON(w, 404, map[string]any{"error": "no such sandbox: sb1"})
	})
	_, err := m.client(t, nil).Shell(ctx, "sb1", "ls", nil)
	if !errors.Is(err, ErrNotFound) || calls != 1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
	r, _ := m.last()
	if r.URL.Path != tbS+"/sessions/main/exec" {
		t.Fatalf("must not create a session: %s", r.URL.Path)
	}
}

func TestPTYPreviewLifecycle(t *testing.T) {
	m := newMock(t)
	m.on("POST", tbS+"/pty", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"id": "c9", "image": "alpine", "resized": true})
	})
	m.on("POST", tbS+"/containers/c9/resize", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"ok": true})
	})
	m.on("POST", tbS+"/previews", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"url": "http://p/x", "token": "t", "expires_at": "2026-01-01T00:00:00Z", "container": "c9", "port": 8000, "listen": "127.0.0.1:1"})
	})
	m.on("POST", "/kosa8/v1/previews/rotate", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"ok": true})
	})
	m.on("POST", tbS+"/lifecycle", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"id": "sb1"})
	})
	c := m.client(t, nil)
	sb := c.Sandbox("sb1")

	id, err := sb.StartPTY(ctx, &PTYOptions{Command: []string{"sh"}, Cols: 80, Rows: 24})
	if err != nil || id != "c9" {
		t.Fatalf("%q %v", id, err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"command": []any{"sh"}, "cols": 80.0, "rows": 24.0}) {
		t.Fatal(body)
	}
	if TTYPath("sb1", "c9") != tbS+"/containers/c9/tty" {
		t.Fatal(TTYPath("sb1", "c9"))
	}
	if err := sb.ResizePTY(ctx, "c9", 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := sb.ResizePTY(ctx, "c9", 0, 30); !errors.Is(err, ErrUsage) {
		t.Fatalf("%v", err)
	}
	p, err := sb.CreatePreview(ctx, 8000, &PreviewOptions{Session: "main", ExpiresIn: time.Minute})
	if err != nil || p.URL != "http://p/x" || p.Port != 8000 || p.Container != "c9" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"port": 8000.0, "session": "main", "expires_in_s": 60.0}) {
		t.Fatal(body)
	}
	if _, err := c.RevokePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.SetLifecycle(ctx, LifecycleOptions{AutoStopMin: Ptr(30), TTLMin: Ptr(0), Ephemeral: Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"auto_stop_min": 30.0, "ttl_min": 0.0, "ephemeral": false}) {
		t.Fatalf("lifecycle body %s (nil fields must be left out, 0 sent)", body)
	}
}

func TestCreateSandboxLifecycleAndTemplate(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/kosa8/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 201, map[string]any{"id": "x"})
	})
	c := m.client(t, nil)
	if _, err := c.CreateSandbox(ctx, CreateOptions{Name: "x", AutoStopMin: 5, TTLMin: 60, AutoDeleteMin: 10, Ephemeral: true, Template: "py"}); err != nil {
		t.Fatal(err)
	}
	_, body := m.last()
	want := map[string]any{"name": "x", "auto_stop_min": 5.0, "ttl_min": 60.0, "auto_delete_min": 10.0, "ephemeral": true, "template": "py"}
	if got := jsonOf(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{Name: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, body := m.last(); !reflect.DeepEqual(jsonOf(t, body), map[string]any{"name": "y"}) {
		t.Fatalf("zero lifecycle must be left out: %s", body)
	}
	if _, err := c.CreateSandbox(ctx, CreateOptions{TTLMin: -1}); !errors.Is(err, ErrUsage) {
		t.Fatalf("%v", err)
	}
}

func buildContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM alpine\n")
	write("app/main.py", "print(1)\n")
	write("node_modules/x/y.js", "big")
	write("secret.env", "K=V")
	write("app/cache/z", "z")
	write(".dockerignore", "# comment\nnode_modules\n/secret.env\n./app/cache\n")
	return dir
}

func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}

func TestTarContextHonoursDockerignore(t *testing.T) {
	dir := buildContext(t)
	b, err := tarContext(dir, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".dockerignore", "Dockerfile", "app/", "app/main.py"}
	if got := tarNames(t, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("tar has %v, want %v", got, want)
	}
	if _, err := tarContext(dir, "Missing.Dockerfile"); !errors.Is(err, ErrUsage) {
		t.Fatalf("%v", err)
	}
}

func TestBuildTemplate(t *testing.T) {
	m := newMock(t)
	var gotTar []byte
	m.on("POST", "/kosa8/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		gotTar, _ = io.ReadAll(r.Body)
		if r.URL.Query().Get("name") == "bad" {
			sendFrames(w, frame("status", "booting\n"), map[string]any{"done": true, "error": "build: step 2 failed"})
			return
		}
		sendFrames(w, frame("status", "booting\n"), frame("stdout", "Step 1/1\n"),
			map[string]any{"done": true, "ok": true, "image": "kosa8-template/py:latest"})
	})
	m.on("GET", "/kosa8/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, []map[string]any{{"name": "py", "image": "kosa8-template/py:latest", "size_bytes": 10, "created": "2026-11-03T00:00:00Z"}})
	})
	m.on("DELETE", "/kosa8/v1/templates/py", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"deleted": "py"})
	})
	c := m.client(t, nil)
	dir := buildContext(t)
	var out bytes.Buffer
	res, err := c.BuildTemplate(ctx, "py", dir, &BuildOptions{BuildArgs: map[string]string{"B": "2", "A": "1"}, Egress: "allow:pypi.org", Replace: true,
		OnOutput: func(b []byte) { out.Write(b) }})
	if err != nil || res.Image != "kosa8-template/py:latest" {
		t.Fatalf("%+v %v", res, err)
	}
	if out.String() != "booting\nStep 1/1\n" {
		t.Fatalf("output %q", out.String())
	}
	r, _ := m.last()
	q := r.URL.Query()
	if q.Get("name") != "py" || q.Get("dockerfile") != "Dockerfile" || !reflect.DeepEqual(q["buildarg"], []string{"A=1", "B=2"}) ||
		q.Get("egress") != "allow:pypi.org" || q.Get("replace") != "1" {
		t.Fatalf("query %s", r.URL.RawQuery)
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || !reflect.DeepEqual(tarNames(t, gotTar), []string{".dockerignore", "Dockerfile", "app/", "app/main.py"}) {
		t.Fatalf("body is not the context tar")
	}
	_, err = c.BuildTemplate(ctx, "bad", dir, nil)
	if e, ok := AsError(err); !ok || e.Kind != KindStream || e.DaemonMessage != "build: step 2 failed" {
		t.Fatalf("%v", err)
	}
	if r, _ := m.last(); r.URL.Query().Has("replace") || r.URL.Query().Has("egress") {
		t.Fatalf("defaults must be left out: %s", r.URL.RawQuery)
	}
	l, err := c.ListTemplates(ctx)
	if err != nil || len(l) != 1 || l[0].SizeBytes != 10 || l[0].Created.Year() != 2026 {
		t.Fatalf("%v %v", l, err)
	}
	if _, err := c.DeleteTemplate(ctx, "py"); err != nil {
		t.Fatal(err)
	}
}
