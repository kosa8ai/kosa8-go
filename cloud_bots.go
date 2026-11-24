package kosa8

import (
	"context"
	"net/url"
	"strconv"
	"unicode/utf8"
)

// Autopilot bots: small, sponsored automations on the org's sandboxes (sleep
// what is idle, guard spend, block a threat, open a draft PR). A bot is made
// as a draft from a template, a spec or a sentence (Compile), tried with a dry
// run, and only a person turns it on (Enable). Actions above T2 wait for a
// person's approval (Cloud.Approvals).
//
// An agent never approves its own action: a person approves in the web app,
// the desktop app, the CLI or Slack. Enable, ResumeAll and
// CloudApprovals.Approve / Reject answer ErrHumanApprovalRequired (403
// human_required) for a capability token, MCP or an org key bound to no
// member; there is no way around that from the SDK.

// CloudBotSpecMaxText is the longest sentence Compile takes.
const CloudBotSpecMaxText = 500

// CloudBotLastRun is a bot's most recent run.
type CloudBotLastRun struct {
	ID    string `json:"id"`
	State string `json:"state"`
	At    int64  `json:"at"`
}

// CloudBot is one bot. Times are ms since the epoch. Spec, Plan and Versions
// come with Get (and Create / Update); List leaves them out.
type CloudBot struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	Template            *string          `json:"template"` // "spend-guard@1", or nil
	State               string           `json:"state"`    // draft, on, paused, budget_paused, broken
	Sponsor             string           `json:"sponsor"`
	Owner               string           `json:"owner"`
	CreatedBy           string           `json:"created_by"`
	Source              string           `json:"source"` // template, nl, api
	Version             int              `json:"version"`
	Scope               Info             `json:"scope"`
	BudgetUSD           *float64         `json:"budget_usd"`
	SpentUSD            float64          `json:"spent_usd"` // this month
	Notify              Info             `json:"notify"`
	ConsecutiveFailures int              `json:"consecutive_failures"`
	LastRun             *CloudBotLastRun `json:"last_run"`
	CreatedAt           int64            `json:"created_at"`
	UpdatedAt           int64            `json:"updated_at"`
	Free                bool             `json:"free"`
	Tier                string           `json:"tier"`
	Spec                Info             `json:"spec,omitempty"`
	Plan                Info             `json:"plan,omitempty"`
	Versions            []Info           `json:"versions,omitempty"`
}

// CloudBotList is GET /v1/bots: the bots, whether everything is paused, and
// how many approvals wait for a person.
type CloudBotList struct {
	Bots             []CloudBot `json:"bots"`
	PausedAll        bool       `json:"paused_all"`
	PausedAllAt      *int64     `json:"paused_all_at"`
	PendingApprovals int        `json:"pending_approvals"`
}

// CloudBotTemplate is a ready-made bot. Spec is what Create would save; the
// rest (plan, never, needs, icp, rank, why, ...) is kept in Raw as sent.
type CloudBotTemplate struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Tier    string `json:"tier"`
	Free    bool   `json:"free"`
	Spec    Info   `json:"spec"`
	Raw     Info   `json:"-"`
}

// CloudBotPlan is POST /v1/bots/plan: what a spec would do, the spec itself
// and, with Replay, what it would have done over the last 7 days.
type CloudBotPlan struct {
	Plan   Info `json:"plan"`
	Spec   Info `json:"spec"`
	Replay Info `json:"replay,omitempty"`
}

// CloudBotCompiled is POST /v1/bots/compile: a sentence made into a spec.
// Nothing is saved or enabled.
type CloudBotCompiled struct {
	Spec   Info     `json:"spec"`
	Plan   Info     `json:"plan"`
	Source string   `json:"source"` // ai or rules
	Notes  []string `json:"notes"`
}

// CloudBotStep is one step of a run.
type CloudBotStep struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Status     string `json:"status"` // pending, running, done, failed, skipped, waiting
	Detail     string `json:"detail,omitempty"`
	Action     string `json:"action,omitempty"`
	Args       Info   `json:"args,omitempty"`
	Tier       string `json:"tier,omitempty"`
	Result     any    `json:"result,omitempty"`
	Undo       Info   `json:"undo,omitempty"` // {available, done_at?}
	ApprovalID string `json:"approval_id,omitempty"`
	StartedAt  *int64 `json:"started_at,omitempty"`
	EndedAt    *int64 `json:"ended_at,omitempty"`
}

