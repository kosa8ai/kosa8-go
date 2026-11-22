package kosa8

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- retries on pre-execution rejections ----

func TestCloudRetryDatabaseBusyPOST(t *testing.T) {
	resetSlept()
	var n int32
	s, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) == 1 {
			return rawAnswer{status: 503, json: map[string]any{"error": "database_busy", "message": "busy",
				"failure_class": "infra", "retryable": true, "retry_after": 2, "retry_after_s": 2}}
		}
		return rawAnswer{status: 201, json: map[string]any{"webhook": map[string]any{"id": "wh_1", "url": "https://x"}}}
	})
	w, err := c.Webhooks.Create(context.Background(), "https://x", nil, "")
	if err != nil || w.ID != "wh_1" {
		t.Fatalf("%+v %v", w, err)
	}
	if len(s.seen) != 2 {
		t.Fatalf("requests %d", len(s.seen))
	}
	if sl := sleeps(); len(sl) != 1 || sl[0] < 2*time.Second {
		t.Fatalf("slept %v, want >= 2 s (retry_after)", sl)
	}
}

func TestCloudRetryAfterHeaderAndBody(t *testing.T) {
	resetSlept()
	var n int32
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) == 1 {
			return rawAnswer{status: 503, json: map[string]any{"error": "database_busy", "retry_after": 1}, headers: map[string]string{"Retry-After": "5"}}
		}
		return rawAnswer{status: 200, json: map[string]any{"active": 1}}
	})
	if _, err := c.Limits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sl := sleeps(); len(sl) != 1 || sl[0] < 5*time.Second {
		t.Fatalf("slept %v, want the header's 5 s", sl)
	}
}

func TestCloudRetry429POST(t *testing.T) {
	resetSlept()
	var n int32
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) < 3 {
			return rawAnswer{status: 429, json: map[string]any{"error": "rate_limited"}, headers: map[string]string{"Retry-After": "1"}}
		}
		return rawAnswer{status: 200, json: map[string]any{"browser": map[string]any{"id": "brw_1", "state": "open"}}}
	})
	b, err := c.Browsers.Create(context.Background(), CloudBrowserOptions{})
	if err != nil || b.ID() != "brw_1" {
		t.Fatalf("%v %v", b, err)
	}
	if n != 3 {
		t.Fatalf("tries %d", n)
	}
}

func TestCloudRetryTemplateBuilding(t *testing.T) {
	resetSlept()
	var n int32
	s, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) == 1 {
			return rawAnswer{status: 409, json: map[string]any{"error": "template_building", "eta_s": 40, "retry_after_s": 10,
				"failure_class": "infra", "retryable": true}, headers: map[string]string{"Retry-After": "10"}}
		}
		return rawAnswer{status: 201, json: map[string]any{"sandbox": fullSandboxJSON("sbx_9")}}
	})
	b, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{Template: "swe/x"})
	if err != nil || b.ID() != "sbx_9" {
		t.Fatalf("%v %v", b, err)
	}
	if len(s.seen) != 2 {
		t.Fatalf("requests %d", len(s.seen))
	}
	if sl := sleeps(); len(sl) != 1 || sl[0] < 10*time.Second {
		t.Fatalf("slept %v", sl)
	}
}

func TestCloudNoRetryOtherPOSTFailure(t *testing.T) {
	resetSlept()
	var n int32
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		atomic.AddInt32(&n, 1)
		return rawAnswer{status: 502, json: map[string]any{"error": "host_failed", "retryable": true}}
	})
	// A POST without an Idempotency-Key that may have run: not resent.
	if _, err := c.Webhooks.Test(context.Background(), "wh_1"); err == nil {
		t.Fatal("want an error")
	}
	if n != 1 {
		t.Fatalf("tries %d, want 1", n)
	}
	// A GET with retryable: true is.
	atomic.StoreInt32(&n, 0)
	if _, err := c.Pricing(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if n != 3 {
		t.Fatalf("GET tries %d, want 3", n)
	}
}

func TestCloudRetryableTrueHonoured(t *testing.T) {
	resetSlept()
	var n int32
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) == 1 {
			// A 4xx the server says may work again.
			return rawAnswer{status: 409, json: map[string]any{"error": "busy_thing", "retryable": true}}
		}
		return rawAnswer{status: 200, json: map[string]any{"benchmarks": []any{map[string]any{"name": "swe"}}}}
	})
	bs, err := c.Benchmarks.List(context.Background())
	if err != nil || len(bs) != 1 {
		t.Fatalf("%v %v", bs, err)
	}
}

