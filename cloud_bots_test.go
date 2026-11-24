package kosa8

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCloudBotsCalls(t *testing.T) {
	bot := map[string]any{"id": "bot_1", "name": "Spend guard", "template": "spend-guard@1", "state": "draft", "version": 1,
		"budget_usd": 5, "last_run": map[string]any{"id": "brun_1", "state": "succeeded", "at": 1}, "free": true, "tier": "T1"}
	run := map[string]any{"id": "brun_1", "bot_id": "bot_1", "dry_run": true, "state": "succeeded",
		"steps":  []any{map[string]any{"key": "s1", "label": "Sleep idle box", "status": "done", "undo": map[string]any{"available": true}}},
		"replay": map[string]any{"events": 3}}
	tpl := map[string]any{"id": "spend-guard", "version": 1, "title": "Spend guard", "tier": "T2", "free": true,
		"spec": map[string]any{"triggers": []any{}}, "plan": map[string]any{"summary": "x"}, "rank": 1}
	s, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case r.Path == "/v1/bots" && r.Method == "GET":
			return 200, map[string]any{"bots": []any{bot}, "paused_all": true, "paused_all_at": 9, "pending_approvals": 2}
		case r.Path == "/v1/bot-templates":
			return 200, map[string]any{"templates": []any{tpl}}
		case strings.HasPrefix(r.Path, "/v1/bot-templates/"):
			return 200, map[string]any{"template": tpl}
		case r.Path == "/v1/bots/plan":
			return 200, map[string]any{"plan": map[string]any{"summary": "s"}, "spec": map[string]any{"a": 1}, "replay": map[string]any{"events": 1}}
		case r.Path == "/v1/bots/compile":
			return 200, map[string]any{"spec": map[string]any{}, "plan": map[string]any{}, "source": "rules", "notes": []any{"n"}}
		case strings.HasSuffix(r.Path, "-all"):
			return 200, map[string]any{"paused_all": strings.Contains(r.Path, "pause"), "at": 5}
		case r.Method == "DELETE":
			return 200, map[string]any{"deleted": true}
		case strings.Contains(r.Path, "/runs?"):
			return 200, map[string]any{"runs": []any{run}}
		case strings.Contains(r.Path, "/runs") || strings.HasPrefix(r.Path, "/v1/bot-runs/"):
			return 201, map[string]any{"run": run}
		default:
			return 200, map[string]any{"bot": bot}
		}
	})
	ctx := context.Background()
	l, err := c.Bots.List(ctx)
	if err != nil || !l.PausedAll || l.PendingApprovals != 2 || l.Bots[0].ID != "bot_1" || *l.Bots[0].Template != "spend-guard@1" || l.Bots[0].LastRun.ID != "brun_1" {
		t.Fatalf("%+v %v", l, err)
	}
	ts, err := c.Bots.Templates(ctx)
	if err != nil || ts[0].ID != "spend-guard" || ts[0].Version != 1 || ts[0].Spec == nil || ts[0].Raw["rank"] != float64(1) {
		t.Fatalf("%+v %v", ts, err)
	}
	if tp, err := c.Bots.Template(ctx, "spend-guard"); err != nil || tp.Title != "Spend guard" {
		t.Fatalf("%+v %v", tp, err)
	}
	pl, err := c.Bots.Plan(ctx, CloudBotPlanOptions{Template: "spend-guard", Name: "g", Replay: true})
	if err != nil || pl.Plan["summary"] != "s" || pl.Replay == nil {
		t.Fatalf("%+v %v", pl, err)
	}
	if _, err := c.Bots.Plan(ctx, CloudBotPlanOptions{Spec: Info{"triggers": []any{}}}); err != nil {
		t.Fatal(err)
	}
	if cm, err := c.Bots.Compile(ctx, "sleep idle dev boxes after 20 minutes"); err != nil || cm.Source != "rules" || cm.Notes[0] != "n" {
		t.Fatalf("%+v %v", cm, err)
	}
	if b, err := c.Bots.Create(ctx, CloudBotCreateOptions{Template: "spend-guard", Name: "g", Sponsor: "a@b.co", BudgetUSD: Ptr(5.0),
		Scope: Info{"labels": Info{"env": "dev"}}, Notify: Info{"mode": "digest"}}); err != nil || b.State != "draft" || *b.BudgetUSD != 5 {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := c.Bots.Create(ctx, CloudBotCreateOptions{Spec: Info{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.Get(ctx, "bot_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.Update(ctx, "bot_1", CloudBotUpdateOptions{Name: "n2", Spec: Info{"y": 2}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Bots.Delete(ctx, "bot_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.Enable(ctx, "bot_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.Disable(ctx, "bot_1"); err != nil {
		t.Fatal(err)
	}
	r, err := c.Bots.Run(ctx, "bot_1", nil)
	if err != nil || !r.DryRun || r.Steps[0].Key != "s1" || r.Steps[0].Undo["available"] != true {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := c.Bots.Run(ctx, "bot_1", &CloudBotRunOptions{DryRun: Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Bots.Runs(ctx, "bot_1", 10); err != nil || len(rs) != 1 {
		t.Fatalf("%+v %v", rs, err)
	}
	if _, err := c.Bots.GetRun(ctx, "brun_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.CancelRun(ctx, "brun_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bots.UndoStep(ctx, "brun_1", "s1"); err != nil {
		t.Fatal(err)
	}
	if p, err := c.Bots.PauseAll(ctx); err != nil || !p.PausedAll {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := c.Bots.ResumeAll(ctx); err != nil || p.PausedAll {
		t.Fatalf("%+v %v", p, err)
	}
	// An id with a slash is escaped, not a new path segment.
	if _, err := c.Bots.Get(ctx, "bot_a/b c"); err != nil {
		t.Fatal(err)
	}

	type call struct {
		Line string
		Body map[string]any
	}
	var got []call
	for _, r := range s.seen {
		got = append(got, call{r.Method + " " + r.Path, r.Body})
	}
	want := []call{
		{"GET /v1/bots", nil},
		{"GET /v1/bot-templates", nil},
		{"GET /v1/bot-templates/spend-guard", nil},
		{"POST /v1/bots/plan", map[string]any{"template": "spend-guard", "name": "g", "replay": true}},
		{"POST /v1/bots/plan", map[string]any{"spec": map[string]any{"triggers": []any{}}}},
		{"POST /v1/bots/compile", map[string]any{"text": "sleep idle dev boxes after 20 minutes"}},
		{"POST /v1/bots", map[string]any{"template": "spend-guard", "name": "g", "sponsor": "a@b.co", "budget_usd": 5.0,
			"scope": map[string]any{"labels": map[string]any{"env": "dev"}}, "notify": map[string]any{"mode": "digest"}}},
		{"POST /v1/bots", map[string]any{"spec": map[string]any{"x": 1.0}}},
		{"GET /v1/bots/bot_1", nil},
		{"PATCH /v1/bots/bot_1", map[string]any{"name": "n2", "spec": map[string]any{"y": 2.0}}},
		{"DELETE /v1/bots/bot_1", nil},
		{"POST /v1/bots/bot_1/enable", map[string]any{}},
		{"POST /v1/bots/bot_1/disable", map[string]any{}},
		{"POST /v1/bots/bot_1/runs", map[string]any{"dry_run": true}},
		{"POST /v1/bots/bot_1/runs", map[string]any{"dry_run": false}},
		{"GET /v1/bots/bot_1/runs?limit=10", nil},
		{"GET /v1/bot-runs/brun_1", nil},
		{"POST /v1/bot-runs/brun_1/cancel", map[string]any{}},
		{"POST /v1/bot-runs/brun_1/steps/s1/undo", map[string]any{}},
		{"POST /v1/bots/pause-all", map[string]any{}},
		{"POST /v1/bots/resume-all", map[string]any{}},
		{"GET /v1/bots/bot_a%2Fb%20c", nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d calls: %+v", len(got), got)
	}
	for i := range want {
		if got[i].Line != want[i].Line || !reflect.DeepEqual(got[i].Body, want[i].Body) {
			t.Fatalf("call %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestCloudBotsChecks(t *testing.T) {
	s, c := newCloudStub(t, func(seenReq) (int, any) { return 200, map[string]any{} })
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["create none"] = c.Bots.Create(ctx, CloudBotCreateOptions{Name: "x"})
	_, checks["create both"] = c.Bots.Create(ctx, CloudBotCreateOptions{Template: "t", Spec: Info{}})
	_, checks["plan none"] = c.Bots.Plan(ctx, CloudBotPlanOptions{})
	_, checks["compile long"] = c.Bots.Compile(ctx, strings.Repeat("a", 501))
	_, checks["compile empty"] = c.Bots.Compile(ctx, "")
	_, checks["get empty"] = c.Bots.Get(ctx, "")
	_, checks["update nothing"] = c.Bots.Update(ctx, "bot_1", CloudBotUpdateOptions{})
	_, checks["runs limit"] = c.Bots.Runs(ctx, "bot_1", 101)
	_, checks["undo key"] = c.Bots.UndoStep(ctx, "brun_1", "")
	_, checks["approvals state"] = c.Approvals.List(ctx, "maybe", 0)
	_, checks["approve empty"] = c.Approvals.Approve(ctx, "", nil)
	for k, err := range checks {
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%s: %v", k, err)
		}
	}
	if len(s.seen) != 0 {
		t.Fatalf("sent %v", s.seen)
	}
	// 500 characters (counted as characters, not bytes) is allowed.
	if _, err := c.Bots.Compile(ctx, strings.Repeat("é", 500)); err != nil {
		t.Fatal(err)
	}
}

func TestCloudApprovalsCalls(t *testing.T) {
	appr := map[string]any{"id": "appr_1", "run_id": "brun_1", "bot_id": "bot_1", "action": "sandbox.delete", "tier": "T3",
		"args": map[string]any{"id": "sbx_1"}, "state": "pending", "decision": nil}
	s, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case strings.HasPrefix(r.Path, "/v1/approvals?"):
			return 200, map[string]any{"approvals": []any{appr}}
		case strings.HasSuffix(r.Path, "/decide"):
			d := map[string]any{}
			for k, v := range appr {
				d[k] = v
			}
			d["state"] = "approved"
			d["decision"] = map[string]any{"by": "a@b.co", "via": "api", "at": 3, "reason": "ok"}
			return 200, map[string]any{"approval": d, "run": map[string]any{"id": "brun_1", "state": "running"}}
		default:
			return 200, map[string]any{"approval": appr}
		}
	})
	ctx := context.Background()
	l, err := c.Approvals.List(ctx, "", 0)
	if err != nil || l[0].ID != "appr_1" || l[0].Args["id"] != "sbx_1" || l[0].Decision != nil {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := c.Approvals.List(ctx, "all", 5); err != nil {
		t.Fatal(err)
	}
	if a, err := c.Approvals.Get(ctx, "appr_1"); err != nil || a.Tier != "T3" {
		t.Fatalf("%+v %v", a, err)
	}
	d, err := c.Approvals.Approve(ctx, "appr_1", &CloudApproveOptions{Reason: "ok", EditedArgs: Info{"id": "sbx_2"}})
	if err != nil || d.Approval.State != "approved" || d.Approval.Decision.Via != "api" || d.Run.State != "running" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := c.Approvals.Approve(ctx, "appr_1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Approvals.Reject(ctx, "appr_1", "no"); err != nil {
		t.Fatal(err)
	}
	type call struct {
		Line string
		Body map[string]any
	}
	var got []call
	for _, r := range s.seen {
		got = append(got, call{r.Method + " " + r.Path, r.Body})
	}
	want := []call{
		{"GET /v1/approvals?state=pending", nil},
		{"GET /v1/approvals?limit=5&state=all", nil},
		{"GET /v1/approvals/appr_1", nil},
		{"POST /v1/approvals/appr_1/decide", map[string]any{"decision": "approve", "reason": "ok", "edited_args": map[string]any{"id": "sbx_2"}}},
		{"POST /v1/approvals/appr_1/decide", map[string]any{"decision": "approve"}},
		{"POST /v1/approvals/appr_1/decide", map[string]any{"decision": "reject", "reason": "no"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestCloudHumanRequired(t *testing.T) {
	_, c := newCloudStub(t, func(r seenReq) (int, any) {
		return 403, map[string]any{"error": "human_required", "message": "a person must approve this: open app.kosa8.com/app#autopilot or Slack"}
	})
	ctx := context.Background()
	errs := map[string]error{}
	_, errs["approve"] = c.Approvals.Approve(ctx, "appr_1", nil)
	_, errs["reject"] = c.Approvals.Reject(ctx, "appr_1", "")
	_, errs["enable"] = c.Bots.Enable(ctx, "bot_1")
	_, errs["resume_all"] = c.Bots.ResumeAll(ctx)
	for k, err := range errs {
		e, ok := AsError(err)
		if !errors.Is(err, ErrHumanApprovalRequired) || !errors.Is(err, ErrPolicy) || !errors.Is(err, ErrAPI) || !ok ||
			e.Status != 403 || e.Code != "human_required" || !strings.Contains(e.Message, "a person must approve") {
			t.Errorf("%s: %v", k, err)
		}
		if errors.Is(err, ErrGitHubNotConnected) {
			t.Errorf("%s matched another code", k)
		}
	}
	// A plain 403 is a policy refusal but not human_required.
	_, c2 := newCloudStub(t, func(seenReq) (int, any) { return 403, map[string]any{"error": "forbidden", "message": "no"} })
	if _, err := c2.Bots.Enable(ctx, "bot_1"); !errors.Is(err, ErrPolicy) || errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatal(err)
	}
}
