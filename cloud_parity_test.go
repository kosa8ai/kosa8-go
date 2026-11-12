package kosa8

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawReq is one request a rawStub saw, body kept as bytes.
type rawReq struct {
	Method, Path, ContentType, Accept, UA string
	Body                                  []byte
}

func (r rawReq) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%s %s body %q: %v", r.Method, r.Path, r.Body, err)
	}
	return m
}

type rawAnswer struct {
	status  int
	json    any
	raw     []byte
	headers map[string]string
}

type rawStub struct {
	mu   sync.Mutex
	seen []rawReq
}

func (s *rawStub) last() rawReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[len(s.seen)-1]
}

func newRawStub(t *testing.T, answer func(r rawReq) rawAnswer) (*rawStub, *Cloud) {
	t.Helper()
	s := &rawStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rr := rawReq{Method: r.Method, Path: r.URL.RequestURI(), ContentType: r.Header.Get("Content-Type"),
			Accept: r.Header.Get("Accept"), UA: r.Header.Get("User-Agent"), Body: b}
		s.mu.Lock()
		s.seen = append(s.seen, rr)
		s.mu.Unlock()
		a := answer(rr)
		for k, v := range a.headers {
			w.Header().Set(k, v)
		}
		if a.raw != nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(a.status)
			_, _ = w.Write(a.raw)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		if a.json != nil {
			_ = json.NewEncoder(w).Encode(a.json)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func fullSandboxJSON(id string) map[string]any {
	return map[string]any{
		"id": id, "state": "running", "template": "base", "origin_item_version": "1.2.3",
		"cpus": 1, "mem_mb": 512, "source": "fork", "parent_id": "sbx_1", "region": "eu-hel1",
		"own_host": true, "created_at": 1700000000000, "started_at": 1700000000100, "stopped_at": nil,
		"deleted_at": nil, "delete_after": 1700003600000, "sleep_after_s": 300, "last_error": nil,
		"host_unreachable": true, "running_s": 12, "asleep_s": 3, "cost_usd": 0.25,
	}
}

func TestCloudDefaultBaseURL(t *testing.T) {
	if DefaultCloudURL != "https://api.kosa8.com" {
		t.Fatalf("DefaultCloudURL = %q", DefaultCloudURL)
	}
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != "https://api.kosa8.com" {
		t.Fatalf("base %q", c.BaseURL())
	}
}

func TestCloudVersionAndUserAgent(t *testing.T) {
	if Version != "0.4.0" || UserAgent != "kosa8-go/0.4.0" {
		t.Fatalf("%s %s", Version, UserAgent)
	}
	stub, c := newRawStub(t, func(rawReq) rawAnswer { return rawAnswer{status: 200, json: map[string]any{}} })
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ua := stub.last().UA; ua != UserAgent {
		t.Fatalf("user agent %q", ua)
	}
}

func TestCloudSandboxInfoFields(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 200, json: map[string]any{"sandbox": fullSandboxJSON("sbx_2")}}
	})
	b, err := c.Sandboxes.Get(context.Background(), "sbx_2")
	if err != nil {
		t.Fatal(err)
	}
	i := b.Info
	if i.ParentID == nil || *i.ParentID != "sbx_1" || i.Source == nil || *i.Source != "fork" ||
		i.OriginItemVersion == nil || *i.OriginItemVersion != "1.2.3" || i.DeleteAfter == nil || *i.DeleteAfter != 1700003600000 ||
		i.SleepAfterS == nil || *i.SleepAfterS != 300 || !i.HostUnreachable || !i.OwnHost || i.Region == nil ||
		i.CostUSD != 0.25 || i.StartedAt == nil || i.StoppedAt != nil {
		t.Fatalf("%+v", i)
	}
}