func TestCloudRetryBounded(t *testing.T) {
	resetSlept()
	var n int32
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		atomic.AddInt32(&n, 1)
		return rawAnswer{status: 503, json: map[string]any{"error": "database_busy", "retry_after": 1}}
	})
	_, err := c.Jobs.Get(context.Background(), "job_1")
	e, ok := AsError(err)
	if !ok || e.Code != "database_busy" || !e.Retryable || e.RetryAfter != time.Second {
		t.Fatalf("%#v", err)
	}
	if n != 3 {
		t.Fatalf("tries %d", n)
	}
}

func TestFailureOfServerRules(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
		class  FailureClass
		retry  bool
	}{
		{503, "database_busy", FailureInfra, true},
		{409, "template_building", FailureInfra, true},
		{409, "idempotency_in_progress", FailureInfra, true},
		{400, "invalid_timeout", FailureClient, false},
		{501, "not_available", FailureInfra, false},
	} {
		cl, rt := FailureOf(c.status, c.code)
		if cl != c.class || rt != c.retry {
			t.Errorf("%d %s: %s %v", c.status, c.code, cl, rt)
		}
	}
}

func TestCloudStreamRetry(t *testing.T) {
	resetSlept()
	var n int32
	s, c := newRawStub(t, func(r rawReq) rawAnswer {
		if atomic.AddInt32(&n, 1) == 1 {
			return rawAnswer{status: 503, json: map[string]any{"error": "database_busy", "retry_after": 1}}
		}
		if r.Method == "PUT" {
			return rawAnswer{status: 200, json: map[string]any{"file": map[string]any{"path": "/workspace/a"}}}
		}
		return rawAnswer{status: 200, raw: []byte("hello")}
	})
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1", State: "running"}}
	var w bytes.Buffer
	if _, err := b.Files().Download(context.Background(), "/workspace/a", &w); err != nil || w.String() != "hello" {
		t.Fatalf("%q %v", w.String(), err)
	}
	atomic.StoreInt32(&n, 0)
	if _, err := b.Files().Upload(context.Background(), "/workspace/a", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if got := string(s.last().Body); got != "data" {
		t.Fatalf("resent body %q", got)
	}
}

// ---- small sizes ----

func TestCloudSmallSizeCreate(t *testing.T) {
	s, c := newRawStub(t, func(r rawReq) rawAnswer {
		j := fullSandboxJSON("sbx_s")
		j["cpus"], j["mem_mb"], j["region"] = 0.25, 512, "cf-auto"
		return rawAnswer{status: 201, json: map[string]any{"sandbox": j}}
	})
	b, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{CPUs: 0.25, MemMB: 512, NoWait: true})
	if err != nil {
		t.Fatal(err)
	}
	m := s.last().JSON(t)
	if m["cpus"] != 0.25 || m["mem_mb"] != 512.0 {
		t.Fatalf("body %v", m)
	}
	if b.Info.CPUs != 0.25 || !b.Overflow() {
		t.Fatalf("%+v", b.Info)
	}
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{Size: "0.25x0.5", NoWait: true}); err != nil {
		t.Fatal(err)
	}
	if s.last().JSON(t)["size"] != "0.25x0.5" {
		t.Fatal("size not sent")
	}
	if _, err := c.OrgTemplates.Build(context.Background(), CloudTemplateBuild{Name: "x", Image: "i", Size: "0.5x1"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("template small size: %v", err)
	}
}

func TestCloudOverflowRefusal(t *testing.T) {
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 501, json: map[string]any{"error": "not_available", "message": "Fork is not available for this sandbox: it runs on kosa8's overflow capacity (Cloudflare)"}}
	})
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1", State: "running"}}
	_, err := b.Fork(context.Background(), 2)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "overflow capacity") {
		t.Fatalf("%v", err)
	}
	if _, rt := ClassifyFailure(err); rt {
		t.Fatal("overflow refusal must not be retried")
	}
}

// ---- endpoint groups ----

