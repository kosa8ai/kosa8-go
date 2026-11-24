package kosa8

// kosa8 Cloud: the same sandboxes, run on kosa8's hosts (or your own, with
// bring-your-own-cloud) instead of the local daemon.
//
//	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{}) // KOSA8_API_KEY from the environment
//	box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "python", TTLSeconds: 3600})
//	r, _ := box.Exec(ctx, []string{"python3", "-c", "print(6 * 7)"}, nil)
//	fmt.Print(r.Stdout) // 42
//	_ = box.Delete(ctx)
//
// Authentication is an API key (k8c_live_...) made in the cloud dashboard.
// Errors are the SDK's usual *Error; each carries the cloud's stable Code
// (for example payment_required, quota_exceeded); see ErrPaymentRequired,
// ErrOrgFrozen, ErrRateLimit, ErrExecTimeout and ErrUnsupported.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultCloudURL is the kosa8 Cloud control plane.
const DefaultCloudURL = "https://api.kosa8.com"

// CloudKeyPrefix starts every kosa8 Cloud API key.
const CloudKeyPrefix = "k8c_live_"

// CloudTokenPrefix starts a capability token (CloudTokens.Create), which a
// Cloud client takes in place of an API key.
const CloudTokenPrefix = "k8c_tok_"

// CloudOptions configures a Cloud client.
type CloudOptions struct {
	APIKey     string              // default $KOSA8_API_KEY
	BaseURL    string              // default $KOSA8_CLOUD_URL, then DefaultCloudURL
	HTTPClient *http.Client        // default a plain http.Client
	Timeout    time.Duration       // per request; default 120 s
	Getenv     func(string) string // for tests; default os.Getenv
	// Retry is applied to every request where trying again is safe: reads,
	// deletes, heartbeats and drains on infra failures and on any answer
	// with retryable: true; the calls that make sandboxes and jobs (create,
	// batch, fork, grade, jobs), which carry an Idempotency-Key (the SDK's
	// own is reused after no answer, a 429 or a 409, and renewed after a
	// 5xx; yours is never changed); and any request, a POST too, answered 429, 503
	// database_busy or 409 template_building (the cloud refused it before
	// acting on it), waiting at least the Retry-After header or the body's
	// retry_after / retry_after_s. Backoff is exponential with full jitter.
	// Default DefaultRetryPolicy(); &NoRetry turns it off.
	Retry *RetryPolicy
}

// Cloud is a client for the kosa8 Cloud control plane.
type Cloud struct {
	Sandboxes *CloudSandboxes
	Functions *CloudFunctions
	Snapshots *CloudSnapshots // durable snapshots
	Secrets   *CloudSecrets   // the org's secrets
	Pipelines *CloudPipelines // CI runs and hooks

	baseURL string
	key     string
	hc      *http.Client
	timeout time.Duration
	retry   RetryPolicy

	OrgTemplates *CloudOrgTemplates // the org's own templates, built once and restored
	Tokens       *CloudTokens       // short-lived capability tokens

	Browsers   *CloudBrowsers   // headless browsers over CDP
	AI         *CloudAI         // the AI gateway: provider keys, settings, usage
	Jobs       *CloudJobs       // hosted jobs: tasks, a solver and a sealed grader
	Benchmarks *CloudBenchmarks // ready-made benchmarks and their environments
	Recordings *CloudRecordings // desktop session recordings
	APIKeys    *CloudAPIKeys    // the org's API keys
	Webhooks   *CloudWebhooks   // webhook endpoints
	Audit      *CloudAudit      // the tamper-evident audit log
	RLAlerts   *CloudRLAlerts   // alert rules on RL runs
}

func checkCloudBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", usage("kosa8 Cloud address is not a URL: %s", raw)
	}
	h := u.Hostname()
	local := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		// The API key travels in every request.
		return "", usage("kosa8 Cloud address must use https: %s", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// NewCloud returns a Cloud client. The key must start with k8c_live_ (or be
// a k8c_tok_ capability token) and the
// address must use https (plain http only to localhost).
func NewCloud(o CloudOptions) (*Cloud, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	key := o.APIKey
	if key == "" {
		key = getenv("KOSA8_API_KEY")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, usage("no kosa8 Cloud API key: %s", keyHint)
	}
	if !strings.HasPrefix(key, CloudKeyPrefix) && !strings.HasPrefix(key, CloudTokenPrefix) {
		return nil, usage("a kosa8 Cloud API key starts with %s (a capability token with %s); copy the whole key from https://app.kosa8.com/app#keys", CloudKeyPrefix, CloudTokenPrefix)
	}
	base := o.BaseURL
	if base == "" {
		base = getenv("KOSA8_CLOUD_URL")
	}
	if base == "" {
		base = DefaultCloudURL
	}
	base, err := checkCloudBase(base)
	if err != nil {
		return nil, err
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	retry := DefaultRetryPolicy()
	if o.Retry != nil {
		retry = *o.Retry
	}
	c := &Cloud{baseURL: base, key: key, hc: hc, timeout: timeout, retry: retry}
	c.OrgTemplates = &CloudOrgTemplates{c: c}
	c.Tokens = &CloudTokens{c: c}
	c.Sandboxes = &CloudSandboxes{c: c}
	c.Functions = &CloudFunctions{c: c}
	c.Snapshots = &CloudSnapshots{c: c}
	c.Secrets = &CloudSecrets{c: c}
	c.Pipelines = &CloudPipelines{c: c}
	c.Browsers = &CloudBrowsers{c: c}
	c.AI = &CloudAI{c: c}
	c.Jobs = &CloudJobs{c: c}
	c.Benchmarks = &CloudBenchmarks{c: c}
	c.Recordings = &CloudRecordings{c: c}
	c.APIKeys = &CloudAPIKeys{c: c}
	c.Webhooks = &CloudWebhooks{c: c}
	c.Audit = &CloudAudit{c: c}
	c.RLAlerts = &CloudRLAlerts{c: c}
	return c, nil
}

// BaseURL is the control-plane address in use.
func (c *Cloud) BaseURL() string { return c.baseURL }

// String never includes the API key.
func (c *Cloud) String() string { return fmt.Sprintf("kosa8.Cloud(%s)", c.baseURL) }

// GoString keeps %#v from printing the key.
func (c *Cloud) GoString() string { return c.String() }

// Me reports the org, the key in use and, for a member's key, the member.
func (c *Cloud) Me(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/me", nil, &out)
}

// Usage reports this month's usage, bill estimate and whether work is paused
// for payment.
func (c *Cloud) Usage(ctx context.Context) (Info, error) {
	var out Info
	return out, c.request(ctx, "GET", "/v1/usage", nil, &out)
}

// Templates lists the templates the org can create sandboxes from now.
func (c *Cloud) Templates(ctx context.Context) ([]CloudTemplate, error) {
	var r struct {
		Templates []CloudTemplate `json:"templates"`
	}
	if err := c.request(ctx, "GET", "/v1/templates", nil, &r); err != nil {
		return nil, err
	}
	return r.Templates, nil
}

// CloudTemplate is a template a cloud sandbox can start from.
type CloudTemplate struct {
	Key     string   `json:"key"`
	CPUs    float64  `json:"cpus"`
	MemMB   int      `json:"mem_mb"`
	Image   string   `json:"image"`
	Regions []string `json:"regions"`
	Hosts   int      `json:"hosts"`
	Warm    int      `json:"warm"`
	OwnHost bool     `json:"own_host"`
}

// request sends a JSON body (or none) and decodes a JSON answer into out.
func (c *Cloud) request(ctx context.Context, method, path string, body, out any) error {
	return c.requestKeyed(ctx, method, path, body, out, "")
}

// requestKeyed is request with an Idempotency-Key: key, sent unchanged on
// every retry, or one of the SDK's own on the calls that make sandboxes and
// jobs (keyedRoute), renewed after a 5xx (see send).
func (c *Cloud) requestKeyed(ctx context.Context, method, path string, body, out any, key string) error {
	var payload []byte
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return usage("encode request: %v", err)
		}
		payload = b
		ct = "application/json"
	}
	b, err := c.send(ctx, method, path, ct, payload, "application/json", key)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &Error{Kind: KindAPI, Message: fmt.Sprintf("decode %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	return nil
}

// keyedRE: the POSTs kosa8 Cloud keeps an Idempotency-Key for (24 hours).
var keyedRE = regexp.MustCompile(`^/v1/sandboxes(/batch|/[A-Za-z0-9_]{1,64}/(fork|grade))?$|^/v1/jobs$`)

// idempotentPosts are POSTs that are safe to send twice.
var idempotentPosts = map[string]bool{"/v1/sandboxes/heartbeat": true, "/v1/sandboxes/drain": true}

func barePath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

func keyedRoute(method, path string) bool {
	return method == "POST" && keyedRE.MatchString(barePath(path))
}

var idemKeyRE = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)

func checkIdempotencyKey(k string) error {
	if k != "" && !idemKeyRE.MatchString(k) {
		return usage("IdempotencyKey must be 1-255 printable ASCII characters, e.g. a UUID")
	}
	return nil
}

// NewIdempotencyKey is a random UUID (version 4).
func NewIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("k8-%d", time.Now().UnixNano())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// send is one call, retried as c.retry allows for this method and path.
//
// A key the caller gave goes unchanged with every retry. The SDK's own key
// (on a keyedRoute) is kept after no answer (the first try may still be
// running and may have made the sandbox), a 429 or a 409, and replaced
// after a 5xx (renewsIdempotencyKey), so each retry of a server-side
// failure is a new attempt.
func (c *Cloud) send(ctx context.Context, method, path, contentType string, body []byte, accept, key string) ([]byte, error) {
	if err := checkIdempotencyKey(key); err != nil {
		return nil, err
	}
	ownKey := key == "" && keyedRoute(method, path)
	if ownKey {
		key = NewIdempotencyKey()
	}
	safe := key != "" || method == "GET" || method == "HEAD" || method == "PUT" || method == "DELETE" || idempotentPosts[barePath(path)]
	for attempt := 1; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		b, err := c.once(ctx, method, path, contentType, rd, accept, key)
		if err == nil {
			return b, nil
		}
		if !(safe || preExecution(err)) || !c.retry.ShouldRetry(err, attempt) {
			return nil, err
		}
		if sleepCtx(ctx, c.retry.Delay(err, attempt)) != nil {
			return nil, err
		}
		if ownKey && renewsIdempotencyKey(err) {
			key = NewIdempotencyKey()
		}
	}
}

// renewsIdempotencyKey reports whether a retry after err gets a new SDK-made
// Idempotency-Key: only when kosa8 Cloud answered with a 5xx (502
// host_failed, 503 no_capacity, ...). No answer, a 429 and a 409 keep it.
func renewsIdempotencyKey(err error) bool {
	e, ok := AsError(err)
	return ok && e.cloud && e.Status >= 500
}

// preExecution reports whether err is an answer the cloud gives before it
// acts on a request, so sending even a non-idempotent POST again is safe:
// 429 (rate limited), 503 database_busy (its database was overloaded) and
// 409 template_building (the template is still being built).
func preExecution(err error) bool {
	e, ok := AsError(err)
	if !ok || !e.cloud {
		return false
	}
	return e.Status == 429 || (e.Status == 503 && e.Code == codeDatabaseBusy) || (e.Status == 409 && e.Code == codeTemplateBuilding)
}

