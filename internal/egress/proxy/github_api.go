// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The GitHub CLI door: a run that attaches a GitHub CLI row reaches
// api.github.com through this gate and nowhere else. The sandbox holds only an
// inert GH_TOKEN; every request is classified by the capability catalogue
// (internal/ghscope) and forwarded with the organisation's App installation
// token only when the run holds that capability and the request stays inside
// the run's own repositories.
//
// THIS IS THE BOUNDARY: the installation token is minted for the run's
// repositories with the row's permissions, but the classifier is what tells
// capabilities that share one permission apart, and what refuses the denied
// areas (token doors, hooks, secrets, search, GraphQL). The gate runs on both
// lanes: the terminated tunnel judges each request, and the plain forward lane
// refuses every request to the host, so an absolute-form request line cannot
// reach it ungated.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

const (
	ruleSourceGitHubAPI        = "brokered:github-api"
	ruleSourceGitHubAPIRefused = "brokered:github-api:refused"

	// gitHubAPIRefused is the `wardyn` code of every refusal the door answers.
	gitHubAPIRefused = "github_api_refused"

	// gitHubAPIPort is the one port the door terminates.
	gitHubAPIPort = 443

	// gitHubAPIRevokeBudget bounds the shutdown hand-back of every cached
	// token, well inside a container runtime's stop timeout (10 s on Docker).
	gitHubAPIRevokeBudget = 3 * time.Second
)

// GitHubAPIGrant is the run's GitHub CLI grant: the capabilities the row
// grants (already without the ones a CLI token never holds), the run's
// repositories as "owner/name", and the github_token grant to mint from for
// each owner, since an installation token covers one owner's repositories.
type GitHubAPIGrant struct {
	Capabilities []ghscope.Capability `json:"capabilities"`
	Repos        []string             `json:"repos"`
	Grants       map[string]uuid.UUID `json:"grants"`
}

// grantFor is the grant a classified request mints from: its owner's, or for
// a request that names no owner the first owner's. Only metadata (the API
// root, the rate limit) and identity (GET /user, which GitHub refuses an
// installation token) classify with no owner; every route that names an owner
// or repository is pinned by Permits first. A verdict class added to the
// owner-less set must not reach anything an owner's grant could not.
func (g *GitHubAPIGrant) grantFor(v ghscope.Verdict) (uuid.UUID, bool) {
	if v.Owner != "" {
		id, ok := g.Grants[v.Owner]
		return id, ok
	}
	owners := slices.Sorted(maps.Keys(g.Grants))
	if len(owners) == 0 {
		return uuid.Nil, false
	}
	return g.Grants[owners[0]], true
}

// holds reports whether id is one of the door's grants. Nil-safe.
func (g *GitHubAPIGrant) holds(id uuid.UUID) bool {
	return g != nil && slices.Contains(slices.Collect(maps.Values(g.Grants)), id)
}

// serveAPIDoor serves a request on a host an API door owns — the GitHub CLI's,
// the Azure CLI's (azure_arm_door.go), or a git_pat grant's with api: true
// (pat_api.go) — and reports whether it did.
func (p *Proxy) serveAPIDoor(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if p.isGitHubAPIHost(host) {
		p.serveGitHubAPI(w, r, host, port)
		return true
	}
	if p.isAzureARMHost(host) {
		p.serveAzureARM(w, r, host, port)
		return true
	}
	if grant, ok := p.patAPI[patAPIHost(host)]; ok {
		p.servePATAPI(w, r, host, port, grant)
		return true
	}
	return false
}

// isGitHubAPIHost reports whether host is the door's host on a run that has one.
func (p *Proxy) isGitHubAPIHost(host string) bool {
	return p.ghAPI != nil && patAPIHost(host) == ghscope.APIHost
}

// gitHubAPIRefusedPrefix opens every refusal sentence but the GraphQL one.
const gitHubAPIRefusedPrefix = "Wardyn refused this GitHub API request: "

// gitHubAPIRESTOnly is the GraphQL refusal. gh's issue, pull request and
// repository commands use GraphQL, so this is the sentence a person meets
// most; it names the way that works.
const gitHubAPIRESTOnly = "Wardyn's GitHub CLI works through GitHub's REST API only; this command uses GraphQL. " +
	"Use `gh api repos/{owner}/{repo}/...` instead."

// gitHubAPIAdmit is the whole gate: "" admits the request, otherwise the
// sentence its refusal carries. It mints nothing.
func gitHubAPIAdmit(r *http.Request, g *GitHubAPIGrant) (ghscope.Verdict, string) {
	v, err := ghscope.Classify(ghscope.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header})
	switch {
	case err != nil:
		return v, gitHubAPIRefusedPrefix + "it could not be read as one GitHub operation."
	case ghscope.Permits(g.Capabilities, g.Repos, v):
		return v, ""
	case v.Capability == ghscope.CapDeniedGraphQL:
		return v, gitHubAPIRESTOnly
	case !v.Capability.Grantable() && v.Capability != ghscope.CapMetadata:
		return v, "Wardyn refused this GitHub API request. " + ghscope.Label(v.Capability) + "."
	case !ghscope.Permits(ghscope.GrantableCapabilities(), g.Repos, v):
		return v, gitHubAPIRefusedPrefix + "it names a repository or an owner outside this run's own."
	}
	return v, gitHubAPIRefusedPrefix + "this run's GitHub CLI is not granted \"" + ghscope.Label(v.Capability) + "\"."
}

