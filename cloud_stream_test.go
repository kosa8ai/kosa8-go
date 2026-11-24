package kosa8

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ndjson(frames ...map[string]any) []byte {
	var b strings.Builder
	for _, f := range frames {
		j, _ := json.Marshal(f)
		b.Write(j)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestCloudExecStream(t *testing.T) {
	big := strings.Repeat("x", 200*1024) // a frame longer than bufio.Scanner's 64 KiB default
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 200, raw: ndjson(
			map[string]any{"stream": "stdout", "data": b64("hel")},
			map[string]any{"stream": "stdout", "data": "", "heartbeat": true},
			map[string]any{"stream": "stdout", "data": b64("lo\n")},
			map[string]any{"stream": "stderr", "data": b64("warn")},
			map[string]any{"stream": "stdout", "data": b64(big)},
			map[string]any{"done": true, "exit": 3},
		)}
	})
	b := cloudBox(t, c)
	var got []string
	r, err := b.ExecStream(context.Background(), []string{"make", "test"}, &CloudExecOptions{TimeoutS: 3600, Env: map[string]string{"A": "1"}},
		func(stream string, data []byte) error {
			got = append(got, stream+":"+string(data[:min(len(data), 5)]))
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode == nil || *r.ExitCode != 3 || r.Stdout != "hello\n"+big || r.Stderr != "warn" || r.FailureClass != FailureExitNonzero {
		t.Fatalf("%+v", r.FailureClass)
	}
	if want := []string{"stdout:hel", "stdout:lo\n", "stderr:warn", "stdout:xxxxx"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks %q", got)
	}
	if body := stub.last().JSON(t); !reflect.DeepEqual(body, map[string]any{"cmd": []any{"make", "test"}, "stream": true, "timeout_s": float64(3600), "env": []any{"A=1"}}) {
		t.Fatalf("body %v", body)
	}
}

// A buffered exec refused TimeoutS over 90; the other SDKs stream it.
func TestCloudExecLongTimeoutStreams(t *testing.T) {
	stub, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 200, raw: ndjson(map[string]any{"stream": "stdout", "data": b64("ok")}, map[string]any{"done": true, "exit": 0})}
	})
	b := cloudBox(t, c)
	r, err := b.Exec(context.Background(), []string{"sleep", "100"}, &CloudExecOptions{TimeoutS: 600})
	if err != nil || !r.OK() || r.Stdout != "ok" {
		t.Fatalf("%+v %v", r, err)
	}
	if body := stub.last().JSON(t); body["stream"] != true || body["timeout_s"] != float64(600) {
		t.Fatalf("body %v", body)
	}
	if _, err := b.Exec(context.Background(), []string{"true"}, &CloudExecOptions{TimeoutS: CloudMaxTimeoutS + 1}); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

func TestCloudExecStreamTimeoutFrame(t *testing.T) {
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 200, raw: ndjson(
			map[string]any{"stream": "stdout", "data": b64("partial")},
			map[string]any{"done": true, "error": "the command ran past timeout_s (5 s)", "code": "exec_timeout"},
		)}
	})
	_, err := cloudBox(t, c).ExecStream(context.Background(), []string{"sleep", "9"}, &CloudExecOptions{TimeoutS: 5}, nil)
	e, ok := AsError(err)
	if !ok || !errors.Is(err, ErrExecTimeout) || !errors.Is(err, ErrTimeout) || e.Stdout != "partial" {
		t.Fatalf("%v", err)
	}
}

func TestCloudExecStreamErrorAndTruncation(t *testing.T) {
	answers := [][]byte{
		ndjson(map[string]any{"stream": "stderr", "data": b64("boom")}, map[string]any{"done": true, "error": "host went away", "failure_class": "infra", "retryable": true}),
		ndjson(map[string]any{"stream": "stdout", "data": b64("half")}), // no final frame
	}
	i := 0
	_, c := newRawStub(t, func(r rawReq) rawAnswer { a := answers[i]; i++; return rawAnswer{status: 200, raw: a} })
	b := cloudBox(t, c)
	_, err := b.ExecStream(context.Background(), []string{"x"}, nil, nil)
	if e, ok := AsError(err); !ok || !errors.Is(err, ErrStream) || e.Stderr != "boom" || !e.Retryable || e.FailureClass != FailureInfra {
		t.Fatalf("%v", err)
	}
	_, err = b.ExecStream(context.Background(), []string{"x"}, nil, nil)
	if e, ok := AsError(err); !ok || !errors.Is(err, ErrStream) || e.Stdout != "half" || e.Code != "stream_broken" {
		t.Fatalf("%v", err)
	}
}

func TestCloudExecStreamCallbackStops(t *testing.T) {
	_, c := newRawStub(t, func(r rawReq) rawAnswer {
		return rawAnswer{status: 200, raw: ndjson(map[string]any{"stream": "stdout", "data": b64("a")}, map[string]any{"done": true, "exit": 0})}
	})
	stop := errors.New("enough")
	_, err := cloudBox(t, c).ExecStream(context.Background(), []string{"x"}, nil, func(string, []byte) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestCloudExecStreamStalls(t *testing.T) {
	old := CloudStreamIdle
	CloudStreamIdle = 100 * time.Millisecond
	defer func() { CloudStreamIdle = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(ndjson(map[string]any{"stream": "stdout", "data": b64("a")}))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cloudBox(t, c).ExecStream(context.Background(), []string{"x"}, nil, nil)
	if e, ok := AsError(err); !ok || e.Code != "stream_stalled" || e.Stdout != "a" {
		t.Fatalf("%v", err)
	}
}

func TestCloudExecStreamHTTPError(t *testing.T) {
	c := errCloud(t, 501, map[string]any{"error": "not_available", "message": "no data plane"}, nil)
	_, err := cloudBox(t, c).ExecStream(context.Background(), []string{"x"}, nil, nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := cloudBox(t, c).ExecStream(context.Background(), nil, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}