// do sends one request and returns the answer's body; an HTTP error answer
// becomes a typed *Error.
func (c *Cloud) do(ctx context.Context, method, path, contentType string, body io.Reader, accept string) ([]byte, error) {
	var payload []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, usage("read request body: %v", err)
		}
		payload = b
	}
	return c.send(ctx, method, path, contentType, payload, accept, "")
}

// once sends one request, without retries.
func (c *Cloud) once(ctx context.Context, method, path, contentType string, body io.Reader, accept, key string) ([]byte, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, usage("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", UserAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if parent.Err() != nil {
				// The caller's own deadline, not CloudOptions.Timeout.
				return nil, &Error{Kind: KindTimeout, Message: fmt.Sprintf("%s %s: the context's deadline passed before kosa8 Cloud answered", method, path), Method: method, Path: path, Err: err}
			}
			return nil, &Error{Kind: KindTimeout, Message: fmt.Sprintf("kosa8 Cloud did not answer %s %s within %s; try again, or raise CloudOptions.Timeout", method, path, c.timeout), Method: method, Path: path, Err: err}
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, c.unreachable(method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Kind: KindConnection, Message: fmt.Sprintf("read %s %s: %v", method, path, err), Method: method, Path: path, Err: err}
	}
	if resp.StatusCode >= 400 {
		return nil, withHeaders(cloudError(resp.StatusCode, decodeBody(b), method, path), resp.Header)
	}
	return b, nil
}

// unreachable is the error for a request that got no answer.
func (c *Cloud) unreachable(method, path string, err error) *Error {
	return &Error{Kind: KindConnection, Method: method, Path: path, Err: err,
		Message: fmt.Sprintf("could not reach kosa8 Cloud at %s: %v; check the network connection and KOSA8_CLOUD_URL, then try again", c.baseURL, err)}
}

// withHeaders adds an error answer's Retry-After (over the body's
// retry_after_s) and request id to e.
func withHeaders(e *Error, h http.Header) *Error {
	if ra := parseRetryAfter(h.Get("Retry-After"), time.Now()); ra > 0 {
		e.RetryAfter = ra
	}
	e.RequestID = h.Get("X-Request-Id")
	if e.RequestID == "" {
		e.RequestID = h.Get("Cf-Ray")
	}
	return e
}

// parseRetryAfter reads a Retry-After header: seconds, or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

// execTimeoutHint is added to the cloud's own words when an exec is cut off.
const execTimeoutHint = "kosa8 Cloud stops a buffered exec after 90 s; run long jobs in the background " +
	"(nohup cmd > /workspace/job.log 2>&1 &) and poll /workspace for the result"

// cloudError reads the cloud's {error: code, message, upgrade_url?,
// agent_message?}: the sentence becomes the message and the code is kept.
func cloudError(status int, body any, method, path string) *Error {
	msgBody := body
	code, upgrade, agent := "", "", ""
	if m, ok := body.(map[string]any); ok {
		code, _ = m["error"].(string)
		msg, _ := m["message"].(string)
		if msg == "" {
			msg = code
		}
		upgrade, _ = m["upgrade_url"].(string)
		agent, _ = m["agent_message"].(string)
		msgBody = map[string]any{"error": msg}
	}
	e := errorFromResponse(status, msgBody, method, path)
	e.Code = code
	e.Body = body
	e.UpgradeURL = upgrade
	e.AgentMessage = agent
	e.cloud = true
	if m, ok := body.(map[string]any); ok {
		e.RequestURL, _ = m["request_url"].(string)
		if status == 402 || status == 429 {
			e.Quota = quotaOf(m)
		}
	}
	e.FailureClass, e.Retryable = FailureOf(status, code)
	if m, ok := body.(map[string]any); ok {
		if fc, ok := m["failure_class"].(string); ok && fc != "" {
			e.FailureClass = FailureClass(fc)
			if rt, ok := m["retryable"].(bool); ok {
				e.Retryable = rt
			} else {
				e.Retryable = e.FailureClass == FailureInfra
			}
		}
		// The server's word wins: retryable: true (with or without a
		// failure_class) means the same call may work if sent again.
		if rt, ok := m["retryable"].(bool); ok && rt {
			e.Retryable = true
		}
		// e.g. 409 idempotency_in_progress {retry_after_s: 2}; 503
		// database_busy {retry_after: 2}. Seconds; the header wins.
		for _, k := range []string{"retry_after_s", "retry_after"} {
			if ra, ok := m[k].(float64); ok && ra > 0 {
				e.RetryAfter = time.Duration(ra * float64(time.Second))
				break
			}
		}
	}
	if code == codeIdempotencyInProgress || code == codeDatabaseBusy || code == codeTemplateBuilding {
		e.Retryable = true
	}
	switch {
	case status == 423 || code == "org_frozen":
		e.Kind = KindOrgFrozen
	case isExecTimeout(status, code, path, e.Message):
		e.Kind = KindExecTimeout
		// Its own TimeoutS needs no hint about the 90 s buffered limit.
		if !strings.Contains(e.Message, "timeout_s") {
			e.Message += " (" + execTimeoutHint + ")"
		}
	}
	return e
}

func isExecTimeout(status int, code, path, msg string) bool {
	if status == 504 || code == "exec_timeout" {
		return true
	}
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return status == 502 && code == "host_failed" && strings.HasSuffix(p, "/exec") && strings.Contains(strings.ToLower(msg), "timed out")
}

