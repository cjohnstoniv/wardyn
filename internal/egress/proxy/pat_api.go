// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The git_pat forge API door: a git_pat grant with api: true reaches its forge's
// REST API through this gate and nowhere else. It sits on the MITM path at the
// serveMITMRequest seam, beside the Azure DevOps gate, because forge CLIs and
// libraries call the API host directly; a second, plain-HTTP broker route would
// leave that direct call ungoverned.
//
// THIS IS THE BOUNDARY, not a second opinion: the PAT is exactly as broad as its
// issuer made it, and these tables are the only thing narrowing what the run does
// with it. A request is judged on its effective method, its path and every field
// it names (pat_api_fields.go) BEFORE the PAT is minted and before anything is
// sent upstream, so a refused request spends neither: an approval-gated
// single-use grant keeps its one mint for the next admitted request. The door
// never registers the PAT as an injection rule, which would mint at proxy start.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	ruleSourcePATAPI       = "brokered:git-pat:api"
	ruleSourcePATAPIDenied = "brokered:git-pat:api:denied"

	// patAPIRefused is the `wardyn` code of every refusal the door answers.
	patAPIRefused = "git_pat_api_refused"

	// patAPIPort is the one port the door terminates: the broker re-originates
	// git to the grant's host on 443, so the API is the same service.
	patAPIPort = 443
)

// patAPIMethodOverrides are the headers a framework reads a method override from.
var patAPIMethodOverrides = []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"}

// patAPIMethodMismatch reports a method override that names another method than
// the request line. The tables judge the wire method, and a framework that
// honoured the override would run another: so an override that turns a read into
// a write is not a read, and none of the admitted rows needs one.
func patAPIMethodMismatch(r *http.Request) bool {
	for _, h := range patAPIMethodOverrides {
		for _, v := range r.Header.Values(h) {
			if !strings.EqualFold(strings.TrimSpace(v), r.Method) {
				return true
			}
		}
	}
	return false
}

// patAPIAdmit is the whole gate: "" admits the request, otherwise the clause that
// completes "Wardyn refused this forge API request: …". It reads and restores the
// body and mints nothing.
func patAPIAdmit(r *http.Request, g PATGrant) string {
	sc := g.scope()
	forge := patAPIForges[sc.Forge]
	if forge == nil {
		return "this grant's forge has no API table"
	}
	if patAPIMethodMismatch(r) {
		return "it carries a method override that names another method than the request line"
	}
	if len(r.Header.Values("Sudo")) > 0 {
		return "it carries a Sudo header, which acts as another user"
	}
	segs, why := patAPISplit(adoRawPath(r))
	if why != "" {
		return why
	}
	key, row, tail, why := patAPIMatch(forge, r.Method, segs)
	if why != "" {
		return why
	}
	if sc.Access == types.PATAccessRead && row.kind != patAPIRead {
		return "this run's git_pat grant is read-only"
	}
	if g.Repos != nil && !slices.ContainsFunc(*g.Repos, func(entry string) bool { return types.PATRepoCovers(entry, key) }) {
		return "the repository " + key + " is not granted to this run"
	}
	f, why := collectPATAPIFields(r)
	if why != "" {
		return why
	}
	if k := f.refusedField(); k != "" {
		return "it sets the field " + k + ", which names a target or a merge this door does not admit"
	}
	return forge.check(row, tail, key, f)
}

// gitlabCheck refuses a GitLab quick action in free text: a comment or a
// description whose line starts with "/" runs a command, and "/merge" would merge
// through a note.
func gitlabCheck(_ patAPIRow, _ []patAPISeg, _ string, f *patAPIFields) string {
	for _, name := range []string{"body", "description", "note"} {
		for _, v := range f.vals[name] {
			for line := range strings.Lines(v) {
				if strings.HasPrefix(strings.TrimLeft(line, " \t"), "/") {
					return "a line of " + name + " starts with \"/\", which GitLab runs as a quick action (merge among them)"
				}
			}
		}
	}
	return ""
}

