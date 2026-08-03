// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The Wardyn Git Broker: an authenticating git-smart-HTTP reverse-proxy local
// route so a sandbox NEVER dials github.com directly. The sandbox's git is
// pointed (url.<broker>.insteadOf, set by agent-run) at
// http://wardyn-proxy:3128/wardyn/gh/<org>/<repo>, which lands here as an
// origin-form local route (wardyn-proxy is in the sandbox NO_PROXY). This
// handler enforces the per-repo allowlist (p.gitGrants) in CLEARTEXT — the
// sandbox->proxy hop is plain HTTP on the proxy's own endpoint, so no third-party
// TLS-MITM is needed to see which repo is requested — mints the repo-scoped
// GitHub App installation token server-side, and re-originates to github.com with
// the token as Basic auth on the OUTBOUND request only. The token is never
// returned to the sandbox (git responses carry no Authorization).
//
// Repo is the unit of trust: a repo not in p.gitGrants is 403, so the sandbox can
// reach ONLY the repos its run was granted, never all of github.com.
const (
	routeGitBroker = "/wardyn/gh/"
	ruleSourceGit  = "brokered:git"
	githubHost     = "github.com"
	// ruleSourceGitRef marks a decision-log row denied by push branch-namespace
	// confinement (as opposed to the per-repo allowlist), so audit says WHICH gate
	// closed. The offending ref goes to slog, never to the decision log (which has
	// no free-text field and is SIEM-fanned).
	ruleSourceGitRef = "brokered:git:branch-ns"
	// ruleSourceGitEnc marks the refusal of a push body this proxy could not
	// INSPECT (non-identity Content-Encoding) while enforcement is on — a
	// distinct row so audit never reads an unparseable body as a ref violation.
	ruleSourceGitEnc = "brokered:git:branch-ns-encoding"
	// envEnforceBranchNS opts THIS proxy process OUT of push branch-namespace
	// confinement (=false). See BranchNSEnforced: it is ON by default.
	envEnforceBranchNS = "WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS"
	// ruleSourceGitNSOff marks a push this proxy FORWARDED WITHOUT PARSING because
	// envEnforceBranchNS opted the process out. It is the after-the-fact half of
	// that opt-out being visible: the boot log (cmd/wardyn-proxy) states the
	// posture once, and this row proves per push which posture actually applied,
	// in the same append-only audit stream the ordinary "brokered:git" allow lands
	// in (handlePostDecision records rule_source verbatim). Without it an
	// unparsed push and a parsed one were the same audit record.
	ruleSourceGitNSOff = "brokered:git:branch-ns-off"
	// maxReceivePackCmds caps the pkt-line COMMAND SECTION buffered ahead of the
	// (still-streamed) packfile. Hundreds of ref updates fit in 64 KiB; anything
	// larger is pathological and is refused rather than buffered.
	maxReceivePackCmds = 64 << 10
)

// gitServices is the closed set of valid ?service= values / smart-HTTP verbs.
var gitServices = map[string]bool{"git-upload-pack": true, "git-receive-pack": true}

// gitTokEntry caches one grant's minted GitHub App installation token. token +
// expiresAt are guarded by reMu, which ALSO single-flights the mint so info/refs
// and the following git-upload-pack don't stampede a double-mint — fatal for a
// single-use (approval-gated) grant, whose second mint 409s (broker ErrAlreadyMinted).
type gitTokEntry struct {
	reMu      sync.Mutex
	token     string
	expiresAt int64 // unix ms; 0 = unset
}

