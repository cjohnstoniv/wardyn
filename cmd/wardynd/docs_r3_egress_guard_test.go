// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Doc guards for the round-3 egress fix lane. Same shape as docs_r3_guard_test.go:
// each test reads the PREMISE off the code the prose describes, then demands the
// document still carries the sentence that premise obliges. No line numbers.

import (
	"strings"
	"testing"
)

// TestApprovalScopeIsDocumentedAsPortWide (F001) pins POLICIES.md's approval
// scope table to the fact that an egress approval has no port in it anywhere.
//
// The human is shown a bare host, the proxy caches the answer under that bare
// host, and the durable `always` write goes through hostrules.ValidApprovedHost,
// which refuses a port by construction — so an approve raised by a CONNECT to
// :443 releases :22 and :5432 with no second approval. The scope table described
// reach only in TIME (one connection / this run / until T / every future run),
// so the decision a human made was strictly wider than the one they were shown.
//
// If a port ever enters this path, this guard goes red on the PREMISE and the
// paragraph has to be rewritten rather than silently outlived.
func TestApprovalScopeIsDocumentedAsPortWide(t *testing.T) {
	ap := readSrc(t, "internal", "egress", "proxy", "approvals.go")

	// (1) The raise body the human is shown carries no port.
	at := strings.Index(ap, "type egressScope struct {")
	if at < 0 {
		t.Fatal("egressScope is gone — the guard's anchor moved, so it is asserting nothing")
	}
	decl := ap[at:]
	end := strings.Index(decl, "}")
	if end < 0 {
		t.Fatal("egressScope's declaration has no closing brace — the guard's anchor moved, so it is asserting nothing")
	}
	decl = decl[:end]
	if strings.Contains(decl, "port") {
		t.Errorf("egressScope now carries a port (%q) — POLICIES.md's HOST-wide paragraph must be rewritten, not left standing", strings.TrimSpace(decl))
	}

	// (2) The proxy's first-use cache and its two entry points are keyed on the
	// host alone: no port reaches the state machine to key on.
	if !strings.Contains(ap, "hosts map[string]*hostApproval") {
		t.Error("the first-use approval cache is no longer map[string]*hostApproval — re-read POLICIES.md's port paragraph")
	}
	for _, sig := range []string{
		"func (a *approvalClient) Resolve(ctx context.Context, host string) resolveResult",
		"func (a *approvalClient) ResolveWait(ctx context.Context, host string) resolveResult",
	} {
		if !strings.Contains(ap, sig) {
			t.Errorf("missing %q — an approval that took a port would make the doc's claim false", sig)
		}
	}

	// (3) The durable `always` entry cannot carry a port even in principle.
	if !strings.Contains(readSrc(t, "internal", "api", "approvals.go"), "hostrules.ValidApprovedHost(host)") {
		t.Error("the always-scope write no longer validates through hostrules.ValidApprovedHost — the doc's permanence claim rests on it")
	}

	// (4) What the document must now say.
	doc := readDoc(t, "docs/POLICIES.md")
	mustSay(t, doc, "docs/POLICIES.md",
		"**Every scope in that table is HOST-wide — on every port.**",
		"also releases `example.org:22`, `:5432` and every other port",
		"read the scope column as *how long*, never as *how narrow*",
		// The remedy the paragraph offers has to BE one (adversarial fix-up):
		// it recommended denied_domains for "this host must not be reachable on
		// its other ports", but a bare deny entry is port-blind by the very
		// paragraph above it, so the deny takes :443 away with :22. The
		// allowlist IS port-qualifiable — that is the honest answer.
		"**There is no way to approve one port and refuse another on the same host.**",
		"`allowed_domains: [\"files.example.org:443\"]`",
	)
	mustNotSay(t, doc, "docs/POLICIES.md",
		"If a host\nmust not be reachable on its other ports, deny it (`denied_domains`)",
	)
	// The escape hatch the corrected remedy names must actually exist.
	if !strings.Contains(readSrc(t, "internal", "egress", "proxy", "policy.go"), "func classifyDomain(d string) (exact, wild string, port int)") {
		t.Error("classifyDomain's port qualifier is gone — POLICIES.md's corrected remedy (port-qualify " +
			"the allowlist entry) would then name something that does not exist")
	}
}

