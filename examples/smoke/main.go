// Smoke test against a real kosa8 daemon: create, pull, run, checkpoint, rewind, delete.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	kosa8 "github.com/kosa8ai/kosa8-go"
)

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return v
}

func main() {
	ctx := context.Background()
	k := must(kosa8.New(kosa8.Options{}))
	fmt.Println("daemon", must(k.Version(ctx)))
	name := "gosdk-smoke"
	t := time.Now()
	must(k.CreateSandbox(ctx, kosa8.CreateOptions{Name: name, Egress: "deny"}))
	fmt.Printf("created in %.1fs\n", time.Since(t).Seconds())
	defer func() { fmt.Println("delete", must(k.DeleteSandbox(ctx, name, kosa8.DeleteOptions{}))) }()
	sb := k.Sandbox(name)
	must(sb.Pull(ctx, "alpine:latest"))
	res := must(sb.Run(ctx, "alpine:latest", []string{"sh", "-c", "echo hello from kosa8-go; uname -r"}, nil))
	fmt.Printf("run exit=%d out=%q\n", res.ExitCode, res.Stdout())
	cp := must(sb.Checkpoint(ctx, "base"))
	fmt.Println("checkpoint", cp.Checkpoint.ID)
	rw := must(sb.Rewind(ctx, ""))
	fmt.Println("rewind restore_ms", rw.RestoreMS)
	eg := must(sb.Egress(ctx))
	fmt.Println("egress policy", eg["policy"])
}
