package kosa8_test

// These examples are the README's snippets. They compile with the tests but
// have no Output comment, so `go test` never runs them (they would need a
// kosa8 Cloud API key or a running daemon).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	kosa8 "github.com/kosa8ai/kosa8-go"
)

func ExampleNewCloud() {
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
	if r.ExitCode != nil { // nil when the cloud reported no exit status
		fmt.Println("exit", *r.ExitCode)
	}
	fmt.Print(r.Stdout) // 42

	// Files (needs the host's data plane; 501 ErrUnsupported without it).
	if _, err := box.Files().WriteString(ctx, "/workspace/hello.txt", "hi\n"); err != nil {
		log.Fatal(err)
	}
	text, _ := box.Files().ReadText(ctx, "/workspace/hello.txt")
	fmt.Print(text)
	entries, _ := box.Files().List(ctx, "") // "" is /workspace
	for _, e := range entries {
		fmt.Println(e.Type, e.Path, e.Size)
	}
}

func ExampleCloudSandbox_Update() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	box, _ := cloud.Sandboxes.Get(ctx, "sbx_...")
	_, _ = box.Update(ctx, kosa8.CloudUpdateOptions{TTLSeconds: 24 * 3600})     // a day from now
	_, _ = box.Update(ctx, kosa8.CloudUpdateOptions{ClearLifetime: true})       // never delete it automatically
	_, _ = box.Update(ctx, kosa8.CloudUpdateOptions{SleepAfterS: kosa8.Ptr(0)}) // never sleep
}

func ExampleCloudSandboxes_ListAll() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	all, err := cloud.Sandboxes.ListAll(ctx, kosa8.CloudListOptions{Limit: 100})
	if err != nil {
		log.Fatal(err)
	}
	for _, b := range all {
		fmt.Println(b.ID(), b.State())
	}
	page, _ := cloud.Sandboxes.ListPage(ctx, kosa8.CloudListOptions{Limit: 20})
	fmt.Println(len(page.Sandboxes), page.NextCursor) // NextCursor "" on the last page
}

func ExampleCloudSandbox_Checkpoint() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{TTLSeconds: 3600})
	cp, err := box.Checkpoint(ctx, "clean")
	if errors.Is(err, kosa8.ErrUnsupported) {
		fmt.Println("this host has no data plane yet")
		return
	}
	_, _ = box.ExecShell(ctx, "rm -rf /workspace/*", nil)
	_, _ = box.Rewind(ctx, cp.ID) // back to "clean"
	kids, _ := box.Fork(ctx, 3)   // three copies as it is now
	ports, _ := box.Ports(ctx)    // TCP ports listening inside
	fmt.Println(len(kids), len(ports))
}

func ExampleCloudFiles() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	box, _ := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{TTLSeconds: 3600})

	f := box.Files()
	data, _ := f.Read(ctx, "/workspace/out.bin")   // []byte
	_, _ = f.Write(ctx, "/workspace/in.bin", data) // raw bytes, up to 8 MiB
	_, _ = f.Mkdir(ctx, "/workspace/build")
	_, _ = f.Delete(ctx, "/workspace/build", true) // recursive

	cp, _ := box.Checkpoint(ctx, "clean") // memory, processes and disk
	list, _ := box.Checkpoints(ctx)       // oldest first
	_, _ = box.Rewind(ctx, cp.ID)         // .Backup is the state just before
	kids, _ := box.Fork(ctx, 3)           // 1..100 copies as it is now
	ports, _ := box.Ports(ctx)            // TCP ports listening inside
	fmt.Println(len(list), len(kids), len(ports))
}

func ExampleCloud_Templates() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	ts, _ := cloud.Templates(ctx)
	for _, t := range ts {
		fmt.Println(t.Key, t.Image)
	}
}

func Example_cloudErrors() {
	ctx := context.Background()
	cloud, _ := kosa8.NewCloud(kosa8.CloudOptions{})
	box, err := cloud.Sandboxes.Create(ctx, kosa8.CloudCreateOptions{TTLSeconds: 600})
	switch {
	case errors.Is(err, kosa8.ErrPaymentRequired):
		e, _ := kosa8.AsError(err)
		fmt.Println(e.Code, e.UpgradeURL)
		fmt.Println(e.AgentMessage) // relay word for word to the person
		return
	case errors.Is(err, kosa8.ErrOrgFrozen):
		fmt.Println("an owner or admin must unfreeze the org in the Security Center")
		return
	case errors.Is(err, kosa8.ErrRateLimit):
		e, _ := kosa8.AsError(err)
		time.Sleep(e.RetryAfter) // zero when the server sent no Retry-After
		return
	case err != nil:
		log.Fatal(err)
	}
	_, err = box.ExecShell(ctx, "sleep 120", nil)
	if errors.Is(err, kosa8.ErrExecTimeout) {
		// Over ~30 s: run it in the background and poll instead.
		_, _ = box.ExecShell(ctx, "nohup sh -c 'sleep 120; echo done' > /workspace/job.log 2>&1 &", nil)
	}
}

func ExampleNew() {
	ctx := context.Background()
	k, err := kosa8.New(kosa8.Options{}) // local daemon at ~/.kosa8/run/kosa8.sock
	if err != nil {
		log.Fatal(err)
	}
	sb := k.Sandbox("demo")
	_, err = k.CreateSandbox(ctx, kosa8.CreateOptions{Name: "demo", EgressAllow: []string{"pypi.org"}})
	if err != nil {
		log.Fatal(err)
	}
	_, _ = sb.Pull(ctx, "python:3.12-alpine")
	res, _ := sb.Run(ctx, "python:3.12-alpine", []string{"python", "-c", "print(6*7)"}, nil)
	fmt.Print(res.Stdout()) // 42
	_, _ = sb.Delete(ctx, kosa8.DeleteOptions{})
}