// CloudBotRun is one run of a bot (a dry run changes nothing).
type CloudBotRun struct {
	ID           string         `json:"id"`
	BotID        string         `json:"bot_id"`
	Version      int            `json:"version"`
	DryRun       bool           `json:"dry_run"`
	State        string         `json:"state"` // queued, running, waiting_approval, succeeded, failed, cancelled, budget_paused, skipped
	Trigger      Info           `json:"trigger"`
	Steps        []CloudBotStep `json:"steps"`
	Rationale    string         `json:"rationale"`
	CostUSD      float64        `json:"cost_usd"`
	FailureClass string         `json:"failure_class"`
	Replay       Info           `json:"replay,omitempty"`
	CreatedAt    int64          `json:"created_at"`
	StartedAt    *int64         `json:"started_at"`
	EndedAt      *int64         `json:"ended_at"`
}

// CloudBotPauseState is what PauseAll and ResumeAll answer.
type CloudBotPauseState struct {
	PausedAll bool   `json:"paused_all"`
	At        *int64 `json:"at"`
}

// CloudBotPlanOptions are what Plan plans: exactly one of Template and Spec.
type CloudBotPlanOptions struct {
	Template string // a template id, e.g. "spend-guard"
	Spec     Info
	Name     string
	Replay   bool // also replay the last 7 days of real events
}

// CloudBotCreateOptions make a draft bot: exactly one of Template and Spec.
// Sponsor (a member email) defaults to the person whose key this is; it is
// required for a key that is not a person's.
type CloudBotCreateOptions struct {
	Template  string
	Spec      Info
	Name      string
	Sponsor   string
	BudgetUSD *float64
	Scope     Info // {"labels": {"env": "dev"}}
	Notify    Info // {"mode": "digest"}
}

// CloudBotUpdateOptions change a bot; nil / "" fields are left alone. A new
// Spec is a new version (and a bot that is on goes back to draft when a key
// that is not a person's changes it).
type CloudBotUpdateOptions struct {
	Name      string
	Spec      Info
	Sponsor   string
	BudgetUSD *float64
	Scope     Info
	Notify    Info
}

// CloudBotRunOptions are optional settings for Run.
type CloudBotRunOptions struct {
	// DryRun: nil (default) is a dry run, what it would do now plus a replay
	// of the last 7 days; never billed, changes nothing. Ptr(false) runs it
	// for real (the bot must be on).
	DryRun *bool
}

// CloudBots are the org's Autopilot bots (Cloud.Bots).
type CloudBots struct{ c *Cloud }

func needID(what, id string) error {
	if id == "" {
		return usage("%s needs an id", what)
	}
	return nil
}

func oneOf(what, template string, spec Info) error {
	if (template == "") == (spec == nil) {
		return usage("%s needs exactly one of Template and Spec", what)
	}
	return nil
}

// List is the org's bots, whether everything is paused, and how many
// approvals are waiting.
func (s *CloudBots) List(ctx context.Context) (CloudBotList, error) {
	var r CloudBotList
	err := s.c.request(ctx, "GET", "/v1/bots", nil, &r)
	if r.Bots == nil {
		r.Bots = []CloudBot{}
	}
	return r, err
}

func templateOf(m Info) CloudBotTemplate {
	t := CloudBotTemplate{Raw: m}
	t.ID, _ = m["id"].(string)
	if v, ok := m["version"].(float64); ok {
		t.Version = int(v)
	}
	t.Title, _ = m["title"].(string)
	t.Summary, _ = m["summary"].(string)
	t.Tier, _ = m["tier"].(string)
	t.Free, _ = m["free"].(bool)
	t.Spec, _ = m["spec"].(map[string]any)
	return t
}

// Templates are the ready-made bots, ranked for this org.
func (s *CloudBots) Templates(ctx context.Context) ([]CloudBotTemplate, error) {
	var r struct {
		Templates []Info `json:"templates"`
	}
	if err := s.c.request(ctx, "GET", "/v1/bot-templates", nil, &r); err != nil {
		return nil, err
	}
	out := make([]CloudBotTemplate, 0, len(r.Templates))
	for _, m := range r.Templates {
		out = append(out, templateOf(m))
	}
	return out, nil
}

