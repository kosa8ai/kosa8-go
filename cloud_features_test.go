package kosa8

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCloudSizedCreateWaitsForRunning(t *testing.T) {
	ctx := context.Background()
	gets := 0
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		if r.Method == "POST" {
			return rawAnswer{status: 202, json: map[string]any{"sandbox": map[string]any{"id": "sbx_9", "state": "creating"}}}
		}
		gets++
		state := "creating"
		if gets >= 2 {
			state = "running"
		}
		return rawAnswer{status: 200, json: map[string]any{"sandbox": map[string]any{
			"id": "sbx_9", "state": state, "persistent": true, "name": "web", "image": "img:1", "disk_gb": 64,
			"cpu": "dedicated", "snapshot_id": nil, "labels": map[string]any{"team": "a"}}}}
	})
	b, err := c.Sandboxes.Create(ctx, CloudCreateOptions{
		Size: "4x8", Image: "kosa8-template/universal-24.04-20261115.2", DiskGB: 64, Docker: true, Persistent: true,
		CPU: "dedicated", Region: "us", Name: "web", Labels: map[string]string{"team": "a"},
		Env: map[string]string{"GREETING": "hi"}, Secrets: []string{"openai"},
		Egress: &CloudEgress{Allow: []string{"pypi.org"}}, SleepAfterS: Ptr(0), TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != "running" || gets != 2 || !b.Info.Persistent || *b.Info.Name != "web" || *b.Info.DiskGB != 64 ||
		b.Info.CPU != "dedicated" || b.Info.Labels["team"] != "a" {
		t.Fatalf("%+v gets=%d", b.Info, gets)
	}
	body := stub.seen[0].JSON(t)
	want := map[string]any{
		"size": "4x8", "image": "kosa8-template/universal-24.04-20261115.2", "disk_gb": float64(64), "docker": true,
		"persistent": true, "cpu": "dedicated", "region": "us", "name": "web", "labels": map[string]any{"team": "a"},
		"env": map[string]any{"GREETING": "hi"}, "secrets": []any{"openai"}, "sleep_after_s": float64(0), "ttl_s": float64(3600),
		"egress": map[string]any{"internet": false, "allow": []any{"pypi.org"}, "deny": []any{}, "presets": []any{}},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body %v", body)
	}
}

func TestCloudCreateNoWaitAndFailedBoot(t *testing.T) {
	ctx := context.Background()
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		if r.Method == "POST" {
			return rawAnswer{status: 202, json: map[string]any{"sandbox": map[string]any{"id": "sbx_9", "state": "creating"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"sandbox": map[string]any{"id": "sbx_9", "state": "error", "last_error": "no image"}}}
	})
	b, err := c.Sandboxes.Create(ctx, CloudCreateOptions{CPUs: 2, MemMB: 4096, NoWait: true})
	if err != nil || b.State() != "creating" {
		t.Fatalf("%v %v", b, err)
	}
	if err := b.WaitRunning(ctx, time.Millisecond); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("want boot error, got %v", err)
	}
}

func TestCloudCreateValidation(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer { t.Fatal("no request expected"); return rawAnswer{} })
	bad := []CloudCreateOptions{
		{Size: "3x5"},
		{CPUs: 2},
		{Size: "2x4", CPUs: 2, MemMB: 4096},
		{DiskGB: 4},
		{CPU: "fast"},
		{Name: "-bad"},
		{Env: map[string]string{"1X": "y"}},
		{Env: map[string]string{"X": strings.Repeat("a", CloudMaxEnvValue+1)}},
		{Labels: map[string]string{"bad key": "v"}},
		{Snapshot: "sbx_1"},
		{Snapshot: "snap_abcdefgh", Size: "2x4"},
		{Secrets: []string{"Bad_Name"}},
	}
	for i, o := range bad {
		if _, err := c.Sandboxes.Create(context.Background(), o); !errors.Is(err, ErrUsage) {
			t.Fatalf("%d: %+v: want usage error, got %v", i, o, err)
		}
	}
}