// TestUpstreamGuardResidualIsDocumented (F008) pins the threat model and the
// proxy package's own SECURITY INVARIANTS to what egressTarget's corp-upstream
// branch now does — and to the two residuals it deliberately keeps.
//
// That branch used to return BEFORE p.vetHost, so the "unconditional"
// private/loopback/metadata guard held only for the LITERAL spelling under an
// operator upstream: a name the agent controlled that resolved to
// 169.254.169.254 was handed to the corp proxy to resolve and dial. It now
// resolves for the guard and denies a blocked answer, forwarding only a name it
// cannot resolve at all — which the documents have to say out loud, because a
// reader who takes "unconditional" literally would be wrong about that case.
//
// TWO residuals, not one: the target is still sent BY NAME, so the guard is
// checked against THIS proxy's resolution while the corp proxy performs its own.
// A name that answers differently to the two resolvers (short-TTL rebinding, or
// a split-horizon zone only the corp proxy can see) is bound at check time only.
// The mustNotSay arms below are the other half of the fix: the retired
// "the guard is SKIPPED under an upstream" claim survived verbatim in five
// passages this lane did not first touch — the fifth being egress_target.go's
// own file header — so the guard pins its absence in the documents AND in the
// code comments rather than only pinning the replacement in one.
//
// The arms also hold the line the first replacement crossed. What this branch
// decides is whether the sidecar hands the HOSTNAME on; whether the corp proxy
// will then CONNECT to a private address is the estate's routing, which no
// code here determines. So the OPERATIONS matrix and the file header may state
// the lift and the stamp, and may not state reachability through the corp
// proxy — and the CHANGELOG's residual count must match §4.2's.
func TestUpstreamGuardResidualIsDocumented(t *testing.T) {
	// (1) The premise, read off the branch itself: it vets, and it excuses
	// exactly one denial — the unresolvable one.
	tgt := funcBody(t, readSrc(t, "internal", "egress", "proxy", "egress_target.go"), "(p *Proxy) egressTarget")
	for _, want := range []string{
		"if p.upstream != nil && !p.bypassUpstream(host) {",
		"guard := p.vetHost(host); guard.Denied && !guard.Unresolved",
	} {
		if !strings.Contains(tgt, want) {
			t.Errorf("egressTarget's upstream branch no longer contains %q — the documented residual has changed shape; re-read THREAT-MODEL.md §4.2 before trusting this guard", want)
		}
	}
	// Unresolved must stay the "we could not learn the addresses" signal, not a
	// general escape: only the two no-answer paths may set it.
	pol := readSrc(t, "internal", "egress", "proxy", "policy.go")
	if got := strings.Count(pol, "Unresolved: true"); got != 2 {
		t.Errorf(`policy.go sets Unresolved on %d paths, want 2 (resolve failed + no addresses) — a third would widen the upstream branch's excuse`, got)
	}

	// (2) What the documents must now say — and, for every passage that carried
	// the retired claim, what none of them may say again.
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	mustSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"**Under a corporate upstream the pin is relaxed, the guard is not.**",
		"so the guard binds the HOSTNAME spelling and not only the literal one `evaluate` step 0 catches",
		"**Two residuals, stated:** a name this proxy cannot resolve at all",
		"a name that answers differently to the two resolvers",
		"The one hop that relaxes the resolved-IP PIN",
		"the guard itself still runs there (§4.2) and step 0 still holds",
		"**Upstream corp-proxy hop relaxes the resolved-IP PIN, not the guard.**",
		"so the dial is not pinned to a proxy-resolved address; `egressTarget` still resolves the name for the guard and denies an answer in a blocked range",
		"Only the PIN is deferred, to the operator's own corp proxy",
	)
	mustNotSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"resolved-IP re-check is skipped for that hop",
		"Only the resolved-IP re-check is deferred",
		"**One residual, stated:**",
		"The one hop that defers the post-resolution re-check",
	)

	// docs/OPERATIONS.md is the operator-facing half of the same claim: the
	// upstream-secret disclosure and the bypass x internal_hosts matrix both
	// rested on "the guard is skipped / never runs" and both are now wrong.
	ops := readDoc(t, "docs/OPERATIONS.md")
	mustSay(t, ops, "docs/OPERATIONS.md",
		"under a configured upstream the sidecar hands the corp proxy a HOSTNAME rather than a pinned address for every host it proxies",
		"would put a member in control of which proxy resolves and dials every one of them",
		"**It changes which hop dials; the guard binds both columns.**",
		"the guard resolves the name and refuses `builtin:private-ip` before the corp proxy is asked",
		// The matrix states what the SIDECAR decides and stops there: a lift is
		// not a reachability claim, because the corp proxy's own routing is not
		// this process's to promise. The bottom-right cell — a local dial with
		// the guard lifted — is the one cell where reachability IS ours to state.
		"the guard lifts and the HOSTNAME is handed to the corp proxy (`rule_source: site-config:internal-host`); whether that proxy will `CONNECT` to a private address is the estate's own routing",
		"**reaches the endpoint** dialled directly (`rule_source: site-config:internal-host`)",
		"`internal_hosts` is the only field that lifts the guard — the bypass never",
		"lifting the guard is necessary, never sufficient, because the dial still has to",
	)
	mustNotSay(t, ops, "docs/OPERATIONS.md",
		"skips the SSRF guard for every host it proxies",
		"the guard never even runs",
		"corp proxy takes the dial, cannot reach an internal address",
		"**It changes which hop dials, and nothing else.**",
		// Retired in the same pass: both asserted an OUTCOME (the endpoint is
		// reached / only this field admits a private address) that the section
		// intro fifteen lines above and the Bedrock recipe below both deny.
		"**reaches the endpoint** through the corp proxy",
		"is the one field that admits a private address",
		// Retired with F008's guard-runs-here semantics: the section intro promised a
		// timeout for the case the matrix immediately below says is refused.
		"every one of them times out",
	)
	mustSay(t, ops, "docs/OPERATIONS.md",
		"the sidecar resolves the name for its own private/reserved-IP guard before it does",
		"a name that resolves here is refused",
	)

	// The ProxyConfig field comment is the sixth copy of the pre-F008 narrative;
	// it must describe the guard-runs-here branch and never the timeout story.
	cfg := readSrc(t, "internal", "egress", "proxy", "config.go")
	mustSay(t, cfg, "internal/egress/proxy/config.go",
		"vets the",
		"name on the upstream branch too (egressTarget)",
		"either way the bypass is what moves the dial local",
	)
	mustNotSay(t, cfg, "internal/egress/proxy/config.go",
		"every private endpoint times out",
	)

	// The lane's own release note is the one user-facing place the residual
	// count is stated; it must carry the same TWO §4.2 now carries.
	cl := readDoc(t, "CHANGELOG.md")
	mustSay(t, cl, "CHANGELOG.md",
		"with the two residuals stated in `threatmodel/THREAT-MODEL.md` §4.2: a name this proxy cannot resolve at all is forwarded unvetted",
		"a name that answers differently to this proxy and the corp proxy is bound at check time only",
	)
	mustNotSay(t, cl, "CHANGELOG.md",
		"with the one residual",
	)

	// The package doc is a Go comment, so fold the `//` markers out before
	// asking whether the sentence still reads as one sentence.
	mustSay(t, strings.Join(strings.Fields(strings.ReplaceAll(pol, "//", " ")), " "),
		"internal/egress/proxy/policy.go SECURITY INVARIANTS",
		"Under a corporate upstream the PIN is relaxed",
		"but the GUARD is not: egressTarget resolves for the guard and denies a name that answers into a blocked range",
		"Its two stated residuals are a name this proxy cannot resolve at all",
		"a name that answers differently to the two resolvers, which the guard binds only at check time",
	)
	// The branch itself and evaluate's step 4 carry the same bound, so a reader
	// who never opens the threat model still sees both residuals.
	fold := func(src string) string { return strings.Join(strings.Fields(strings.ReplaceAll(src, "//", " ")), " ") }
	egt := fold(readSrc(t, "internal", "egress", "proxy", "egress_target.go"))
	mustSay(t, egt,
		"internal/egress/proxy/egress_target.go upstream branch",
		"the guard binds the name at CHECK time only — the corp proxy resolves again for the dial",
		// The file header's own composition paragraph, re-derived for post-F008
		// behaviour: the guard runs on the upstream branch too, InternalHosts
		// lifts it THERE, and routing stays the estate's.
		"the guard runs on BOTH branches and InternalHosts is what lifts it on either",
		"on a lift stamps site-config:internal-host on the HOSTNAME it then hands the corp proxy — which still has to be able to dial that address",
		"Bypass routes, InternalHosts admits.",
	)
	mustNotSay(t, egt,
		"internal/egress/proxy/egress_target.go upstream branch",
		// Pre-F008: the guard ran on the local-dial path only, so the header
		// could describe the upstream hop purely as a routing failure.
		"neither alone suffices",
		"cannot CONNECT to an internal address (it times out)",
	)
	mustSay(t, fold(readSrc(t, "internal", "egress", "proxy", "proxy.go")),
		"internal/egress/proxy/proxy.go evaluate step 4",
		"Two residuals, stated rather than papered over",
		"the guard binds it at CHECK time only",
	)
	mustNotSay(t, fold(readSrc(t, "internal", "egress", "proxy", "proxy.go")),
		"internal/egress/proxy/proxy.go evaluate step 4",
		"The single residual, stated rather than papered over",
	)
}