// Template is one ready-made bot.
func (s *CloudBots) Template(ctx context.Context, id string) (CloudBotTemplate, error) {
	if err := needID("Template", id); err != nil {
		return CloudBotTemplate{}, err
	}
	var r struct {
		Template Info `json:"template"`
	}
	if err := s.c.request(ctx, "GET", "/v1/bot-templates/"+esc(id), nil, &r); err != nil {
		return CloudBotTemplate{}, err
	}
	return templateOf(r.Template), nil
}

// Plan says what a template or spec would do, saving nothing.
func (s *CloudBots) Plan(ctx context.Context, o CloudBotPlanOptions) (CloudBotPlan, error) {
	var r CloudBotPlan
	if err := oneOf("Plan", o.Template, o.Spec); err != nil {
		return r, err
	}
	body := map[string]any{}
	if o.Template != "" {
		body["template"] = o.Template
	} else {
		body["spec"] = o.Spec
	}
	if o.Name != "" {
		body["name"] = o.Name
	}
	if o.Replay {
		body["replay"] = true
	}
	err := s.c.request(ctx, "POST", "/v1/bots/plan", body, &r)
	return r, err
}

// Compile makes a sentence (at most 500 characters) into a spec and its
// plan. It never saves or enables anything.
func (s *CloudBots) Compile(ctx context.Context, text string) (CloudBotCompiled, error) {
	var r CloudBotCompiled
	if text == "" {
		return r, usage("Compile needs a sentence")
	}
	if n := utf8.RuneCountInString(text); n > CloudBotSpecMaxText {
		return r, usage("Compile takes at most %d characters, got %d", CloudBotSpecMaxText, n)
	}
	err := s.c.request(ctx, "POST", "/v1/bots/compile", map[string]any{"text": text}, &r)
	return r, err
}

func botBody(name, sponsor string, budget *float64, scope, notify Info) map[string]any {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if sponsor != "" {
		body["sponsor"] = sponsor
	}
	if budget != nil {
		body["budget_usd"] = *budget
	}
	if scope != nil {
		body["scope"] = scope
	}
	if notify != nil {
		body["notify"] = notify
	}
	return body
}

func (s *CloudBots) bot(ctx context.Context, method, path string, body any) (CloudBot, error) {
	var r struct {
		Bot CloudBot `json:"bot"`
	}
	err := s.c.request(ctx, method, path, body, &r)
	return r.Bot, err
}

// Create saves a draft bot from a template or a spec. It is not on until a
// person enables it.
func (s *CloudBots) Create(ctx context.Context, o CloudBotCreateOptions) (CloudBot, error) {
	if err := oneOf("Create", o.Template, o.Spec); err != nil {
		return CloudBot{}, err
	}
	body := botBody(o.Name, o.Sponsor, o.BudgetUSD, o.Scope, o.Notify)
	if o.Template != "" {
		body["template"] = o.Template
	} else {
		body["spec"] = o.Spec
	}
	return s.bot(ctx, "POST", "/v1/bots", body)
}

// Get is one bot, with its spec, plan and versions.
func (s *CloudBots) Get(ctx context.Context, id string) (CloudBot, error) {
	if err := needID("Get", id); err != nil {
		return CloudBot{}, err
	}
	return s.bot(ctx, "GET", "/v1/bots/"+esc(id), nil)
}

// Update changes a bot.
func (s *CloudBots) Update(ctx context.Context, id string, o CloudBotUpdateOptions) (CloudBot, error) {
	if err := needID("Update", id); err != nil {
		return CloudBot{}, err
	}
	body := botBody(o.Name, o.Sponsor, o.BudgetUSD, o.Scope, o.Notify)
	if o.Spec != nil {
		body["spec"] = o.Spec
	}
	if len(body) == 0 {
		return CloudBot{}, usage("Update needs at least one field to change")
	}
	return s.bot(ctx, "PATCH", "/v1/bots/"+esc(id), body)
}

// Delete deletes a bot; its runs are kept and its pending approvals rejected.
func (s *CloudBots) Delete(ctx context.Context, id string) error {
	if err := needID("Delete", id); err != nil {
		return err
	}
	return s.c.request(ctx, "DELETE", "/v1/bots/"+esc(id), nil, nil)
}