func TestCloudCreateFromSnapshot(t *testing.T) {
	stub, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 201, json: map[string]any{"sandbox": map[string]any{"id": "sbx_3", "state": "running", "snapshot_id": "snap_abcdefgh"}}}
	})
	b, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{Snapshot: "snap_abcdefgh", Env: map[string]string{"A": "1"}, Name: "copy"})
	if err != nil || *b.Info.SnapshotID != "snap_abcdefgh" {
		t.Fatal(err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"snapshot": "snap_abcdefgh", "env": map[string]any{"A": "1"}, "name": "copy"}) {
		t.Fatalf("body %v", got)
	}
}

func TestCloudListNameAndLabels(t *testing.T) {
	stub, c := newRawStub(t, func(rawReq) rawAnswer { return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{}}} })
	if _, err := c.Sandboxes.ListPage(context.Background(), CloudListOptions{Name: "web", Labels: map[string]string{"team": "a", "ci": ""}}); err != nil {
		t.Fatal(err)
	}
	if p := stub.last().Path; p != "/v1/sandboxes?label=ci&label=team%3Da&name=web" {
		t.Fatalf("path %s", p)
	}
}

func TestCloudBackgroundExec(t *testing.T) {
	ctx := context.Background()
	polls := 0
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		if r.Method == "POST" {
			return rawAnswer{status: 202, json: map[string]any{"exec": map[string]any{"id": "bg_0123456789abcdef", "state": "running", "pid": 7}}}
		}
		polls++
		if polls < 2 {
			return rawAnswer{status: 200, json: map[string]any{"exec": map[string]any{"id": "bg_0123456789abcdef", "state": "running", "output": "a"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"exec": map[string]any{"id": "bg_0123456789abcdef", "state": "exited", "exit": 0, "output": "ab"}}}
	})
	b := cloudBox(t, c)
	e, err := b.ExecBackground(ctx, []string{"sleep", "60"}, &CloudExecOptions{Env: map[string]string{"B": "2", "A": "1"}})
	if err != nil || e.ID != "bg_0123456789abcdef" || e.Done() || *e.PID != 7 {
		t.Fatalf("%+v %v", e, err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"cmd": []any{"sleep", "60"}, "background": true, "env": []any{"A=1", "B=2"}}) {
		t.Fatalf("body %v", got)
	}
	e, err = b.WaitBackground(ctx, e.ID, time.Millisecond)
	if err != nil || !e.Done() || *e.ExitCode != 0 || *e.Output != "ab" {
		t.Fatalf("%+v %v", e, err)
	}
	if p := stub.last().Path; p != "/v1/sandboxes/sbx_1/exec/bg_0123456789abcdef" {
		t.Fatalf("path %s", p)
	}
	if _, err := b.ExecBackground(ctx, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

// TimeoutS was dropped on the way to a background exec, so a runaway
// background command ran until the sandbox was deleted.
func TestCloudBackgroundExecTimeout(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		if r.Method == "POST" {
			return rawAnswer{status: 202, json: map[string]any{"exec": map[string]any{"id": "bg_1", "state": "running", "timeout_s": 600}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"exec": map[string]any{"id": "bg_1", "state": "exited", "exit": 137, "timeout_s": 600, "timed_out": true, "stopped": false}}}
	})
	b := cloudBox(t, c)
	e, err := b.ExecBackground(ctx, []string{"sleep", "9999"}, &CloudExecOptions{TimeoutS: 600})
	if err != nil || e.TimeoutS == nil || *e.TimeoutS != 600 {
		t.Fatalf("%+v %v", e, err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"cmd": []any{"sleep", "9999"}, "background": true, "timeout_s": float64(600)}) {
		t.Fatalf("body %v", got)
	}
	e, err = b.BackgroundExec(ctx, "bg_1")
	if err != nil || !e.TimedOut || e.Stopped || !e.Done() {
		t.Fatalf("%+v %v", e, err)
	}
	for _, bad := range []int{-1, CloudMaxTimeoutS + 1} {
		if _, err := b.ExecBackground(ctx, []string{"true"}, &CloudExecOptions{TimeoutS: bad}); !errors.Is(err, ErrUsage) {
			t.Fatalf("TimeoutS %d: %v", bad, err)
		}
	}
}

func TestCloudSnapshots(t *testing.T) {
	ctx := context.Background()
	snap := map[string]any{"id": "snap_abcdefgh", "name": "warm", "state": "ready", "source_sandbox_id": "sbx_1", "bytes": 1024, "created_at": 1}
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "POST":
			return rawAnswer{status: 201, json: map[string]any{"snapshot": snap}}
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"ok": true, "id": "snap_abcdefgh"}}
		case strings.HasPrefix(r.Path, "/v1/snapshots/"):
			return rawAnswer{status: 200, json: map[string]any{"snapshot": snap}}
		}
		return rawAnswer{status: 200, json: map[string]any{"snapshots": []any{snap}, "max": 20}}
	})
	s, err := cloudBox(t, c).Snapshot(ctx, "warm")
	if err != nil || s.ID != "snap_abcdefgh" || *s.Name != "warm" || s.Bytes != 1024 {
		t.Fatalf("%+v %v", s, err)
	}
	if r := stub.last(); r.Path != "/v1/sandboxes/sbx_1/snapshots" || !reflect.DeepEqual(r.JSON(t), map[string]any{"name": "warm"}) {
		t.Fatalf("%+v", r)
	}
	l, err := c.Snapshots.List(ctx, "sbx_1")
	if err != nil || len(l) != 1 || stub.last().Path != "/v1/snapshots?sandbox_id=sbx_1" {
		t.Fatalf("%v %v", l, err)
	}
	if _, err := c.Snapshots.Get(ctx, "snap_abcdefgh"); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshots.Delete(ctx, "snap_abcdefgh"); err != nil || stub.last().Method != "DELETE" {
		t.Fatal(err)
	}
}