// serveGitHubAPI is the door: judge, mint, forward.
func (p *Proxy) serveGitHubAPI(w http.ResponseWriter, r *http.Request, host string, port int) {
	v, why := gitHubAPIAdmit(r, p.ghAPI)
	if why != "" {
		p.refuseGitHubAPI(w, r, host, port, why)
		return
	}
	grantID, ok := p.ghAPI.grantFor(v)
	if !ok {
		p.refuseGitHubAPI(w, r, host, port, gitHubAPIRefusedPrefix+"no grant of this run covers "+v.Owner+".")
		return
	}
	p.forwardAPIDoor(w, r, host, port, apiDoor{allow: ruleSourceGitHubAPI, denied: ruleSourceGitHubAPIRefused, mintFailed: "mint github token",
		credential: func(ctx context.Context) (string, string, error) {
			tok, _, err := p.brokeredToken(ctx, grantID, func(string) string { return gitBrokerUsername })
			if err != nil {
				return "", "", err
			}
			return "Authorization", "token " + tok, nil
		}})
}

// refuseGitHubAPI is the door's one refusal point: a decision row under
// brokered:github-api:refused and a 403 (never 401, which gh reads as "sign in
// again") whose JSON carries the code and the sentence.
func (p *Proxy) refuseGitHubAPI(w http.ResponseWriter, r *http.Request, host string, port int, message string) {
	p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourceGitHubAPIRefused)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"wardyn":  gitHubAPIRefused,
		"message": message,
	})
}

// refuseGitHubAPIPlain refuses, on the plain forward lane, every request to the
// door's host, and reports whether it did. That lane never runs the door.
func (p *Proxy) refuseGitHubAPIPlain(w http.ResponseWriter, r *http.Request) bool {
	if p.ghAPI == nil || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isGitHubAPIHost(host) {
		return false
	}
	p.refuseGitHubAPI(w, r, host, port, gitHubAPIRefusedPrefix+"api.github.com is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request.")
	return true
}

// refuseGitHubAPITunnel refuses a CONNECT to the door's host that the proxy
// will not terminate (another port): a blind tunnel would carry it past the gate.
func (p *Proxy) refuseGitHubAPITunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isGitHubAPIHost(host) {
		return false
	}
	p.refuseGitHubAPI(w, r, host, port, gitHubAPIRefusedPrefix+"this run reaches api.github.com only through Wardyn's checked HTTPS door on port 443.")
	return true
}

// newGitHubAPIDoor compiles the grant and adds the door's host to the
// MITM-eligible set on its one port. Nil without a grant.
func newGitHubAPIDoor(g *GitHubAPIGrant, mitmHosts map[string]bool, mitmPorts map[string]int) *GitHubAPIGrant {
	if g == nil || len(g.Grants) == 0 {
		return nil
	}
	mitmHosts[ghscope.APIHost], mitmPorts[ghscope.APIHost] = true, gitHubAPIPort
	return g
}

// revokeGitHubAPITokens hands every cached CLI token back to GitHub
// (DELETE /installation/token, authenticated as the token itself) when the
// proxy stops. Best effort: GitHub's expiry within the hour is the backstop.
// A grant whose mint is still in flight is skipped rather than waited for.
func (p *Proxy) revokeGitHubAPITokens(ctx context.Context) {
	if p.ghAPI == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitHubAPIRevokeBudget)
	defer cancel()
	for _, id := range p.ghAPI.Grants {
		p.gitTokMu.Lock()
		e := p.gitTokens[id]
		p.gitTokMu.Unlock()
		if e == nil || !e.reMu.TryLock() {
			continue
		}
		tok := e.token
		e.reMu.Unlock()
		if tok == "" {
			continue
		}
		if err := p.revokeGitHubToken(ctx, tok); err != nil {
			slog.Warn("wardyn-proxy: could not hand a GitHub CLI token back; it expires within the hour", "grant_id", id, "err", err)
		}
	}
}

func (p *Proxy) revokeGitHubToken(ctx context.Context, tok string) error {
	target, _, err := p.egressTarget(ghscope.APIHost, gitHubAPIPort)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, vettedIPKey{}, target), http.MethodDelete, "https://"+ghscope.APIHost+"/installation/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.roundTripUpstream(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE /installation/token answered %d", resp.StatusCode)
	}
	return nil
}
