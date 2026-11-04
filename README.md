# kosa8-go

The Go SDK for [kosa8](https://kosa8.com): microVM sandboxes for AI agents. Standard library only.
Method for method it matches [`@kosa8/sdk`](https://github.com/kosa8ai/kosa8-js) (TypeScript) and `kosa8` (Python).

```sh
go get github.com/kosa8ai/kosa8-go
```

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

## Connecting

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

## Errors

Every error is a `*kosa8.Error` with a `Kind`, the daemon's own message and its remediation.
Use `errors.Is(err, kosa8.ErrNotFound)`, `ErrPolicy`, `ErrApprovalRequired` (read `e.Approval`),
`ErrConflict`, `ErrPaymentRequired` (402), `ErrRateLimit` (429), `ErrConnection`, `ErrStream` (output so far in `e.Stdout`/`e.Stderr`), `ErrTimeout`.
A policy that needs a signed approval takes it per call: `kosa8.Call{Approval: sig}`.

## API

Daemon: `Health`, `Version`, `Pull`.
Sandboxes: `ListSandboxes`, `GetSandbox`, `CreateSandbox`, `StartSandbox`, `StopSandbox`, `DeleteSandbox`,
`PullIntoSandbox`, `SandboxImages`, `SandboxContainers`, `SandboxEgress`.
Processes: `Run`, `StartContainer`, `Exec`, `ExecFrames`, `StartInteractive`, `WriteStdin`, `Attach`.
Snapshots: `Snapshot`, `ListSnapshots`, `RestoreSnapshot`, `DeleteSnapshot`, `Fork`, `Explore`.
Checkpoints: `Checkpoint`, `ListCheckpoints`, `Rewind` (store: `~/.kosa8/checkpoints.json`, flock-shared with the CLI).
Toolbox: see below.
`Client.Sandbox(id)` binds all of them to one sandbox.

## Toolbox

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

## kosa8 Cloud

The same sandboxes on kosa8 Cloud instead of the local daemon. Make an API
key in the cloud dashboard and set `KOSA8_API_KEY` (and `KOSA8_CLOUD_URL` to
point somewhere other than `https://api.kosa8.com`).

```go
cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{}) // or CloudOptions{APIKey: "k8c_live_..."}

box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{Template: "python", SleepAfterS: kosa8.Ptr(600)})
r, _ := box.Exec(ctx, []string{"python3", "-c", "print(6 * 7)"}, nil)
fmt.Println(*r.ExitCode, r.Stdout)      // 0 42
_ = box.Sleep(ctx)                      // costs only snapshot storage while asleep
_, _ = box.ExecShell(ctx, "ls /workspace", nil) // wakes it, files and all
_ = box.Delete(ctx)

_, _ = cloud.Functions.Deploy(ctx, "hello", kosa8.CloudFunctionSpec{
	Runtime: "python3.12",
	Code:    "def handler(event):\n    return {\"hello\": event.get(\"name\", \"world\")}\n",
})
out, _ := cloud.Functions.Invoke(ctx, "hello", map[string]any{"name": "kosa8"})
fmt.Println(out.OK, string(out.Result)) // true {"hello":"kosa8"}
```

A new organization runs nothing until a payment method is on file; until
then creating a sandbox fails with `errors.Is(err, kosa8.ErrPaymentRequired)`
(`e.Code == "payment_required"`), whose message says what to do. Every error
from the cloud carries its stable `Code`; 429 is `ErrRateLimit`. A function
that raised is `out.OK == false` with `out.Error`, not a Go error.
Also: `Sandboxes.List` / `Get`, `CloudSandbox.Wake` / `Refresh`, `Functions.List` / `Get` /
`Delete` / `Map`, `Cloud.Me`, `Cloud.Usage`.

## Development

```sh
go test -race ./...            # mock daemon on a unix socket
go run ./examples/smoke        # live, against a running `kosa8 daemon`
```

MIT licensed.