func TestCloudPreviews(t *testing.T) {
	ctx := context.Background()
	pv := map[string]any{"port": 3000, "public": false, "url": "https://p-x.kosa8labs.com/?t=tok", "host": "p-x.kosa8labs.com", "token": "tok"}
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"deleted": true, "port": 3000}}
		case r.Method == "GET":
			return rawAnswer{status: 200, json: map[string]any{"previews": []any{pv}, "configured": true}}
		}
		return rawAnswer{status: 200, json: map[string]any{"preview": pv}}
	})
	p := cloudBox(t, c).Previews()
	got, err := p.Create(ctx, 3000, &CloudPreviewOptions{ExpiresInS: 600, Wake: Ptr(true)})
	if err != nil || !strings.Contains(got.URL, "kosa8labs.com") {
		t.Fatalf("%+v %v", got, err)
	}
	if b := stub.last().JSON(t); !reflect.DeepEqual(b, map[string]any{"port": float64(3000), "expires_in_s": float64(600), "wake": true}) {
		t.Fatalf("body %v", b)
	}
	if l, err := p.List(ctx); err != nil || len(l) != 1 {
		t.Fatal(err)
	}
	if _, err := p.Link(ctx, 3000, 60); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/previews/3000/link" {
		t.Fatal(err)
	}
	if _, err := p.Rotate(ctx, 3000); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/previews/3000/rotate" {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, 3000); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/previews/3000" {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, 0, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestCloudTerminalAndFileLinkAndRaw(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.HasSuffix(r.Path, "/terminal"):
			return rawAnswer{status: 200, json: map[string]any{"terminal": map[string]any{"url": "wss://h/t?x", "expires_at": 5, "protocol": "kosa8-pty-1"}}}
		case strings.HasSuffix(r.Path, "/files/link"):
			return rawAnswer{status: 200, json: map[string]any{"link": map[string]any{"url": "https://h/f?sig", "method": "PUT", "path": "/workspace/a", "expires_at": 9}}}
		case r.Method == "GET":
			return rawAnswer{status: 200, raw: []byte("big file")}
		}
		return rawAnswer{status: 200, json: map[string]any{"file": map[string]any{"path": "/workspace/a", "size": len(r.Body)}}}
	})
	b := cloudBox(t, c)
	term, err := b.Terminal(ctx, &CloudTerminalOptions{Cols: 80, Rows: 24, Command: []string{"bash"}})
	if err != nil || term.URL != "wss://h/t?x" {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"cols": float64(80), "rows": float64(24), "command": []any{"bash"}}) {
		t.Fatalf("body %v", body)
	}
	l, err := b.Files().Link(ctx, "/workspace/a", "put", 120)
	if err != nil || l.Method != "PUT" {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"path": "/workspace/a", "method": "PUT", "ttl_s": float64(120)}) {
		t.Fatalf("body %v", body)
	}
	if _, err := b.Files().Link(ctx, "/a", "POST", 0); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := b.Files().Download(ctx, "/workspace/a", &buf)
	if err != nil || n != 8 || buf.String() != "big file" || stub.last().Path != "/v1/sandboxes/sbx_1/files/raw?path=%2Fworkspace%2Fa" {
		t.Fatalf("%d %q %v %s", n, buf.String(), err, stub.last().Path)
	}
	fi, err := b.Files().Upload(ctx, "/workspace/a", strings.NewReader("hello"))
	if err != nil || fi.Size != 5 || stub.last().ContentType != "application/octet-stream" {
		t.Fatalf("%+v %v", fi, err)
	}
}