// handleGitBroker serves /wardyn/gh/<org>/<repo>[.git]/<rest>. It is
// method-agnostic at the switch; it validates the method against the smart-HTTP
// subpath itself.
func (p *Proxy) handleGitBroker(w http.ResponseWriter, r *http.Request) {
	orgRepo, rest, ok := parseGitBrokerPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid git broker path", http.StatusNotFound)
		return
	}
	// The allowlist lookup is the STRUCTURAL traversal + authorization gate
	// (mirrors handleBrokerApproval's uuid.Parse): anything not an exact granted
	// key — including ".."-bearing or extra-segment paths — 403s before any
	// github URL is formed.
	grantID, granted := p.gitGrants[orgRepo]
	if !granted {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "repository not granted to this run", http.StatusForbidden)
		return
	}
	if !validGitRest(r.Method, rest, r.URL.Query().Get("service")) {
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}

	// Push confinement (ON by default; =false opts out). A receive-pack POST carries its ref updates in a
	// small pkt-line command section AHEAD of the packfile, so buffer only that
	// section, validate every ref against this run's branch namespace, then forward
	// the buffered bytes followed by the still-streaming pack. Fetch/clone
	// (info/refs, upload-pack) never enter this branch: pure streaming, zero added
	// latency. A denial happens BEFORE gitToken, so a refused push never mints.
	//
	// allowSrc is the rule_source the ALLOW row below carries. A push forwarded
	// with the parser opted out gets its own value (ruleSourceGitNSOff) so the
	// audit stream distinguishes the two postures per push; everything else keeps
	// the ordinary "brokered:git".
	var reqBody io.Reader = r.Body
	allowSrc := ruleSourceGit
	isPush := rest == "git-receive-pack"
	if isPush && !BranchNSEnforced() {
		allowSrc = ruleSourceGitNSOff
	} else if isPush {
		// git does not gzip receive-pack bodies (remote-curl only sets
		// gzip_request for fetch), but a compressed body must never be waved
		// through unparsed — that would be a silent bypass.
		if encs := r.Header.Values("Content-Encoding"); len(encs) > 1 ||
			(len(encs) == 1 && encs[0] != "" && !strings.EqualFold(encs[0], "identity")) {
			p.emitGitDecision(r, egress.Deny, ruleSourceGitEnc)
			http.Error(w, "wardyn: cannot enforce branch-namespace confinement on a "+
				strings.Join(encs, ",")+"-encoded push body", http.StatusUnsupportedMediaType)
			return
		}
		prefix := BranchNSPrefix(p.runID)
		head, err := readReceivePackCommands(r.Body, prefix)
		if err != nil {
			p.emitGitDecision(r, egress.Deny, ruleSourceGitRef)
			slog.WarnContext(r.Context(), "wardyn-proxy: git push denied by branch-namespace confinement",
				slog.String("run_id", p.runID.String()),
				slog.String("repo", orgRepo),
				slog.String("reason", err.Error()))
			// ponytail: plain 403 + text/plain body (git surfaces it as "remote:"
			// on the paths that show server messages, and always shows the 403).
			// A sideband report-status would read better but means claiming
			// "unpack ok" for a pack we never forwarded.
			http.Error(w, "wardyn: "+err.Error()+
				"\nthis run may push only to "+prefix+"*", http.StatusForbidden)
			return
		}
		reqBody = io.MultiReader(bytes.NewReader(head), r.Body)
	}

	token, err := p.gitToken(r.Context(), grantID)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "git credential unavailable", err, http.StatusBadGateway)
		return
	}

	target, err := p.vetURL("https://" + githubHost)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "git upstream vet failed", err, http.StatusBadGateway)
		return
	}

	// Build the upstream URL from the MATCHED key + validated rest — never the raw
	// request path — so nothing attacker-controlled beyond a granted map key and a
	// closed-enum verb flows outbound.
	upstreamURL := "https://" + githubHost + "/" + orgRepo + ".git/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL, reqBody)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "build git request", err, http.StatusBadGateway)
		return
	}
	// Stream the body straight through (the only buffered part is a validated
	// receive-pack command section, re-prepended above) — upload-pack/receive-pack
	// packs can be large. Preserve the client's declared length; unknown => chunked.
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// Defensive: strip any sandbox-supplied credential before injecting ours, so a
	// rogue in-sandbox client can't smuggle its own onto the outbound request.
	outReq.Header.Del("Authorization")
	outReq.SetBasicAuth("x-access-token", token) // GitHub App installation-token auth
	outReq.Host = githubHost
	outReq.Header.Del("Host")

	p.emitGitDecision(r, egress.Allow, allowSrc)

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.httpError(w, "git upstream error", err, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // stream the pack back
}

