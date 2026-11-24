package kosa8

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// GitHub repos: the org connects the kosa8 GitHub App once (an admin, in
// app.kosa8.com Settings or with `kosa8 github connect`), then a sandbox
// starts from a repo (CloudCreateOptions.Repo) and sends changes back
// (CloudSandbox.Push, CloudSandbox.PullRequest). kosa8 mints a GitHub token
// for one repo and one operation that expires within an hour; it never
// enters the sandbox, is never returned, and every use is audited.

// repoNameRe is owner/name as GitHub allows them.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// CloudRepo is a repo to start a sandbox from. Name is owner/name; Ref a
// branch, tag or commit sha (default the repo's default branch); Path where
// to clone it (default /workspace/<name>); Depth the clone depth (default 1).
// Repo("owner/name@ref") builds one from the short form.
type CloudRepo struct {
	Name  string
	Ref   string
	Path  string
	Depth int
}

// Repo parses "owner/name" or "owner/name@ref" into a CloudRepo.
func Repo(spec string) *CloudRepo {
	r := &CloudRepo{Name: spec}
	if i := strings.IndexByte(spec, '@'); i >= 0 {
		r.Name, r.Ref = spec[:i], spec[i+1:]
		if r.Ref == "" {
			r.Ref = "@" // caught by body(): an empty ref after @
		}
	}
	return r
}

func checkRepoName(name string) error {
	if !repoNameRe.MatchString(name) {
		return usage("repo must be owner/name (optionally @ref), not %q", name)
	}
	return nil
}

// body is the API's `repo` field: "owner/name[@ref]" when only a name and
// ref are set, else {name, ref?, path?, depth?}.
func (r *CloudRepo) body() (any, error) {
	if err := checkRepoName(r.Name); err != nil {
		return nil, err
	}
	if r.Ref == "@" {
		return nil, usage("repo %s@ has an empty ref after @", r.Name)
	}
	if r.Depth < 0 {
		return nil, usage("repo Depth must be 1 or more, not %d", r.Depth)
	}
	if r.Path == "" && r.Depth == 0 {
		if r.Ref == "" {
			return r.Name, nil
		}
		return r.Name + "@" + r.Ref, nil
	}
	m := map[string]any{"name": r.Name}
	if r.Ref != "" {
		m["ref"] = r.Ref
	}
	if r.Path != "" {
		m["path"] = r.Path
	}
	if r.Depth > 0 {
		m["depth"] = r.Depth
	}
	return m, nil
}

// CloudSandboxRepo is the repo a sandbox was started from.
type CloudSandboxRepo struct {
	FullName string  `json:"full_name"`
	Ref      *string `json:"ref"`
	SHA      *string `json:"sha"`
	Path     string  `json:"path"`
	// Status is cloned, pending (a cold boot: it clones once the sandbox is
	// ready; Repo asks again) or failed (Error says why).
	Status string  `json:"status"`
	Error  *string `json:"error"`
	// Changes is set by CloudSandbox.Repo: what changed since the clone.
	Changes *CloudRepoChanges `json:"changes"`
}

// CloudRepoChanges summarises git status in the sandbox's checkout.
type CloudRepoChanges struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// CloudGitHubInstallation is one installation of the kosa8 GitHub App
// linked to the org.
type CloudGitHubInstallation struct {
	InstallationID      int64  `json:"installation_id"`
	AccountLogin        string `json:"account_login"`
	AccountType         string `json:"account_type"`         // User | Organization
	RepositorySelection string `json:"repository_selection"` // all | selected
	RepoCount           int    `json:"repo_count"`
	// ManageURL is where on GitHub to pick repos or approve new permissions.
	ManageURL string `json:"manage_url"`
	// MissingPermissions are the permissions the installation lacks
	// (contents:write, pull_requests:write, metadata:read); empty when none.
	MissingPermissions []string `json:"missing_permissions"`
}

// CloudGitHubStatus says whether GitHub is connected.
type CloudGitHubStatus struct {
	Configured bool `json:"configured"`
	Connected  bool `json:"connected"`
	// CanManage: this key may connect or disconnect GitHub (an admin's).
	CanManage bool `json:"can_manage"`
	// InstallURL is where to install the kosa8 GitHub App; nil unless CanManage.
	InstallURL    *string                   `json:"install_url"`
	Installations []CloudGitHubInstallation `json:"installations"`
}

// CloudGitHubRepo is a repo the org's installations can see.
type CloudGitHubRepo struct {
	FullName       string  `json:"full_name"`
	Private        bool    `json:"private"`
	DefaultBranch  string  `json:"default_branch"`
	Description    *string `json:"description"`
	PushedAt       *string `json:"pushed_at"`
	HTMLURL        string  `json:"html_url"`
	InstallationID int64   `json:"installation_id"`
	Language       *string `json:"language"`
}

// CloudGitHubRepoPage is one page of repos; NextPage is 0 on the last.
type CloudGitHubRepoPage struct {
	Repos    []CloudGitHubRepo
	NextPage int
}

// CloudGitHubBranch is one branch of a repo.
type CloudGitHubBranch struct {
	Name      string `json:"name"`
	SHA       string `json:"sha"`
	Protected bool   `json:"protected"`
}

