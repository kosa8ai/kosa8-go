# Changelog

## Unreleased

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

## 0.5.0 — unreleased

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