// isBrokeredGitGrant reports whether a sandbox-supplied mint body names one of
// THIS run's git-broker grants — the guard that keeps the App installation token
// out of the sandbox (handleBrokerMint). handleGitBroker's own mint does NOT come
// through here: it calls mintGitToken -> forwardToControlPlane directly, so the
// broker never refuses itself.
//
// Decoding mirrors the control plane's handleInternalMint EXACTLY (same
// encoding/json Decoder, same one-field struct, same "grant_id" tag), so any body
// that would mint a brokered grant there decodes to that grant id here —
// including trailing-garbage and duplicate-key bodies, where a json.Unmarshal
// here would have differed and become a bypass.
//
// ponytail: fail OPEN on an undecodable body. A body we cannot decode cannot name
// a brokered grant the control plane would accept either (it 400s), so forwarding
// it preserves today's error behavior for legitimate callers without opening a
// hole. Fail-closed here would only convert a control-plane 400 into a proxy 403.
func (p *Proxy) isBrokeredGitGrant(body []byte) bool {
	if len(p.gitGrants) == 0 {
		return false
	}
	var req struct {
		GrantID uuid.UUID `json:"grant_id"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil || req.GrantID == uuid.Nil {
		return false
	}
	for _, id := range p.gitGrants {
		if id == req.GrantID {
			return true
		}
	}
	return false
}

// gitToken returns a cached (or freshly minted) installation token for grantID,
// re-minting only when unset or within injectRefreshMargin of expiry. reMu
// single-flights per grant so concurrent git sub-requests don't double-mint.
func (p *Proxy) gitToken(ctx context.Context, grantID uuid.UUID) (string, error) {
	p.gitTokMu.Lock()
	e, ok := p.gitTokens[grantID]
	if !ok {
		e = &gitTokEntry{}
		if p.gitTokens == nil {
			p.gitTokens = make(map[uuid.UUID]*gitTokEntry)
		}
		p.gitTokens[grantID] = e
	}
	p.gitTokMu.Unlock()

	e.reMu.Lock()
	defer e.reMu.Unlock()
	if e.token != "" && time.Now().Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
		return e.token, nil
	}
	// one token per grant for the run. Auto-mintable grants re-mint past
	// TTL fine; approval-gated grants are single-use, so a run outliving the ~1h
	// installation-token TTL fails here on re-mint — a pre-existing ceiling.
	tok, expMs, err := p.mintGitToken(ctx, grantID)
	if err != nil {
		return "", err
	}
	// Register the installation token in the process-global mask registry, the
	// same place injector values land (inject.go) and the same one httpError's
	// maskDecisionBytes reads — so a `ghs_...` can never ride out on a
	// sandbox-visible error string or a decision-log line. Defense in depth: no
	// live leak is known (this token is set as Basic auth on the OUTBOUND request
	// only and never appears in an error the sandbox sees), but "no path today"
	// is not a property of the token, it is a property of the current call sites.
	// AddGlobal dedupes by value, so the cache's re-mints add at most one entry
	// per rotation on a process that lives one run.
	//
	// HONEST RESIDUAL, same as inject.go's: verbatim bytes only — a base64/hex or
	// model-narrated form of the token is not caught.
	procRegistry.AddGlobal([]byte(tok))
	e.token, e.expiresAt = tok, expMs
	return tok, nil
}

// mintGitToken calls the control-plane mint route server-side (run token injected
// by forwardToControlPlane) — the exact route wardyn-git-helper uses, so no broker
// change is needed. Returns the token and its expiry (unix ms; 0 if unparseable).
func (p *Proxy) mintGitToken(ctx context.Context, grantID uuid.UUID) (string, int64, error) {
	body, err := json.Marshal(map[string]string{"grant_id": grantID.String()})
	if err != nil {
		return "", 0, err
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost,
		"/api/v1/internal/credentials/mint", body, "application/json")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("mint status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var mr struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return "", 0, fmt.Errorf("decode mint response: %w", err)
	}
	if mr.Token == "" {
		return "", 0, fmt.Errorf("mint response missing token")
	}
	var expMs int64
	if t, perr := time.Parse(time.RFC3339, mr.ExpiresAt); perr == nil {
		expMs = t.UnixMilli()
	}
	return mr.Token, expMs, nil
}

// parseGitBrokerPath splits /wardyn/gh/<org>/<repo>[.git]/<rest...> into the
// canonical lowercased "<org>/<repo>" key (.git stripped) and the smart-HTTP
// subpath. ok=false on a malformed shape; the allowlist lookup is the real gate.
func parseGitBrokerPath(path string) (orgRepo, rest string, ok bool) {
	segs := strings.Split(strings.TrimPrefix(path, routeGitBroker), "/")
	if len(segs) < 3 {
		return "", "", false
	}
	org, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
	if !gitSegSafe(org) || !gitSegSafe(repo) {
		return "", "", false
	}
	return strings.ToLower(org + "/" + repo), strings.Join(segs[2:], "/"), true
}

// gitSegSafe allows one GitHub owner/repo segment ([A-Za-z0-9._-]) and rejects
// empty / "." / ".." so no path-traversal segment survives parsing.
func gitSegSafe(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// validGitRest restricts the smart-HTTP subpath + method to the git v2 verbs the
// broker serves: GET info/refs?service=git-{upload,receive}-pack, and POST
// git-upload-pack (fetch) / git-receive-pack (push, scoped to the granted repo).
func validGitRest(method, rest, service string) bool {
	switch rest {
	case "info/refs":
		return method == http.MethodGet && gitServices[service]
	case "git-upload-pack", "git-receive-pack":
		return method == http.MethodPost
	default:
		return false
	}
}

// BranchNSPrefix is the ONLY ref prefix a governed run may push to: the branch
// namespace the broker records in github_token grant metadata, `wardyn/<run-id>/*`,
// rooted under refs/heads/. It is a pure function of the run id (which this proxy
// already holds), so enforcement needs no extra plumbing — but it MUST stay in
// lockstep with internal/broker.branchNamespaceFormat; that constant's comment
// points back here and broker's TestBranchNamespaceLockstepWithProxy fails on
// drift (the reason this is exported). Everything else (other branches, the
// default branch, tags, refs/pull/*, refs/notes/*) is outside the namespace.
func BranchNSPrefix(runID uuid.UUID) string {
	return "refs/heads/wardyn/" + runID.String() + "/"
}

// BranchNSEnforced reports whether push branch-namespace confinement is ON for
// THIS proxy process (env, like the WARDYN_LLM_SCAN kill-switch). Exported so
// cmd/wardyn-proxy can state the posture ONCE at boot: this is the only
// default-ON control in the git-broker path, and =false used to produce no
// signal anywhere — no boot line, no distinct decision row — while the per-mint
// branch_namespace metadata is written identically either way, so a run on an
// opted-out proxy read exactly like a confined one.
//
// ON by default. The reason it could not be is now closed: agent-run checks every
// cloned repo out onto `wardyn/$WARDYN_RUN_ID/work` and sets push.default=current
// (name_run_branch in deploy/images/common/agent-run-lib.sh), so a stock run is
// already inside its namespace and complies without the operator pinning the
// convention in task text. Set WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false to opt
// back out (e.g. an image whose agent-run predates the run branch).
//
// SCOPE, honestly: this binds the BROKERED path only. It sees git-receive-pack
// because the sandbox->proxy hop is cleartext on the proxy's own route; a PAT push
// or an SSH push is an opaque tunnel no pkt-line parser can read, and the
// installation token itself is repo-scoped but not ref-scoped. Dispatch removes AND
// denies the broker-managed GitHub host names on a brokered run — plus that forge's
// ssh.<forge> endpoint, so a co-granted ssh_key leaves no push path beside the
// brokered one (confineGitBrokerEgress in internal/api/runs_dispatch_gitbroker.go,
// and validateGrantLaneExclusivity refuses the combination at policy-write), and
// the forge's ssh_key AND git_pat grants are withheld from the sandbox entirely
// at dispatch (dropBrokeredGrants) so no unusable credential is left resident.
// With one
// standing caveat, the same one docs/POLICIES.md gives the four HTTPS denies:
// those are NAME-based denies and a name-based deny does not bind an IP LITERAL
// (under allow_all_egress a CONNECT to 140.82.114.4:22 is still allowed). The
// brokered route is the only CONVENIENT route — the one git itself takes, since
// a clone URL carries a name — not the only conceivable one.
//
// Loud parse: an unrecognized value fails CLOSED (enforce + error log) rather than
// silently disabling a security control on a typo.
func BranchNSEnforced() bool {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(envEnforceBranchNS))); v {
	case "":
		return true
	case "0", "false", "no", "off", "disable", "disabled", "none":
		return false
	case "1", "true", "yes", "on", "enable", "enabled", "enforce":
		return true
	default:
		// Once per process, not per push: the misconfiguration is boot-level
		// state and per-request ERROR spam would bury the signal.
		branchNSWarnOnce.Do(func() {
			slog.Error("wardyn-proxy: unrecognized "+envEnforceBranchNS+
				" value; ENFORCING push branch-namespace confinement (fail closed)",
				slog.String("value", v))
		})
		return true
	}
}

var branchNSWarnOnce sync.Once

// readReceivePackCommands consumes the pkt-line COMMAND SECTION of a
// git-receive-pack request body — everything up to and INCLUDING the flush-pkt —
// validating every ref update against prefix, and returns those bytes VERBATIM so
// the caller can forward them ahead of the still-streaming packfile.
//
// Wire shape (protocol v2 leaves push unchanged): every pkt-line starts with 4 hex
// length digits that COUNT THEMSELVES; "0000" is the flush-pkt ending the section;
// the first command carries "\0<capability-list>" after the refname; optional
// "shallow <oid>" lines may precede the commands. A delete (new-oid all zeros) is a
// mutation like any other, so it is checked identically — a delete outside the
// namespace is refused. Anything not understood — a signed push-cert, a bad length,
// a section over maxReceivePackCmds — is refused: fail closed.
func readReceivePackCommands(body io.Reader, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(body, hdr); err != nil {
			return nil, fmt.Errorf("unreadable pkt-line length: %w", err)
		}
		buf.Write(hdr)
		n, err := strconv.ParseUint(string(hdr), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed pkt-line length %q", hdr)
		}
		if n == 0 { // flush-pkt: command section done, the packfile follows.
			return buf.Bytes(), nil
		}
		// 0001/0002 (delim / response-end) and empty 0004 lines are not part of a
		// receive-pack command section.
		if n < 5 {
			return nil, fmt.Errorf("unexpected pkt-line length %d in receive-pack command section", n)
		}
		if buf.Len()+int(n)-4 > maxReceivePackCmds {
			return nil, fmt.Errorf("receive-pack command section exceeds %d bytes", maxReceivePackCmds)
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(body, payload); err != nil {
			return nil, fmt.Errorf("truncated pkt-line: %w", err)
		}
		buf.Write(payload)
		if err := checkPushCommand(string(payload), prefix); err != nil {
			return nil, err
		}
	}
}

// checkPushCommand validates ONE command-section pkt-line payload:
// "<old-oid> SP <new-oid> SP <refname>", with "\0<capabilities>" on the first and
// an optional trailing LF, or a "shallow <oid>" line (no ref to check).
func checkPushCommand(line, prefix string) error {
	line, _, _ = strings.Cut(line, "\x00") // capabilities ride the FIRST command only
	line = strings.TrimSuffix(line, "\n")
	if strings.HasPrefix(line, "shallow ") {
		return nil
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[2] == "" {
		return fmt.Errorf("unsupported receive-pack command %q (only <old> <new> <ref> updates are allowed)", line)
	}
	ref := parts[2]
	// Defense in depth: receive-pack refuses funny refnames server-side, but a
	// prefix test must never be the only thing between "wardyn/<id>/x" and a
	// traversal or an embedded second ref. Control characters are rejected here
	// rather than left to the forge: git's own check_refname_format would catch
	// them, but leaning on the server makes this parser's guarantee weaker than
	// it reads — an embedded LF or CR is exactly the shape that smuggles a
	// second command past a line-oriented reader.
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\\^~:?*[") {
		return fmt.Errorf("refusing malformed refname %q", ref)
	}
	if i := strings.IndexFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("refusing refname %q: control character at byte %d", ref, i)
	}
	if !strings.HasPrefix(ref, prefix) || len(ref) <= len(prefix) {
		return fmt.Errorf("push to %q is outside this run's branch namespace", ref)
	}
	return nil
}

// emitGitDecision records a brokered:git decision-log row (host github.com, port
// 443) so clone/fetch/push land in audit as allow/deny rows.
func (p *Proxy) emitGitDecision(r *http.Request, decision egress.Decision, ruleSource string) {
	if p.sink == nil {
		return
	}
	p.sink.emit(decisionLog(p.reqOf(r, githubHost, 443), decision, ruleSource))
}