func TestCloudCreateLifetimeAndPrice(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 201, json: map[string]any{"sandbox": sbxJSON("running")}}
	})
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Template: "alice/agent@1.0.0", TTLSeconds: 3600, AcceptPrice: Ptr(2.5)}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"template": "alice/agent@1.0.0", "ttl_s": 3600.0, "accept_price": 2.5}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{DeleteAfter: 1700003600000, SleepAfterS: Ptr(0)}); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"delete_after": 1700003600000.0, "sleep_after_s": 0.0}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := stub.last().JSON(t); len(got) != 0 {
		t.Fatalf("unset fields sent: %v", got)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{TTLSeconds: 7776000}); err != nil {
		t.Fatal(err)
	}
	n := len(stub.seen)
	bad := []CloudCreateOptions{
		{TTLSeconds: 59}, {TTLSeconds: 7776001}, {TTLSeconds: -1},
		{TTLSeconds: 600, DeleteAfter: 1700003600000},
		{DeleteAfter: -5},
		{SleepAfterS: Ptr(-1)}, {SleepAfterS: Ptr(604801)},
	}
	for _, o := range bad {
		if _, err := c.Sandboxes.Create(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if len(stub.seen) != n {
		t.Fatalf("a refused create still sent %d requests", len(stub.seen)-n)
	}
}

func TestCloudUpdate(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		if r.Method == "PATCH" {
			return rawAnswer{status: 200, json: map[string]any{"sandbox": fullSandboxJSON("sbx_1"), "notes": []any{"sleep timer changed"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{"sandbox": sbxJSON("running")}}
	})
	b, err := c.Sandboxes.Get(ctx, "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	notes, err := b.Update(ctx, CloudUpdateOptions{TTLSeconds: 120, SleepAfterS: Ptr(60)})
	if err != nil || !reflect.DeepEqual(notes, []string{"sleep timer changed"}) {
		t.Fatalf("%v %v", notes, err)
	}
	r := stub.last()
	if r.Method != "PATCH" || r.Path != "/v1/sandboxes/sbx_1" || r.ContentType != "application/json" {
		t.Fatalf("%+v", r)
	}
	if got := r.JSON(t); !reflect.DeepEqual(got, map[string]any{"ttl_s": 120.0, "sleep_after_s": 60.0}) {
		t.Fatalf("%v", got)
	}
	if b.Info.DeleteAfter == nil || !b.Info.HostUnreachable {
		t.Fatalf("info not refreshed: %+v", b.Info)
	}
	if _, err := b.Update(ctx, CloudUpdateOptions{ClearLifetime: true}); err != nil {
		t.Fatal(err)
	}
	if string(bytes.TrimSpace(stub.last().Body)) != `{"ttl_s":null}` {
		t.Fatalf("clear body %s", stub.last().Body)
	}
	if _, err := b.Update(ctx, CloudUpdateOptions{DeleteAfter: 1700003600000}); err != nil {
		t.Fatal(err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"delete_after": 1700003600000.0}) {
		t.Fatalf("%v", got)
	}
	n := len(stub.seen)
	for _, o := range []CloudUpdateOptions{
		{}, {TTLSeconds: 10}, {ClearLifetime: true, TTLSeconds: 120}, {ClearLifetime: true, DeleteAfter: 1},
		{TTLSeconds: 120, DeleteAfter: 1}, {SleepAfterS: Ptr(-1)},
	} {
		if _, err := b.Update(ctx, o); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if len(stub.seen) != n {
		t.Fatal("a refused update still sent a request")
	}
}

func TestCloudListPaging(t *testing.T) {
	ctx := context.Background()
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.Contains(r.Path, "cursor=c2"):
			return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{fullSandboxJSON("sbx_3")}, "next_cursor": nil}}
		case strings.Contains(r.Path, "cursor=c1"):
			return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{fullSandboxJSON("sbx_2")}, "next_cursor": "c2"}}
		default:
			return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{fullSandboxJSON("sbx_1")}, "next_cursor": "c1"}}
		}
	})
	p, err := c.Sandboxes.ListPage(ctx, CloudListOptions{All: true, Limit: 1})
	if err != nil || len(p.Sandboxes) != 1 || p.NextCursor != "c1" {
		t.Fatalf("%+v %v", p, err)
	}
	if stub.last().Path != "/v1/sandboxes?all=1&limit=1" {
		t.Fatalf("path %s", stub.last().Path)
	}
	all, err := c.Sandboxes.ListAll(ctx, CloudListOptions{Limit: 1})
	if err != nil || len(all) != 3 || all[2].ID() != "sbx_3" {
		t.Fatalf("%v %v", all, err)
	}
	var paths []string
	for _, s := range stub.seen[1:] {
		paths = append(paths, s.Path)
	}
	want := []string{"/v1/sandboxes?limit=1", "/v1/sandboxes?cursor=c1&limit=1", "/v1/sandboxes?cursor=c2&limit=1"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("got %v want %v", paths, want)
	}
	if _, err := c.Sandboxes.ListPage(ctx, CloudListOptions{Limit: -1}); !errors.Is(err, ErrUsage) {
		t.Fatalf("negative limit: %v", err)
	}
}

