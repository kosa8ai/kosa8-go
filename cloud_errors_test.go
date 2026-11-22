package kosa8

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// errCloud is a Cloud whose every request is answered with status, body and
// headers, tried once.
func errCloud(t *testing.T, status int, body map[string]any, headers map[string]string) *Cloud {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Retry: &NoRetry, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cloudErr(t *testing.T, c *Cloud) *Error {
	t.Helper()
	_, err := c.Me(context.Background())
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("not a *Error: %v", err)
	}
	return e
}

func TestCloudErrorNoCapacity(t *testing.T) {
	c := errCloud(t, 503, map[string]any{
		"error": "no_capacity", "message": "no capacity: fork fewer or retry in 10 s",
		"retryable": true, "retry_after_s": 10, "failure_class": "infra",
	}, map[string]string{"Retry-After": "10", "Cf-Ray": "a5e77ddee9ef8120-BOS"})
	e := cloudErr(t, c)
	if e.Message != "no capacity: fork fewer or retry in 10 s" || e.Code != "no_capacity" || e.Status != 503 {
		t.Fatalf("%+v", e)
	}
	if e.RetryAfter != 10*time.Second || !e.Retryable || e.FailureClass != FailureInfra || e.RequestID != "a5e77ddee9ef8120-BOS" {
		t.Fatalf("%+v", e)
	}
	want := "no capacity: fork fewer or retry in 10 s [HTTP 503 no_capacity; retry in 10 s; request a5e77ddee9ef8120-BOS]"
	if e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestCloudErrorRequestIDPrefersHeader(t *testing.T) {
	c := errCloud(t, 404, map[string]any{"error": "not_found", "message": "no sandbox sbx_1"},
		map[string]string{"X-Request-Id": "req_123", "Cf-Ray": "ray-1"})
	e := cloudErr(t, c)
	if e.RequestID != "req_123" || e.Error() != "no sandbox sbx_1 [HTTP 404 not_found; request req_123]" {
		t.Fatalf("%q %+v", e.Error(), e)
	}
}

func TestCloudErrorTemplateBuildingBodyRetryAfter(t *testing.T) {
	c := errCloud(t, 409, map[string]any{
		"error": "template_building", "message": "the environment of swe/x is being built; ready in about 120 s",
		"eta_s": 120, "retry_after_s": 30, "failure_class": "quota", "retryable": true,
	}, nil)
	e := cloudErr(t, c)
	if e.Kind != KindConflict || e.Code != "template_building" || e.RetryAfter != 30*time.Second || !e.Retryable {
		t.Fatalf("%+v", e)
	}
	if b, _ := e.Body.(map[string]any); b["eta_s"] != float64(120) {
		t.Fatalf("eta_s not kept: %v", e.Body)
	}
	if !strings.HasSuffix(e.Error(), "[HTTP 409 template_building; retry in 30 s]") {
		t.Fatalf("%q", e.Error())
	}
}

func TestCloudErrorPaymentUpgradeURL(t *testing.T) {
	c := errCloud(t, 402, map[string]any{
		"error": "payment_required", "message": "add a payment method to start sandboxes",
		"upgrade_url": "https://app.kosa8.com/app#billing", "agent_message": "Tell the person to add a card.",
	}, nil)
	e := cloudErr(t, c)
	if !errors.Is(e, ErrPaymentRequired) || e.UpgradeURL != "https://app.kosa8.com/app#billing" || e.AgentMessage == "" || e.Retryable {
		t.Fatalf("%+v", e)
	}
	want := "add a payment method to start sandboxes [HTTP 402 payment_required; upgrade: https://app.kosa8.com/app#billing]"
	if e.Error() != want {
		t.Fatalf("Error() = %q", e.Error())
	}
	// Not repeated when the sentence already has it.
	c = errCloud(t, 402, map[string]any{"error": "plan_required", "message": "pick a plan at https://x.test/p", "upgrade_url": "https://x.test/p"}, nil)
	if got := cloudErr(t, c).Error(); strings.Contains(got, "upgrade:") {
		t.Fatalf("%q", got)
	}
}

func TestCloudErrorUnauthorizedHint(t *testing.T) {
	c := errCloud(t, 401, map[string]any{"error": "unauthorized", "message": "API key is unknown, revoked or expired"}, nil)
	e := cloudErr(t, c)
	got := e.Error()
	if !errors.Is(e, ErrAuth) || e.Message != "API key is unknown, revoked or expired" ||
		!strings.Contains(got, "KOSA8_API_KEY") || !strings.Contains(got, "https://app.kosa8.com/app#keys") ||
		!strings.HasSuffix(got, "[HTTP 401 unauthorized]") {
		t.Fatalf("%q", got)
	}
}

func TestCloudErrorNotAvailableNotRetryable(t *testing.T) {
	c := errCloud(t, 501, map[string]any{"error": "not_available", "message": "background commands are not available on this sandbox's host yet"}, nil)
	e := cloudErr(t, c)
	if !errors.Is(e, ErrUnsupported) || e.Retryable || e.RetryAfter != 0 || e.FailureClass != FailureInfra {
		t.Fatalf("%+v", e)
	}
	if !strings.HasSuffix(e.Error(), "[HTTP 501 not_available]") {
		t.Fatalf("%q", e.Error())
	}
}

func TestCloudClientErrorHints(t *testing.T) {
	_, err := NewCloud(CloudOptions{Getenv: func(string) string { return "" }})
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "set KOSA8_API_KEY") || !strings.Contains(err.Error(), "https://app.kosa8.com/app#keys") {
		t.Fatalf("missing key: %v", err)
	}
	_, err = NewCloud(CloudOptions{APIKey: "sk-other", Getenv: func(string) string { return "" }})
	if !strings.Contains(err.Error(), "https://app.kosa8.com/app#keys") {
		t.Fatalf("bad prefix: %v", err)
	}
	c, _ := NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: "http://127.0.0.1:1", Retry: &NoRetry})
	_, err = c.Me(context.Background())
	if !errors.Is(err, ErrConnection) || !strings.Contains(err.Error(), "KOSA8_CLOUD_URL") {
		t.Fatalf("unreachable: %v", err)
	}
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	c, _ = NewCloud(CloudOptions{APIKey: testCloudKey, BaseURL: srv.URL, Timeout: 50 * time.Millisecond, Retry: &NoRetry})
	_, err = c.Me(context.Background())
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "CloudOptions.Timeout") {
		t.Fatalf("timeout: %v", err)
	}
	// A daemon error keeps its plain message.
	if d := errorFromResponse(404, map[string]any{"error": "no such sandbox"}, "GET", "/x"); d.Error() != "no such sandbox" {
		t.Fatalf("daemon: %q", d.Error())
	}
}
