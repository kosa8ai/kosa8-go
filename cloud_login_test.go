package kosa8

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homeWith(t *testing.T, creds string) string {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, ".kosa8"), 0o700); err != nil {
		t.Fatal(err)
	}
	if creds != "" {
		if err := os.WriteFile(filepath.Join(d, ".kosa8", "cloud.json"), []byte(creds), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestNewCloudUsesKosa8Login(t *testing.T) {
	home := homeWith(t, `{"key":"k8c_live_fromlogin","api_url":"http://localhost:8787","org_id":"org_1"}`)
	c, err := NewCloud(CloudOptions{Getenv: envOf(map[string]string{"HOME": home})})
	if err != nil {
		t.Fatal(err)
	}
	if c.key != "k8c_live_fromlogin" || c.baseURL != "http://localhost:8787" {
		t.Fatalf("got key %q base %q", c.key, c.baseURL)
	}
	c, err = NewCloud(CloudOptions{Getenv: envOf(map[string]string{"HOME": home, "KOSA8_API_KEY": "k8c_live_env"})})
	if err != nil || c.key != "k8c_live_env" || c.baseURL != DefaultCloudURL {
		t.Fatalf("env key should win and skip the saved url: %v %q %q", err, c.key, c.baseURL)
	}
}

func TestNewCloudNoLoginNamesKosa8Login(t *testing.T) {
	for _, creds := range []string{"", "not json", `{"org_id":"x"}`} {
		_, err := NewCloud(CloudOptions{Getenv: envOf(map[string]string{"HOME": homeWith(t, creds)})})
		if err == nil || !strings.Contains(err.Error(), "kosa8 login") {
			t.Fatalf("creds %q: %v", creds, err)
		}
	}
}

func TestStarterLimit429IsPaymentRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"starter_limit","message":"Starter runs 2 sandboxes at once"}`))
	}))
	defer srv.Close()
	c, err := NewCloud(CloudOptions{APIKey: "k8c_live_x", BaseURL: srv.URL, Getenv: envOf(nil), Retry: &NoRetry})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sandboxes.Create(t.Context(), CloudCreateOptions{})
	if !errors.Is(err, ErrPaymentRequired) || !errors.Is(err, ErrRateLimit) {
		t.Fatalf("want payment required (and still rate limit): %v", err)
	}
}