// TestBranchNSScopeRationaleMatchesThePATLane (F015) pins the two comments that
// justify confining pushes on the github_token lane only, plus docs/ENV.md's
// row, to what the git_pat lane actually is.
//
// Both said a PAT push is "an opaque CONNECT / tunnel no pkt-line parser can
// read". The never-resident git_pat lane, default ON since 0.7, removes exactly
// that tunnel — it terminates the request proxy-side and admits POST
// git-receive-pack in cleartext — so the stated impossibility was false and a
// live scoping decision was reading as a structural fact. If the lane ever stops
// admitting receive-pack, or the confinement is extended to it, this guard reds
// and the three texts get re-decided rather than silently outliving their reason.
func TestBranchNSScopeRationaleMatchesThePATLane(t *testing.T) {
	// (1) The premise: the PAT lane is brokered cleartext smart-HTTP and admits
	// the push verb, i.e. the parser COULD read it.
	pat := readSrc(t, "internal", "egress", "proxy", "pat_broker.go")
	if !strings.Contains(pat, `validGitRest(r.Method, verb, r.URL.Query().Get("service"))`) {
		t.Error("the git_pat lane no longer routes its verb through validGitRest — re-read the branch-namespace scope comments before trusting this guard")
	}
	gitb := readSrc(t, "internal", "egress", "proxy", "git_broker.go")
	if !strings.Contains(gitb, `case "git-upload-pack", "git-receive-pack":`) {
		t.Error("validGitRest no longer admits git-receive-pack — the shared premise of the scope comments has moved")
	}
	// The confinement is still wired to handleGitBroker alone; if that changes,
	// the texts below are the ones to rewrite.
	if strings.Contains(pat, "readReceivePackCommands") {
		t.Error("pat_broker.go now parses receive-pack commands — the three texts saying the confinement binds the App lane only are stale")
	}

	// (2) The rationale each text must now give, and the claim none may make again.
	fold := func(src string) string { return strings.Join(strings.Fields(strings.ReplaceAll(src, "//", " ")), " ") }
	mustSay(t, fold(gitb), "internal/egress/proxy/git_broker.go BranchNSEnforced",
		"A PAT push is NOT, and saying so was the stale half of this comment",
		"the reason it is not wired in is a DECISION",
	)
	mustNotSay(t, fold(gitb), "internal/egress/proxy/git_broker.go BranchNSEnforced",
		"a PAT push or an SSH push is an opaque tunnel no pkt-line parser can read",
	)
	mustSay(t, fold(readSrc(t, "internal", "broker", "broker.go")), "internal/broker/broker.go",
		"A git_pat push DOES traverse a brokered, cleartext smart-HTTP route since 0.7",
	)
	mustNotSay(t, fold(readSrc(t, "internal", "broker", "broker.go")), "internal/broker/broker.go",
		"SSH is not smart-HTTP; a PAT push is an opaque CONNECT",
	)
	mustSay(t, readDoc(t, "docs/ENV.md"), "docs/ENV.md",
		"Since 0.7 that is a scoping DECISION for `git_pat`, not an impossibility",
	)
}