func TestCloudStreamErrorIsTyped(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 404, json: map[string]any{"error": "not_found", "message": "no such file"}}
	})
	_, err := cloudBox(t, c).Files().Download(context.Background(), "/x", &bytes.Buffer{})
	var e *Error
	if !errors.As(err, &e) || e.Code != "not_found" || !strings.Contains(e.Message, "no such file") {
		t.Fatalf("%v", err)
	}
}

func TestCloudEgress(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		if strings.HasSuffix(r.Path, "/presets") {
			return rawAnswer{status: 200, json: map[string]any{"presets": map[string]any{"pypi": []any{"pypi.org"}}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"egress": map[string]any{"internet": false, "allow": []any{"github.com"}, "source": "sandbox"}}}
	})
	b := cloudBox(t, c)
	e, err := b.SetEgress(ctx, CloudEgress{Presets: []string{"pypi"}, Allow: []string{"github.com"}})
	if err != nil || e.Source != "sandbox" {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"internet": false, "allow": []any{"github.com"}, "deny": []any{}, "presets": []any{"pypi"}}) {
		t.Fatalf("body %v", body)
	}
	if _, err := b.Egress(ctx); err != nil || stub.last().Method != "GET" {
		t.Fatal(err)
	}
	if _, err := b.ResetEgress(ctx); err != nil || stub.last().Method != "DELETE" {
		t.Fatal(err)
	}
	if p, err := c.EgressPresets(ctx); err != nil || p["pypi"] == nil {
		t.Fatal(err)
	}
}