// Enable turns a bot on. Only a person can: any other key gets
// ErrHumanApprovalRequired.
func (s *CloudBots) Enable(ctx context.Context, id string) (CloudBot, error) {
	if err := needID("Enable", id); err != nil {
		return CloudBot{}, err
	}
	return s.bot(ctx, "POST", "/v1/bots/"+esc(id)+"/enable", map[string]any{})
}

// Disable pauses a bot. Any key with sandboxes:write may.
func (s *CloudBots) Disable(ctx context.Context, id string) (CloudBot, error) {
	if err := needID("Disable", id); err != nil {
		return CloudBot{}, err
	}
	return s.bot(ctx, "POST", "/v1/bots/"+esc(id)+"/disable", map[string]any{})
}

// Run runs a bot now: a dry run unless o.DryRun is Ptr(false). o may be nil.
func (s *CloudBots) Run(ctx context.Context, id string, o *CloudBotRunOptions) (CloudBotRun, error) {
	if err := needID("Run", id); err != nil {
		return CloudBotRun{}, err
	}
	dry := true
	if o != nil && o.DryRun != nil {
		dry = *o.DryRun
	}
	return s.run(ctx, "POST", "/v1/bots/"+esc(id)+"/runs", map[string]any{"dry_run": dry})
}

func (s *CloudBots) run(ctx context.Context, method, path string, body any) (CloudBotRun, error) {
	var r struct {
		Run CloudBotRun `json:"run"`
	}
	err := s.c.request(ctx, method, path, body, &r)
	return r.Run, err
}

// Runs are a bot's recent runs, newest first; limit 1..100 (0: the server's default).
func (s *CloudBots) Runs(ctx context.Context, id string, limit int) ([]CloudBotRun, error) {
	if err := needID("Runs", id); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 100 {
		return nil, usage("Runs limit is 1..100, got %d", limit)
	}
	p := "/v1/bots/" + esc(id) + "/runs"
	if limit > 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var r struct {
		Runs []CloudBotRun `json:"runs"`
	}
	if err := s.c.request(ctx, "GET", p, nil, &r); err != nil {
		return nil, err
	}
	if r.Runs == nil {
		r.Runs = []CloudBotRun{}
	}
	return r.Runs, nil
}

// GetRun is one run.
func (s *CloudBots) GetRun(ctx context.Context, runID string) (CloudBotRun, error) {
	if err := needID("GetRun", runID); err != nil {
		return CloudBotRun{}, err
	}
	return s.run(ctx, "GET", "/v1/bot-runs/"+esc(runID), nil)
}

// CancelRun cancels a run; its pending approvals are rejected.
func (s *CloudBots) CancelRun(ctx context.Context, runID string) (CloudBotRun, error) {
	if err := needID("CancelRun", runID); err != nil {
		return CloudBotRun{}, err
	}
	return s.run(ctx, "POST", "/v1/bot-runs/"+esc(runID)+"/cancel", map[string]any{})
}

// UndoStep undoes one step of a run that has an undo (wake a sandbox, lift
// the bot's own block).
func (s *CloudBots) UndoStep(ctx context.Context, runID, key string) (CloudBotRun, error) {
	if err := needID("UndoStep", runID); err != nil {
		return CloudBotRun{}, err
	}
	if key == "" {
		return CloudBotRun{}, usage("UndoStep needs a step key")
	}
	return s.run(ctx, "POST", "/v1/bot-runs/"+esc(runID)+"/steps/"+esc(key)+"/undo", map[string]any{})
}

// PauseAll stops every bot of the org (admin; an agent may).
func (s *CloudBots) PauseAll(ctx context.Context) (CloudBotPauseState, error) {
	var r CloudBotPauseState
	err := s.c.request(ctx, "POST", "/v1/bots/pause-all", map[string]any{}, &r)
	return r, err
}

// ResumeAll lets the bots run again (admin, and only a person: any other key
// gets ErrHumanApprovalRequired).
func (s *CloudBots) ResumeAll(ctx context.Context) (CloudBotPauseState, error) {
	var r CloudBotPauseState
	err := s.c.request(ctx, "POST", "/v1/bots/resume-all", map[string]any{}, &r)
	return r, err
}