// CloudSandboxInfo is a cloud sandbox as the control plane reports it (the
// OpenAPI Sandbox schema). Nullable fields are pointers.
type CloudSandboxInfo struct {
	ID string `json:"id"`
	// State: creating | running | paused | sleeping | stopped | expired |
	// error | deleted | host_removed | host_lost. expired: past its
	// DeleteAfter; it takes no more work and is deleted within a minute.
	State    string `json:"state"`
	Template string `json:"template"`
	// OriginItemVersion is the kosa8 Store version it was started (or
	// forked) from.
	OriginItemVersion *string `json:"origin_item_version"`
	CPUs              float64 `json:"cpus"`
	MemMB             int     `json:"mem_mb"`
	Source            *string `json:"source"`    // pool | restore | cold | fork | snapshot
	ParentID          *string `json:"parent_id"` // the sandbox it was forked from
	Region            *string `json:"region"`
	OwnHost           bool    `json:"own_host"`
	CreatedAt         int64   `json:"created_at"` // Unix ms
	StartedAt         *int64  `json:"started_at"`
	StoppedAt         *int64  `json:"stopped_at"`
	DeletedAt         *int64  `json:"deleted_at"`
	DeleteAfter       *int64  `json:"delete_after"`  // Unix ms: deleted automatically then
	SleepAfterS       *int    `json:"sleep_after_s"` // idle seconds before it sleeps; 0 never
	LastError         *string `json:"last_error"`
	HostUnreachable   bool    `json:"host_unreachable"`
	RunningS          int64   `json:"running_s"`
	AsleepS           int64   `json:"asleep_s"`
	SleepBytes        int64   `json:"sleep_bytes"`
	CostUSD           float64 `json:"cost_usd"` // estimate so far
	// SnapshotID is the durable snapshot it was made from, if any.
	SnapshotID *string `json:"snapshot_id"`
	Persistent bool    `json:"persistent"` // one long-lived container for every exec
	Name       *string `json:"name"`
	Image      *string `json:"image"`   // the OCI image of a sized create
	DiskGB     *int    `json:"disk_gb"` // the data disk
	CPU        string  `json:"cpu"`     // shared | dedicated
	// LeaseS is a lease's window in seconds (a heartbeat moves DeleteAfter
	// to now + LeaseS); nil when the sandbox is not leased.
	LeaseS *int `json:"lease_s"`
	// Labels are the caller's key=value labels ({} when none).
	Labels map[string]string `json:"labels"`
}

// Limits the Cloud API checks; the SDK checks them first.
const (
	CloudMaxSleepAfterS = 604800  // 7 days
	CloudMinTTLSeconds  = 60      // 1 minute
	CloudMaxTTLSeconds  = 7776000 // 90 days
	CloudMaxForks       = 100     // copies a plain Fork made before 0.5.0; Fork and ForkWith now take CloudMaxBatch
	CloudMaxLabelLen    = 200
	CloudMinDiskGB      = 8
	CloudMaxDiskGB      = 500 // the largest plan's sandbox disk (Scale)
)

// CloudCreateOptions are the settings for a new cloud sandbox; zero values
// take the org's defaults and are not sent.
type CloudCreateOptions struct {
	// Template is a key from Cloud.Templates (base, python, node, ...) or a
	// kosa8 Store item: creator/name or creator/name@1.2.3.
	Template string
	Region   string // such as us-ash1
	// SleepAfterS is idle seconds before the sandbox sleeps (0..604800);
	// Ptr(0) never.
	SleepAfterS *int
	// TTLSeconds deletes the sandbox this many seconds from now
	// (60..7776000). 0 means no lifetime.
	TTLSeconds int
	// DeleteAfter deletes the sandbox at this Unix time in milliseconds. 0
	// means none. Set TTLSeconds or DeleteAfter, not both.
	DeleteAfter int64
	// AcceptPrice is a paid Store item's current price in USD, which the
	// person agreed to. Without it a paid item answers 402
	// price_not_accepted; a different price answers 409 price_changed.
	AcceptPrice *float64

	// Size is vCPU x GiB: one of CloudSizes. The standard sizes are "2x4",
	// "4x8", "8x16" and "16x32"; the small sizes "0.25x0.5", "0.5x1" and
	// "1x2" (a fraction of a vCPU reserved, bursting to a whole one while
	// the host is idle; for fleets of mostly idle sandboxes) run only where
	// a host offers them, else the create answers 400 invalid_size naming
	// what you can use. A size, CPUs+MemMB, Image or DiskGB boots the
	// sandbox at that size; Create waits for it unless NoWait.
	Size string
	// CPUs and MemMB: or name a size by its parts (4 and 8192 for 4x8;
	// 0.25 and 512 for 0.25x0.5).
	CPUs  float64
	MemMB int
	// Image is an OCI image to run, such as
	// kosa8-template/universal-24.04-20261115.2 (default size 2x4).
	Image string
	// DiskGB is the data disk holding / and /workspace (8..500; default
	// 32). Past 200 only on a plan whose sandbox disk is bigger (Team 256,
	// Scale 500): GET /v1/templates max_disk_gb is yours, and the API
	// refuses more.
	DiskGB int
	// Persistent runs every exec in one long-lived container, so packages
	// and background processes outside /workspace persist.
	Persistent bool
	// Docker lets the sandbox run its own Docker engine (start dockerd
	// first); it needs a size or image and makes the sandbox persistent.
	Docker bool
	// KVM (preview) gives the sandbox's container /dev/kvm, so an emulator
	// runs at hardware speed. nil takes the default: true for the
	// ghcr.io/azmxai/kosa8-android image, false otherwise. Needs a size or
	// image, implies Persistent and Docker, and the sandbox never sleeps;
	// 501 not_available while no host offers nested KVM.
	KVM *bool
	// CPU is "shared" (default) or "dedicated": whole physical cores, never
	// sleeps, cannot be checkpointed or forked.
	CPU string
	// Name is your label for it (1..64 of letters, digits, '.', '_', '-');
	// CloudListOptions.Name finds it.
	Name string
	// Labels are up to 16 key=value labels.
	Labels map[string]string
	// Env is given to every command the sandbox runs (an exec's own env
	// wins per name). Values are never logged or returned by the API.
	Env map[string]string
	// Snapshot makes the sandbox from a durable snapshot (snap_...): no
	// Template, Size, CPUs, MemMB, Image, DiskGB, Docker, Persistent, CPU or
	// Region with it.
	Snapshot string
	// Secrets are names from Cloud.Secrets whose values the sandbox's
	// egress broker adds on the way out; the sandbox holds placeholders.
	// Needs a sized create; such a sandbox never sleeps.
	Secrets []string
	// Egress is the sandbox's own egress policy (default the org's).
	Egress *CloudEgress
	// NoWait returns as soon as the API answers, even while a sized sandbox
	// is still booting (State "creating"); call Refresh or WaitRunning.
	NoWait bool
	// LeaseS leases the sandbox (60..86400 s): it is deleted LeaseS from now
	// unless a heartbeat (CloudSandboxes.Heartbeat) moves that forward, so a
	// crashed client leaks nothing. Not with TTLSeconds or DeleteAfter.
	LeaseS int
	// IdempotencyKey is sent as the Idempotency-Key header, unchanged on
	// every retry. The default (a new UUID) is reused after a timeout, a
	// 429 or a 409, so a retried create makes one sandbox, not two, and
	// renewed after a 5xx, so that retry is a new attempt. For 24 hours the same key and
	// body get the first answer again; another body is 422
	// idempotency_mismatch.
	IdempotencyKey string
}