// TestGatewayPredicateHasOneBody (F089) keeps the model-gateway refusal
// predicate structurally single, not "mirrored" by a comment.
//
// api.llmGatewayIPRefused (the boot validator) and proxy.trustedGatewayIPRefused
// (the per-request re-check on the RESOLVED answer, which is where the NAT64 arm
// earns its keep) were byte-identical bodies in two packages with nothing
// asserting they agreed — and the proxy copy's NAT64 arm was unpinned: replacing
// it with `return false` left the whole proxy suite green while a gateway name
// resolving to a NAT64-mapped 169.254.169.254 became dialable with the brokered
// model credential. Both now call ipguard.GatewayIPRefused, whose table test is
// the single pin. Re-introducing a local copy reds this.
func TestGatewayPredicateHasOneBody(t *testing.T) {
	for _, f := range []struct {
		name string
		rel  []string
	}{
		{"internal/api/llm_gateway.go", []string{"internal", "api", "llm_gateway.go"}},
		{"internal/egress/proxy/egress_target.go", []string{"internal", "egress", "proxy", "egress_target.go"}},
	} {
		src := readSrc(t, f.rel...)
		if !strings.Contains(src, "ipguard.GatewayIPRefused(ip)") {
			t.Errorf("%s no longer calls ipguard.GatewayIPRefused — the two gateway guards are coupled by that call, not by a comment", f.name)
		}
		// The tell of a re-introduced local copy: its own NAT64 arm beside its own
		// loopback/link-local arm.
		if strings.Contains(src, "ipguard.NAT64EmbeddedV4(ip)") && strings.Contains(src, "ip.IsLinkLocalUnicast()") {
			t.Errorf("%s has grown its own copy of the gateway predicate again — one body, in internal/ipguard", f.name)
		}
	}
	if !strings.Contains(readSrc(t, "internal", "ipguard", "ipguard.go"), "func GatewayIPRefused(ip net.IP) bool {") {
		t.Fatal("ipguard.GatewayIPRefused is gone — this guard is asserting nothing")
	}
}

