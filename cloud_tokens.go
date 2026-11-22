package kosa8

import (
	"context"
	"net/url"
)

// Capability token scopes.
const (
	ScopeSandboxesRead   = "sandboxes:read"
	ScopeSandboxesWrite  = "sandboxes:write"
	ScopeFunctionsInvoke = "functions:invoke"
)

// CloudToken is a short-lived capability token. Secret is set only in the
// answer to Create; pass it as a Cloud client's APIKey (it starts k8c_tok_).
type CloudToken struct {
	ID           string            `json:"id"`
	Name         *string           `json:"name"`
	Prefix       string            `json:"prefix"`
	Scopes       []string          `json:"scopes"`
	SandboxIDs   []string          `json:"sandbox_ids"`
	Labels       map[string]string `json:"labels"`
	CreatedAt    int64             `json:"created_at"`
	ExpiresAt    int64             `json:"expires_at"`
	RevokedAt    *int64            `json:"revoked_at"`
	LastUsedAt   *int64            `json:"last_used_at"`
	CreatedByKey *string           `json:"created_by_key"`
	MemberEmail  *string           `json:"member_email"`
	State        string            `json:"state"` // active | revoked | expired
	Secret       string            `json:"secret,omitempty"`
}

// CloudTokenOptions describe a new token.
type CloudTokenOptions struct {
	// Scopes: from sandboxes:read, sandboxes:write and functions:invoke, and
	// only ones the key making it holds.
	Scopes []string
	// SandboxIDs limits it to these sandboxes (1..100) of the org.
	SandboxIDs []string
	// Labels limits it to sandboxes carrying every label; a sandbox it
	// creates gets them.
	Labels map[string]string
	TTLS   int    // lifetime, 60..86400 s; 0 the API's default (3600)
	Name   string // up to 64 characters
}

// CloudTokens makes, lists and revokes capability tokens: short-lived bearer
// tokens limited to some scopes and sandboxes, for handing a sandbox to an
// end user without handing over the API key. A token cannot make tokens.
type CloudTokens struct{ c *Cloud }

// Create makes a token; its Secret is shown once.
func (t *CloudTokens) Create(ctx context.Context, o CloudTokenOptions) (CloudToken, error) {
	if len(o.Scopes) == 0 {
		return CloudToken{}, usage("a token needs Scopes")
	}
	seen := map[string]bool{}
	for _, s := range o.Scopes {
		if s != ScopeSandboxesRead && s != ScopeSandboxesWrite && s != ScopeFunctionsInvoke {
			return CloudToken{}, usage("scope %q: one of sandboxes:read, sandboxes:write, functions:invoke", s)
		}
		if seen[s] {
			return CloudToken{}, usage("scope %q given twice", s)
		}
		seen[s] = true
	}
	body := map[string]any{"scopes": o.Scopes}
	if len(o.SandboxIDs) > 0 {
		if len(o.SandboxIDs) > 100 {
			return CloudToken{}, usage("at most 100 SandboxIDs, not %d", len(o.SandboxIDs))
		}
		body["sandbox_ids"] = o.SandboxIDs
	}
	if len(o.Labels) > 0 {
		if err := checkLabels(o.Labels); err != nil {
			return CloudToken{}, err
		}
		body["labels"] = o.Labels
	}
	if o.TTLS != 0 {
		if o.TTLS < 60 || o.TTLS > 86400 {
			return CloudToken{}, usage("TTLS must be 60..86400 seconds, not %d", o.TTLS)
		}
		body["ttl_s"] = o.TTLS
	}
	if o.Name != "" {
		if len(o.Name) > 64 {
			return CloudToken{}, usage("Name is at most 64 characters")
		}
		body["name"] = o.Name
	}
	var r struct {
		Token CloudToken `json:"token"`
	}
	err := t.c.request(ctx, "POST", "/v1/tokens", body, &r)
	return r.Token, err
}

// List returns the tokens (never their secrets); with all, also the last
// day's revoked and expired ones.
func (t *CloudTokens) List(ctx context.Context, all bool) ([]CloudToken, error) {
	p := "/v1/tokens"
	if all {
		p += "?all=1"
	}
	var r struct {
		Tokens []CloudToken `json:"tokens"`
	}
	if err := t.c.request(ctx, "GET", p, nil, &r); err != nil {
		return nil, err
	}
	if r.Tokens == nil {
		r.Tokens = []CloudToken{}
	}
	return r.Tokens, nil
}

// Revoke stops a token at once.
func (t *CloudTokens) Revoke(ctx context.Context, id string) (CloudToken, error) {
	if id == "" {
		return CloudToken{}, usage("Revoke needs a token id")
	}
	var r struct {
		Token CloudToken `json:"token"`
	}
	err := t.c.request(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(id), nil, &r)
	return r.Token, err
}
