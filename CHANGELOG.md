# Changelog

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
