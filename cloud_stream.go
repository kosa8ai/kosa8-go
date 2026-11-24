package kosa8

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// CloudStreamIdle is how long ExecStream waits for any line (output or the
// cloud's 15 s heartbeat) before it gives the stream up as stalled.
var CloudStreamIdle = 60 * time.Second

// cloudExecFrame is one NDJSON line of a streamed exec.
type cloudExecFrame struct {
	Stream       string  `json:"stream"`
	Data         string  `json:"data"`
	Heartbeat    bool    `json:"heartbeat"`
	Done         bool    `json:"done"`
	Exit         *int    `json:"exit"`
	Error        *string `json:"error"`
	Code         string  `json:"code"`
	FailureClass *string `json:"failure_class"`
	Retryable    *bool   `json:"retryable"`
}

// ExecStream runs argv as a streamed exec: onOutput (which may be nil) gets
// each chunk of output as the command prints it, stream "stdout" or
// "stderr", and the result holds all of it once the command exits. A
// streamed exec is not held to the 90 s limit of a buffered one: TimeoutS
// may be up to CloudMaxTimeoutS (a day; 0 is none). Heartbeats are skipped.
//
// Returning an error from onOutput stops the stream (closing the
// connection) and returns that error. A command cut off by TimeoutS is an
// ErrExecTimeout and one the cloud ended otherwise an ErrStream, each with
// the output so far in Stdout and Stderr. When no line arrives for
// CloudStreamIdle the stream is given up as an ErrStream with Code
// stream_stalled. A host without the data plane answers 501 (ErrUnsupported).
func (b *CloudSandbox) ExecStream(ctx context.Context, argv []string, o *CloudExecOptions, onOutput func(stream string, data []byte) error) (CloudExecResult, error) {
	if len(argv) == 0 {
		return CloudExecResult{}, usage("ExecStream needs a command")
	}
	body := map[string]any{"cmd": argv, "stream": true}
	if o != nil {
		if o.Image != "" {
			body["image"] = o.Image
		}
		if len(o.Env) > 0 {
			body["env"] = envList(o.Env)
		}
		if o.TimeoutS != 0 {
			if o.TimeoutS < 1 || o.TimeoutS > CloudMaxTimeoutS {
				return CloudExecResult{}, usage("TimeoutS must be 1..%d, not %d", CloudMaxTimeoutS, o.TimeoutS)
			}
			body["timeout_s"] = o.TimeoutS
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return CloudExecResult{}, usage("encode request: %v", err)
	}
	path := b.path("/exec")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := b.c.stream(ctx, "POST", path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return CloudExecResult{}, err
	}
	defer resp.Body.Close()

	// A watchdog ends a stream that has gone quiet: the cloud sends a
	// heartbeat every 15 s while the command prints nothing.
	idle := CloudStreamIdle
	stalled := make(chan struct{})
	var once sync.Once
	var watchdog *time.Timer
	if idle > 0 {
		watchdog = time.AfterFunc(idle, func() { once.Do(func() { close(stalled) }); cancel() })
		defer watchdog.Stop()
	}
	isStalled := func() bool {
		select {
		case <-stalled:
			return true
		default:
			return false
		}
	}

	var out, errb bytes.Buffer
	streamErr := func(msg, code string, cause error) *Error {
		return &Error{Kind: KindStream, Message: msg, Code: code, Stdout: out.String(), Stderr: errb.String(),
			Method: "POST", Path: path, Err: cause, FailureClass: FailureInfra, Retryable: true}
	}
	rd := bufio.NewReader(resp.Body)
	for {
		line, rerr := rd.ReadBytes('\n')
		if watchdog != nil && len(line) > 0 && !isStalled() {
			watchdog.Reset(idle)
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var f cloudExecFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return CloudExecResult{}, streamErr(fmt.Sprintf("kosa8 Cloud sent a stream line that is not JSON: %.200q", line), "", err)
			}
			if f.Done {
				if f.Error != nil {
					msg := *f.Error
					if msg == "" {
						msg = "the command failed"
					}
					if f.Code == "exec_timeout" {
						return CloudExecResult{}, &Error{Kind: KindExecTimeout, Message: msg, Code: f.Code, Status: 504, Stdout: out.String(), Stderr: errb.String(),
							Method: "POST", Path: path, FailureClass: FailureTimeout, cloud: true}
					}
					e := streamErr(msg, f.Code, nil)
					if f.FailureClass != nil && *f.FailureClass != "" {
						e.FailureClass = FailureClass(*f.FailureClass)
						e.Retryable = e.FailureClass == FailureInfra
					}
					if f.Retryable != nil {
						e.Retryable = *f.Retryable
					}
					return CloudExecResult{}, e
				}
				fc, rt := execClass{FC: f.FailureClass, RT: f.Retryable}.class(f.Exit)
				return CloudExecResult{ExitCode: f.Exit, Stdout: out.String(), Stderr: errb.String(), FailureClass: fc, Retryable: rt}, nil
			}
			if (f.Stream == "stdout" || f.Stream == "stderr") && f.Data != "" && !f.Heartbeat {
				chunk, derr := base64.StdEncoding.DecodeString(f.Data)
				if derr != nil {
					chunk = []byte(f.Data)
				}
				if f.Stream == "stdout" {
					out.Write(chunk)
				} else {
					errb.Write(chunk)
				}
				if onOutput != nil {
					if err := onOutput(f.Stream, chunk); err != nil {
						return CloudExecResult{}, err
					}
				}
			}
		}
		if rerr != nil {
			switch {
			case isStalled():
				return CloudExecResult{}, streamErr(fmt.Sprintf("kosa8 Cloud stopped sending the exec stream (no data or heartbeat in %s)", idle), "stream_stalled", rerr)
			case errors.Is(rerr, io.EOF):
				return CloudExecResult{}, streamErr("the exec stream ended before the command finished (no final frame)", "stream_broken", nil)
			case ctx.Err() != nil:
				return CloudExecResult{}, ctx.Err()
			default:
				return CloudExecResult{}, streamErr("the exec stream broke: "+rerr.Error(), "stream_broken", rerr)
			}
		}
	}
}