// TestPATPushIsNotDocumentedAsAnImpossibility (F121) pins the THREAT-MODEL's
// half of the branch-namespace scope claim to the same premise its code sibling
// is pinned to above.
//
// The document justified the absent git_pat push confinement with an
// impossibility — "a `git_pat` push is an opaque CONNECT ... so no receive-pack
// parser can bind either". The 0.7 PAT broker falsifies that for git_pat: the
// sandbox->proxy hop is cleartext on the proxy's own /wardyn/git/ route,
// handlePATBroker terminates it, and validGitRest admits POST git-receive-pack —
// the identical shape readReceivePackCommands parses on the App lane. A stated
// impossibility that is really a scoping decision is the worst kind of drift in
// a threat model: it tells a reader no choice was ever available.
func TestPATPushIsNotDocumentedAsAnImpossibility(t *testing.T) {
	// (1) The premise: the git_pat lane terminates the request itself and admits
	// the push verb, so a parser COULD bind it.
	pat := readSrc(t, "internal", "egress", "proxy", "pat_broker.go")
	for _, want := range []string{
		"func (p *Proxy) handlePATBroker(w http.ResponseWriter, r *http.Request) {",
		`validGitRest(r.Method, verb, r.URL.Query().Get("service"))`,
	} {
		if !strings.Contains(pat, want) {
			t.Errorf("pat_broker.go no longer contains %q — the threat model's git_pat paragraph rests on it; re-derive the claim before trusting this guard", want)
		}
	}
	if !strings.Contains(readSrc(t, "internal", "egress", "proxy", "git_broker.go"),
		`case "git-upload-pack", "git-receive-pack":`) {
		t.Error("validGitRest no longer admits git-receive-pack — the premise of the threat model's paragraph has moved")
	}

	// (2) The document must give the real reason, and must never restate the
	// impossibility again.
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	mustSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"An `ssh_key` push is not smart-HTTP, so no",
		"receive-pack parser can bind it. A `git_pat` push is a different case since",
		"unconfined is a scoping DECISION rather than an impossibility",
	)
	mustNotSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"A `git_pat` push is an opaque CONNECT",
		"so no receive-pack parser can bind either",
	)
}