func TestCloudListWithoutCursor(t *testing.T) {
	// Today's server: no next_cursor, limit and cursor ignored.
	stub, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{sbxJSON("running"), sbxJSON("sleeping")}}}
	})
	ctx := context.Background()
	p, err := c.Sandboxes.ListPage(ctx, CloudListOptions{})
	if err != nil || p.NextCursor != "" || len(p.Sandboxes) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	all, err := c.Sandboxes.ListAll(ctx, CloudListOptions{Limit: 50})
	if err != nil || len(all) != 2 {
		t.Fatalf("%v %v", all, err)
	}
	l, err := c.Sandboxes.List(ctx, false)
	if err != nil || len(l) != 2 {
		t.Fatalf("%v %v", l, err)
	}
	if len(stub.seen) != 3 {
		t.Fatalf("%d requests", len(stub.seen))
	}
}

func TestCloudListAllStopsOnRepeatedCursor(t *testing.T) {
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		next := "a"
		if strings.Contains(r.Path, "cursor=a") {
			next = "b"
		} else if strings.Contains(r.Path, "cursor=b") {
			next = "a" // a loop
		}
		return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{sbxJSON("running")}, "next_cursor": next}}
	})
	all, err := c.Sandboxes.ListAll(context.Background(), CloudListOptions{})
	if err != nil || len(all) != 3 || len(stub.seen) != 3 {
		t.Fatalf("%d sandboxes, %d requests, %v", len(all), len(stub.seen), err)
	}
	// A server that echoes the cursor it was given.
	stub, c = newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 200, json: map[string]any{"sandboxes": []any{}, "next_cursor": "same"}}
	})
	if _, err := c.Sandboxes.ListAll(context.Background(), CloudListOptions{Cursor: "same"}); err != nil || len(stub.seen) != 1 {
		t.Fatalf("%d requests %v", len(stub.seen), err)
	}
}

func TestCloudTemplates(t *testing.T) {
	stub, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 200, json: map[string]any{"default_template": "base", "templates": []any{
			map[string]any{"key": "base", "cpus": 1, "mem_mb": 512, "image": "alpine:3", "regions": []any{"eu-hel1"}, "hosts": 2, "warm": 3, "own_host": false},
			map[string]any{"key": "python"},
		}}}
	})
	ts, err := c.Templates(context.Background())
	if err != nil || len(ts) != 2 || ts[0].Key != "base" || ts[0].MemMB != 512 || ts[0].Regions[0] != "eu-hel1" || ts[0].Warm != 3 {
		t.Fatalf("%+v %v", ts, err)
	}
	if r := stub.last(); r.Method != "GET" || r.Path != "/v1/templates" {
		t.Fatalf("%+v", r)
	}
}

func cloudBox(t *testing.T, c *Cloud) *CloudSandbox {
	t.Helper()
	return &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1", State: "running"}}
}