func TestCloudSecrets(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "PUT":
			return rawAnswer{status: 201, json: map[string]any{"secret": map[string]any{"name": "npm-token", "pipelines": true}, "replaced": true}}
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"deleted": "npm-token", "still_held_by": []any{"sbx_1"}}}
		case strings.HasSuffix(r.Path, "/secrets") && strings.Contains(r.Path, "sandboxes"):
			return rawAnswer{status: 200, json: map[string]any{"id": "sbx_1", "secrets": []any{"npm-token"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"secrets": []any{map[string]any{"name": "npm-token"}}}}
	})
	spec := CloudSecretSpec{Value: "s3cr3t", Upstream: "https://registry.npmjs.org", Pipelines: true}
	if strings.Contains(fmt.Sprintf("%#v", spec), "s3cr3t") {
		t.Fatal("GoString leaks the value")
	}
	s, replaced, err := c.Secrets.Set(ctx, "npm-token", spec)
	if err != nil || !replaced || !s.Pipelines {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"value": "s3cr3t", "upstream": "https://registry.npmjs.org", "pipelines": true}) {
		t.Fatalf("body %v", body)
	}
	if l, err := c.Secrets.List(ctx); err != nil || len(l) != 1 {
		t.Fatal(err)
	}
	held, err := c.Secrets.Delete(ctx, "npm-token")
	if err != nil || !reflect.DeepEqual(held, []string{"sbx_1"}) {
		t.Fatal(held, err)
	}
	if n, err := cloudBox(t, c).Secrets(ctx); err != nil || n[0] != "npm-token" {
		t.Fatal(err)
	}
	if _, _, err := c.Secrets.Set(ctx, "Bad Name", spec); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, _, err := c.Secrets.Set(ctx, "ok", CloudSecretSpec{}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestCloudSchedules(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"deleted": true}}
		case strings.HasSuffix(r.Path, "/run"):
			return rawAnswer{status: 200, json: map[string]any{"run": map[string]any{"status": "ok", "exit": 0, "output": "hi"}}}
		case r.Method == "POST":
			return rawAnswer{status: 201, json: map[string]any{"schedule": map[string]any{"id": "sch_1", "cron": "0 * * * *", "cmd": []any{"echo", "hi"}}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"schedules": []any{map[string]any{"id": "sch_1", "last_run": map[string]any{"status": "ok"}}}}}
	})
	s := cloudBox(t, c).Schedules()
	got, err := s.Create(ctx, CloudScheduleSpec{Cron: "0 * * * *", Cmd: []string{"echo", "hi"}, Env: map[string]string{"A": "1"}, Name: "hourly", Wake: Ptr(true)})
	if err != nil || got.ID != "sch_1" {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"cron": "0 * * * *", "cmd": []any{"echo", "hi"}, "env": []any{"A=1"}, "name": "hourly", "wake": true}) {
		t.Fatalf("body %v", body)
	}
	if l, err := s.List(ctx); err != nil || l[0].LastRun.Status != "ok" {
		t.Fatal(err)
	}
	if run, err := s.Run(ctx, "sch_1"); err != nil || *run.Output != "hi" {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "sch_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, CloudScheduleSpec{Cron: "* * * * *"}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestCloudCodeInterpreter(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.HasSuffix(r.Path, "/code"):
			return rawAnswer{status: 200, json: map[string]any{"context": "default", "execution_count": 2,
				"results": []any{map[string]any{"type": "image/png", "data": "iVBOR"}}, "stdout": "42\n",
				"error": map[string]any{"name": "ValueError", "value": "x", "traceback": "tb"}}}
		case strings.HasSuffix(r.Path, "/reset"):
			return rawAnswer{status: 200, json: map[string]any{"reset": "a"}}
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"deleted": "a"}}
		case r.Method == "POST":
			return rawAnswer{status: 201, json: map[string]any{"context": map[string]any{"name": "a"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"contexts": []any{map[string]any{"name": "a", "execution_count": 3}}}}
	})
	b := cloudBox(t, c)
	r, err := b.RunCode(ctx, "print(42)", &CloudCodeOptions{Context: "a", TimeoutS: 10})
	if err != nil || r.Stdout != "42\n" || r.Results[0].Type != "image/png" || r.Error.Name != "ValueError" {
		t.Fatalf("%+v %v", r, err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"code": "print(42)", "language": "python", "context": "a", "timeout_s": float64(10)}) {
		t.Fatalf("body %v", body)
	}
	if cx, err := b.CreateCodeContext(ctx, "a"); err != nil || cx.Name != "a" {
		t.Fatal(err)
	}
	if l, err := b.CodeContexts(ctx); err != nil || l[0].ExecutionCount != 3 {
		t.Fatal(err)
	}
	if err := b.ResetCodeContext(ctx, "a"); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/code/contexts/a/reset" {
		t.Fatal(err)
	}
	if err := b.DeleteCodeContext(ctx, "a"); err != nil {
		t.Fatal(err)
	}
}

func TestCloudMetricsLogsLabels(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.HasSuffix(r.Path, "/metrics"):
			return rawAnswer{status: 200, json: map[string]any{"metrics": map[string]any{"cpu_pct": 12.5, "mem_bytes": 100, "net": map[string]any{"in_bytes": 9}}}}
		case strings.Contains(r.Path, "/logs"):
			return rawAnswer{status: 200, json: map[string]any{"entries": []any{map[string]any{"cmd": []any{"ls"}, "exit": 0, "stdout_tail": "a"}}}}
		case strings.HasSuffix(r.Path, "/capabilities"):
			return rawAnswer{status: 200, json: map[string]any{"background_exec": true}}
		}
		return rawAnswer{status: 200, json: map[string]any{"id": "sbx_1", "labels": map[string]any{"env": "prod"}}}
	})
	b := cloudBox(t, c)
	m, err := b.Metrics(ctx)
	if err != nil || *m.CPUPct != 12.5 || m.Net.InBytes != 9 {
		t.Fatal(err)
	}
	l, err := b.Logs(ctx, 5)
	if err != nil || l[0].StdoutTail != "a" || stub.last().Path != "/v1/sandboxes/sbx_1/logs?limit=5" {
		t.Fatal(err)
	}
	labels, err := b.SetLabels(ctx, map[string]string{"env": "prod"})
	if err != nil || labels["env"] != "prod" || b.Info.Labels["env"] != "prod" {
		t.Fatal(err)
	}
	if _, err := b.SetLabels(ctx, nil); err != nil || string(stub.last().Body) != `{"labels":null}`+"\n" && string(stub.last().Body) != `{"labels":null}` {
		t.Fatalf("%v %q", err, stub.last().Body)
	}
	if cp, err := c.Capabilities(ctx); err != nil || cp["background_exec"] != true {
		t.Fatal(err)
	}
}

