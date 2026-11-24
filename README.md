# kosa8-go

**[kosa8](https://kosa8.com): Zero Trust cybersecurity infrastructure for AI agents.**

The Go SDK for [kosa8](https://kosa8.com): each agent in its own microVM, on kosa8 Cloud or on
your own machine. Standard library only.
Method for method it matches [`@kosa8/sdk`](https://github.com/kosa8ai/kosa8-js) (TypeScript) and `kosa8` (Python).

```sh
go get github.com/kosa8ai/kosa8-go@v0.6.1
```

## kosa8 Cloud quickstart

Make an API key (`k8c_live_...`) in the dashboard at app.kosa8.com and set it:

```sh
export KOSA8_API_KEY=k8c_live_...
```

```go
ctx := context.Background()
cloud, err := kosa8.NewCloud(kosa8.CloudOptions{}) // KOSA8_API_KEY from the environment
if err != nil {
	log.Fatal(err)
}

// Deleted automatically in an hour, even if this program dies.
box, err := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "python", TTLSeconds: 3600})
if err != nil {
	log.Fatal(err)
}
defer box.Delete(ctx)

r, err := box.Exec(ctx, []string{"python3", "-c", "print(6 * 7)"}, nil)
if err != nil {
	log.Fatal(err)
}
if r.ExitCode != nil { // nil when the cloud reported no exit status; r.OK() checks for 0
	fmt.Println("exit", *r.ExitCode)
}
fmt.Print(r.Stdout) // 42

// Files (needs the host's data plane, see below).
_, _ = box.Files().WriteString(ctx, "/workspace/hello.txt", "hi\n")
text, _ := box.Files().ReadText(ctx, "/workspace/hello.txt")
entries, _ := box.Files().List(ctx, "") // "" is /workspace
fmt.Print(text, len(entries))
```

The API is at `https://api.kosa8.com`; set `KOSA8_CLOUD_URL` (or `CloudOptions.BaseURL`) to use
another address. These snippets are compiled as examples in `example_test.go`.

### Sandboxes

- `Sandboxes.Create(ctx, CloudCreateOptions{...})`: `Template`, `Region`, `SleepAfterS` (0..604800,
  `kosa8.Ptr(0)` never sleeps), `TTLSeconds` (60..7776000) or `DeleteAfter` (Unix ms; not both),
  `AcceptPrice` (a paid kosa8 Store item's price you agreed to). Bad values are refused before
  any request.
- `CloudSandbox.Update(ctx, CloudUpdateOptions{...})`: change `TTLSeconds`, `DeleteAfter`,
  `SleepAfterS`, or `ClearLifetime: true` to never delete it automatically. Returns the server's notes.
- `Sandboxes.List(ctx, all)`, `ListPage(ctx, CloudListOptions{All, Limit, Cursor})` (one page and its
  `NextCursor`, `""` on the last), `ListAll(ctx, opts)` (every page), `Get(ctx, id)`.
- `CloudSandbox.Exec` (argv) / `ExecShell` (`sh -c`), `Sleep`, `Wake`, `Refresh`, `Delete`.
- `Cloud.Templates(ctx)`, `Cloud.Me(ctx)`, `Cloud.Usage(ctx)`.
- `CloudSandboxInfo` carries the API's Sandbox fields, including `ParentID`, `DeleteAfter`,
  `SleepAfterS`, `HostUnreachable`, `OriginItemVersion`, `Source` (`pool`, `restore`, `cold`, `fork`)
  and `CostUSD`.

### The base template

- The default template, `base`, is Alpine Linux. `sh` is busybox: there is no bash, python or git.
  For Python create with `Template: "python"` (or another key from `cloud.Templates(ctx)`).
- Each exec is a fresh process (it may run in a fresh container), so `cd`, `export` and processes
  it leaves running are gone by the next one. Files in `/workspace` persist for the sandbox's
  life. Packages installed outside `/workspace` (apk, apt, a global pip) need `Persistent: true`
  (one long-lived container: the whole filesystem and background processes last between
  commands); install into `/workspace` otherwise. The code interpreter (`RunCode`) needs a
  persistent sandbox, and persistent sandboxes count against your plan's persistent limit.
- A buffered exec runs at most **90 s** (`ErrExecTimeout`). A **streamed exec** has no such limit
  and prints as it goes: `box.ExecStream(ctx, argv, &kosa8.CloudExecOptions{TimeoutS: 3600},
  func(stream string, data []byte) error { os.Stdout.Write(data); return nil })` (`TimeoutS` up
  to a day); `Exec` with a `TimeoutS` over 90 streams too. Or run it in the background and poll
  (`ExecBackground`, below). `&kosa8.CloudExecOptions{TimeoutS: 30}` stops a command sooner, in
  the sandbox.
- `box.ID()` is a method: `fmt.Println(box.ID)` without the call prints a func value (`go vet`
  reports "func value, not called"). `r.ExitCode` is an `*int`, nil when the cloud sent no exit
  status; check it, or use `r.OK()`.
- A request body is at most 64 KiB (413 `too_large`); a file read or write is at most 8 MiB.

### Data plane: files, checkpoints, rewind, fork, ports

```go
f := box.Files()
data, _ := f.Read(ctx, "/workspace/out.bin")          // []byte
_, _ = f.Write(ctx, "/workspace/in.bin", data)         // raw bytes, up to 8 MiB
_, _ = f.Mkdir(ctx, "/workspace/build")
_, _ = f.Delete(ctx, "/workspace/build", true)         // recursive

cp, _ := box.Checkpoint(ctx, "clean")                  // memory, processes and disk
list, _ := box.Checkpoints(ctx)                        // oldest first
_, _ = box.Rewind(ctx, cp.ID)                          // .Backup is the state just before
kids, _ := box.Fork(ctx, 3)                            // 1..100 copies as it is now
ports, _ := box.Ports(ctx)                             // TCP ports listening inside
fmt.Println(len(list), len(kids), len(ports))
```

These need the host's data plane. On hosts without it the API answers 501 (`not_available` /
`not_supported`), which is `errors.Is(err, kosa8.ErrUnsupported)`. Not every kosa8 Cloud host has
it yet.

### Sized and Docker sandboxes, env, snapshots

```go
box, err := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{
	Size:   "4x8", // 0.25x0.5, 0.5x1, 1x2, 2x4, 4x8, 8x16, 16x32; or CPUs + MemMB
	Image:  "kosa8-template/universal-24.04-20261115.2",
	Docker: true, // run dockerd inside; implies Persistent
	Env:    map[string]string{"NODE_ENV": "production"},
	Name:   "web", Labels: map[string]string{"team": "a"},
	TTLSeconds: 3600,
}) // a sized create boots cold; Create waits until it runs (NoWait: true to skip)

snap, _ := box.Snapshot(ctx, "warm")                       // durable: kept until deleted
clone, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Snapshot: snap.ID})
_ = cloud.Snapshots.Delete(ctx, snap.ID)                   // also List, Get
```

Also `DiskGB`, `Persistent`, `CPU: "dedicated"`, `Region` (`us`, `eu` or a region id), `Secrets`
(names) and `Egress`. `CloudListOptions` takes `Name` and `Labels`.

Small sizes, for fleets of mostly idle sandboxes (RL rollouts, evals): a fraction of a vCPU
reserved and billed, bursting to a whole one while the host is idle. Only a sandbox create takes
them (`kosa8.CloudSmallSizes`), and only where a host offers them; elsewhere the create answers
400 `invalid_size` with the sizes you can use.

```go
tiny, err := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{
	Template: "python", CPUs: 0.25, MemMB: 512, // or Size: "0.25x0.5"
	TTLSeconds: 600,
})
```

A sandbox may run on kosa8's overflow capacity (Cloudflare Containers; `box.Overflow()`, region
`cf-auto`). It has no fork, checkpoints, rewind, snapshots, file links or sleep (idle, it is
ended): those calls return the server's 501 `not_available` with its reason
(`errors.Is(err, kosa8.ErrUnsupported)`, not retried). Background commands work there.

### Background commands, previews, terminal

```go
job, _ := box.ExecBackground(ctx, []string{"sh", "-c", "npm run build"}, nil) // no 90 s limit; TimeoutS up to a day
job, _ = box.WaitBackground(ctx, job.ID, 0)                                   // or BackgroundExec to poll once
fmt.Println(*job.ExitCode, *job.Output)

p, _ := box.Previews().Create(ctx, 3000, nil) // private signed URL on kosa8labs.com; Public: true for none
fmt.Println(p.URL)                            // also List, Link, Rotate, Delete

t, _ := box.Terminal(ctx, &kosa8.CloudTerminalOptions{Cols: 120, Rows: 40}) // a wss:// PTY URL
```

### More of the sandbox

- Files: `Files().Download(ctx, path, w)` / `Upload(ctx, path, r)` stream any size;
  `Files().Link(ctx, path, "GET"|"PUT", ttlS)` is a presigned URL straight to the host.
- `Egress`, `SetEgress(ctx, kosa8.CloudEgress{Presets: []string{"pypi"}})`, `ResetEgress`;
  `Cloud.EgressPresets`.
- `RunCode(ctx, "import pandas", &kosa8.CloudCodeOptions{Context: "a"})` (a stateful Python
  interpreter; needs a persistent sandbox with python3), `CodeContexts`, `CreateCodeContext`,
  `ResetCodeContext`, `DeleteCodeContext`.
- `Schedules().Create(ctx, kosa8.CloudScheduleSpec{Cron: "0 * * * *", Cmd: argv})`, `List`, `Run`,
  `Delete`.
- `Metrics`, `Logs(ctx, limit)`, `SetLabels`, `Secrets` (names it holds); `Cloud.Capabilities`.

### Secrets and Pipelines

```go
// Read the value from a file or stdin, never from argv or code.
_, _, err = cloud.Secrets.Set(ctx, "npm-token", kosa8.CloudSecretSpec{
	Value: value, Upstream: "https://registry.npmjs.org", Pipelines: true,
}) // also List, Delete

run, _ := cloud.Pipelines.Run(ctx, kosa8.CloudPipelineRunOptions{
	RepoURL: "https://github.com/me/app", Ref: "main", Secrets: []string{"npm-token"},
})
run, _ = cloud.Pipelines.Wait(ctx, run.ID, 0) // success | failure | cancelled | ...
logs, _ := cloud.Pipelines.Logs(ctx, run.ID, "", -1)
```

Also `Runs`, `Get`, `Cancel`, `Artifacts`, `DownloadArtifact`, `CreateHook` (webhook URL and
optional cron), `Hooks`, `DeleteHook`.

### Functions

```go
_, _ = cloud.Functions.Deploy(ctx, "hello", kosa8.CloudFunctionSpec{
	Runtime: "python3.12",
	Code:    "def handler(event):\n    return {\"hello\": event.get(\"name\", \"world\")}\n",
})
out, _ := cloud.Functions.Invoke(ctx, "hello", map[string]any{"name": "kosa8"})
fmt.Println(out.OK, string(out.Result)) // true {"hello":"kosa8"}
```

A function that raised is `out.OK == false` with `out.Error`, not a Go error.
Also `Functions.List` / `Get` / `Delete` / `Map`.

### RL training and fan-out

Many sandboxes at once, leased so a crashed trainer leaks none, graded in a sealed copy, and
recorded on an RL run (app.kosa8.com, RL runs).

```go
// Up to 1000 sandboxes in one call; each is deleted 10 minutes after the last heartbeat.
batch, _ := cloud.Sandboxes.CreateMany(ctx, kosa8.CloudBatchOptions{
	CloudCreateOptions: kosa8.CloudCreateOptions{Template: "python", LeaseS: 600},
	Count: 64, RunID: "exp-7", Prefix: "roll",
})
fmt.Println(batch.Created, len(batch.Failed)) // Failed: index, code, FailureClass, Retryable
_, _ = cloud.Sandboxes.Heartbeat(ctx, kosa8.CloudSelector{RunID: "exp-7"}, 0)
_, _ = cloud.Sandboxes.Drain(ctx, kosa8.CloudSelector{RunID: "exp-7"})

// Copies of a set-up sandbox, with labels, a run and a lease.
kids, _ := base.ForkWith(ctx, kosa8.CloudForkOptions{N: 200, RunID: "exp-7", LeaseS: 600})

// Grade a solved sandbox with hidden tests in a sealed copy (no network).
g, _ := box.Grade(ctx, []string{"python", "/grader/test.py"}, &kosa8.CloudGradeOptions{
	Files: map[string][]byte{"/grader/test.py": hidden},
})
fmt.Println(g.Reward, g.FailureClass) // Reward nil when the grader did not finish
```

`ForkMap` runs a function in a fork per item, at most `Concurrency` at once, retrying infra
failures on a fresh fork, and deletes every child however it ends:

```go
res, _ := kosa8.ForkMap(ctx, base, prompts, func(ctx context.Context, child *kosa8.CloudSandbox, p string) (string, error) {
	r, err := child.ExecShell(ctx, "python agent.py "+strconv.Quote(p), nil)
	return r.Stdout, err
}, &kosa8.ForkMapOptions{Concurrency: 32, Timeout: time.Minute})
```

An RL run labels and leases its sandboxes, heartbeats them in the background, records rollouts in
batches of at most 500, and drains on `Finish`:

```go
run, _ := cloud.StartRun(ctx, kosa8.CloudRunOptions{Name: "grpo-qwen", Project: "swe"})
defer run.Finish(ctx, "finished")
results, _ := kosa8.MapRollouts(ctx, run, base, tasks, func(ctx context.Context, sb *kosa8.CloudSandbox, t Task) (any, error) {
	return nil, runAgent(ctx, sb, t) // the grader gives the reward
}, &kosa8.RunMapOptions[Task]{
	GradeCmd:  []string{"python", "/grader/test.py"},
	Files:     func(t Task) map[string][]byte { return t.Hidden },
	GroupSize: 8, // GRPO's G
})
rewards, mask := results.Rewards(), results.Mask() // masked (infra, quota, your own errors): reward nil, never 0
data, _ := run.Export(ctx) // every rollout as JSON lines
```

Every call that makes sandboxes (create, batch, fork, grade) carries an `Idempotency-Key` that its
retries reuse. `CloudOptions.Retry` (default `kosa8.DefaultRetryPolicy()`, 3 tries with full
jitter, at least `Retry-After`) retries only what is safe; `&kosa8.NoRetry` turns it off.
`kosa8.ClassifyFailure(err)` says whose fault any error was.

Org templates are built once and restored in about a second:

```go
tpl, _ := cloud.OrgTemplates.Build(ctx, kosa8.CloudTemplateBuild{Name: "swe-django-1", Image: "ghcr.io/org/task@sha256:..."})
tpl, _ = cloud.OrgTemplates.WaitReady(ctx, tpl.ID, 0)
box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "swe-django-1"})
// also Import (many images at once), List, Get, Delete
```

Capability tokens hand one sandbox to an end user without the API key:

```go
tok, _ := cloud.Tokens.Create(ctx, kosa8.CloudTokenOptions{
	Scopes: []string{kosa8.ScopeSandboxesRead, kosa8.ScopeSandboxesWrite}, SandboxIDs: []string{box.ID()}, TTLS: 3600,
})
userCloud, _ := kosa8.NewCloud(kosa8.CloudOptions{APIKey: tok.Secret}) // k8c_tok_...
// also Tokens.List, Tokens.Revoke
```

### Browsers, AI gateway, hosted jobs, benchmarks

```go
// A headless browser ($0.12 per browser-hour, billed per second), driven over CDP.
br, _ := cloud.Browsers.Create(ctx, kosa8.CloudBrowserOptions{TTLS: 120})
defer br.Close(ctx)
png, _ := br.Screenshot(ctx, "https://example.com", false)
cdp, _ := br.CDPEndpoint(ctx) // wss://api.kosa8.com/v1/browsers/{id}/cdp?token=... (chromedp, Playwright)

// The AI gateway: provider keys kept server-side; point a client at GatewayURL.
_, _ = cloud.AI.SetKey(ctx, "anthropic", key)
base := cloud.AI.GatewayURL("anthropic") // https://api.kosa8.com/v1/ai/gw/anthropic/, API key = a kosa8 key
use, _ := cloud.AI.Usage(ctx, "")        // also Settings, UpdateSettings, Keys, DeleteKey, Call

// A hosted job: kosa8 runs every rollout server-side, graded sealed, into an RL run.
job, _ := cloud.Jobs.Submit(ctx, kosa8.CloudJobOptions{
	Name: "eval", Template: "python", Tasks: tasks,
	Solver: kosa8.CloudJobCmd{Cmd: []string{"python /workspace/solve.py"}, TimeoutS: 1800},
	Grader: kosa8.CloudJobCmd{Cmd: []string{"pytest -q /hidden/test.py"}, Files: hidden},
	Concurrency: 64, BudgetUSD: 20,
})
job, _ = cloud.Jobs.Wait(ctx, job.ID, 0, nil) // also Estimate, Get, List, Cancel

bs, _ := cloud.Benchmarks.List(ctx) // also Get, Instances, Instance, Prepare
```

### More of the org

- `Recordings` (List, Link, Delete; `box.Recordings`), `box.Embeds()` (Create, List, Revoke),
  `box.Previews().Domains(port)` and `cloud.PreviewDomains(previewID)` (custom domains: List, Add,
  Get, Remove).
- `APIKeys` (List, Create, Revoke, Rotate), `Webhooks` (List, Create, Delete, Deliveries, Test),
  `Audit` (List, Verify, Export, ExportEvents).
- `Limits`, `RequestLimit`, `UsageInsights`, `Pricing`, `Burst`, `ActivateBurst`, `OrgEgress`,
  `SetOrgEgress`, `Checkpoints`, `Lineage`, `Images`, `Functions.Invocations`, `CompareRuns`,
  `RLAlerts` (List, Create, Update, Delete), `AttackScorecards`.
- `box.StopBackground(ctx, execID)` stops a background command.

### Retries

Every Cloud request is retried with exponential backoff and full jitter (3 tries by default,
`CloudOptions.Retry`), waiting at least the `Retry-After` header or the body's `retry_after`:

- reads, deletes and puts on infra failures and on any answer with `retryable: true`;
- creates, batches, forks, grades and job submits, which carry an `Idempotency-Key`. The SDK's
  own key is kept after no answer (the first try may still be running and may have made the
  sandbox), a 429 and a 409 `idempotency_in_progress` / `template_building`; after a 5xx (502
  `host_failed`, 503 `no_capacity`, ...) the retry gets a new key, so it is a new attempt. A key
  you pass as `IdempotencyKey` is never changed;
- any request, a POST too, answered 429, 503 `database_busy` or 409 `template_building`: the
  cloud refused it before acting on it.

### Cloud errors

Every error is a `*kosa8.Error`; `kosa8.AsError(err)` gives its `Status`, the cloud's stable `Code`
and its sentence in `Message`. All of these also match `kosa8.ErrAPI`.

| `errors.Is(err, ...)` | When | Read |
|---|---|---|
| `ErrPaymentRequired` | 402: `plan_required`, `plan_limit`, `plan_feature`, `starter_limit`, `spend_cap`, `payment_required`, `price_not_accepted`, ... | `e.UpgradeURL` (where to pay), `e.AgentMessage` (relay word for word); `e.Quota` (a `plan_limit`'s `LimitName`, `Limit`, `Used`, `Plan`, `NextPlan`, `PackURL`, `Options`); empty or nil when absent |
| `ErrOrgFrozen` | 423 `org_frozen`: the org is frozen in the Security Center | an owner or admin must unfreeze it |
| `ErrRateLimit` | 429: rate limited or `quota_exceeded` | `e.RetryAfter` (zero without a `Retry-After` header); for `quota_exceeded`, `e.AgentMessage` and `e.RequestURL` (where to ask for a higher limit) |
| `ErrExecTimeout` | 504, code `exec_timeout`, or 502 `host_failed` on `/exec` saying "timed out" | run the job in the background; also matches `ErrTimeout` |
| `ErrUnsupported` | 501 `not_available` / `not_supported`: no data plane on this host | |
| `ErrNotFound`, `ErrAuth`, `ErrPolicy`, `ErrConflict`, `ErrBadRequest` | 404, 401, 403, 409, 400 | |

Each also has `e.FailureClass` (`infra`, `quota`, `client`, `timeout`) and `e.Retryable`.

A new organization starts on the free Starter plan (no card): past its allowance or limits, or with
no plan at all, creating a sandbox fails with `ErrPaymentRequired` (402 `starter_limit` or
`plan_required`; Starter's running limit is a 429 `starter_limit`), with `e.Quota` and `e.UpgradeURL` set.

## The same thing in each language

The five kosa8 SDKs (Python `kosa8`, JS `@kosa8/sdk`, Go `kosa8-go`, Ruby `kosa8`, Java
`com.kosa8:kosa8`) call the same kosa8 Cloud API, but some names differ by language. The ones
that catch people switching:

| | Python | JS / TS | Go | Ruby | Java |
|---|---|---|---|---|---|
| client | `Kosa8Cloud()` | `new Kosa8Cloud()` | `kosa8.NewCloud(kosa8.CloudOptions{})`, returns `(cloud, err)` | `Kosa8::Cloud.new` | `Kosa8Cloud.builder().build()` |
| lifetime on create | `ttl_s=3600` | `{ ttlS: 3600 }` | `CloudCreateOptions{TTLSeconds: 3600}` | `ttl_s: 3600` | `new CloudOptions.Create().ttlSeconds(3600)` |
| persistent sandbox | `persistent=True` | `{ persistent: true }` | `CloudCreateOptions{Persistent: true}` | `persistent: true` | `.persistent(true)` |
| sandbox id | `box.id` | `box.id` | `box.ID()`, a method (`fmt.Println(box.ID)` prints a func value; `go vet` reports it) | `box.id` | `box.id()` |
| shell command | `box.exec("ls /")` | `box.exec('ls /')` | `box.ExecShell(ctx, "ls /", nil)` | `box.exec("ls /")` | `box.exec("ls /")` |
| argv command | `box.exec(["ls", "/"])` | `box.exec(['ls', '/'])` | `box.Exec(ctx, []string{"ls", "/"}, nil)` | `box.exec(["ls", "/"])` | `box.exec(List.of("ls", "/"))` |
| per-exec timeout | `exec(cmd, timeout_s=30)` | `exec(cmd, { timeoutS: 30 })` | `&kosa8.CloudExecOptions{TimeoutS: 30}` | `exec(cmd, timeout_s: 30)` | `exec(cmd, null, null, 30)` |
| exit code | `r.exit_code` | `r.exitCode` | `r.ExitCode`, an `*int` (nil when the cloud sent none; `r.OK()` checks for 0) | `r.exit_code` | `r.exitCode()`, an `Integer` (may be null) |
| files | `box.files.write` / `read_text` / `list` | `box.files.write` / `readText` / `list` | `box.Files().WriteString` / `ReadText` / `List(ctx, "")` | `box.files.write` / `read_text` / `list` | `box.files().write` / `readText` / `list()` |
| no such sandbox | `Kosa8NotFoundError` | `Kosa8NotFoundError` | `*kosa8.Error` with `Kind` `KindNotFound` (`errors.Is(err, kosa8.ErrNotFound)`) | `Kosa8::NotFoundError` | `Kosa8Exception.NotFoundException` |

In every SDK a non-zero exit is returned in the result, not raised. Each exec is a fresh process;
files in `/workspace` persist for the sandbox's life; installed packages need a persistent sandbox.

## Local daemon

The same API shape drives sandboxes on your own machine through the kosa8 daemon.

```go
k, err := kosa8.New(kosa8.Options{}) // local daemon at ~/.kosa8/run/kosa8.sock
sb := k.Sandbox("demo")
_, err = k.CreateSandbox(ctx, kosa8.CreateOptions{Name: "demo", EgressAllow: []string{"pypi.org"}})
_, err = sb.Pull(ctx, "python:3.12-alpine")
res, err := sb.Run(ctx, "python:3.12-alpine", []string{"python", "-c", "print(6*7)"}, nil)
fmt.Print(res.Stdout()) // 42

cp, _ := sb.Checkpoint(ctx, "clean")      // restore point, shared with `kosa8 rewind`
_, _ = sb.Rewind(ctx, "clean")            // back to it, processes resumed
best, _ := sb.Explore(ctx, []string{"make test", "make test-fast"}, kosa8.ExploreOptions{}) // best-of-N on forks
_, _ = sb.Delete(ctx, kosa8.DeleteOptions{})
```

### Connecting

| Setting | Default |
|---|---|
| `Options.SocketPath` | `~/.kosa8/run/kosa8.sock` |
| `Options.Host` / `KOSA8_HOST` | a remote worker: `https://`, `http://` (loopback only unless `Insecure` / `KOSA8_INSECURE=1`), `tcp://`, `unix://` |
| `Options.Token` / `KOSA8_TOKEN` | sent as `Authorization: Bearer` to a remote worker |
| `Options.WorkerFile` | `~/.kosa8/worker.json` (`%USERPROFILE%\.kosa8\worker.json` on Windows); `NoWorkerFile` skips it |
| `Options.Actor` | `tool` (`human`, `agent`, `tool`), sent as `X-Kosa8-Actor` |
| `Options.AgentToken` / `KOSA8_AGENT_TOKEN` | sent as `X-Kosa8-Agent-Token` |

The client picks its endpoint as the CLI does:

1. `Options.Host`, else a non-blank `KOSA8_HOST` (with `Options.Token`, else `KOSA8_TOKEN`).
2. Else, unless `Options.SocketPath` is set, the connection the CLI saved in the worker file:
   `kosa8 wsl setup` writes it on Windows, where the engine runs in WSL2, and `kosa8 worker join`
   writes it on any OS. Its `host` takes the same spellings as `KOSA8_HOST`; its `token` is sent
   unless `Options.Token` is set; `"insecure": true` (or `Insecure` / `KOSA8_INSECURE=1`) allows
   plain http to a non-loopback host. A missing or malformed file is ignored. A saved token is
   never sent to a host from `Options.Host` or `KOSA8_HOST`.
3. Else the unix socket.

`Client.Endpoint().Target()` shows where requests go; `kosa8.Resolve(opts)` answers without
building a client. On Windows, a failed connection to the default socket says to run
`kosa8 wsl setup` or set `KOSA8_HOST`.

### Errors

Every error is a `*kosa8.Error` with a `Kind`, the daemon's own message and its remediation.
Use `errors.Is(err, kosa8.ErrNotFound)`, `ErrPolicy`, `ErrApprovalRequired` (read `e.Approval`),
`ErrConflict`, `ErrPaymentRequired` (402), `ErrRateLimit` (429), `ErrConnection`, `ErrStream` (output so far in `e.Stdout`/`e.Stderr`), `ErrTimeout`.
A policy that needs a signed approval takes it per call: `kosa8.Call{Approval: sig}`.

### API

Daemon: `Health`, `Version`, `Pull`.
Sandboxes: `ListSandboxes`, `GetSandbox`, `CreateSandbox`, `StartSandbox`, `StopSandbox`, `DeleteSandbox`,
`PullIntoSandbox`, `SandboxImages`, `SandboxContainers`, `SandboxEgress`.
Processes: `Run`, `StartContainer`, `Exec`, `ExecFrames`, `StartInteractive`, `WriteStdin`, `Attach`.
Snapshots: `Snapshot`, `ListSnapshots`, `RestoreSnapshot`, `DeleteSnapshot`, `Fork`, `Explore`.
Checkpoints: `Checkpoint`, `ListCheckpoints`, `Rewind` (store: `~/.kosa8/checkpoints.json`, flock-shared with the CLI).
Toolbox: see below.
`Client.Sandbox(id)` binds all of them to one sandbox.

### Toolbox

```go
sb := k.Sandbox("demo")
code, _ := sb.RunCode(ctx, "print(6*7)", nil)                     // python | javascript | typescript
_, _ = sb.WriteFileBytes(ctx, "app/main.py", []byte("print(1)"), nil) // paths under /workspace
data, _ := sb.ReadFile(ctx, "app/main.py")
_, _ = sb.GitClone(ctx, "https://github.com/owner/repo", "/workspace/repo", nil)
_, _ = sb.GitCommit(ctx, "/workspace/repo", "fix", &kosa8.GitOptions{AuthorName: "bot", AuthorEmail: "bot@x"}) // stages first
out, _ := sb.Shell(ctx, "cd repo && make test", nil)                // a session that keeps cd and export
p, _ := sb.CreatePreview(ctx, 8000, &kosa8.PreviewOptions{Session: "main"}) // signed link to a port
_, _ = sb.SetLifecycle(ctx, kosa8.LifecycleOptions{AutoStopMin: kosa8.Ptr(30)})
_, _ = k.BuildTemplate(ctx, "py-data", "./ctx", nil)                // then CreateOptions{Template: "py-data"}
```

Files: `ListFiles`, `StatFile`, `ReadFile`, `ReadFileTo`, `WriteFile` (`io.Reader`), `WriteFileBytes`, `DeleteFile`, `MakeDir`, `MoveFile`.
Git: `Git`, `GitClone`, `GitStatus`, `GitCommit`. Sessions: `CreateSession`, `ListSessions`, `SessionExec`, `Shell`, `DeleteSession`.
Terminals: `StartPTY`, `ResizePTY`, `TTYPath`. Previews: `CreatePreview`, `RevokePreviews`. Lifecycle: `SetLifecycle`,
and `CreateOptions.AutoStopMin` / `TTLMin` / `AutoDeleteMin` / `Ephemeral`. Templates: `ListTemplates`, `BuildTemplate`, `DeleteTemplate`.

## Development

```sh
go test -race ./...            # mock daemon on a unix socket
go run ./examples/smoke        # live, against a running `kosa8 daemon`
```

MIT licensed.