func TestCloudFiles(t *testing.T) {
	ctx := context.Background()
	payload := []byte{0, 1, 2, 0xff, 'h', 'i'}
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/sandboxes/sbx_1/files/list"):
			return rawAnswer{status: 200, json: map[string]any{"path": "/workspace", "files": []any{
				map[string]any{"name": "a.txt", "path": "/workspace/a.txt", "type": "file", "size": 6, "mode": "-rw-r--r--", "mod_time": "2026-11-12T00:00:00Z"},
			}}}
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/sandboxes/sbx_1/files?"):
			return rawAnswer{status: 200, raw: payload}
		case r.Method == "PUT":
			return rawAnswer{status: 200, json: map[string]any{"file": map[string]any{"name": "a b.txt", "path": "/workspace/a b.txt", "type": "file", "size": len(r.Body)}}}
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/files/mkdir"):
			return rawAnswer{status: 200, json: map[string]any{"file": map[string]any{"name": "d", "path": "/workspace/d", "type": "dir"}}}
		case r.Method == "DELETE":
			return rawAnswer{status: 200, json: map[string]any{"removed": "/workspace/d"}}
		}
		return rawAnswer{status: 404, json: map[string]any{"error": "not_found", "message": "no route"}}
	})
	f := cloudBox(t, c).Files()

	got, err := f.Read(ctx, "/workspace/a&b.bin")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("%v %v", got, err)
	}
	r := stub.last()
	if r.Path != "/v1/sandboxes/sbx_1/files?path=%2Fworkspace%2Fa%26b.bin" || r.Accept != "application/octet-stream" {
		t.Fatalf("%+v", r)
	}
	if s, err := f.ReadText(ctx, "/workspace/a.txt"); err != nil || s != string(payload) {
		t.Fatalf("%q %v", s, err)
	}

	fi, err := f.Write(ctx, "/workspace/a b.txt", payload)
	if err != nil || fi.Size != int64(len(payload)) || fi.Type != "file" {
		t.Fatalf("%+v %v", fi, err)
	}
	r = stub.last()
	if r.Method != "PUT" || r.Path != "/v1/sandboxes/sbx_1/files?path=%2Fworkspace%2Fa+b.txt" || r.ContentType != "application/octet-stream" || !bytes.Equal(r.Body, payload) {
		t.Fatalf("%+v", r)
	}
	if _, err := f.WriteString(ctx, "/workspace/s.txt", "hello"); err != nil || string(stub.last().Body) != "hello" {
		t.Fatalf("%v %q", err, stub.last().Body)
	}

	l, err := f.List(ctx, "")
	if err != nil || len(l) != 1 || l[0].Name != "a.txt" || l[0].Mode != "-rw-r--r--" || l[0].ModTime == "" {
		t.Fatalf("%+v %v", l, err)
	}
	if stub.last().Path != "/v1/sandboxes/sbx_1/files/list?path=%2Fworkspace" {
		t.Fatalf("%s", stub.last().Path)
	}
	if _, err := f.List(ctx, "/tmp"); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/files/list?path=%2Ftmp" {
		t.Fatalf("%v %s", err, stub.last().Path)
	}

	d, err := f.Mkdir(ctx, "/workspace/d")
	if err != nil || d.Type != "dir" {
		t.Fatalf("%+v %v", d, err)
	}
	if got := stub.last().JSON(t); !reflect.DeepEqual(got, map[string]any{"path": "/workspace/d"}) {
		t.Fatalf("%v", got)
	}

	rm, err := f.Delete(ctx, "/workspace/d", true)
	if err != nil || rm != "/workspace/d" || stub.last().Path != "/v1/sandboxes/sbx_1/files?path=%2Fworkspace%2Fd&recursive=1" {
		t.Fatalf("%q %v %s", rm, err, stub.last().Path)
	}
	if _, err := f.Delete(ctx, "/workspace/a.txt", false); err != nil || stub.last().Path != "/v1/sandboxes/sbx_1/files?path=%2Fworkspace%2Fa.txt" {
		t.Fatalf("%v %s", err, stub.last().Path)
	}

	n := len(stub.seen)
	if _, err := f.Read(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := f.Write(ctx, "/workspace/big", make([]byte, CloudMaxFileBytes+1)); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := f.Mkdir(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, err := f.Delete(ctx, "", true); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if len(stub.seen) != n {
		t.Fatal("refused file calls still sent requests")
	}
}

func TestCloudFileReadErrorIsTyped(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 413, json: map[string]any{"error": "too_large", "message": "the file is over 8 MiB"}}
	})
	_, err := cloudBox(t, c).Files().Read(context.Background(), "/workspace/huge")
	e, ok := AsError(err)
	if !ok || e.Status != 413 || e.Code != "too_large" || e.Message != "the file is over 8 MiB" || !errors.Is(err, ErrAPI) {
		t.Fatalf("%+v", err)
	}
}