// TestAllowAllPublicOnlyClaimNamesTheUpstreamResidual (F118) keeps evalHost's
// allow-all comment honest about the one lane where its claim does not hold.
//
// The comment told a reader of the policy code that allow-all "reaches PUBLIC
// hosts only" because the unconditional IP guard is "applied later in the
// pipeline". Under a configured corporate upstream that guard now runs (F008)
// but deliberately excuses ONE denial — a name this proxy cannot resolve at all
// is forwarded to the corp proxy unvetted — and, the target being sent by name,
// binds what it does check at CHECK time only, because the corp proxy resolves
// again for the dial. So the claim is true of what the proxy can verify, not
// absolutely, and the comment a reader relies on has to name BOTH residuals:
// one is a hole in the check, the other is a hole in its lifetime.
func TestAllowAllPublicOnlyClaimNamesTheUpstreamResidual(t *testing.T) {
	pol := readSrc(t, "internal", "egress", "proxy", "policy.go")
	// (1) The premise: allow-all still returns a bare allow, and the upstream
	// branch still excuses exactly the unresolvable denial.
	if !strings.Contains(funcBody(t, pol, "(p *Policy) evalHost"), "if p.allowAll {") {
		t.Fatal("evalHost no longer has an allowAll branch — this guard is asserting nothing")
	}
	if !strings.Contains(funcBody(t, readSrc(t, "internal", "egress", "proxy", "egress_target.go"), "(p *Proxy) egressTarget"),
		"guard := p.vetHost(host); guard.Denied && !guard.Unresolved") {
		t.Error("egressTarget's upstream branch no longer excuses only the unresolvable denial — evalHost's residual sentence is stale")
	}

	// (2) What the comment must now carry.
	fold := func(src string) string { return strings.Join(strings.Fields(strings.ReplaceAll(src, "//", " ")), " ") }
	mustSay(t, fold(pol), "internal/egress/proxy/policy.go evalHost",
		"so allow-all reaches PUBLIC hosts only — with TWO residuals",
		"a name THIS proxy cannot resolve at all is forwarded to it unvetted",
		"the guard binds the name at CHECK time only — the corp proxy resolves again for the dial",
	)
	mustNotSay(t, fold(pol), "internal/egress/proxy/policy.go evalHost",
		"so allow-all reaches PUBLIC hosts only — with ONE residual",
	)
}

// TestApprovalPortBlindnessIsStatedInCode (F108) is the code-side half of the
// port-wide approval claim POLICIES.md now carries.
//
// The behaviour is a property of three places at once — the raise body the human
// sees, this cache's key, and the durable always-write's validator — so a reader
// of any one of them cannot see it. The document says it; the code that
// implements it said nothing, and a maintainer adding a port to any one of the
// three would have had no reason to go and re-read the document.
func TestApprovalPortBlindnessIsStatedInCode(t *testing.T) {
	ap := readSrc(t, "internal", "egress", "proxy", "approvals.go")
	// (1) The premise: evaluate hands Resolve a host and no port.
	if !strings.Contains(readSrc(t, "internal", "egress", "proxy", "proxy.go"), "p.approval.Resolve(ctx, host)") {
		t.Error("evaluate no longer calls Resolve(ctx, host) — if a port reaches the state machine, both the comment and POLICIES.md's paragraph must be re-decided")
	}
	// (2) What Resolve's own doc must now say.
	fold := func(src string) string { return strings.Join(strings.Fields(strings.ReplaceAll(src, "//", " ")), " ") }
	mustSay(t, fold(ap), "internal/egress/proxy/approvals.go Resolve",
		"KEYED ON THE HOST, AND ON NOTHING ELSE",
		"ONE approval releases EVERY port of that host",
		"Port scoping would therefore be a behaviour change at all three places, not a key change here",
	)
}
