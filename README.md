# kosa8-go

The Go SDK for [kosa8](https://kosa8.com): microVM sandboxes for AI agents, on kosa8 Cloud or on
your own machine. Standard library only.
Method for method it matches [`@kosa8/sdk`](https://github.com/kosa8ai/kosa8-js) (TypeScript) and `kosa8` (Python).

```sh
go get github.com/kosa8ai/kosa8-go@v0.4.0
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
- Each exec runs in a fresh container; only `/workspace` persists between calls. Install packages
  into `/workspace`.
- A buffered exec is cut off after about **30 s** (`ErrExecTimeout`). Run long jobs in the background
  and poll: `box.ExecShell(ctx, "nohup make test > /workspace/test.log 2>&1 &", nil)`.
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

### Cloud errors

Every error is a `*kosa8.Error`; `kosa8.AsError(err)` gives its `Status`, the cloud's stable `Code`
and its sentence in `Message`. All of these also match `kosa8.ErrAPI`.

| `errors.Is(err, ...)` | When | Read |
|---|---|---|
| `ErrPaymentRequired` | 402: `payment_required`, `spending_cap_reached`, `price_not_accepted`, ... | `e.UpgradeURL` (where to pay), `e.AgentMessage` (relay word for word); empty when absent |
| `ErrOrgFrozen` | 423 `org_frozen`: the org is frozen in the Security Center | an owner or admin must unfreeze it |
| `ErrRateLimit` | 429: rate limited or `quota_exceeded` | `e.RetryAfter` (zero without a `Retry-After` header) |
| `ErrExecTimeout` | 504, code `exec_timeout`, or 502 `host_failed` on `/exec` saying "timed out" | run the job in the background; also matches `ErrTimeout` |
| `ErrUnsupported` | 501 `not_available` / `not_supported`: no data plane on this host | |
| `ErrNotFound`, `ErrAuth`, `ErrPolicy`, `ErrConflict`, `ErrBadRequest` | 404, 401, 403, 409, 400 | |

A new organization runs nothing until a payment method is on file; until then creating a sandbox
fails with `ErrPaymentRequired`.

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
