package kosa8

import (
	"context"
	"net/url"
	"regexp"
	"time"
)

// CloudOrgTemplate is one of the org's own templates: built once on a kosa8
// Cloud host from an image or a Dockerfile and kept as a snapshot, so a
// sandbox created from it (CloudCreateOptions.Template = its Name) is a
// restore of about a second instead of a cold boot that pulls the image.
type CloudOrgTemplate struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // queued | building | ready | failed
	// Source: image or dockerfile.
	Source           string   `json:"source"`
	Image            *string  `json:"image"`
	Digest           *string  `json:"digest"`
	CPUs             float64  `json:"cpus"`
	MemMB            int      `json:"mem_mb"`
	SetupCmd         any      `json:"setup_cmd"`
	Region           *string  `json:"region"`
	Phase            *string  `json:"phase"`
	SnapshotID       *string  `json:"snapshot_id"`
	BuilderSandboxID *string  `json:"builder_sandbox_id"`
	Bytes            int64    `json:"bytes"`
	ImportID         *string  `json:"import_id"`
	CreatedAt        int64    `json:"created_at"`
	StartedAt        *int64   `json:"started_at"`
	ReadyAt          *int64   `json:"ready_at"`
	LastError        *string  `json:"last_error"`
	Dockerfile       *string  `json:"dockerfile"` // Get only
	BuildArgs        []string `json:"build_args"` // Get only
	Log              string   `json:"log"`        // Get only: the build log's last 16 KiB
}

// Ready reports whether sandboxes can be made from it.
func (t CloudOrgTemplate) Ready() bool { return t.State == "ready" }

// CloudOrgTemplates builds, lists and deletes the org's own templates.
type CloudOrgTemplates struct{ c *Cloud }

var orgTemplateRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// CloudTemplateBuild is a template to build: Image or Dockerfile.
type CloudTemplateBuild struct {
	Name       string // lower-case, 1..63 of letters, digits, '.', '_', '-'
	Image      string // an OCI reference, by tag or @sha256 digest
	Dockerfile string // the Dockerfile's text; no build context (COPY/ADD only --from=)
	BuildArgs  map[string]string
	// SetupCmd runs once in the booted template before it is saved; a
	// single string runs under sh -c.
	SetupCmd []string
	Size     string // one of CloudSizes
	CPUs     int
	MemMB    int
	Region   string
}

func sizeFields(body map[string]any, size string, cpus, mem int) error {
	if size != "" {
		ok := false
		for _, s := range CloudSizes {
			ok = ok || s == size
		}
		if !ok {
			return usage("Size must be one of %v, not %q", CloudSizes, size)
		}
		body["size"] = size
	}
	if (cpus == 0) != (mem == 0) {
		return usage("set CPUs and MemMB together")
	}
	if cpus != 0 {
		body["cpus"] = cpus
		body["mem_mb"] = mem
	}
	return nil
}

func setupCmd(body map[string]any, cmd []string) {
	switch len(cmd) {
	case 0:
	case 1:
		body["setup_cmd"] = cmd[0]
	default:
		body["setup_cmd"] = cmd
	}
}

// Build queues a build (at most two of the org's build at once) and
// returns the template, queued or building; WaitReady waits for it. The
// build runs in a sandbox of the template's size and is billed as one; the
// snapshot counts toward the plan's snapshots.
func (t *CloudOrgTemplates) Build(ctx context.Context, b CloudTemplateBuild) (CloudOrgTemplate, error) {
	if !orgTemplateRE.MatchString(b.Name) {
		return CloudOrgTemplate{}, usage("template name %q: lower-case, 1..63 of letters, digits, '.', '_' or '-'", b.Name)
	}
	if (b.Image == "") == (b.Dockerfile == "") {
		return CloudOrgTemplate{}, usage("give Image or Dockerfile")
	}
	body := map[string]any{"name": b.Name}
	if b.Image != "" {
		body["image"] = b.Image
	}
	if b.Dockerfile != "" {
		if len(b.Dockerfile) > 64*1024 {
			return CloudOrgTemplate{}, usage("Dockerfile is over 64 KiB")
		}
		body["dockerfile"] = b.Dockerfile
	}
	if len(b.BuildArgs) > 0 {
		body["build_args"] = b.BuildArgs
	}
	setupCmd(body, b.SetupCmd)
	if err := sizeFields(body, b.Size, b.CPUs, b.MemMB); err != nil {
		return CloudOrgTemplate{}, err
	}
	if b.Region != "" {
		body["region"] = b.Region
	}
	var r struct {
		Template CloudOrgTemplate `json:"template"`
	}
	err := t.c.request(ctx, "POST", "/v1/templates", body, &r)
	return r.Template, err
}

