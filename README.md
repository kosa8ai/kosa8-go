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
| `Options.Actor` | `tool` (`human`, `agent`, `tool`), sent as `X-Kosa8-Actor` |
| `Options.AgentToken` / `KOSA8_AGENT_TOKEN` | sent as `X-Kosa8-Agent-Token` |

## Errors

Every error is a `*kosa8.Error` with a `Kind`, the daemon's own message and its remediation.
Use `errors.Is(err, kosa8.ErrNotFound)`, `ErrPolicy`, `ErrApprovalRequired` (read `e.Approval`),
`ErrConflict`, `ErrConnection`, `ErrStream` (output so far in `e.Stdout`/`e.Stderr`), `ErrTimeout`.
A policy that needs a signed approval takes it per call: `kosa8.Call{Approval: sig}`.

## API

Daemon: `Health`, `Version`, `Pull`.
Sandboxes: `ListSandboxes`, `GetSandbox`, `CreateSandbox`, `StartSandbox`, `StopSandbox`, `DeleteSandbox`,
`PullIntoSandbox`, `SandboxImages`, `SandboxContainers`, `SandboxEgress`.
Processes: `Run`, `StartContainer`, `Exec`, `ExecFrames`, `StartInteractive`, `WriteStdin`, `Attach`.
Snapshots: `Snapshot`, `ListSnapshots`, `RestoreSnapshot`, `DeleteSnapshot`, `Fork`, `Explore`.
Checkpoints: `Checkpoint`, `ListCheckpoints`, `Rewind` (store: `~/.kosa8/checkpoints.json`, flock-shared with the CLI).
`Client.Sandbox(id)` binds all of them to one sandbox.

## Development

```sh
go test -race ./...            # mock daemon on a unix socket
go run ./examples/smoke        # live, against a running `kosa8 daemon`
```

MIT licensed.