func TestCloudCheckpointsRewindForkPorts(t *testing.T) {
	ctx := context.Background()
	cp := map[string]any{"id": "cp_1", "label": "clean", "auto": false, "created_at": "2026-11-12T00:00:00Z", "bytes": 1024}
	backup := map[string]any{"id": "cp_2", "label": "", "auto": true, "created_at": nil, "bytes": 2048}
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/checkpoints"):
			return rawAnswer{status: 201, json: map[string]any{"checkpoint": cp, "evicted": []any{}, "ms": 40}}
		case r.Method == "GET" && strings.HasSuffix(r.Path, "/checkpoints"):
			return rawAnswer{status: 200, json: map[string]any{"checkpoints": []any{cp, backup}, "max": 10}}
		case strings.HasSuffix(r.Path, "/rewind"):
			s := fullSandboxJSON("sbx_1")
			s["state"] = "running"
			return rawAnswer{status: 200, json: map[string]any{"sandbox": s, "checkpoint": cp, "backup": backup, "ms": 55}}
		case strings.HasSuffix(r.Path, "/fork"):
			return rawAnswer{status: 201, json: map[string]any{"parent": "sbx_1", "sandboxes": []any{fullSandboxJSON("sbx_8"), fullSandboxJSON("sbx_9")}, "host_ms": 30}}
		case strings.HasSuffix(r.Path, "/ports"):
			return rawAnswer{status: 200, json: map[string]any{"ports": []any{map[string]any{"port": 8000, "address": "0.0.0.0", "container": "main", "loopback": false}}}}
		}
		return rawAnswer{status: 404, json: map[string]any{"error": "not_found", "message": "no route"}}
	})
	b := cloudBox(t, c)

	got, err := b.Checkpoint(ctx, "clean")
	if err != nil || got.ID != "cp_1" || got.Label != "clean" || got.Bytes != 1024 || got.CreatedAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if r := stub.last(); r.Path != "/v1/sandboxes/sbx_1/checkpoints" || !reflect.DeepEqual(r.JSON(t), map[string]any{"label": "clean"}) {
		t.Fatalf("%+v", r)
	}
	if _, err := b.Checkpoint(ctx, ""); err != nil || len(stub.last().JSON(t)) != 0 {
		t.Fatalf("%v %s", err, stub.last().Body)
	}
	if _, err := b.Checkpoint(ctx, strings.Repeat("é", CloudMaxLabelLen)); err != nil {
		t.Fatalf("200-character label refused: %v", err)
	}
	n := len(stub.seen)
	if _, err := b.Checkpoint(ctx, strings.Repeat("x", CloudMaxLabelLen+1)); !errors.Is(err, ErrUsage) || len(stub.seen) != n {
		t.Fatalf("long label: %v", err)
	}

	list, err := b.Checkpoints(ctx)
	if err != nil || len(list) != 2 || !list[1].Auto || list[1].CreatedAt != nil {
		t.Fatalf("%+v %v", list, err)
	}

	rw, err := b.Rewind(ctx, "cp_1")
	if err != nil || rw.Checkpoint.ID != "cp_1" || rw.Backup.ID != "cp_2" || !rw.Backup.Auto || rw.Ms == nil || *rw.Ms != 55 {
		t.Fatalf("%+v %v", rw, err)
	}
	if !reflect.DeepEqual(stub.last().JSON(t), map[string]any{"checkpoint_id": "cp_1"}) {
		t.Fatalf("%s", stub.last().Body)
	}
	if b.Info.ParentID == nil || b.Info.Region == nil {
		t.Fatalf("rewind did not refresh Info: %+v", b.Info)
	}
	if _, err := b.Rewind(ctx, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}

	kids, err := b.Fork(ctx, 2)
	if err != nil || len(kids) != 2 || kids[0].ID() != "sbx_8" || *kids[1].Info.ParentID != "sbx_1" {
		t.Fatalf("%v %v", kids, err)
	}
	if !reflect.DeepEqual(stub.last().JSON(t), map[string]any{"n": 2.0}) {
		t.Fatalf("%s", stub.last().Body)
	}
	if _, err := b.Fork(ctx, 0); err != nil || !reflect.DeepEqual(stub.last().JSON(t), map[string]any{"n": 1.0}) {
		t.Fatalf("%v %s", err, stub.last().Body)
	}
	// The forks are usable sandboxes on the same client.
	if kids[0].path("/exec") != "/v1/sandboxes/sbx_8/exec" || kids[0].c != c {
		t.Fatal("fork not bound to the client")
	}
	n = len(stub.seen)
	for _, bad := range []int{-1, 101} {
		if _, err := b.Fork(ctx, bad); !errors.Is(err, ErrUsage) {
			t.Fatalf("fork %d: %v", bad, err)
		}
	}
	if len(stub.seen) != n {
		t.Fatal("refused fork still sent a request")
	}

	ports, err := b.Ports(ctx)
	if err != nil || len(ports) != 1 || ports[0].Port != 8000 || ports[0].Container != "main" || ports[0].Loopback {
		t.Fatalf("%+v %v", ports, err)
	}
	if r := stub.last(); r.Method != "GET" || r.Path != "/v1/sandboxes/sbx_1/ports" {
		t.Fatalf("%+v", r)
	}
}