func TestCloudMoreRoutes(t *testing.T) {
	s, c := newRawStub(t, func(r rawReq) rawAnswer {
		switch {
		case strings.HasPrefix(r.Path, "/v1/audit/export"):
			return rawAnswer{status: 200, raw: []byte("{\"seq\":1}\n{\"seq\":2}\n")}
		case strings.HasPrefix(r.Path, "/v1/browsers/brw_1/page"):
			return rawAnswer{status: 200, json: map[string]any{"page": map[string]any{"url": "u", "title": "T", "screenshot_png_base64": "aGk="}}}
		case strings.HasPrefix(r.Path, "/v1/browsers"):
			return rawAnswer{status: 200, json: map[string]any{"browser": map[string]any{"id": "brw_1", "state": "open",
				"cdp_url": "wss://api/v1/browsers/brw_1/cdp?token=t"}}}
		case strings.HasPrefix(r.Path, "/v1/jobs"):
			return rawAnswer{status: 200, json: map[string]any{"job_id": "job_1", "run_id": "run_1", "job": map[string]any{"status": "done"}}}
		}
		return rawAnswer{status: 200, json: map[string]any{}}
	})
	ctx := context.Background()
	b := &CloudSandbox{c: c, Info: CloudSandboxInfo{ID: "sbx_1", State: "running"}}
	steps := []struct {
		want string
		call func() error
	}{
		{"POST /v1/browsers", func() error { _, err := c.Browsers.Create(ctx, CloudBrowserOptions{TTLS: 60}); return err }},
		{"GET /v1/browsers?state=all", func() error { _, err := c.Browsers.List(ctx, true); return err }},
		{"GET /v1/browsers/brw_1", func() error {
			br, err := c.Browsers.Get(ctx, "brw_1")
			if err == nil {
				var u string
				if u, err = br.CDPEndpoint(ctx); err == nil && !strings.Contains(u, "/cdp?token=") {
					err = errors.New(u)
				}
			}
			return err
		}},
		{"POST /v1/browsers/brw_1/page", func() error {
			png, err := (&CloudBrowser{c: c, Info: CloudBrowserInfo{ID: "brw_1"}}).Screenshot(ctx, "https://x", false)
			if err == nil && string(png) != "hi" {
				err = errors.New(string(png))
			}
			return err
		}},
		{"DELETE /v1/browsers/brw_1", func() error { return c.Browsers.Delete(ctx, "brw_1") }},
		{"GET /v1/ai/settings", func() error { _, err := c.AI.Settings(ctx); return err }},
		{"PATCH /v1/ai/settings", func() error { _, err := c.AI.UpdateSettings(ctx, CloudAISettingsUpdate{Enabled: Ptr(true)}); return err }},
		{"GET /v1/ai/keys", func() error { _, err := c.AI.Keys(ctx); return err }},
		{"PUT /v1/ai/keys/openai", func() error { _, err := c.AI.SetKey(ctx, "openai", "sk-x"); return err }},
		{"DELETE /v1/ai/keys/openai", func() error { return c.AI.DeleteKey(ctx, "openai") }},
		{"GET /v1/ai/usage?month=2026-11", func() error { _, err := c.AI.Usage(ctx, "2026-11"); return err }},
		{"POST /v1/ai/gw/openai/v1/chat/completions", func() error { return c.AI.Call(ctx, "openai", "/v1/chat/completions", map[string]any{"model": "m"}, nil) }},
		{"POST /v1/jobs", func() error {
			j, err := c.Jobs.Submit(ctx, CloudJobOptions{Name: "j", Template: "python", Tasks: []CloudJobTask{{ID: "1", Files: map[string][]byte{"/a": []byte("x")}}},
				Solver: CloudJobCmd{Cmd: []string{"python solve.py"}, TimeoutS: 60}, Grader: CloudJobCmd{Cmd: []string{"pytest"}}})
			if err == nil && (j.ID != "job_1" || j.RunID != "run_1" || !j.Done()) {
				err = errors.New("job fields")
			}
			return err
		}},
		{"POST /v1/jobs/estimate", func() error {
			_, err := c.Jobs.Estimate(ctx, CloudJobEstimateOptions{CloudJobOptions: CloudJobOptions{Name: "j", Image: "i", Tasks: []CloudJobTask{{ID: "1"}},
				Solver: CloudJobCmd{Cmd: []string{"a"}, TimeoutS: 5}, Grader: CloudJobCmd{Cmd: []string{"b"}}}, Sample: 2})
			return err
		}},
		{"GET /v1/jobs/job_1", func() error { _, err := c.Jobs.Get(ctx, "job_1"); return err }},
		{"GET /v1/jobs?status=running", func() error { _, err := c.Jobs.List(ctx, CloudJobListOptions{Status: "running"}); return err }},
		{"POST /v1/jobs/job_1/cancel", func() error { _, err := c.Jobs.Cancel(ctx, "job_1"); return err }},
		{"GET /v1/sandboxes/sbx_1/exec/bg_1", func() error { _, err := b.BackgroundExec(ctx, "bg_1"); return err }},
		{"POST /v1/sandboxes/sbx_1/exec/bg_1/stop", func() error { _, err := b.StopBackground(ctx, "bg_1"); return err }},
		{"GET /v1/checkpoints", func() error { _, err := c.Checkpoints(ctx); return err }},
		{"GET /v1/lineage", func() error { _, err := c.Lineage(ctx); return err }},
		{"GET /v1/images", func() error { _, err := c.Images(ctx); return err }},
		{"GET /v1/benchmarks", func() error { _, err := c.Benchmarks.List(ctx); return err }},
		{"GET /v1/benchmarks/swe", func() error { _, err := c.Benchmarks.Get(ctx, "swe"); return err }},
		{"GET /v1/benchmarks/swe/instances?limit=5", func() error {
			_, err := c.Benchmarks.Instances(ctx, "swe", CloudBenchmarkInstanceQuery{Limit: 5})
			return err
		}},
		{"GET /v1/benchmarks/swe/instances/i1", func() error { _, err := c.Benchmarks.Instance(ctx, "swe", "i1"); return err }},
		{"POST /v1/benchmarks/swe/prepare", func() error { _, err := c.Benchmarks.Prepare(ctx, "swe", []string{"i1"}, 0); return err }},
		{"GET /v1/rl/compare?ids=run_a%2Crun_b", func() error { _, err := c.CompareRuns(ctx, "run_a", "run_b"); return err }},
		{"GET /v1/rl/alerts?run_id=run_a", func() error { _, _, err := c.RLAlerts.List(ctx, "run_a"); return err }},
		{"POST /v1/rl/alerts", func() error {
			_, err := c.RLAlerts.Create(ctx, CloudRLAlertSpec{Kind: "reward_below", Threshold: 0.1})
			return err
		}},
		{"PATCH /v1/rl/alerts/al_1", func() error { _, err := c.RLAlerts.Update(ctx, "al_1", CloudRLAlertUpdate{Enabled: Ptr(false)}); return err }},
		{"DELETE /v1/rl/alerts/al_1", func() error { return c.RLAlerts.Delete(ctx, "al_1") }},
		{"GET /v1/usage/insights", func() error { _, err := c.UsageInsights(ctx); return err }},
		{"GET /v1/limits", func() error { _, err := c.Limits(ctx); return err }},
		{"POST /v1/limits/request", func() error { _, err := c.RequestLimit(ctx, 20, "evals"); return err }},
		{"GET /v1/pricing", func() error { _, err := c.Pricing(ctx); return err }},
		{"GET /v1/audit?limit=10", func() error { _, err := c.Audit.List(ctx, CloudAuditQuery{Limit: 10}); return err }},
		{"GET /v1/audit/verify", func() error { _, err := c.Audit.Verify(ctx); return err }},
		{"GET /v1/audit/export", func() error {
			ev, err := c.Audit.ExportEvents(ctx)
			if err == nil && len(ev) != 2 {
				err = errors.New("events")
			}
			return err
		}},
		{"GET /v1/api-keys", func() error { _, err := c.APIKeys.List(ctx); return err }},
		{"POST /v1/api-keys", func() error { _, err := c.APIKeys.Create(ctx, "ci", []string{"sandboxes:read"}, 0); return err }},
		{"DELETE /v1/api-keys/key_1", func() error { return c.APIKeys.Revoke(ctx, "key_1") }},
		{"POST /v1/api-keys/key_1/rotate", func() error { _, err := c.APIKeys.Rotate(ctx, "key_1", 60); return err }},
		{"GET /v1/functions/f/invocations?limit=5", func() error { _, err := c.Functions.Invocations(ctx, "f", 5); return err }},
		{"GET /v1/webhooks", func() error { _, _, err := c.Webhooks.List(ctx); return err }},
		{"POST /v1/webhooks", func() error { _, err := c.Webhooks.Create(ctx, "https://h", []string{"sandbox.created"}, ""); return err }},
		{"DELETE /v1/webhooks/wh_1", func() error { return c.Webhooks.Delete(ctx, "wh_1") }},
		{"GET /v1/webhooks/wh_1/deliveries", func() error { _, err := c.Webhooks.Deliveries(ctx, "wh_1", 0); return err }},
		{"POST /v1/webhooks/wh_1/test", func() error { _, err := c.Webhooks.Test(ctx, "wh_1"); return err }},
		{"DELETE /v1/sandboxes/sbx_1/previews/3000", func() error { return b.Previews().Delete(ctx, 3000) }},
		{"DELETE /v1/sandboxes/sbx_1/schedules/sch_1", func() error { return b.Schedules().Delete(ctx, "sch_1") }},
		{"DELETE /v1/sandboxes/sbx_1/code/contexts/main", func() error { return b.DeleteCodeContext(ctx, "main") }},
		{"GET /v1/recordings?sandbox_id=sbx_1", func() error { _, err := c.Recordings.List(ctx, "sbx_1", 0); return err }},
		{"POST /v1/recordings/rec_1/link", func() error { _, err := c.Recordings.Link(ctx, "rec_1", 900); return err }},
		{"DELETE /v1/recordings/rec_1", func() error { return c.Recordings.Delete(ctx, "rec_1") }},
		{"GET /v1/sandboxes/sbx_1/recordings", func() error { _, err := b.Recordings(ctx); return err }},
		{"POST /v1/sandboxes/sbx_1/embed", func() error { _, err := b.Embeds().Create(ctx, CloudEmbedOptions{Origins: []string{"https://a"}}); return err }},
		{"GET /v1/sandboxes/sbx_1/embeds", func() error { _, err := b.Embeds().List(ctx); return err }},
		{"DELETE /v1/sandboxes/sbx_1/embeds/emb_1", func() error { return b.Embeds().Revoke(ctx, "emb_1") }},
		{"POST /v1/sandboxes/sbx_1/previews/3000/domains", func() error { _, err := b.Previews().Domains(3000).Add(ctx, "a.example.com", ""); return err }},
		{"GET /v1/sandboxes/sbx_1/previews/3000/domains/a.example.com", func() error { _, err := b.Previews().Domains(3000).Get(ctx, "a.example.com"); return err }},
		{"GET /v1/previews/sbx_1:3000/domains", func() error { _, err := c.PreviewDomains("sbx_1:3000").List(ctx); return err }},
		{"DELETE /v1/previews/sbx_1:3000/domains/a.example.com", func() error { return c.PreviewDomains("sbx_1:3000").Remove(ctx, "a.example.com") }},
		{"GET /v1/org/egress", func() error { _, err := c.OrgEgress(ctx); return err }},
		{"PUT /v1/org/egress", func() error { _, err := c.SetOrgEgress(ctx, nil, false); return err }},
		{"GET /v1/billing/burst", func() error { _, err := c.Burst(ctx); return err }},
		{"POST /v1/billing/burst", func() error { _, err := c.ActivateBurst(ctx, 1); return err }},
		{"GET /v1/attack/scorecards", func() error { _, err := c.AttackScorecards(ctx, 0); return err }},
		{"GET /v1/attack/scorecards/atk_1", func() error { _, err := c.AttackScorecard(ctx, "atk_1"); return err }},
		{"POST /v1/attack/scorecards", func() error { _, err := c.UploadAttackScorecard(ctx, json.RawMessage(`{"a":1}`)); return err }},
	}
	for _, st := range steps {
		if err := st.call(); err != nil {
			t.Fatalf("%s: %v", st.want, err)
		}
		got := s.last().Method + " " + s.last().Path
		if got != st.want {
			t.Fatalf("sent %q, want %q", got, st.want)
		}
	}
	if g := c.AI.GatewayURL("anthropic"); g != c.BaseURL()+"/v1/ai/gw/anthropic/" {
		t.Fatal(g)
	}
}