// CloudApprovalDecisionInfo is who decided an approval, how and when.
type CloudApprovalDecisionInfo struct {
	By         string `json:"by"`
	Via        string `json:"via"` // web, slack, api
	At         int64  `json:"at"`
	Reason     string `json:"reason"`
	EditedArgs Info   `json:"edited_args"`
}

// CloudApproval is a bot action waiting for (or given) a person's decision.
type CloudApproval struct {
	ID            string                     `json:"id"`
	RunID         string                     `json:"run_id"`
	BotID         string                     `json:"bot_id"`
	BotName       string                     `json:"bot_name"`
	StepKey       string                     `json:"step_key"`
	Action        string                     `json:"action"`
	Tier          string                     `json:"tier"`
	Args          Info                       `json:"args"`
	Rationale     string                     `json:"rationale"`
	RequestedRole string                     `json:"requested_role"`
	State         string                     `json:"state"` // pending, approved, rejected, expired
	ExpiresAt     int64                      `json:"expires_at"`
	Decision      *CloudApprovalDecisionInfo `json:"decision"`
	CreatedAt     int64                      `json:"created_at"`
}

// CloudApprovalResult is what a decision answers: the approval and its run.
type CloudApprovalResult struct {
	Approval CloudApproval `json:"approval"`
	Run      CloudBotRun   `json:"run"`
}

// CloudApproveOptions are optional settings for Approve.
type CloudApproveOptions struct {
	Reason     string
	EditedArgs Info // checked against the action's schema by the server
}

// CloudApprovals are the bot actions waiting for a person (Cloud.Approvals).
//
// An agent never approves its own action: a person approves in the web app,
// the desktop app, the CLI or Slack. Approve and Reject work only with a
// person's key (one bound to a member, as `kosa8 login` makes); a capability
// token, MCP or an org key bound to no member gets ErrHumanApprovalRequired.
type CloudApprovals struct{ c *Cloud }

// List is the approvals in state ("" or "pending", "decided", "all"); limit 0
// is the server's default.
func (s *CloudApprovals) List(ctx context.Context, state string, limit int) ([]CloudApproval, error) {
	switch state {
	case "", "pending", "decided", "all":
	default:
		return nil, usage("approval state is pending, decided or all, got %q", state)
	}
	if limit < 0 {
		return nil, usage("limit must not be negative")
	}
	if state == "" {
		state = "pending"
	}
	q := url.Values{"state": {state}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var r struct {
		Approvals []CloudApproval `json:"approvals"`
	}
	if err := s.c.request(ctx, "GET", "/v1/approvals?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	if r.Approvals == nil {
		r.Approvals = []CloudApproval{}
	}
	return r.Approvals, nil
}

// Get is one approval.
func (s *CloudApprovals) Get(ctx context.Context, id string) (CloudApproval, error) {
	if err := needID("Get", id); err != nil {
		return CloudApproval{}, err
	}
	var r struct {
		Approval CloudApproval `json:"approval"`
	}
	err := s.c.request(ctx, "GET", "/v1/approvals/"+esc(id), nil, &r)
	return r.Approval, err
}

func (s *CloudApprovals) decide(ctx context.Context, id string, body map[string]any) (CloudApprovalResult, error) {
	var r CloudApprovalResult
	if err := needID("a decision", id); err != nil {
		return r, err
	}
	err := s.c.request(ctx, "POST", "/v1/approvals/"+esc(id)+"/decide", body, &r)
	return r, err
}

// Approve approves a waiting action as the person whose key this is. o may
// be nil. Not a person's key: ErrHumanApprovalRequired. Expired or already
// decided: ErrConflict (code expired / decided).
func (s *CloudApprovals) Approve(ctx context.Context, id string, o *CloudApproveOptions) (CloudApprovalResult, error) {
	body := map[string]any{"decision": "approve"}
	if o != nil {
		if o.Reason != "" {
			body["reason"] = o.Reason
		}
		if o.EditedArgs != nil {
			body["edited_args"] = o.EditedArgs
		}
	}
	return s.decide(ctx, id, body)
}

// Reject rejects a waiting action (reason may be ""). Only a person's key.
func (s *CloudApprovals) Reject(ctx context.Context, id, reason string) (CloudApprovalResult, error) {
	body := map[string]any{"decision": "reject"}
	if reason != "" {
		body["reason"] = reason
	}
	return s.decide(ctx, id, body)
}