func TestCloudDataplaneNotSupported(t *testing.T) {
	ctx := context.Background()
	for _, code := range []string{"not_available", "not_supported"} {
		_, c := newRawStub(t, func(rawReq) rawAnswer {
			return rawAnswer{status: 501, json: map[string]any{"error": code, "message": "checkpoints is not available on this host yet"}}
		})
		b := cloudBox(t, c)
		calls := map[string]func() error{
			"checkpoint":  func() error { _, err := b.Checkpoint(ctx, ""); return err },
			"checkpoints": func() error { _, err := b.Checkpoints(ctx); return err },
			"rewind":      func() error { _, err := b.Rewind(ctx, "cp_1"); return err },
			"fork":        func() error { _, err := b.Fork(ctx, 1); return err },
			"ports":       func() error { _, err := b.Ports(ctx); return err },
			"read":        func() error { _, err := b.Files().Read(ctx, "/workspace/a"); return err },
			"write":       func() error { _, err := b.Files().Write(ctx, "/workspace/a", []byte("x")); return err },
			"list":        func() error { _, err := b.Files().List(ctx, ""); return err },
			"mkdir":       func() error { _, err := b.Files().Mkdir(ctx, "/workspace/d"); return err },
			"delete":      func() error { _, err := b.Files().Delete(ctx, "/workspace/a", false); return err },
		}
		for name, call := range calls {
			err := call()
			e, _ := AsError(err)
			if !errors.Is(err, ErrUnsupported) || !errors.Is(err, ErrAPI) || e.Code != code || e.Status != 501 {
				t.Errorf("%s %s: %v", code, name, err)
			}
		}
	}
}

func TestCloudPaymentRequiredFields(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 402, json: map[string]any{
			"error": "spending_cap_reached", "message": "The spending cap is reached.",
			"upgrade_url": "https://app.kosa8.com/#billing", "agent_message": "Tell your person to raise the cap.",
		}}
	})
	_, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{})
	e, ok := AsError(err)
	if !ok || !errors.Is(err, ErrPaymentRequired) || e.Code != "spending_cap_reached" ||
		e.UpgradeURL != "https://app.kosa8.com/#billing" || e.AgentMessage != "Tell your person to raise the cap." ||
		e.Message != "The spending cap is reached." {
		t.Fatalf("%+v", err)
	}
	// Absent: empty.
	_, c = newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 402, json: map[string]any{"error": "price_not_accepted", "message": "Pass accept_price."}}
	})
	_, err = c.Sandboxes.Create(context.Background(), CloudCreateOptions{})
	if e, _ := AsError(err); e == nil || e.UpgradeURL != "" || e.AgentMessage != "" || e.Code != "price_not_accepted" {
		t.Fatalf("%+v", err)
	}
}

