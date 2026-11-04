package kosa8

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorker(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSavedWorker(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	saved := writeWorker(t, `{"host": "http://127.0.0.1:7428", "token": "saved-tok", "via": "wsl", "distro": "Ubuntu", "insecure": false, "saved": "2026-11-03T00:00:00Z"}`)
	lan := `{"host": "http://192.168.1.20:7428", "token": "lan-tok"}`
	cases := []struct {
		name   string
		o      Options
		want   string
		token  string
		errHas string
	}{
		{"saved used when KOSA8_HOST unset", Options{WorkerFile: saved, Getenv: env(nil)}, "http://127.0.0.1:7428", "saved-tok", ""},
		{"saved token over KOSA8_TOKEN", Options{WorkerFile: saved, Getenv: env(map[string]string{"KOSA8_TOKEN": "env-tok"})}, "http://127.0.0.1:7428", "saved-tok", ""},
		{"KOSA8_HOST wins, saved token unused", Options{WorkerFile: saved, Getenv: env(map[string]string{"KOSA8_HOST": "https://w.example.com"})}, "https://w.example.com", "", ""},
		{"KOSA8_HOST with its own token", Options{WorkerFile: saved, Getenv: env(map[string]string{"KOSA8_HOST": "https://w.example.com", "KOSA8_TOKEN": "env-tok"})}, "https://w.example.com", "env-tok", ""},
		{"explicit host wins, saved token unused", Options{Host: "https://h.example.com", WorkerFile: saved, Getenv: env(nil)}, "https://h.example.com", "", ""},
		{"explicit socket skips the file", Options{SocketPath: "/s.sock", WorkerFile: saved, Getenv: env(nil)}, "unix:///s.sock", "", ""},
		{"NoWorkerFile skips the file", Options{NoWorkerFile: true, WorkerFile: saved, Getenv: env(nil)}, "unix://" + DefaultSocketPath(), "", ""},
		{"malformed file", Options{WorkerFile: writeWorker(t, `{"host": `), Getenv: env(nil)}, "unix://" + DefaultSocketPath(), "", ""},
		{"blank host", Options{WorkerFile: writeWorker(t, `{"host": "  ", "token": "x"}`), Getenv: env(nil)}, "unix://" + DefaultSocketPath(), "", ""},
		{"missing file", Options{WorkerFile: filepath.Join(t.TempDir(), "nope.json"), Getenv: env(nil)}, "unix://" + DefaultSocketPath(), "", ""},
		{"saved insecure allows LAN http", Options{WorkerFile: writeWorker(t, `{"host": "http://192.168.1.20:7428", "token": "lan-tok", "insecure": true}`), Getenv: env(nil)}, "http://192.168.1.20:7428", "lan-tok", ""},
		{"LAN http refused without insecure", Options{WorkerFile: writeWorker(t, lan), Getenv: env(nil)}, "", "", "plain http"},
		{"Insecure option allows LAN http", Options{Insecure: true, WorkerFile: writeWorker(t, lan), Getenv: env(nil)}, "http://192.168.1.20:7428", "lan-tok", ""},
		{"KOSA8_INSECURE allows LAN http", Options{WorkerFile: writeWorker(t, lan), Getenv: env(map[string]string{"KOSA8_INSECURE": "1"})}, "http://192.168.1.20:7428", "lan-tok", ""},
		{"explicit token overrides saved", Options{Token: "mine", WorkerFile: saved, Getenv: env(nil)}, "http://127.0.0.1:7428", "mine", ""},
		{"bare host:port spelling", Options{WorkerFile: writeWorker(t, `{"host": "localhost:7428/"}`), Getenv: env(nil)}, "http://localhost:7428", "", ""},
		{"tcp spelling", Options{WorkerFile: writeWorker(t, `{"host": "tcp://127.0.0.1:7428"}`), Getenv: env(nil)}, "http://127.0.0.1:7428", "", ""},
		{"unix spelling", Options{WorkerFile: writeWorker(t, `{"host": "unix:///w.sock"}`), Getenv: env(nil)}, "unix:///w.sock", "", ""},
	}
	for _, tc := range cases {
		ep, err := Resolve(tc.o)
		if tc.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("%s: want error %q, got %v", tc.name, tc.errHas, err)
			}
			continue
		}
		if err != nil || ep.Target() != tc.want || ep.Token != tc.token {
			t.Errorf("%s: got %s token %q err %v", tc.name, ep.Target(), ep.Token, err)
		}
	}
}

func TestSavedWorkerClientTarget(t *testing.T) {
	saved := writeWorker(t, `{"host": "http://127.0.0.1:7428/", "token": "t"}`)
	c, err := New(Options{WorkerFile: saved, Checkpoints: &MemoryCheckpointStore{}, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Endpoint().Target(); got != "http://127.0.0.1:7428" {
		t.Fatalf("target %s", got)
	}
}

func TestWindowsHint(t *testing.T) {
	old := goos
	t.Cleanup(func() { goos = old })
	nope := filepath.Join(t.TempDir(), "nope.sock")
	missing := filepath.Join(t.TempDir(), "worker.json")
	for _, tc := range []struct {
		os, sock string
		want     bool
	}{
		{"windows", "", true},    // default socket on Windows: hint
		{"windows", nope, false}, // an explicit socket is the caller's choice
		{"linux", "", false},     // not Windows
	} {
		goos = tc.os
		// Every dial goes to a socket that cannot exist, so the default-socket
		// case never reaches a real daemon on this machine.
		hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", nope)
		}}}
		c, err := New(Options{SocketPath: tc.sock, WorkerFile: missing, HTTPClient: hc, Checkpoints: &MemoryCheckpointStore{}, Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Health(ctx)
		if !errors.Is(err, ErrConnection) {
			t.Fatalf("%v: want connection error, got %v", tc, err)
		}
		if got := strings.Contains(err.Error(), "kosa8 wsl setup"); got != tc.want {
			t.Errorf("%v: hint %v in %v", tc, got, err)
		}
	}
}