// CloudTemplateImage is one image of an Import.
type CloudTemplateImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// CloudTemplateImportOptions apply to every image of an Import.
type CloudTemplateImportOptions struct {
	Size      string
	CPUs      int
	MemMB     int
	SetupCmd  []string
	Region    string
	PartialOK bool // queue what can be queued and list the rest in Failed
}

// CloudTemplateImport is what an Import queued.
type CloudTemplateImport struct {
	ImportID     string `json:"import_id"`
	Requested    int    `json:"requested"`
	Queued       int    `json:"queued"`
	BuildsAtOnce int    `json:"builds_at_once"`
	Templates    []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Image string `json:"image"`
	} `json:"templates"`
	Failed []struct {
		Index   int     `json:"index"`
		Name    *string `json:"name"`
		Code    string  `json:"code"`
		Message string  `json:"message"`
	} `json:"failed"`
}

// Import queues many image templates at once (up to 1000), such as RL task
// images by digest: each becomes one template and one snapshot.
func (t *CloudOrgTemplates) Import(ctx context.Context, images []CloudTemplateImage, o *CloudTemplateImportOptions) (CloudTemplateImport, error) {
	if len(images) == 0 || len(images) > 1000 {
		return CloudTemplateImport{}, usage("Import takes 1..1000 images, not %d", len(images))
	}
	for _, im := range images {
		if !orgTemplateRE.MatchString(im.Name) || im.Image == "" {
			return CloudTemplateImport{}, usage("image %q: a lower-case template name and an image", im.Name)
		}
	}
	body := map[string]any{"images": images}
	if o != nil {
		if err := sizeFields(body, o.Size, o.CPUs, o.MemMB); err != nil {
			return CloudTemplateImport{}, err
		}
		setupCmd(body, o.SetupCmd)
		if o.Region != "" {
			body["region"] = o.Region
		}
		if o.PartialOK {
			body["partial_ok"] = true
		}
	}
	var r CloudTemplateImport
	err := t.c.request(ctx, "POST", "/v1/templates/import", body, &r)
	return r, err
}

// List returns the org's own templates (Cloud.Templates lists kosa8's).
func (t *CloudOrgTemplates) List(ctx context.Context) ([]CloudOrgTemplate, error) {
	var r struct {
		OrgTemplates []CloudOrgTemplate `json:"org_templates"`
	}
	if err := t.c.request(ctx, "GET", "/v1/templates", nil, &r); err != nil {
		return nil, err
	}
	if r.OrgTemplates == nil {
		r.OrgTemplates = []CloudOrgTemplate{}
	}
	return r.OrgTemplates, nil
}

// Get returns one template by id (tpl_...) or name, with its build log.
func (t *CloudOrgTemplates) Get(ctx context.Context, ref string) (CloudOrgTemplate, error) {
	if ref == "" {
		return CloudOrgTemplate{}, usage("Get needs a template id or name")
	}
	var r struct {
		Template CloudOrgTemplate `json:"template"`
	}
	err := t.c.request(ctx, "GET", "/v1/templates/"+url.PathEscape(ref), nil, &r)
	return r.Template, err
}

// Delete deletes a template: a build in flight stops and its snapshot is
// deleted; sandboxes made from it are not touched.
func (t *CloudOrgTemplates) Delete(ctx context.Context, ref string) error {
	if ref == "" {
		return usage("Delete needs a template id or name")
	}
	return t.c.request(ctx, "DELETE", "/v1/templates/"+url.PathEscape(ref), nil, nil)
}

// WaitReady polls Get every interval (0: 2 s) until the template is ready;
// a failed build is an error with its last_error. Bound it with ctx.
func (t *CloudOrgTemplates) WaitReady(ctx context.Context, ref string, interval time.Duration) (CloudOrgTemplate, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		tpl, err := t.Get(ctx, ref)
		if err != nil {
			return tpl, err
		}
		switch tpl.State {
		case "ready":
			return tpl, nil
		case "failed":
			msg := "template " + tpl.Name + " failed to build"
			if tpl.LastError != nil {
				msg += ": " + *tpl.LastError
			}
			return tpl, &Error{Kind: KindAPI, Message: msg, Code: "template_failed", FailureClass: FailureClient}
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return tpl, &Error{Kind: KindTimeout, Message: "template " + ref + " is still " + tpl.State, Err: err}
		}
	}
}