// giteaCheck refuses a cross-repository head or base ("owner:branch" is a fork's
// branch; a git ref name cannot contain a colon) and a cross-repository compare.
func giteaCheck(row patAPIRow, tail []patAPISeg, _ string, f *patAPIFields) string {
	if row.kind == patAPICreate {
		for _, name := range []string{"head", "base"} {
			if slices.ContainsFunc(f.vals[name], func(v string) bool { return strings.Contains(v, ":") }) {
				return "the " + name + " names a branch of another repository"
			}
		}
	}
	if len(tail) > 0 && tail[0].s == "compare" && slices.ContainsFunc(tail, func(s patAPISeg) bool { return strings.Contains(s.s, ":") }) {
		return "a compare across repositories is not available through this door"
	}
	return ""
}

// bitbucketCheck refuses a pull request whose from or to ref names another
// repository than the path's.
func bitbucketCheck(row patAPIRow, _ []patAPISeg, repo string, f *patAPIFields) string {
	if row.kind != patAPICreate {
		return ""
	}
	project, slug, _ := strings.Cut(repo, "/")
	for _, name := range []string{"fromRef", "toRef"} {
		ref, _ := f.obj[name].(map[string]any)
		repoObj, ok := ref["repository"].(map[string]any)
		if !ok {
			continue
		}
		proj, _ := repoObj["project"].(map[string]any)
		gotSlug, _ := repoObj["slug"].(string)
		gotKey, _ := proj["key"].(string)
		if !strings.EqualFold(gotSlug, slug) || !strings.EqualFold(gotKey, project) {
			return "the pull request's " + name + " names another repository"
		}
	}
	return ""
}

// patAPIHost is the grant's host, normalised as the proxy keys every lookup.
func patAPIHost(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

// isPATAPIHost reports whether host carries a git_pat grant with api: true.
func (p *Proxy) isPATAPIHost(host string) bool {
	_, ok := p.patAPI[patAPIHost(host)]
	return ok
}

// servePATAPI is the door: judge, mint, forward. The mint is lazy: patToken runs
// only for a request the tables admitted.
func (p *Proxy) servePATAPI(w http.ResponseWriter, r *http.Request, host string, port int, grant PATGrant) {
	if why := patAPIAdmit(r, grant); why != "" {
		p.refusePATAPI(w, r, host, port, why)
		return
	}
	// The door returns before the generic forward-body scan in serveMITMRequest, so it runs the
	// same scan here: a registered secret in an MR/PR body or query (the forge reads create params
	// from either) must not leave with the PAT attached. Before the mint, so a refused request
	// never redeems a token.
	var bodyReader io.Reader = r.Body
	var scanSummary *egress.ScanSummary
	if p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff {
		if p.scanner.InspectForwardEgress() {
			if q, err := url.QueryUnescape(r.URL.RawQuery); err == nil && q != "" {
				res, _, serr := p.scanner.ScanRequest(contentscan.ChannelGeneric, []byte(q))
				if p.scanner.ShouldBlock(res) {
					p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLMBlocked, scanSummaryFrom(res, serr, p.scanner, "block", contentscan.ChannelGeneric))
					writeScanBlocked(w, len(res.Findings), categoriesOf(res.Findings), res.SkipReason)
					return
				}
				if len(res.Findings) > 0 || res.Skipped || serr != nil {
					scanSummary = scanSummaryFrom(res, serr, p.scanner, "", contentscan.ChannelGeneric)
				}
			}
			if hasScannableBody(r) {
				var release func()
				var blocked bool
				var bodySummary *egress.ScanSummary
				bodyReader, bodySummary, release, blocked = p.inspectForwardBody(w, r, host, port)
				defer release()
				if blocked {
					return
				}
				if bodySummary != nil {
					scanSummary = bodySummary
				}
			}
		} else if hasScannableBody(r) {
			// Say so on the allow row, as every other MITM'd generic host does.
			scanSummary = p.skipSummary("skip", "uninspected_channel", contentscan.ChannelGeneric)
		}
	}
	token, _, err := p.patToken(r.Context(), grant)
	if err != nil {
		p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourcePATAPIDenied)
		p.httpError(w, "mint git_pat", err, http.StatusBadGateway)
		return
	}
	name, value := patAPIForges[grant.scope().Forge].header(token)
	registerHeaderCredential(value) // the rendering that leaves the process, beside the raw token patToken masked

	target, _, err := p.egressTarget(host, port)
	if err != nil {
		p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourcePATAPIDenied)
		p.httpError(w, "vet forge host", err, http.StatusForbidden)
		return
	}
	scheme, defaultPort := p.upstreamSchemeFor(host, port)
	hostport := host
	if port != defaultPort {
		hostport = net.JoinHostPort(host, strconv.Itoa(port))
	}
	// The path forwarded is the path compared: the raw, still-encoded string the
	// tables read (an encoded "/" in a GitLab project path must survive).
	raw := adoRawPath(r)
	path, _ := url.PathUnescape(raw)
	upstream := (&url.URL{Scheme: scheme, Host: hostport, Path: path, RawPath: raw, RawQuery: r.URL.RawQuery}).String()
	body := bodyReader
	if r.Body == http.NoBody {
		body = nil
	}
	out, err := http.NewRequestWithContext(context.WithValue(r.Context(), vettedIPKey{}, target), r.Method, upstream, body)
	if err != nil {
		p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourcePATAPIDenied)
		p.httpError(w, "build forge request", err, http.StatusBadGateway)
		return
	}
	out.ContentLength = r.ContentLength
	copyHeader(out.Header, r.Header)
	removeHopByHop(out.Header)
	stripSandboxCredentials(out.Header, "")
	out.Header.Set(name, value)
	out.Host = hostport
	out.Header.Del("Host")

	resp, err := p.roundTripUpstream(out)
	if err != nil {
		p.failUpstream(w, err, &egress.DecisionLog{Request: p.reqOf(r, host, port)}, host, "forge api upstream error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.emitLLMDecision(r, host, port, egress.Allow, ruleSourcePATAPI, scanSummary)
	relay(w, resp)
}

func (p *Proxy) emitPATAPIDecision(r *http.Request, host string, port int, d egress.Decision, ruleSource string) {
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), d, ruleSource))
	}
}