func TestCloudJobBodyChecks(t *testing.T) {
	ok := CloudJobOptions{Name: "j", Template: "python", Tasks: []CloudJobTask{{ID: "1"}},
		Solver: CloudJobCmd{Cmd: []string{"a"}, TimeoutS: 5}, Grader: CloudJobCmd{Cmd: []string{"b"}}}
	if _, err := jobBody(ok); err != nil {
		t.Fatal(err)
	}
	bad := []func(o *CloudJobOptions){
		func(o *CloudJobOptions) { o.Name = "" },
		func(o *CloudJobOptions) { o.Image = "x" },
		func(o *CloudJobOptions) { o.Solver.TimeoutS = 0 },
		func(o *CloudJobOptions) { o.Tasks = append(o.Tasks, CloudJobTask{ID: "1"}) },
		func(o *CloudJobOptions) { o.Tasks = nil },
		func(o *CloudJobOptions) { o.Tasks[0].Files = map[string][]byte{"rel": nil} },
	}
	for i, f := range bad {
		o := ok
		o.Tasks = append([]CloudJobTask(nil), ok.Tasks...)
		f(&o)
		if _, err := jobBody(o); !errors.Is(err, ErrUsage) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	o := ok
	o.Tasks = nil
	o.TasksJSONL = "{\"id\":\"a\"}\n\n{\"id\":\"b\"}\n"
	b, err := jobBody(o)
	if err != nil || len(b["tasks"].([]map[string]any)) != 2 {
		t.Fatalf("%v %v", b, err)
	}
}
