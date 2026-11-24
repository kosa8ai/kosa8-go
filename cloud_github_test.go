package kosa8

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCloudGitHubCalls(t *testing.T) {
	s, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case r.Path == "/v1/github":
			return 200, map[string]any{"configured": true, "connected": true, "can_manage": false, "install_url": nil,
				"installations": []any{map[string]any{"installation_id": 7, "account_login": "acme", "missing_permissions": []any{"pull_requests:write"}}}}
		case strings.HasPrefix(r.Path, "/v1/github/repos?"):
			return 200, map[string]any{"repos": []any{map[string]any{"full_name": "acme/api", "private": true, "default_branch": "main", "installation_id": 7}}, "next_page": 2}
		default:
			return 200, map[string]any{"default_branch": "main", "branches": []any{map[string]any{"name": "main", "sha": "abc", "protected": true}}}
		}
	})
	ctx := context.Background()
	st, err := c.GitHub.Status(ctx)
	if err != nil || !st.Connected || st.InstallURL != nil || st.Installations[0].MissingPermissions[0] != "pull_requests:write" {
		t.Fatalf("%+v %v", st, err)
	}
	repos, err := c.GitHub.Repos(ctx, "api")
	if err != nil || repos[0].FullName != "acme/api" || !repos[0].Private {
		t.Fatalf("%+v %v", repos, err)
	}
	p, err := c.GitHub.ReposPage(ctx, "x", 2)
	if err != nil || p.NextPage != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	br, err := c.GitHub.Branches(ctx, "acme/api")
	if err != nil || br.DefaultBranch != "main" || !br.Branches[0].Protected {
		t.Fatalf("%+v %v", br, err)
	}
	var got []string
	for _, r := range s.seen {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{"GET /v1/github", "GET /v1/github/repos?q=api", "GET /v1/github/repos?page=2&q=x", "GET /v1/github/repos/acme/api/branches"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestCloudRepoChecks(t *testing.T) {
	s, c := newCloudStub(t, func(seenReq) (int, any) { return 200, map[string]any{} })
	ctx := context.Background()
	if _, err := c.GitHub.Branches(ctx, "acme"); !errors.Is(err, ErrUsage) {
		t.Fatalf("branches: %v", err)
	}
	for _, r := range []*CloudRepo{Repo("not a repo"), Repo("acme/api@"), {Name: "acme/api", Depth: -1}} {
		if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Repo: r}); !errors.Is(err, ErrUsage) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	if len(s.seen) != 0 {
		t.Fatalf("sent %v", s.seen)
	}
}

func TestCloudCreateWithRepo(t *testing.T) {
	repo := map[string]any{"full_name": "acme/api", "ref": "main", "sha": "abc", "path": "/workspace/api", "status": "cloned"}
	s, c := newCloudStub(t, func(seenReq) (int, any) {
		sb := sbxJSON("running")
		sb["repo"] = repo
		return 201, map[string]any{"sandbox": sb}
	})
	ctx := context.Background()
	b, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Repo: Repo("acme/api@main")})
	if err != nil || b.Info.Repo == nil || b.Info.Repo.Status != "cloned" || *b.Info.Repo.SHA != "abc" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Repo: &CloudRepo{Name: "acme/api", Ref: "v1.2", Path: "/workspace/x", Depth: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sandboxes.Create(ctx, CloudCreateOptions{Repo: Repo("acme/api")}); err != nil {
		t.Fatal(err)
	}
	if s.seen[0].Body["repo"] != "acme/api@main" || s.seen[2].Body["repo"] != "acme/api" {
		t.Fatalf("string forms: %v / %v", s.seen[0].Body["repo"], s.seen[2].Body["repo"])
	}
	want := map[string]any{"name": "acme/api", "ref": "v1.2", "path": "/workspace/x", "depth": float64(5)}
	if !reflect.DeepEqual(s.seen[1].Body["repo"], want) {
		t.Fatalf("object form: %v", s.seen[1].Body["repo"])
	}
}