// CloudGitHubBranches are a repo's first 100 branches and its default.
type CloudGitHubBranches struct {
	DefaultBranch string              `json:"default_branch"`
	Branches      []CloudGitHubBranch `json:"branches"`
}

// CloudGitHub is the org's connected GitHub (Cloud.GitHub). Listing repos
// needs only the normal sandbox scope.
type CloudGitHub struct{ c *Cloud }

// Status says whether GitHub is connected, lists the installations and
// (for an admin) where to install the app.
func (g *CloudGitHub) Status(ctx context.Context) (CloudGitHubStatus, error) {
	var r CloudGitHubStatus
	err := g.c.request(ctx, "GET", "/v1/github", nil, &r)
	return r, err
}

// Repos lists the repos the org's installations can see, most recently
// pushed first; query ("" for all) filters by substring. First page only;
// ReposPage pages.
func (g *CloudGitHub) Repos(ctx context.Context, query string) ([]CloudGitHubRepo, error) {
	p, err := g.ReposPage(ctx, query, 0)
	return p.Repos, err
}

// ReposPage is one page of Repos (page 0 or 1 is the first).
func (g *CloudGitHub) ReposPage(ctx context.Context, query string, page int) (CloudGitHubRepoPage, error) {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	p := "/v1/github/repos"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var r struct {
		Repos    []CloudGitHubRepo `json:"repos"`
		NextPage *int              `json:"next_page"`
	}
	if err := g.c.request(ctx, "GET", p, nil, &r); err != nil {
		return CloudGitHubRepoPage{}, err
	}
	out := CloudGitHubRepoPage{Repos: r.Repos}
	if out.Repos == nil {
		out.Repos = []CloudGitHubRepo{}
	}
	if r.NextPage != nil {
		out.NextPage = *r.NextPage
	}
	return out, nil
}

// Branches lists a repo's (owner/name) first 100 branches.
func (g *CloudGitHub) Branches(ctx context.Context, repo string) (CloudGitHubBranches, error) {
	var r CloudGitHubBranches
	if err := checkRepoName(repo); err != nil {
		return r, err
	}
	owner, name, _ := strings.Cut(repo, "/")
	err := g.c.request(ctx, "GET", "/v1/github/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branches", nil, &r)
	return r, err
}

// CloudPushOptions are optional settings for CloudSandbox.Push.
type CloudPushOptions struct {
	Branch  string // default kosa8/<sandbox-short-id>
	Message string // default "Changes from kosa8 sandbox <id>"
	Path    string // the checkout; default the one it was started from
}

// CloudPushResult is what Push pushed.
type CloudPushResult struct {
	Branch       string `json:"branch"`
	SHA          string `json:"sha"`
	CompareURL   string `json:"compare_url"`
	FilesChanged int    `json:"files_changed"`
}

// CloudPullRequestOptions are optional settings for CloudSandbox.PullRequest.
type CloudPullRequestOptions struct {
	Title  string
	Body   string
	Base   string // the branch to merge into; default the repo's default branch
	Branch string // the branch to push; default kosa8/<sandbox-short-id>
	// Draft: nil (default) opens a draft; Ptr(false) a ready one.
	Draft *bool
}

// CloudPullRequest is the pull request PullRequest opened.
type CloudPullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// Repo returns the repo the sandbox was started from, with Changes (files
// changed, insertions, deletions). A clone still pending is tried again.
func (b *CloudSandbox) Repo(ctx context.Context) (CloudSandboxRepo, error) {
	var r struct {
		Repo *CloudSandboxRepo `json:"repo"`
		CloudSandboxRepo
	}
	if err := b.c.request(ctx, "GET", b.path("/repo"), nil, &r); err != nil {
		return CloudSandboxRepo{}, err
	}
	out := r.CloudSandboxRepo
	if r.Repo != nil {
		out = *r.Repo
	}
	b.Info.Repo = &out
	return out, nil
}

// Push commits everything that changed in the sandbox's repo and pushes it
// to a branch. kosa8 pushes with a token for this one repo that never
// enters the sandbox. Nothing changed: ErrNoChanges. o may be nil.
func (b *CloudSandbox) Push(ctx context.Context, o *CloudPushOptions) (CloudPushResult, error) {
	body := map[string]any{}
	if o != nil {
		if o.Branch != "" {
			body["branch"] = o.Branch
		}
		if o.Message != "" {
			body["message"] = o.Message
		}
		if o.Path != "" {
			body["path"] = o.Path
		}
	}
	var r CloudPushResult
	err := b.c.request(ctx, "POST", b.path("/github/push"), body, &r)
	return r, err
}

// PullRequest pushes (as Push) and opens a pull request, a draft unless
// Draft is Ptr(false). o may be nil.
func (b *CloudSandbox) PullRequest(ctx context.Context, o *CloudPullRequestOptions) (CloudPullRequest, error) {
	body := map[string]any{}
	if o != nil {
		for k, v := range map[string]string{"title": o.Title, "body": o.Body, "base": o.Base, "branch": o.Branch} {
			if v != "" {
				body[k] = v
			}
		}
		if o.Draft != nil {
			body["draft"] = *o.Draft
		}
	}
	var r CloudPullRequest
	err := b.c.request(ctx, "POST", b.path("/github/pull-request"), body, &r)
	return r, err
}