func TestCloudOrgFrozen(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 423, json: map[string]any{"error": "org_frozen", "message": "This organization is frozen."}}
	})
	_, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{})
	e, _ := AsError(err)
	if !errors.Is(err, ErrOrgFrozen) || !errors.Is(err, ErrAPI) || errors.Is(err, ErrPaymentRequired) || e.Status != 423 || e.Code != "org_frozen" {
		t.Fatalf("%+v", err)
	}
}

func TestCloudRateLimitRetryAfter(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 429, json: map[string]any{"error": "rate_limited", "message": "Slow down."}, headers: map[string]string{"Retry-After": "7"}}
	})
	_, err := c.Me(context.Background())
	e, _ := AsError(err)
	if !errors.Is(err, ErrRateLimit) || e.RetryAfter != 7*time.Second {
		t.Fatalf("%+v", err)
	}
	_, c = newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 429, json: map[string]any{"error": "quota_exceeded", "message": "Quota."}}
	})
	_, err = c.Me(context.Background())
	if e, _ := AsError(err); e == nil || e.Kind != KindRateLimit || e.RetryAfter != 0 {
		t.Fatalf("%+v", err)
	}
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"-3":                            0,
		"120":                           2 * time.Minute,
		"soon":                          0,
		"Thu, 12 Nov 2026 00:00:30 GMT": 30 * time.Second,
		"Wed, 11 Nov 2026 23:00:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("%q: %v want %v", in, got, want)
		}
	}
}

func TestCloudExecTimeout(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status int
		body   map[string]any
		exec   bool
		want   bool
	}{
		{"504", 504, map[string]any{"error": "gateway_timeout", "message": "the host took too long"}, true, true},
		{"504 not exec", 504, map[string]any{"error": "gateway_timeout", "message": "slow"}, false, true},
		{"exec_timeout code", 408, map[string]any{"error": "exec_timeout", "message": "the command ran too long"}, true, true},
		{"502 host_failed timed out", 502, map[string]any{"error": "host_failed", "message": "exec failed on the host: host unreachable: timed out"}, true, true},
		{"502 host_failed other", 502, map[string]any{"error": "host_failed", "message": "exec failed on the host: boom"}, true, false},
		{"502 timed out not exec", 502, map[string]any{"error": "host_failed", "message": "the host could not put the sandbox to sleep: timed out"}, false, false},
	}
	for _, tc := range cases {
		_, c := newRawStub(t, func(rawReq) rawAnswer { return rawAnswer{status: tc.status, json: tc.body} })
		b := cloudBox(t, c)
		var err error
		if tc.exec {
			_, err = b.ExecShell(ctx, "sleep 60", nil)
		} else {
			err = b.Sleep(ctx)
		}
		if got := errors.Is(err, ErrExecTimeout); got != tc.want {
			t.Errorf("%s: ErrExecTimeout %v, want %v (%v)", tc.name, got, tc.want, err)
			continue
		}
		if !errors.Is(err, ErrAPI) {
			t.Errorf("%s: not ErrAPI: %v", tc.name, err)
		}
		if !tc.want {
			continue
		}
		e, _ := AsError(err)
		msg, _ := tc.body["message"].(string)
		if !errors.Is(err, ErrTimeout) || !strings.HasPrefix(e.Message, msg) || !strings.Contains(e.Message, "30 s") ||
			!strings.Contains(e.Message, "background") || e.Code != tc.body["error"] || e.Status != tc.status {
			t.Errorf("%s: %+v", tc.name, e)
		}
	}
}

func TestCloudExecNullExit(t *testing.T) {
	_, c := newRawStub(t, func(rawReq) rawAnswer {
		return rawAnswer{status: 200, json: map[string]any{"exit": nil, "stdout": "", "stderr": "killed"}}
	})
	r, err := cloudBox(t, c).Exec(context.Background(), []string{"true"}, nil)
	if err != nil || r.ExitCode != nil || r.OK() || r.Stderr != "killed" {
		t.Fatalf("%+v %v", r, err)
	}
}
