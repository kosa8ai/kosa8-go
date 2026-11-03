# Changelog

## Unreleased

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
