# Changelog

## Unreleased

- `Files().Upload` of more than 100 MB failed: the API takes at most 100 MB a request (Cloudflare's
  limit). An upload over `CloudUploadPartBytes` (64 MiB) now goes in parts, the first replacing
  the file and the rest written at their offsets (`PUT …/files/raw?offset=`), each retried on its own.
- `CloudSandbox.ExecStream(ctx, argv, opts, onOutput)`: the streamed exec (`stream: true`) the
  Python and JS SDKs have, with no 90 s limit (`TimeoutS` up to 86400), output delivered as it is
  printed, heartbeats skipped, a stalled stream given up after `CloudStreamIdle` (60 s), and
  `exec_timeout` / error frames returned as `ErrExecTimeout` / `ErrStream` with the output so far.
  `Exec` with a `TimeoutS` over 90 now streams instead of refusing it.
- `ExecBackground` sends `CloudExecOptions.TimeoutS` (1..86400); it was dropped, so a runaway
  background command ran until the sandbox went. `CloudBackgroundExec` gains `TimeoutS`,
  `TimedOut` and `Stopped`.
- Cloud errors carry the 402/429 contract in full: `Error.Quota` (`*QuotaDetail`: a `plan_limit` /
  `plan_feature`'s `LimitName`, `Limit`, `Used`, `Plan`, `NextPlan`, `Feature`, `PackURL` and `Options`,
  the ways past the limit) and `Error.RequestURL` (a 429 `quota_exceeded`'s link to ask for more).
  These were only reachable through the untyped `Error.Body` before.
- Retries of creates, batches, forks, grades and job submits use a new SDK-made `Idempotency-Key`
  after a 5xx (502 `host_failed`, 503 `no_capacity` / `host_restarting` / `host_unavailable`, 504),
  so each retry of a server-side failure is a new attempt. The key is still kept after no answer
  (the first try may have created the sandbox), a 429 and a 409 `idempotency_in_progress` /
  `template_building`. A key you pass as `IdempotencyKey` is never changed. Retry-After is honoured
  as before.

## 0.6.1 - 2026-11-22

The 0.6 release of the five SDKs. Go is v0.6.1, not v0.6.0: a `v0.6.0` tag was pushed by mistake on
the 0.5.3 code and the Go checksum database keeps it, so it cannot be replaced; `go.mod` retracts
v0.6.0.

- **Full Cloud API coverage** (Go was the "core" tier; it now carries what Python and JS have, and more):
  - Browsers: `Cloud.Browsers` (Create, List, Get, Delete), `CloudBrowser.Page`, `Screenshot`,
    `Refresh`, `Close`, `CDPEndpoint` (the `/v1/browsers/{id}/cdp` WebSocket URL).
  - AI gateway: `Cloud.AI` (Settings, UpdateSettings, Keys, SetKey, DeleteKey, Usage, `GatewayURL`
    for pointing OpenAI/Anthropic clients at `/v1/ai/gw/{provider}/`, and `Call` for one model call).
  - Hosted jobs: `Cloud.Jobs` (Submit with an Idempotency-Key, Estimate, Get, List, Cancel, Wait).
  - Benchmarks: `Cloud.Benchmarks` (List, Get, Instances, Instance, Prepare).
  - Recordings (`Cloud.Recordings`, `CloudSandbox.Recordings`), embeds (`CloudSandbox.Embeds()`),
    custom preview domains (`CloudPreviews.Domains(port)`, `Cloud.PreviewDomains(id)`).
  - `Cloud.APIKeys`, `Cloud.Webhooks`, `Cloud.Audit`, `Cloud.RLAlerts`, `Cloud.CompareRuns`.
  - `Cloud.Limits`, `RequestLimit`, `UsageInsights`, `Pricing`, `Burst`, `ActivateBurst`,
    `OrgEgress`, `SetOrgEgress`, `Checkpoints`, `Lineage`, `Images`, `AttackScorecards`,
    `AttackScorecard`, `UploadAttackScorecard`, `Functions.Invocations`,
    `CloudSandbox.StopBackground`.
- **Small sizes**: `CloudCreateOptions.CPUs` is a `float64` (0.25, 0.5, 1 with MemMB 512, 1024,
  2048), and `Size` takes `0.25x0.5`, `0.5x1` and `1x2` (`CloudSmallSizes`; `CloudStandardSizes`
  for templates). `CloudSnapshot.CPUs` and `CloudMetrics.CPUs` are `float64` too, so a small
  sandbox's 0.25 decodes. Source change: an `int` variable assigned to `CPUs` needs a conversion.
- **Overflow honesty**: `CloudSandbox.Overflow()` (region `cf-auto`, Cloudflare Containers). Fork,
  checkpoints, rewind, snapshots and file links there return the server's 501 `not_available` as
  `ErrUnsupported`, not retried.
- **Retries**: every request, POSTs included, is retried on 429, 503 `database_busy` and 409
  `template_building` (refused before the cloud acted on it), with backoff and full jitter, waiting
  at least `Retry-After` or the body's `retry_after` / `retry_after_s`. Reads, puts and deletes
  also retry any answer with `retryable: true`. Streamed downloads and uploads (a seekable body)
  retry the same way. `database_busy` is infra and retryable; `template_building` and
  `idempotency_in_progress` are infra and retryable; a 4xx `invalid_*` / `bad_*` / `missing_*` /
  `unknown_*` code is client, as the server's failures.js says.
- README: `go get github.com/kosa8ai/kosa8-go@v0.6.1`; what persists (each exec is a fresh
  process, files in `/workspace` persist for the sandbox's life, installed packages need
  `Persistent: true`); small sizes, overflow, retries and the new groups.
- Path literals: preview, schedule and code-context deletes, background exec status and pipeline
  artifact downloads now name their full route in the source, so the SDK parity checker sees them.

## 0.5.3

- `CloudExecOptions.TimeoutS` (1..90; `CloudExecMaxTimeoutS`): the API's exec `timeout_s`, which stops the command in the sandbox when it runs that long and answers `ErrExecTimeout` (without the hint about the 90 s buffered limit). Python, JS, Ruby and Java have the same option. Any other value is an `ErrUsage` before the call.
- README: `go get github.com/kosa8ai/kosa8-go@latest` (it said `@v0.5.0`); what persists between commands (only `/workspace`, unless the sandbox is persistent) matches the server; `box.ID()` is a method (`go vet` reports `fmt.Println(box.ID)`), and `ExitCode` is a pointer; a "same thing in each language" table for the five SDKs.

## 0.5.2

- **Actionable Cloud errors**: a kosa8 Cloud API error's `Error()` is the server's sentence followed by
  `[HTTP <status> <code>; retry in <n> s; request <id>]` (and `; upgrade: <url>` on a 402 whose
  sentence lacks it). `Message` stays the bare sentence; daemon errors are unchanged.
- New `Error.RequestID`: the answer's `X-Request-Id`, else Cloudflare's `cf-ray`; quote it to support.
- A 401 says how to fix the key (set `KOSA8_API_KEY` or `CloudOptions.APIKey`; make one at
  https://app.kosa8.com/app#keys). A missing or malformed key, a timeout (try again or raise
  `CloudOptions.Timeout`) and an unreachable API (check the network and `KOSA8_CLOUD_URL`) say what to do next.
- Fix: a streamed download's error no longer drops the body's `retry_after_s` when the answer had no
  Retry-After header.

## 0.5.1

- Failure class `oom`: exit 137 with the guest kernel's OOM count raised (kosa8 Cloud reports it on exec and grade; a grade killed for memory has no reward). It can be added to a rollout mask.

## 0.5.0

RL training and fan-out on kosa8 Cloud, aligned with the Cloud API's RL release and the Python
SDK's `kosa8.rl`.

- **Failure classes**: every Cloud `*Error` carries `FailureClass` (`ok`, `exit_nonzero`,
  `timeout`, `infra`, `quota`, `client`) and `Retryable`, as the server sends them (or by its own
  rule, `FailureOf`, when it does not). `ClassifyFailure(err)` covers any error (`error` for your
  own). `CloudExecResult` gains `FailureClass` and `Retryable`.
- **Retries**: `CloudOptions.Retry` (`RetryPolicy`, default `DefaultRetryPolicy()`: 3 tries, full
  jitter backoff from 0.5 s to 8 s, at least the server's `Retry-After` up to 60 s; `&NoRetry`
  turns it off). Applied only where safe: reads, deletes, heartbeats, drains, the keyed POSTs, and
  any request answered 429. `Retry(ctx, policy, fn)` for your own calls.
- **Idempotency-Key** on create, batch, fork and grade: a new UUID (or `IdempotencyKey`), reused on
  every retry, so a retry after a timeout or a 5xx never doubles the fleet.
- **Fan-out**: `Sandboxes.CreateMany` (up to 1000, `CloudBatchOptions` with `RunID`, `Prefix`,
  `PartialOK`) and `CloudSandbox.ForkWith` (`CloudForkOptions`: `N` up to 1000, `Labels`, `RunID`,
  `LeaseS`, `Prefix`, `PartialOK`) return a `CloudSandboxBatch` with `Failed`. `Fork(n)` now takes
  up to 1000 (still all or nothing).
- **Leases**: `CloudCreateOptions.LeaseS`; `Sandboxes.Heartbeat` / `CloudSandbox.Heartbeat`;
  `Sandboxes.Drain` (by run, labels or ids; follows `more`).
- **Sealed grader**: `CloudSandbox.Grade` / `Sandboxes.Grade` (hidden files, no network, reward,
  optional recording on a run).
- **`ForkMap`**: run a function over items, each in its own fork, bounded concurrency, retries on a
  fresh fork, per-item timeout, and every child deleted however it ends.
- **RL runs**: `Cloud.StartRun` / `AttachRun` -> `*CloudRun` (background heartbeats, `Record` in
  batches of at most 500, `Flush`, `Finish` with drain, `Summary`, `Update`, `Rollouts`, `Export`,
  `Drain`), `Cloud.ListRuns`, and `MapRollouts` (one training step with sealed grading, group
  sampling, masking, results as `Rollouts`).
- **Org templates**: `Cloud.OrgTemplates` (`Build`, `Import`, `List`, `Get`, `Delete`, `WaitReady`).
- **Capability tokens**: `Cloud.Tokens` (`Create`, `List`, `Revoke`); `NewCloud` accepts a
  `k8c_tok_` token as the key.

## 0.4.0 — 2026-11-15

The rest of the kosa8 Cloud API:

- **Sized and Docker creates**: `CloudCreateOptions` `Size`, `CPUs`/`MemMB`, `Image`, `DiskGB`,
  `Persistent`, `Docker`, `CPU` (`dedicated`), `Name`, `Labels`, `Env`, `Secrets`, `Egress`,
  `Snapshot`, `NoWait`; a cold boot (202 `creating`) is waited for with the new `WaitRunning`.
  Checked before any request. `CloudSandboxInfo` gains `SnapshotID`, `Persistent`, `Name`,
  `Image`, `DiskGB`, `CPU`, `Labels`; `CloudListOptions` gains `Name` and `Labels`.
- **Background commands**: `ExecBackground`, `BackgroundExec`, `WaitBackground`.
- **Durable snapshots**: `CloudSandbox.Snapshot`, `Cloud.Snapshots` (`List`, `Get`, `Delete`).
- **Previews** (`Previews()`: `Create`, `List`, `Link`, `Rotate`, `Delete`), **terminal** URLs.
- **Files**: streamed `Download`/`Upload` (`/files/raw`, no 8 MiB cap) and presigned `Link`.
  Fix: `CloudFileInfo.Mode` is now `uint32`; the API sends a number, so `Write` and `List`
  failed to decode on live hosts.
- **Egress**, **secrets** (`Cloud.Secrets`, values never printed by `%#v`), **schedules**,
  **code interpreter** (`RunCode`, contexts), `Metrics`, `Logs`, `SetLabels`,
  `Cloud.Capabilities`, `Cloud.EgressPresets`.
- **Pipelines** (`Cloud.Pipelines`): `Run`, `Runs`, `Get`, `Wait`, `Logs`, `Cancel`,
  `Artifacts`, `DownloadArtifact`, `CreateHook`, `Hooks`, `DeleteHook`.

kosa8 Cloud parity with the Python SDK. The default Cloud address is `https://api.kosa8.com`
(0.3.0 defaulted to `api.kosa8.cloud`; do not use 0.3.0). `KOSA8_CLOUD_URL` still overrides it.

- **Sandbox lifetime**: `CloudCreateOptions.TTLSeconds` (60..7776000), `DeleteAfter` (Unix ms;
  not with `TTLSeconds`) and `AcceptPrice` (paid kosa8 Store items), checked before any request;
  `SleepAfterS` is checked against 0..604800. Only fields you set are sent.
- **`CloudSandbox.Update`** (`PATCH /v1/sandboxes/{id}`) with `CloudUpdateOptions`: `TTLSeconds`,
  `DeleteAfter`, `SleepAfterS`, or `ClearLifetime` (sends `"ttl_s": null`). Refreshes `Info` and
  returns the server's notes.
- **Paging**: `Sandboxes.ListPage` (`CloudListOptions{All, Limit, Cursor}` -> `CloudSandboxPage`
  with `NextCursor`, `""` when the server sends none) and `Sandboxes.ListAll`, which follows
  `NextCursor` and stops if a cursor repeats. `List(ctx, all)` is unchanged.
- **Files** (`CloudSandbox.Files()`): `Read` (raw `[]byte`), `ReadText`, `Write` (raw bytes,
  `application/octet-stream`, up to 8 MiB), `WriteString`, `List` (default `/workspace`), `Mkdir`,
  `Delete` (recursive). Paths are URL-encoded.
- **Checkpoints, rewind, fork, ports**: `CloudSandbox.Checkpoint` (label up to 200 characters),
  `Checkpoints`, `Rewind` (returns the checkpoint and the automatic backup; refreshes `Info`),
  `Fork` (1..100 new `*CloudSandbox`), `Ports`. These and the file methods need the host's data
  plane; hosts without it answer 501 (`ErrUnsupported`).
- **Templates**: `Cloud.Templates` -> `[]CloudTemplate`.
- **`CloudSandboxInfo`** now has every field of the API's Sandbox schema: `OriginItemVersion`,
  `ParentID`, `DeleteAfter`, `SleepAfterS`, `HostUnreachable` (and `Source` may be `fork`).
- **Errors**: `Error.UpgradeURL` and `Error.AgentMessage` (sent with a 402); `Error.RetryAfter`
  from a `Retry-After` header; new `ErrOrgFrozen` (423 `org_frozen`) and `ErrExecTimeout` (504,
  code `exec_timeout`, or a 502 `host_failed` on `/exec` saying "timed out"; also matches
  `ErrTimeout`), whose message keeps the server's words and adds the 30 s limit and the
  background-job workaround. 501 `not_available` / `not_supported` stays `ErrUnsupported`.
- Cloud requests send `User-Agent: kosa8-go/0.4.0` (`kosa8.UserAgent`); `Version` is `0.4.0`.
- **Docs**: the README leads with a kosa8 Cloud quickstart, documents the base template (Alpine,
  busybox `sh`, only `/workspace` persists, 30 s exec limit, 64 KiB body, 8 MiB files) and the
  Cloud errors, and no longer dereferences a possibly nil `ExitCode`. The snippets compile as
  examples in `example_test.go`.

## v0.3.1 — 2026-11-03

- The default kosa8 Cloud address is now `https://api.kosa8.com` (0.3.0 defaulted to `api.kosa8.cloud`, which kosa8 Cloud does not use). `KOSA8_CLOUD_URL` still overrides it.

## v0.3.0 — 2026-11-03

kosa8 Cloud's API (api.kosa8.cloud) is not open yet; the `Kosa8Cloud` client is ready for when it is.

- **Saved worker connection**, as the CLI: with no `Options.Host`, no `KOSA8_HOST` and no
  `Options.SocketPath`, the client uses `~/.kosa8/worker.json` (written by `kosa8 wsl setup` on
  Windows and `kosa8 worker join`): its host, its token unless `Options.Token` is set, and its
  `insecure` flag. A saved token is never sent to a host from an option or the environment.
  New `Options.WorkerFile` / `NoWorkerFile`, `Resolve` and `DefaultWorkerFile`; `ResolveEndpoint`
  is unchanged and does not read the file. On Windows a failed connection to the default socket
  hints at `kosa8 wsl setup` or `KOSA8_HOST`.
- `CreateOptions.Mounts`: the host side is checked as a POSIX absolute path on every OS (the
  daemon's filesystem; WSL2 on Windows), so `/src:/work` is accepted from Windows.
- **CI**: unit tests on ubuntu, macOS and Windows (`.github/workflows/ci.yml`). The mock daemon's
  unix socket works on Windows (AF_UNIX); the tests skip it if a runner lacks it.
- **kosa8 Cloud client**, matching `Kosa8Cloud` in `kosa8` (Python) and `@kosa8/sdk`: `NewCloud`
  (`CloudOptions`: `APIKey` / `KOSA8_API_KEY`, which must start with `k8c_live_`; `BaseURL` /
  `KOSA8_CLOUD_URL`, default `https://api.kosa8.cloud`, https only except localhost; `HTTPClient`;
  `Timeout`, default 120 s). The key is never printed.
- **Sandboxes**: `Sandboxes.Create` / `List` / `Get`; `CloudSandbox.Exec` (argv), `ExecShell`
  (`sh -c`), `Sleep`, `Wake`, `Refresh`, `Delete`. `CloudExecResult.WokeMs` is set when a sleeping
  sandbox woke for the command.
- **Functions**: `Functions.Deploy` / `List` / `Get` / `Delete` / `Invoke` / `Map`, with typed
  `CloudInvokeResult` (`Result` is `json.RawMessage`; `DecodeResult`) and `CloudMapResult`.
- `Cloud.Me`, `Cloud.Usage`.
- **Errors**: `Error.Code` carries the cloud's stable code and `Message` its sentence. New kinds
  `KindPaymentRequired` (402, `ErrPaymentRequired`) and `KindRateLimit` (429, `ErrRateLimit`),
  both also `ErrAPI`; they apply to daemon answers with those statuses too.

## v0.2.0 — 2026-11-03

The sandbox toolbox, matching `kosa8` (Python) 0.2.0 and the daemon's v0.13.0 routes.

- **Run code**: `RunCode` runs a Python, JavaScript or TypeScript snippet in `/workspace`;
  `CodeResult.Artifacts` carries charts as decoded PNG bytes. A non-zero exit is not an error.
- **Files**: `ListFiles`, `StatFile`, `ReadFile` / `ReadFileTo` (an `io.Writer`; a short body is
  a `KindStream` error), `WriteFile` (an `io.Reader`, chunked when its length is unknown) /
  `WriteFileBytes`, `DeleteFile` (`Recursive`), `MakeDir`, `MoveFile`. Typed `FileInfo`.
- **Git**: `Git` (any op) plus `GitClone`, `GitStatus` (parsed `GitStatus`) and `GitCommit`, which
  stages first (`GitOptions.Files`, or everything; `NoStage` skips it). A git failure is
  `GitResult.OK == false`, not an error.
- **Sessions**: `CreateSession`, `ListSessions`, `SessionExec`, `DeleteSession`, and `Shell`, which
  makes the session (default `main`) the first time it is told `no such session`.
- **Terminals**: `StartPTY`, `ResizePTY`, `TTYPath` for the WebSocket route.
- **Previews**: `CreatePreview` (typed `Preview`), `RevokePreviews`.
- **Lifecycle**: `SetLifecycle` (`LifecycleOptions` with pointer fields: nil keeps, 0 turns off;
  `kosa8.Ptr(30)`), and `CreateOptions.AutoStopMin`, `TTLMin`, `AutoDeleteMin`, `Ephemeral`.
- **Templates**: `ListTemplates`, `DeleteTemplate`, `BuildTemplate` (tars a context folder,
  honouring `.dockerignore`; a failed build is a `KindStream` error), and `CreateOptions.Template`.
- Every toolbox method is also on the `Sandbox` handle. `Frame.Image` carries a build's image.
- Still standard library only.

Known daemon behaviour (v0.13.0): `DeleteSession` answers `500 cannot delete running task …:
failed precondition` although the session is gone moments later.

## v0.1.2 — 2026-11-03

- **`Version` reports `0.1.2`** (v0.1.1 still said `0.1.0`).
- **`CaptureOptions.Path` is documented as the host file the capture is written to** (pcapng),
  not a directory; the usage error says so too. Passing a directory fails on the daemon
  with `capture: open …: is a directory`.
- **Mounts must use absolute paths on both sides**; a relative one is a `KindUsage` error
  before the request.
- **The `Exec` Env error points at `StartInteractive`'s `InteractiveOptions.Env`**, not
  `StartContainer`, which takes no environment.

## v0.1.1 — 2026-11-03

Fixes found while writing and testing the docs at https://docs.kosa8.com/guides/go-guide/.

- **`CreateOptions.Mounts` works.** The `host:guest[:ro]` strings are now sent as the
  objects the daemon takes; v0.1.0 sent the strings and every mount was refused with
  `500 mount has no guest path`. A malformed entry is a `KindUsage` error.
- **`CreateOptions.Capture` is now `*CaptureOptions{Path, MaxBytes, MaxFiles}`** (Path is the host file to write; was
  `bool`, which the daemon always refused with `capture: no output path`). This is a
  source-level change, but `Capture: true` never worked.
- **`Exec` with `RunOptions.Env` returns a `KindUsage` error** instead of silently running
  without the variables: the daemon's exec takes no environment.
- **A second `WriteStdin` to the same container returns a `KindUsage` error** instead of
  answering ok for input the process never sees (one body is the whole of stdin).
- **`ExecFrames` returns a `KindStream` error when the final frame carries one** (it
  returned nil before), after passing that frame to your callback.
- **Concurrent checkpoints of one sandbox no longer collide.** The 409 loser steps to the
  next id; a 409 for any other reason is returned.
- **A failed `Rewind` says what happened**: the sandbox was deleted, not stopped, and the
  checkpoint is intact.
- **`ErrUsage`** sentinel for `errors.Is(err, kosa8.ErrUsage)`.

Not changed (daemon behaviour): timeouts and cancellation stop the client, not the
command; status codes for a missing snapshot differ between delete (404) and restore (500).

## v0.1.0 — 2026-11-03

First release.