// CloudSizes are the sizes a sized create takes (vCPU x GiB).
var CloudSizes = []string{"0.25x0.5", "0.5x1", "1x2", "2x4", "4x8", "8x16", "16x32"}

// CloudSmallSizes are the sizes below the standard 2x4: a fraction of a
// vCPU reserved (billed as that fraction), bursting while the host is idle.
// Only a sandbox create takes them, and only where a host offers them.
var CloudSmallSizes = []string{"0.25x0.5", "0.5x1", "1x2"}

// CloudStandardSizes are 2x4 and up: what templates, pipelines and every
// other sized path take.
var CloudStandardSizes = []string{"2x4", "4x8", "8x16", "16x32"}

func checkSleepAfter(v *int) error {
	if v != nil && (*v < 0 || *v > CloudMaxSleepAfterS) {
		return usage("SleepAfterS must be 0..%d seconds, not %d", CloudMaxSleepAfterS, *v)
	}
	return nil
}

func lifetimeBody(body map[string]any, ttl int, deleteAfter int64) error {
	if ttl != 0 && deleteAfter != 0 {
		return usage("set TTLSeconds or DeleteAfter, not both")
	}
	if ttl != 0 {
		if ttl < CloudMinTTLSeconds || ttl > CloudMaxTTLSeconds {
			return usage("TTLSeconds must be %d..%d seconds, not %d", CloudMinTTLSeconds, CloudMaxTTLSeconds, ttl)
		}
		body["ttl_s"] = ttl
	}
	if deleteAfter < 0 {
		return usage("DeleteAfter must be a Unix time in milliseconds, not %d", deleteAfter)
	}
	if deleteAfter != 0 {
		body["delete_after"] = deleteAfter
	}
	return nil
}

// CloudSandboxes creates and finds cloud sandboxes.
type CloudSandboxes struct{ c *Cloud }

// execClass reads an exec answer's failure_class and retryable.
type execClass struct {
	FC *string `json:"failure_class"`
	RT *bool   `json:"retryable"`
}

func (x execClass) class(exit *int) (FailureClass, bool) {
	fc, rt := execFailure(exit)
	if x.FC != nil && *x.FC != "" {
		fc = FailureClass(*x.FC)
		rt = fc == FailureInfra
	}
	if x.RT != nil {
		rt = *x.RT
	}
	return fc, rt
}

type sandboxEnvelope struct {
	Sandbox CloudSandboxInfo `json:"sandbox"`
}

// Create starts a sandbox; it returns once the sandbox is running. Bad
// TTLSeconds, DeleteAfter or SleepAfterS are refused before any request.
func (s *CloudSandboxes) Create(ctx context.Context, o CloudCreateOptions) (*CloudSandbox, error) {
	body := map[string]any{}
	if o.Template != "" {
		body["template"] = o.Template
	}
	if o.Region != "" {
		body["region"] = o.Region
	}
	if err := checkSleepAfter(o.SleepAfterS); err != nil {
		return nil, err
	}
	if o.SleepAfterS != nil {
		body["sleep_after_s"] = *o.SleepAfterS
	}
	if err := lifetimeBody(body, o.TTLSeconds, o.DeleteAfter); err != nil {
		return nil, err
	}
	if o.AcceptPrice != nil {
		body["accept_price"] = *o.AcceptPrice
	}
	if err := sizedBody(body, o); err != nil {
		return nil, err
	}
	if err := leaseBody(body, o.LeaseS, o.TTLSeconds, o.DeleteAfter); err != nil {
		return nil, err
	}
	var r sandboxEnvelope
	if err := s.c.requestKeyed(ctx, "POST", "/v1/sandboxes", body, &r, o.IdempotencyKey); err != nil {
		return nil, err
	}
	b := &CloudSandbox{c: s.c, Info: r.Sandbox}
	if !o.NoWait && b.Info.State == "creating" {
		if err := b.WaitRunning(ctx, 0); err != nil {
			return b, err
		}
	}
	return b, nil
}