func TestCloudPipelines(t *testing.T) {
	ctx := context.Background()
	gets := 0
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.HasSuffix(r.Path, "/cancel"):
			return rawAnswer{status: 200, json: map[string]any{"run": map[string]any{"id": "run_1", "state": "cancelled"}}}
		case strings.Contains(r.Path, "/logs"):
			return rawAnswer{status: 200, json: map[string]any{"run_id": "run_1", "done": true, "logs": []any{map[string]any{"job": "test", "text": "ok", "next": 4}}}}
		case strings.Contains(r.Path, "/artifacts/"):
			return rawAnswer{status: 200, raw: []byte("tgz")}
		case strings.HasSuffix(r.Path, "/artifacts"):
			return rawAnswer{status: 200, json: map[string]any{"artifacts": []any{map[string]any{"job": "build", "idx": 0, "path": "dist"}}}}
		case strings.HasPrefix(r.Path, "/v1/pipelines/hooks") && r.Method == "POST":
			return rawAnswer{status: 201, json: map[string]any{"hook": map[string]any{"id": "hk_1", "url": "https://api.kosa8.com/v1/pipelines/hooks/hk_1", "secret": "whsec"}}}
		case strings.HasPrefix(r.Path, "/v1/pipelines/hooks") && r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"deleted": "hk_1"}}
		case strings.HasPrefix(r.Path, "/v1/pipelines/hooks"):
			return rawAnswer{status: 200, json: map[string]any{"hooks": []any{map[string]any{"id": "hk_1"}}}}
		case r.Method == "POST":
			return rawAnswer{status: 201, json: map[string]any{"run": map[string]any{"id": "run_1", "state": "queued"}}}
		case r.Path == "/v1/pipelines/runs/run_1":
			gets++
			st := "running"
			if gets > 1 {
				st = "success"
			}
			return rawAnswer{status: 200, json: map[string]any{"run": map[string]any{"id": "run_1", "state": st, "jobs": []any{map[string]any{"name": "test", "exit": 0}}}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"runs": []any{map[string]any{"id": "run_1"}}}}
	})
	p := c.Pipelines
	run, err := p.Run(ctx, CloudPipelineRunOptions{RepoURL: "https://github.com/me/app", Ref: "main", Size: "4x8", Secrets: []string{"npm-token"}, Inputs: map[string]string{"x": "1"}})
	if err != nil || run.ID != "run_1" || run.Done() {
		t.Fatal(err)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"repo_url": "https://github.com/me/app", "ref": "main", "size": "4x8", "secrets": []any{"npm-token"}, "inputs": map[string]any{"x": "1"}}) {
		t.Fatalf("body %v", body)
	}
	run, err = p.Wait(ctx, "run_1", time.Millisecond)
	if err != nil || run.State != "success" || *run.Jobs[0].ExitCode != 0 {
		t.Fatal(err)
	}
	if _, err := p.Runs(ctx, "running", 10); err != nil || stub.last().Path != "/v1/pipelines/runs?limit=10&state=running" {
		t.Fatal(err)
	}
	lg, err := p.Logs(ctx, "run_1", "test", 2)
	if err != nil || lg.Logs[0].Next != 4 || stub.last().Path != "/v1/pipelines/runs/run_1/logs?after=2&job=test" {
		t.Fatal(err, stub.last().Path)
	}
	if _, err := p.Logs(ctx, "run_1", "", -1); err != nil || stub.last().Path != "/v1/pipelines/runs/run_1/logs" {
		t.Fatal(err)
	}
	if r, err := p.Cancel(ctx, "run_1"); err != nil || r.State != "cancelled" {
		t.Fatal(err)
	}
	if a, err := p.Artifacts(ctx, "run_1"); err != nil || a[0].Path != "dist" {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := p.DownloadArtifact(ctx, "run_1", "build", 0, &buf); err != nil || buf.String() != "tgz" {
		t.Fatal(err)
	}
	h, err := p.CreateHook(ctx, CloudPipelineHookOptions{RepoURL: "https://github.com/me/app", Cron: "0 3 * * *", CronRef: "main"})
	if err != nil || h.Secret != "whsec" {
		t.Fatal(err)
	}
	if hs, err := p.Hooks(ctx); err != nil || len(hs) != 1 {
		t.Fatal(err)
	}
	if err := p.DeleteHook(ctx, "hk_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, CloudPipelineRunOptions{}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

// Upload sent any reader as one request, but the API takes at most 100 MB a
// request: a big file failed with 413. Past CloudUploadPartBytes it now goes
// in parts, the first replacing the file and the rest at their offsets.
func TestCloudUploadInParts(t *testing.T) {
	old := CloudUploadPartBytes
	CloudUploadPartBytes = 4
	defer func() { CloudUploadPartBytes = old }()
	var file []byte
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		u, _ := url.Parse(r.Path)
		if off := u.Query().Get("offset"); off == "" {
			file = append([]byte(nil), r.Body...)
		} else {
			n, _ := strconv.Atoi(off)
			if n != len(file) {
				t.Errorf("offset %d with %d bytes written", n, len(file))
			}
			file = append(file[:n], r.Body...)
		}
		return rawAnswer{status: 200, json: map[string]any{"file": map[string]any{"path": "/workspace/a", "size": len(file)}}}
	})
	b := cloudBox(t, c)
	for _, tc := range []struct {
		data  string
		parts int
		seek  bool
	}{{"0123456789", 3, false}, {"01234567", 2, false}, {"0123", 1, false}, {"", 1, false}, {"0123456789", 3, true}, {"012", 1, true}} {
		n := len(stub.seen)
		var r io.Reader = struct{ io.Reader }{strings.NewReader(tc.data)} // not seekable
		if tc.seek {
			r = strings.NewReader(tc.data)
		}
		fi, err := b.Files().Upload(context.Background(), "/workspace/a", r)
		if err != nil || string(file) != tc.data || fi.Size != int64(len(tc.data)) {
			t.Fatalf("%q: %q %+v %v", tc.data, file, fi, err)
		}
		if got := len(stub.seen) - n; got != tc.parts {
			t.Fatalf("%q: %d requests, want %d", tc.data, got, tc.parts)
		}
		if p := stub.seen[n].Path; strings.Contains(p, "offset") {
			t.Fatalf("first part has an offset: %s", p)
		}
	}
}

// DiskGB over 200 was refused before any request, though the API takes up to
// the plan's sandbox disk (Team 256, Scale 500). kvm and lease_s were not
// reachable.
func TestCloudCreateDiskKVMLease(t *testing.T) {
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 201, json: map[string]any{"sandbox": map[string]any{"id": "sbx_2", "state": "running", "lease_s": 600}}}
	})
	b, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{Size: "4x8", DiskGB: 500, KVM: Ptr(false), LeaseS: 600})
	if err != nil || b.Info.LeaseS == nil || *b.Info.LeaseS != 600 {
		t.Fatalf("%+v %v", b, err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"size": "4x8", "disk_gb": float64(500), "kvm": false, "lease_s": float64(600)}) {
		t.Fatalf("body %v", got)
	}
	for _, o := range []CloudCreateOptions{{DiskGB: 7}, {DiskGB: 501}, {Snapshot: "snap_abcdefgh", KVM: Ptr(true)}} {
		if _, err := c.Sandboxes.Create(context.Background(), o); !errors.Is(err, ErrUsage) {
			t.Fatalf("%+v: %v", o, err)
		}
	}
}