// refusePATAPI is the one refusal point for the door's own tables: a decision row under
// brokered:git-pat:api:denied and a 403 (never 401, which clients read as "try
// another credential") whose JSON carries the code and the sentence.
func (p *Proxy) refusePATAPI(w http.ResponseWriter, r *http.Request, host string, port int, why string) {
	p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourcePATAPIDenied)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"wardyn":  patAPIRefused,
		"message": "Wardyn refused this forge API request: " + why + ".",
	})
}

// refusePATAPIPlain refuses, on the plain forward lane, every request to a host
// that carries a git_pat API grant, and reports whether it did. That lane never
// runs the door, so an absolute-form request there would otherwise be judged by
// the egress rules alone and carry no PAT; the API is reached only through the
// terminated tunnel.
func (p *Proxy) refusePATAPIPlain(w http.ResponseWriter, r *http.Request) bool {
	if len(p.patAPI) == 0 || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isPATAPIHost(host) {
		return false
	}
	p.refusePATAPI(w, r, host, port, "this host's API is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request")
	return true
}

// refusePATAPITunnel refuses a CONNECT to an API host that the proxy will not
// terminate (another port than the one the door serves): a blind tunnel would
// carry the request past the tables.
func (p *Proxy) refusePATAPITunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isPATAPIHost(host) {
		return false
	}
	p.refusePATAPI(w, r, host, port, "this run reaches this host's API only through Wardyn's checked HTTPS door")
	return true
}

// newPATAPIGrants is the API grants of a run by host. It also adds each host to
// the MITM-eligible set, on port 443, unless the operator already authored an
// entry for it: only the grant's own host is terminated, never one derived from
// the forge.
func newPATAPIGrants(grants map[string]PATGrant, mitmHosts map[string]bool, mitmPorts map[string]int) map[string]PATGrant {
	out := map[string]PATGrant{}
	for host, g := range grants {
		if !g.API {
			continue
		}
		host = patAPIHost(host)
		out[host] = g
		if !mitmHosts[host] {
			mitmHosts[host], mitmPorts[host] = true, patAPIPort
		}
	}
	return out
}
