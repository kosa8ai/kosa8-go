package kosa8

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testCloudKey = "k8c_live_testkey"

type seenReq struct {
	Method, Path, Auth string
	Body               map[string]any
}

type cloudStub struct {
	mu   sync.Mutex
	seen []seenReq
	srv  *httptest.Server
}

func newCloudStub(t *testing.T, answer func(r seenReq) (int, any)) (*cloudStub, *Cloud) {
	t.Helper()
	s := &cloudStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := seenReq{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization")}
		_ = json.NewDecoder(r.Body).Decode(&sr.Body)
		s.mu.Lock()
		s.seen = append(s.seen, sr)
		s.mu.Unlock()
		status, payload := answer(sr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	t.Cleanup(s.srv.Close)
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: s.srv.URL, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func sbxJSON(state string) map[string]any {
	return map[string]any{"id": "sbx_1", "state": state, "template": "python", "cpus": 1, "mem_mb": 512, "region": nil}
}

func TestCloudKeyAndAddress(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := NewCloud(CloudOptions{Getenv: env(map[string]string{"KOSA8_API_KEY": testCloudKey})})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != DefaultCloudURL {
		t.Fatalf("base %q", c.BaseURL())
	}
	for _, s := range []string{c.String(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(s, testCloudKey) {
			t.Fatalf("key printed: %s", s)
		}
	}
	c, err = NewCloud(CloudOptions{Getenv: env(map[string]string{"KOSA8_API_KEY": testCloudKey, "KOSA8_CLOUD_URL": "https://cp.example.com/"})})
	if err != nil || c.BaseURL() != "https://cp.example.com" {
		t.Fatalf("%v %v", c, err)
	}
	bad := []struct {
		o    CloudOptions
		want string
	}{
		{CloudOptions{Getenv: env(nil)}, "KOSA8_API_KEY"},
		{CloudOptions{APIKey: "sk-other", Getenv: env(nil)}, "k8c_live_"},
		{CloudOptions{APIKey: testCloudKey, BaseURL: "http://cp.example.com"}, "https"},
		{CloudOptions{APIKey: testCloudKey, BaseURL: "ftp://localhost"}, "https"},
		{CloudOptions{APIKey: testCloudKey, BaseURL: "not a url"}, "not a URL"},
	}
	for _, b := range bad {
		_, err := NewCloud(b.o)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%+v: got %v, want usage error with %q", b.o.BaseURL, err, b.want)
		}
	}
	for _, u := range []string{"http://localhost:8787/", "http://127.0.0.1:1", "http://[::1]:1"} {
		c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: u})
		if err != nil {
			t.Errorf("%s: %v", u, err)
		} else if strings.HasSuffix(c.BaseURL(), "/") {
			t.Errorf("trailing slash kept: %s", c.BaseURL())
		}
	}
}

func TestCloudSandboxLifecycle(t *testing.T) {
	ctx := context.Background()
	stub, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case r.Method == "POST" && r.Path == "/v1/sandboxes":
			return 201, map[string]any{"sandbox": sbxJSON("running"), "host_ms": 6}
		case strings.HasSuffix(r.Path, "/exec"):
			return 200, map[string]any{"exit": 0, "stdout": "42\n", "stderr": "", "woke_ms": 70}
		case strings.HasSuffix(r.Path, "/sleep"):
			return 200, map[string]any{"sandbox": sbxJSON("sleeping")}
		case strings.HasSuffix(r.Path, "/wake"), r.Method == "GET" && r.Path == "/v1/sandboxes/sbx_1":
			return 200, map[string]any{"sandbox": sbxJSON("running")}
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/sandboxes"):
			return 200, map[string]any{"sandboxes": []any{sbxJSON("running")}}
		case r.Method == "DELETE":
			return 200, map[string]any{"ok": true, "id": "sbx_1"}
		}
		return 404, map[string]any{"error": "not_found", "message": "no route"}
	})
	box, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Template: "python", SleepAfterS: Ptr(600)})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID() != "sbx_1" || box.State() != "running" || box.Info.Region != nil {
		t.Fatalf("%+v", box.Info)
	}
	r, err := box.ExecShell(ctx, `python3 -c "print(6*7)"`, &CloudExecOptions{Env: map[string]string{"B": "2", "A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() || r.Stdout != "42\n" || r.WokeMs == nil || *r.WokeMs != 70 {
		t.Fatalf("%+v", r)
	}
	if _, err := box.Exec(ctx, []string{"ls"}, &CloudExecOptions{Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	if err := box.Sleep(ctx); err != nil || box.State() != "sleeping" {
		t.Fatalf("%v %s", err, box.State())
	}
	if err := box.Wake(ctx); err != nil || box.State() != "running" {
		t.Fatalf("%v %s", err, box.State())
	}
	if err := box.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if l, err := c.Sandboxes.List(ctx, true); err != nil || len(l) != 1 || l[0].ID() != "sbx_1" {
		t.Fatalf("%v %v", l, err)
	}
	if _, err := c.Sandboxes.List(ctx, false); err != nil {
		t.Fatal(err)
	}
	if g, err := c.Sandboxes.Get(ctx, "sbx_1"); err != nil || g.ID() != "sbx_1" {
		t.Fatalf("%v %v", g, err)
	}
	if err := box.Delete(ctx); err != nil {
		t.Fatal(err)
	}

	type mp struct{ M, P string }
	var got []mp
	for _, s := range stub.seen {
		got = append(got, mp{s.Method, s.Path})
		if s.Auth != "Bearer "+testCloudKey {
			t.Fatalf("auth %q", s.Auth)
		}
	}
	want := []mp{
		{"POST", "/v1/sandboxes"}, {"POST", "/v1/sandboxes/sbx_1/exec"}, {"POST", "/v1/sandboxes/sbx_1/exec"},
		{"POST", "/v1/sandboxes/sbx_1/sleep"}, {"POST", "/v1/sandboxes/sbx_1/wake"}, {"GET", "/v1/sandboxes/sbx_1"},
		{"GET", "/v1/sandboxes?all=1"}, {"GET", "/v1/sandboxes"}, {"GET", "/v1/sandboxes/sbx_1"}, {"DELETE", "/v1/sandboxes/sbx_1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes\n got %v\nwant %v", got, want)
	}
	bodies := []map[string]any{
		{"template": "python", "sleep_after_s": 600.0},
		{"cmd": []any{"sh", "-c", `python3 -c "print(6*7)"`}, "env": []any{"A=1", "B=2"}},
		{"cmd": []any{"ls"}, "image": "alpine"},
	}
	for i, b := range bodies {
		if !reflect.DeepEqual(stub.seen[i].Body, b) {
			t.Errorf("body %d: got %v want %v", i, stub.seen[i].Body, b)
		}
	}
	if _, err := box.Exec(ctx, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("empty argv: %v", err)
	}
}

func TestCloudFunctions(t *testing.T) {
	ctx := context.Background()
	inv := map[string]any{
		"invocation": map[string]any{"id": "inv_1", "function": "hello", "function_id": "fn_1", "version": 2, "trigger": "api"},
		"ok":         true, "result": map[string]any{"hello": "kosa8"}, "logs": "", "source": "pool",
		"timings_ms": map[string]any{"start": 3, "run": 1, "host_total": nil, "total": 9},
	}
	stub, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case r.Method == "PUT":
			return 201, map[string]any{"function": map[string]any{"name": "hello", "version": 1}}
		case r.Method == "GET" && r.Path == "/v1/functions":
			return 200, map[string]any{"functions": []any{map[string]any{"name": "hello"}}}
		case r.Method == "GET":
			return 200, map[string]any{"function": map[string]any{"name": "hello", "code": "x"}}
		case strings.HasSuffix(r.Path, "/invoke"):
			if r.Body["event"] == nil {
				return 200, map[string]any{"invocation": nil, "ok": false, "error": map[string]any{"type": "ValueError", "message": "boom"}}
			}
			return 200, inv
		case strings.HasSuffix(r.Path, "/map"):
			return 200, map[string]any{"function": "hello", "version": 2, "count": 2, "succeeded": 1, "failed": 1, "total_ms": 12,
				"results": []any{inv, map[string]any{"ok": false, "error": map[string]any{"type": "no_capacity", "message": "busy"}, "invocation": nil}}}
		case r.Method == "DELETE":
			return 200, map[string]any{"ok": true}
		}
		return 404, nil
	})
	d, err := c.Functions.Deploy(ctx, "hello", CloudFunctionSpec{Runtime: "python3.12", Code: "def handler(e):\n    return e\n", TimeoutS: 30, Extra: map[string]any{"memory": 256}})
	if err != nil || d["function"].(map[string]any)["name"] != "hello" {
		t.Fatalf("%v %v", d, err)
	}
	wantDeploy := map[string]any{"runtime": "python3.12", "code": "def handler(e):\n    return e\n", "timeout_s": 30.0, "memory": 256.0}
	if !reflect.DeepEqual(stub.seen[0].Body, wantDeploy) || stub.seen[0].Path != "/v1/functions/hello" {
		t.Fatalf("deploy %s %v", stub.seen[0].Path, stub.seen[0].Body)
	}
	if l, err := c.Functions.List(ctx); err != nil || len(l) != 1 {
		t.Fatalf("%v %v", l, err)
	}
	if g, err := c.Functions.Get(ctx, "hello"); err != nil || g["function"] == nil {
		t.Fatalf("%v %v", g, err)
	}
	r, err := c.Functions.Invoke(ctx, "hello", map[string]any{"name": "kosa8"})
	if err != nil || !r.OK || r.Invocation.ID != "inv_1" || r.Invocation.Version != 2 || r.TimingsMs.Total != 9 || r.TimingsMs.HostTotal != nil {
		t.Fatalf("%+v %v", r, err)
	}
	var res struct{ Hello string }
	if err := r.DecodeResult(&res); err != nil || res.Hello != "kosa8" {
		t.Fatalf("%+v %v", res, err)
	}
	if !reflect.DeepEqual(stub.seen[3].Body, map[string]any{"event": map[string]any{"name": "kosa8"}}) {
		t.Fatalf("invoke body %v", stub.seen[3].Body)
	}
	// A handler that raised is a result, not an error; nil sends {"event": null}.
	r, err = c.Functions.Invoke(ctx, "hello", nil)
	if err != nil || r.OK || r.Error == nil || r.Error.Type != "ValueError" {
		t.Fatalf("%+v %v", r, err)
	}
	if v, ok := stub.seen[4].Body["event"]; !ok || v != nil {
		t.Fatalf("null event %v", stub.seen[4].Body)
	}
	m, err := c.Functions.Map(ctx, "hello", []any{1, 2}, &CloudMapOptions{Concurrency: 2})
	if err != nil || m.Count != 2 || m.Failed != 1 || len(m.Results) != 2 || m.Results[1].Error.Type != "no_capacity" {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(stub.seen[5].Body, map[string]any{"events": []any{1.0, 2.0}, "concurrency": 2.0}) {
		t.Fatalf("map body %v", stub.seen[5].Body)
	}
	if _, err := c.Functions.Map(ctx, "hello", []any{1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := stub.seen[6].Body["concurrency"]; ok {
		t.Fatal("concurrency sent when unset")
	}
	if err := c.Functions.Delete(ctx, "hello"); err != nil || stub.seen[7].Method != "DELETE" {
		t.Fatal(err)
	}
}

func TestCloudMeUsage(t *testing.T) {
	_, c := newCloudStub(t, func(r seenReq) (int, any) {
		if r.Path == "/v1/me" {
			return 200, map[string]any{"org": map[string]any{"id": "org_1"}}
		}
		return 200, map[string]any{"paused": false}
	})
	me, err := c.Me(context.Background())
	if err != nil || me["org"] == nil {
		t.Fatalf("%v %v", me, err)
	}
	u, err := c.Usage(context.Background())
	if err != nil || u["paused"] != false {
		t.Fatalf("%v %v", u, err)
	}
}

func TestCloudTypedErrors(t *testing.T) {
	cases := []struct {
		status int
		code   string
		target error
	}{
		{402, "payment_required", ErrPaymentRequired},
		{429, "quota_exceeded", ErrRateLimit},
		{404, "not_found", ErrNotFound},
		{401, "unauthorized", ErrAuth},
		{403, "forbidden", ErrPolicy},
		{409, "conflict", ErrConflict},
		{400, "invalid_events", ErrBadRequest},
		{503, "no_capacity", ErrAPI},
	}
	for _, tc := range cases {
		_, c := newCloudStub(t, func(seenReq) (int, any) {
			return tc.status, map[string]any{"error": tc.code, "message": "Sentence for " + tc.code + "."}
		})
		_, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{})
		if !errors.Is(err, tc.target) || !errors.Is(err, ErrAPI) {
			t.Errorf("%d: %v is not %v", tc.status, err, tc.target)
			continue
		}
		e, _ := AsError(err)
		if e.Code != tc.code || e.Message != "Sentence for "+tc.code+"." || e.Status != tc.status || e.Method != "POST" || e.Path != "/v1/sandboxes" {
			t.Errorf("%d: %+v", tc.status, e)
		}
		if b, _ := e.Body.(map[string]any); b["error"] != tc.code {
			t.Errorf("%d: body %v", tc.status, e.Body)
		}
	}
	// No message: the code stands in. Not JSON: the text.
	_, c := newCloudStub(t, func(seenReq) (int, any) { return 429, map[string]any{"error": "rate_limited"} })
	_, err := c.Me(context.Background())
	if e, _ := AsError(err); e == nil || e.Message != "rate_limited" || e.Kind != KindRateLimit {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()
	c, _ = NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL})
	_, err = c.Me(context.Background())
	if e, _ := AsError(err); e == nil || e.Kind != KindAPI || e.Message != "bad gateway" || e.Code != "" {
		t.Fatalf("%v", err)
	}
}

func TestCloudUnreachableAndTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c, _ := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: "http://" + addr})
	_, err = c.Me(context.Background())
	if !errors.Is(err, ErrConnection) || !strings.Contains(err.Error(), "could not reach kosa8 Cloud") || strings.Contains(err.Error(), testCloudKey) {
		t.Fatalf("%v", err)
	}

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	c, _ = NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	if _, err := c.Me(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Me(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
