package kosa8

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// keyScript is a fake transport that gives its answers in turn (a nil
// answer is no answer: a transport error) and keeps every Idempotency-Key.
type keyScript struct {
	mu      sync.Mutex
	answers []*http.Response
	keys    []string
}

type droppedConn struct{}

func (droppedConn) Error() string { return "connection reset by peer" }

func (s *keyScript) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
	a := s.answers[0]
	s.answers = s.answers[1:]
	if a == nil {
		return nil, droppedConn{}
	}
	a.Request = r
	return a, nil
}

func jsonAnswer(status int, body string, headers ...string) *http.Response {
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

const (
	hostFailed  = `{"error":"host_failed","message":"boot failed","failure_class":"infra","retryable":true}`
	created     = `{"sandbox":{"id":"sbx_1","state":"running"}}`
	restarting  = `{"error":"host_restarting","message":"restarting","retryable":true}`
	rateLimited = `{"error":"rate_limited","message":"slow down"}`
	inProgress  = `{"error":"idempotency_in_progress","message":"still running","retry_after_s":1}`
)

func scriptedCloud(t *testing.T, answers ...*http.Response) (*keyScript, *Cloud) {
	t.Helper()
	s := &keyScript{answers: answers}
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: "http://127.0.0.1:9", Getenv: func(string) string { return "" },
		HTTPClient: &http.Client{Transport: s}})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestIdempotencyKeyRenewedAfter5xx(t *testing.T) {
	resetSlept()
	s, c := scriptedCloud(t, jsonAnswer(502, hostFailed), jsonAnswer(201, created))
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || len(s.keys[0]) != 36 || len(s.keys[1]) != 36 || s.keys[0] == s.keys[1] {
		t.Fatalf("keys %q", s.keys)
	}
	// A 503 too, after its Retry-After.
	resetSlept()
	s, c = scriptedCloud(t, jsonAnswer(503, restarting, "Retry-After", "2"), jsonAnswer(201, created))
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || s.keys[0] == s.keys[1] {
		t.Fatalf("keys %q", s.keys)
	}
	if w := sleeps(); len(w) != 1 || w[0].Seconds() < 2 {
		t.Fatalf("waits %v", w)
	}
}

func TestIdempotencyKeyKeptWithoutAnAnswer(t *testing.T) {
	resetSlept()
	s, c := scriptedCloud(t, nil, jsonAnswer(201, created))
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || s.keys[0] == "" || s.keys[0] != s.keys[1] {
		t.Fatalf("keys %q", s.keys)
	}
}

func TestIdempotencyKeyKeptOn429And409(t *testing.T) {
	resetSlept()
	s, c := scriptedCloud(t, jsonAnswer(429, rateLimited, "Retry-After", "1"), jsonAnswer(409, inProgress), jsonAnswer(201, created))
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 3 || s.keys[0] == "" || s.keys[0] != s.keys[1] || s.keys[1] != s.keys[2] {
		t.Fatalf("keys %q", s.keys)
	}
}

func TestCallersIdempotencyKeyNeverChanged(t *testing.T) {
	resetSlept()
	s, c := scriptedCloud(t, jsonAnswer(502, hostFailed), jsonAnswer(201, created))
	if _, err := c.Sandboxes.Create(context.Background(), CloudCreateOptions{IdempotencyKey: "caller-key"}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || s.keys[0] != "caller-key" || s.keys[1] != "caller-key" {
		t.Fatalf("keys %q", s.keys)
	}
	// A job submit's own key is renewed too; the caller's is kept.
	resetSlept()
	s, c = scriptedCloud(t, jsonAnswer(502, hostFailed), jsonAnswer(202, `{"job_id":"job_1"}`))
	if _, err := c.Jobs.Submit(context.Background(), CloudJobOptions{Name: "j", Template: "python", Tasks: []CloudJobTask{{ID: "t1"}}, Solver: CloudJobCmd{Cmd: []string{"true"}, TimeoutS: 60}, Grader: CloudJobCmd{Cmd: []string{"true"}}}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || s.keys[0] == "" || s.keys[0] == s.keys[1] {
		t.Fatalf("job keys %q", s.keys)
	}
}

func TestRenewsIdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&Error{Kind: KindAPI, Status: 502, Code: "host_failed", cloud: true}, true},
		{&Error{Kind: KindAPI, Status: 503, Code: "no_capacity", cloud: true}, true},
		{&Error{Kind: KindAPI, Status: 504, cloud: true}, true},
		{&Error{Kind: KindAPI, Status: 429, Code: "rate_limited", cloud: true}, false},
		{&Error{Kind: KindAPI, Status: 409, Code: "idempotency_in_progress", cloud: true}, false},
		{&Error{Kind: KindTimeout}, false},
		{&Error{Kind: KindConnection}, false},
	} {
		if got := renewsIdempotencyKey(tc.err); got != tc.want {
			t.Errorf("%v: %v", tc.err, got)
		}
	}
}