func TestCloudSandboxRepoPushPR(t *testing.T) {
	s, c := newCloudStub(t, func(r seenReq) (int, any) {
		switch {
		case strings.HasSuffix(r.Path, "/repo"):
			return 200, map[string]any{"repo": map[string]any{"full_name": "acme/api", "path": "/workspace/api", "status": "cloned", "changes": map[string]any{"files_changed": 2, "insertions": 5, "deletions": 1}}}
		case strings.HasSuffix(r.Path, "/github/push"):
			return 200, map[string]any{"branch": "kosa8/abc", "sha": "def", "compare_url": "https://github.com/acme/api/compare/kosa8/abc", "files_changed": 2}
		case strings.HasSuffix(r.Path, "/github/pull-request"):
			return 200, map[string]any{"number": 12, "url": "https://github.com/acme/api/pull/12", "branch": "fix", "sha": "def"}
		}
		return 200, map[string]any{"sandbox": sbxJSON("running")}
	})
	ctx := context.Background()
	b, err := c.Sandboxes.Get(ctx, "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	rp, err := b.Repo(ctx)
	if err != nil || rp.Changes == nil || rp.Changes.FilesChanged != 2 || b.Info.Repo.FullName != "acme/api" {
		t.Fatalf("%+v %v", rp, err)
	}
	if p, err := b.Push(ctx, nil); err != nil || p.Branch != "kosa8/abc" || p.FilesChanged != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := b.Push(ctx, &CloudPushOptions{Branch: "fix", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	pr, err := b.PullRequest(ctx, &CloudPullRequestOptions{Title: "Fix", Base: "main", Draft: Ptr(false)})
	if err != nil || pr.Number != 12 {
		t.Fatalf("%+v %v", pr, err)
	}
	type call struct {
		Line string
		Body map[string]any
	}
	var got []call
	for _, r := range s.seen[1:] {
		got = append(got, call{r.Method + " " + r.Path, r.Body})
	}
	want := []call{
		{"GET /v1/sandboxes/sbx_1/repo", nil},
		{"POST /v1/sandboxes/sbx_1/github/push", map[string]any{}},
		{"POST /v1/sandboxes/sbx_1/github/push", map[string]any{"branch": "fix", "message": "m"}},
		{"POST /v1/sandboxes/sbx_1/github/pull-request", map[string]any{"title": "Fix", "base": "main", "draft": false}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestCloudGitHubErrors(t *testing.T) {
	prErr := func(status int, body map[string]any) error {
		_, c := newCloudStub(t, func(r seenReq) (int, any) {
			if r.Method == "GET" {
				return 200, map[string]any{"sandbox": sbxJSON("running")}
			}
			return status, body
		})
		c.retry = NoRetry
		b, err := c.Sandboxes.Get(context.Background(), "sbx_1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.PullRequest(context.Background(), nil)
		return err
	}
	err := prErr(409, map[string]any{"error": "github_not_connected", "message": "Connect GitHub first.", "connect_url": "https://app.kosa8.com/app#settings/github"})
	e, _ := AsError(err)
	if !errors.Is(err, ErrGitHubNotConnected) || !errors.Is(err, ErrConflict) || errors.Is(err, ErrNoChanges) || e.ConnectURL != "https://app.kosa8.com/app#settings/github" {
		t.Fatalf("%v %+v", err, e)
	}
	err = prErr(409, map[string]any{"error": "github_permissions", "message": "Approve.", "missing": []any{"pull_requests:write"}, "settings_url": "https://github.com/settings/installations/7"})
	e, _ = AsError(err)
	if !errors.Is(err, ErrGitHubPermissions) || !reflect.DeepEqual(e.MissingPermissions, []string{"pull_requests:write"}) || e.SettingsURL != "https://github.com/settings/installations/7" {
		t.Fatalf("%v %+v", err, e)
	}
	if err := prErr(409, map[string]any{"error": "no_changes", "message": "Nothing changed."}); !errors.Is(err, ErrNoChanges) {
		t.Fatal(err)
	}
	if err := prErr(404, map[string]any{"error": "repo_not_found", "message": "No such repo."}); !errors.Is(err, ErrRepoNotFound) || !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	err = prErr(502, map[string]any{"error": "github_unavailable", "message": "GitHub is busy.", "retry_after_s": 30})
	e, _ = AsError(err)
	if !errors.Is(err, ErrGitHubUnavailable) || !errors.Is(err, ErrAPI) || e.RetryAfter != 30*time.Second {
		t.Fatalf("%v %+v", err, e)
	}
}