// List returns the org's sandboxes; with all, deleted ones too. It returns
// the first page only once the Cloud API pages its answer; ListAll follows
// every page.
func (s *CloudSandboxes) List(ctx context.Context, all bool) ([]*CloudSandbox, error) {
	p, err := s.ListPage(ctx, CloudListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return p.Sandboxes, nil
}

// CloudListOptions select a page of sandboxes.
type CloudListOptions struct {
	All    bool   // include deleted sandboxes
	Limit  int    // page size; 0 the server's default
	Cursor string // a page's NextCursor; "" the first page
	Name   string // only the sandbox with this name
	// Labels keep sandboxes with every one of these labels; a "" value
	// matches the key with any value.
	Labels map[string]string
}

// CloudSandboxPage is one page of sandboxes.
type CloudSandboxPage struct {
	Sandboxes []*CloudSandbox
	// NextCursor asks for the next page; "" when this is the last. Servers
	// that do not page their answer yet never send one.
	NextCursor string
}

// ListPage returns one page of the org's sandboxes.
func (s *CloudSandboxes) ListPage(ctx context.Context, o CloudListOptions) (CloudSandboxPage, error) {
	if o.Limit < 0 {
		return CloudSandboxPage{}, usage("Limit must not be negative")
	}
	q := url.Values{}
	if o.All {
		q.Set("all", "1")
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Name != "" {
		q.Set("name", o.Name)
	}
	for _, k := range sortedKeys(o.Labels) {
		if v := o.Labels[k]; v != "" {
			q.Add("label", k+"="+v)
		} else {
			q.Add("label", k)
		}
	}
	p := "/v1/sandboxes"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var r struct {
		Sandboxes  []CloudSandboxInfo `json:"sandboxes"`
		NextCursor *string            `json:"next_cursor"`
	}
	if err := s.c.request(ctx, "GET", p, nil, &r); err != nil {
		return CloudSandboxPage{}, err
	}
	out := CloudSandboxPage{Sandboxes: make([]*CloudSandbox, 0, len(r.Sandboxes))}
	for _, i := range r.Sandboxes {
		out.Sandboxes = append(out.Sandboxes, &CloudSandbox{c: s.c, Info: i})
	}
	if r.NextCursor != nil {
		out.NextCursor = *r.NextCursor
	}
	return out, nil
}

// ListAll follows NextCursor from o.Cursor until the last page and returns
// every sandbox. It stops if the server hands back a cursor it already gave.
func (s *CloudSandboxes) ListAll(ctx context.Context, o CloudListOptions) ([]*CloudSandbox, error) {
	var out []*CloudSandbox
	seen := map[string]bool{}
	for {
		p, err := s.ListPage(ctx, o)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Sandboxes...)
		if p.NextCursor == "" || seen[p.NextCursor] || p.NextCursor == o.Cursor {
			return out, nil
		}
		seen[p.NextCursor] = true
		o.Cursor = p.NextCursor
	}
}

// Get returns one sandbox by id.
func (s *CloudSandboxes) Get(ctx context.Context, id string) (*CloudSandbox, error) {
	var r sandboxEnvelope
	if err := s.c.request(ctx, "GET", "/v1/sandboxes/"+url.PathEscape(id), nil, &r); err != nil {
		return nil, err
	}
	return &CloudSandbox{c: s.c, Info: r.Sandbox}, nil
}

// CloudSandbox is one cloud sandbox. Info is its state as of the last call.
type CloudSandbox struct {
	Info CloudSandboxInfo
	c    *Cloud
}

// ID is the sandbox id. It is a method, unlike box.id in the other SDKs:
// write box.ID(), not box.ID (fmt.Println(box.ID) prints a func value, which
// go vet reports as "func value, not called"). Info.ID holds the same id.
func (b *CloudSandbox) ID() string { return b.Info.ID }

// State is the state as of the last call.
func (b *CloudSandbox) State() string { return b.Info.State }

func (b *CloudSandbox) String() string {
	return fmt.Sprintf("kosa8.CloudSandbox(%s, %s)", b.Info.ID, b.Info.State)
}

func (b *CloudSandbox) path(suffix string) string {
	return "/v1/sandboxes/" + url.PathEscape(b.Info.ID) + suffix
}

// OverflowRegion is the region of a sandbox placed on kosa8's overflow
// capacity (Cloudflare Containers).
const OverflowRegion = "cf-auto"

// Overflow reports whether the sandbox runs on overflow capacity (region
// cf-auto). Such a sandbox has no fork, checkpoints, rewind, snapshots, file
// links or sleep (idle, it is ended): those calls answer 501 not_available
// (ErrUnsupported) with the server's reason, which the SDK passes on as is.
func (b *CloudSandbox) Overflow() bool {
	return b.Info.Region != nil && *b.Info.Region == OverflowRegion
}

// eid is the sandbox's id, escaped for a path.
func (b *CloudSandbox) eid() string { return url.PathEscape(b.Info.ID) }

// CloudExecOptions are optional settings for CloudSandbox.Exec.
type CloudExecOptions struct {
	Image string            // default the template's own image
	Env   map[string]string // sent as K=V, sorted by key
	// TimeoutS stops the command in the sandbox after this many seconds
	// (the API's timeout_s; 0 means none, so the 90 s buffered limit
	// applies). A stopped command is an ErrExecTimeout. Exec takes 1..90 as
	// a buffered exec and 91..86400 as a streamed one (ExecStream);
	// ExecStream and ExecBackground take 1..86400.
	TimeoutS int
}

// CloudExecMaxTimeoutS is the longest CloudExecOptions.TimeoutS: a buffered
// exec is held to 90 s whatever it says.
const CloudExecMaxTimeoutS = 90

// CloudExecResult is a finished cloud command.
type CloudExecResult struct {
	ExitCode *int // nil when the cloud reported no exit status
	Stdout   string
	Stderr   string
	WokeMs   *int // set when the sandbox was asleep and woke for this command
	// FailureClass is ok (exit 0), exit_nonzero (the command failed) or infra
	// (the host gave no exit code); Retryable says the same exec may work if
	// sent again.
	FailureClass FailureClass
	Retryable    bool
}

// OK reports whether the command exited 0.
func (r CloudExecResult) OK() bool { return r.ExitCode != nil && *r.ExitCode == 0 }

// Exec runs argv and waits for it. A sleeping sandbox wakes for it. A
// TimeoutS over 90 runs it as a streamed exec (ExecStream), which has no
// 90 s limit.
//
// Only /workspace is sure to persist between calls: an exec may run in a
// fresh container, unless the sandbox was created Persistent. The Cloud API stops a buffered exec after 90 s (that is
// ErrExecTimeout): run long jobs in the background (nohup ... & writing to
// /workspace) and poll. The request body is capped at 64 KiB (413
// too_large). ExitCode is nil when the cloud reports no exit status, so
// check it (or use OK) before dereferencing.
func (b *CloudSandbox) Exec(ctx context.Context, argv []string, o *CloudExecOptions) (CloudExecResult, error) {
	if len(argv) == 0 {
		return CloudExecResult{}, usage("Exec needs a command")
	}
	body := map[string]any{"cmd": argv}
	if o != nil {
		if o.Image != "" {
			body["image"] = o.Image
		}
		if len(o.Env) > 0 {
			body["env"] = envList(o.Env)
		}
		if o.TimeoutS > CloudExecMaxTimeoutS && o.TimeoutS <= CloudMaxTimeoutS {
			// Past the buffered limit: stream it, which is not held to 90 s.
			return b.ExecStream(ctx, argv, o, nil)
		}
		if o.TimeoutS != 0 {
			if o.TimeoutS < 1 || o.TimeoutS > CloudExecMaxTimeoutS {
				return CloudExecResult{}, usage("TimeoutS must be 1..%d, not %d", CloudMaxTimeoutS, o.TimeoutS)
			}
			body["timeout_s"] = o.TimeoutS
		}
	}
	var r struct {
		Exit   *int   `json:"exit"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		WokeMs *int   `json:"woke_ms"`
		execClass
	}
	if err := b.c.request(ctx, "POST", b.path("/exec"), body, &r); err != nil {
		return CloudExecResult{}, err
	}
	fc, rt := r.class(r.Exit)
	return CloudExecResult{ExitCode: r.Exit, Stdout: r.Stdout, Stderr: r.Stderr, WokeMs: r.WokeMs, FailureClass: fc, Retryable: rt}, nil
}

// ExecShell runs a command line through sh -c (the string form of the
// Python and TypeScript exec).
func (b *CloudSandbox) ExecShell(ctx context.Context, cmd string, o *CloudExecOptions) (CloudExecResult, error) {
	return b.Exec(ctx, []string{"sh", "-c", cmd}, o)
}

func (b *CloudSandbox) update(ctx context.Context, method, suffix string) error {
	var r sandboxEnvelope
	if err := b.c.request(ctx, method, b.path(suffix), nil, &r); err != nil {
		return err
	}
	b.Info = r.Sandbox
	return nil
}

// Sleep snapshots the sandbox and frees its VM; it then costs only snapshot
// storage.
func (b *CloudSandbox) Sleep(ctx context.Context) error { return b.update(ctx, "POST", "/sleep") }

// Wake restores a sleeping sandbox, files and all.
func (b *CloudSandbox) Wake(ctx context.Context) error { return b.update(ctx, "POST", "/wake") }

// Refresh reloads Info.
func (b *CloudSandbox) Refresh(ctx context.Context) error { return b.update(ctx, "GET", "") }

// CloudUpdateOptions change a sandbox's lifetime or sleep timer. Zero values
// are not sent.
type CloudUpdateOptions struct {
	// TTLSeconds deletes the sandbox this many seconds from now (60..7776000).
	TTLSeconds int
	// DeleteAfter deletes the sandbox at this Unix time in milliseconds.
	DeleteAfter int64
	// ClearLifetime removes any lifetime, so the sandbox is never deleted
	// automatically (sends "ttl_s": null). Not with TTLSeconds or DeleteAfter.
	ClearLifetime bool
	// SleepAfterS is idle seconds before it sleeps (0..604800); Ptr(0) never.
	SleepAfterS *int
}

// Update changes the sandbox's lifetime or sleep timer (PATCH) and refreshes
// Info. It returns the server's notes, if any.
func (b *CloudSandbox) Update(ctx context.Context, o CloudUpdateOptions) ([]string, error) {
	body := map[string]any{}
	if o.ClearLifetime && (o.TTLSeconds != 0 || o.DeleteAfter != 0) {
		return nil, usage("ClearLifetime cannot be combined with TTLSeconds or DeleteAfter")
	}
	if err := lifetimeBody(body, o.TTLSeconds, o.DeleteAfter); err != nil {
		return nil, err
	}
	if o.ClearLifetime {
		body["ttl_s"] = nil
	}
	if err := checkSleepAfter(o.SleepAfterS); err != nil {
		return nil, err
	}
	if o.SleepAfterS != nil {
		body["sleep_after_s"] = *o.SleepAfterS
	}
	if len(body) == 0 {
		return nil, usage("Update needs TTLSeconds, DeleteAfter, ClearLifetime or SleepAfterS")
	}
	var r struct {
		Sandbox CloudSandboxInfo `json:"sandbox"`
		Notes   []string         `json:"notes"`
	}
	if err := b.c.request(ctx, "PATCH", b.path(""), body, &r); err != nil {
		return nil, err
	}
	b.Info = r.Sandbox
	return r.Notes, nil
}

// Delete deletes the sandbox.
func (b *CloudSandbox) Delete(ctx context.Context) error {
	return b.c.request(ctx, "DELETE", b.path(""), nil, nil)
}

// CloudFunctionSpec is a function to deploy. Extra fields go in Extra.
type CloudFunctionSpec struct {
	Runtime  string            `json:"runtime"` // python3.12 | node22
	Code     string            `json:"code"`
	Env      map[string]string `json:"env,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
	Web      string            `json:"web,omitempty"`      // off | public | key
	Schedule string            `json:"schedule,omitempty"` // cron
	Extra    map[string]any    `json:"-"`
}

func (s CloudFunctionSpec) body() map[string]any {
	m := map[string]any{}
	for k, v := range s.Extra {
		m[k] = v
	}
	m["runtime"] = s.Runtime
	m["code"] = s.Code
	if len(s.Env) > 0 {
		m["env"] = s.Env
	}
	if s.TimeoutS != 0 {
		m["timeout_s"] = s.TimeoutS
	}
	if s.Web != "" {
		m["web"] = s.Web
	}
	if s.Schedule != "" {
		m["schedule"] = s.Schedule
	}
	return m
}

// CloudInvocation identifies one function call.
type CloudInvocation struct {
	ID         string `json:"id"`
	Function   string `json:"function"`
	FunctionID string `json:"function_id"`
	Version    int    `json:"version"`
	Trigger    string `json:"trigger"` // api | web | batch | cron
}

// CloudFunctionError is what a handler raised.
type CloudFunctionError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// CloudTimings are an invocation's timings in milliseconds.
type CloudTimings struct {
	Start     *float64 `json:"start"`
	Run       *float64 `json:"run"`
	HostTotal *float64 `json:"host_total"`
	Total     float64  `json:"total"`
}

// CloudInvokeResult is one function call. A handler that raised is OK ==
// false with Error set, not a Go error.
type CloudInvokeResult struct {
	Invocation    *CloudInvocation    `json:"invocation"`
	OK            bool                `json:"ok"`
	Result        json.RawMessage     `json:"result,omitempty"`
	Error         *CloudFunctionError `json:"error,omitempty"`
	Logs          string              `json:"logs,omitempty"`
	LogsTruncated bool                `json:"logs_truncated,omitempty"`
	Source        *string             `json:"source,omitempty"` // pool | restore
	TimingsMs     *CloudTimings       `json:"timings_ms,omitempty"`
}

// DecodeResult unmarshals Result into v.
func (r CloudInvokeResult) DecodeResult(v any) error {
	if len(r.Result) == 0 {
		return usage("the invocation has no result")
	}
	return json.Unmarshal(r.Result, v)
}

// CloudMapResult is a Map batch; Results are in input order.
type CloudMapResult struct {
	Function  string              `json:"function"`
	Version   int                 `json:"version"`
	Count     int                 `json:"count"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	TotalMs   float64             `json:"total_ms"`
	Results   []CloudInvokeResult `json:"results"`
}

// CloudFunctions deploys and calls cloud functions.
type CloudFunctions struct{ c *Cloud }

func fnPath(name, suffix string) string { return "/v1/functions/" + url.PathEscape(name) + suffix }

// Deploy creates or replaces a function; every deploy bumps its version. It
// returns the control plane's answer ({"function": {...}}).
func (f *CloudFunctions) Deploy(ctx context.Context, name string, spec CloudFunctionSpec) (Info, error) {
	var out Info
	return out, f.c.request(ctx, "PUT", fnPath(name, ""), spec.body(), &out)
}

// List returns the org's functions.
func (f *CloudFunctions) List(ctx context.Context) ([]Info, error) {
	var r struct {
		Functions []Info `json:"functions"`
	}
	if err := f.c.request(ctx, "GET", "/v1/functions", nil, &r); err != nil {
		return nil, err
	}
	return r.Functions, nil
}

// Get returns one function ({"function": {...}}, with its code).
func (f *CloudFunctions) Get(ctx context.Context, name string) (Info, error) {
	var out Info
	return out, f.c.request(ctx, "GET", fnPath(name, ""), nil, &out)
}

// Delete deletes a function.
func (f *CloudFunctions) Delete(ctx context.Context, name string) error {
	return f.c.request(ctx, "DELETE", fnPath(name, ""), nil, nil)
}

// Invoke calls the function once with event (nil sends null).
func (f *CloudFunctions) Invoke(ctx context.Context, name string, event any) (CloudInvokeResult, error) {
	var out CloudInvokeResult
	err := f.c.request(ctx, "POST", fnPath(name, "/invoke"), map[string]any{"event": event}, &out)
	return out, err
}

// CloudMapOptions are optional settings for Map.
type CloudMapOptions struct {
	Concurrency int // 0 takes the cloud's default
}

// Map makes one call per event; results come back in input order.
func (f *CloudFunctions) Map(ctx context.Context, name string, events []any, o *CloudMapOptions) (CloudMapResult, error) {
	if events == nil {
		events = []any{}
	}
	body := map[string]any{"events": events}
	if o != nil && o.Concurrency != 0 {
		body["concurrency"] = o.Concurrency
	}
	var out CloudMapResult
	err := f.c.request(ctx, "POST", fnPath(name, "/map"), body, &out)
	return out, err
}
