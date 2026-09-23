// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// GET /healthz and everything that exists only to answer it.
//
// Carved out of server.go by seam (file-size gate), not by behaviour: every
// declaration below is byte-identical to its previous home. The seam is real
// rather than convenient — the three ebpfGroundtruth* helpers, groundtruthKinds
// and missingGroundtruthKinds have exactly one caller between them, which is
// handleHealthz, and server.go's remaining job is the Config/Server wiring they
// merely read.
//
// The reason this handler earns its own file at all: /healthz is ANONYMOUS
// (routes.go, classAnonymous in authz_test.go's matrix), so every field added
// here is a disclosure to an unauthenticated caller, and the honesty rules the
// eBPF verdict follows — "healthy" only while events actually arrive, never a
// claim the stream cannot back — are easier to hold in one place than scattered
// through the server's constructor.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// ebpfHeartbeatTTL is how recent the most recent kernel.sensor.heartbeat must
// be for /healthz to report ebpf_groundtruth=healthy. Past it, the stream is
// degraded; with no heartbeat ever, it is unavailable. This makes the overclaim
// structurally impossible: the stream is "healthy" only while events arrive.
const ebpfHeartbeatTTL = 2 * time.Minute

// handleHealthz reports liveness plus the identity provider name so the trust
// boundary (embedded vs spire) is always visible to operators and the UI.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	idp := ""
	if s.cfg.Identity != nil {
		idp = s.cfg.Identity.Name()
	}
	runnerName := ""
	caps := []types.ConfinementClass(nil)
	var substrates map[types.ConfinementClass]string
	netpolVerdict := ""
	if s.cfg.Runner != nil {
		runnerName = s.cfg.Runner.Name()
		if c, err := s.cfg.Runner.Capabilities(r.Context()); err == nil {
			caps = c.ConfinementClasses
			substrates = c.Resolved
			netpolVerdict = k8sNetpolVerdict(runnerName, c)
		}
	}
	body := map[string]any{
		"status": "ok",
		// version is the daemon's own build. It is a DELIBERATE disclosure on the
		// anonymous /healthz: a support issue or a CLI/server skew after a rolling
		// upgrade has to be answerable without a credential, and the sign-in screen
		// reads /healthz before anyone is authenticated. The capability enumeration
		// below is NOT "admin-gated on /setup/status" — that endpoint is
		// classMember, and the class list is member-visible there too; what
		// redactSetupStatusForMember withholds is the operator DETAIL (driver name,
		// per-class substrates, the ephemeral-disk enforcement word). What keeps
		// THIS endpoint honest is that it composes its body field by field, so a
		// field added to the setup status never appears here by accident.
		"version": version.Version,
		// sso reports whether the OIDC login flow is mounted (/auth/login). The
		// sign-in screen reads it BEFORE anyone is authenticated to decide whether to
		// offer the SSO link — without it the console has no usable sign-in at all in
		// the OIDC-configured, admin-token-empty deployment wardynd itself suggests
		// (cmd/wardynd: "Set WARDYN_ADMIN_TOKEN, enable OIDC, or use -local-mode").
		// One bit, no configuration detail: it discloses nothing /auth/login's own
		// presence does not.
		"sso": s.cfg.OIDC != nil,
		// token_login (#378/#379) is whether the sign-in screen should offer the
		// admin-token form at all: a token is actually configured, and neither
		// sso_only nor member mode is set. Member mode's admin token is a PROCESS
		// credential (deploy/desktop/wardyn.env.m-prime.example), not a human
		// sign-in path, and sso_only's whole point is that the token is not a
		// second way in — either one makes the form something that cannot work,
		// which is exactly the disclosure this bit exists to prevent (no store
		// read, like every other field here).
		"token_login": s.cfg.AdminToken != "" && !s.cfg.SSOOnly && !s.cfg.MemberMode,
		// sso_only mirrors WARDYN_SSO_ONLY, enforced at boot by
		// validateSSOOnlyPosture (cmd/wardynd/boot_posture.go) — true here only
		// when OIDC is configured and every other way in (admin token, local
		// mode, member mode, the no-operator-list override) was refused, so the
		// sign-in screen can safely drop SIGNIN.ROLE_SOURCE's "everyone is an
		// admin" caveat: that branch of role derivation is unreachable here.
		"sso_only":          s.cfg.SSOOnly,
		"identity_provider": idp,
		"trust_domain":      s.cfg.TrustDomain,
		"runner":            runnerName,
		// confinement_classes are the enforceable isolation LEVELS; the
		// confinement_substrates map names WHICH runtime backs each (e.g.
		// "CC3":"oci/kata-qemu") — honest visibility into the pluggable substrate.
		// confinement_names is the static CC-code -> friendly-tier-name dictionary
		// (types.ConfinementClassNames, mirroring the UI's cc-meta.ts) so a
		// scriptable consumer can learn "CC1" means "Fence" without hardcoding it.
		"confinement_classes":    caps,
		"confinement_substrates": substrates,
		"confinement_names":      types.ConfinementClassNames,
		// components reports the SELECTED pluggable-component impl per seam, plus
		// what this build's registries actually hold. Runtime facts only.
		"components": s.cfg.Components,
		// ebpf_groundtruth is the honest health of the SECOND audit stream. It
		// is driven by the most recent kernel.sensor.heartbeat: healthy only when
		// beats are fresh AND real kernel events have been observed, idle when the
		// sidecar is alive but blind (no events), degraded if the beat is stale,
		// unavailable if no sensor has ever beaten. The overclaim ("we have eBPF
		// ground truth") is structurally impossible — healthy reflects real events.
		"ebpf_groundtruth": ebpfGroundtruthPublic(s.ebpfGroundtruthStatus(r.Context())),
		// llm_egress_inspection advertises that the OPTIONAL outbound content-
		// inspection capability is built in. Whether a given run actually scans
		// (and in which mode) is per-run policy (RunPolicySpec.LLMInspection),
		// and per-decision coverage is reported on the egress decision/audit
		// stream (scanned / tunneled-opaque / llm.scan.blind), not here.
		"llm_egress_inspection": "available",
		// ssh discloses the gateway's presence + the two facts the run-detail
		// "Connect via SSH" pane needs to render its command/config block before
		// a human is authenticated (anonymous, like every other /healthz field):
		// advertise_addr (WARDYN_SSH_ADVERTISE, purely advisory copy) and the
		// host key's SHA256 fingerprint ("verify on first connect" — Public by
		// design, it identifies the server, it authenticates no one; see
		// docs/SSH.md). nil (renders as JSON null) when the gateway is
		// disabled — the smallest honest wire change: no new endpoint, one
		// field a deployment without SSH simply omits populating.
		"ssh": s.sshGatewayHealthz(),
		// ui_sandbox discloses the UI-sandbox gateway's presence and the ONE
		// field the console needs to open a declared app: the enter-URL
		// template on the gateway's own origin (the console must never build
		// that origin itself — a different origin is the whole point). nil
		// (JSON null) when the gateway is off, the same "a deployment without
		// it simply omits the block" shape as ssh above.
		"ui_sandbox": s.uiSandboxHealthz(),
		// demo_video_base_url is WARDYN_DEMO_VIDEO_BASE_URL — already validated
		// at boot by ValidateDemoVideoBaseURL — beside the ui_sandbox advisory
		// block above: the console's episodeUrl (demo-videos.ts) reads it off
		// this same /healthz poll to build the Getting Started episode
		// download URL, instead of the hardcoded github.com it falls back to.
		// "" (the default, unset) is the honest "no mirror configured" answer,
		// not an omitted key — unlike ssh/ui_sandbox, there is no second field
		// this one would need to appear alongside, so there is nothing an
		// absent key would need to hide.
		"demo_video_base_url": s.cfg.DemoVideoBaseURL,
		// proxy_hop_tls: every run's proxy reaches this daemon over TLS pinned to
		// its internal CA (internal/hoptls). false only on a local install whose
		// control-plane URL is loopback http. One bit, no address or cert detail.
		"proxy_hop_tls": s.cfg.ControlPlaneCAPEM != "",
	}
	// network_policy is k8sNetpolVerdict's "enforced"/"unenforced"/"acknowledged"
	// grade, present ONLY on a k8s substrate — omitted from the map entirely
	// (not even a JSON null) on Docker and every other driver, so a Docker
	// deployment's /healthz shape never sprouts a k8s-only field.
	if netpolVerdict != "" {
		body["network_policy"] = netpolVerdict
	}
	writeJSON(w, http.StatusOK, body)
}

// ebpfGroundtruthStatus reports the eBPF/Tetragon ground-truth stream's health
// from the latest kernel.sensor.heartbeat:
//
//	unavailable — no heartbeat ever (no sensor configured on this host)
//	degraded    — last heartbeat older than ebpfHeartbeatTTL (sensor stalled/dead)
//	idle        — heartbeat fresh but observed_total==0: the sidecar process is
//	              alive and reachable, yet has mapped ZERO kernel events (sensor
//	              blind, or the run is genuinely quiet) — NOT proof of ground truth
//	healthy     — heartbeat fresh AND real kernel events observed (ground truth flowing)
//
// A live heartbeat alone only proves the sidecar PROCESS is alive; "healthy"
// additionally requires observed kernel events, so the "we have eBPF ground
// truth" overclaim is structurally impossible. last_heartbeat is the RFC3339
// time of the most recent beat (omitted if none). dropped_total/observed_total/
// dropped_unmapped are the sensor-reported counts carried on the heartbeat's
// data (0 when absent); dropped_unmapped separates a blind sensor from a broken
// correlation — see the idle branch. When no Store is wired (tests), reports
// unavailable.
func (s *Server) ebpfGroundtruthStatus(ctx context.Context) map[string]any {
	out := map[string]any{"state": "unavailable", "dropped_total": uint64(0)}
	if s.cfg.Store == nil {
		return out
	}
	ev, err := s.cfg.Store.LatestAuditEventByAction(ctx, groundtruth.ActionSensorHeartbeat)
	if err != nil {
		// ErrNotFound (no sensor ever) or any query error: report unavailable.
		return out
	}
	out["last_heartbeat"] = ev.Time.UTC().Format(time.RFC3339)
	// dropped_total and observed_total are published by the sensor on the
	// heartbeat data when available; tolerate their absence.
	var hb struct {
		DroppedTotal    uint64            `json:"dropped_total"`
		ObservedTotal   uint64            `json:"observed_total"`
		DroppedUnmapped uint64            `json:"dropped_unmapped"`
		ObservedByKind  map[string]uint64 `json:"observed_by_kind"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &hb)
	}
	out["dropped_total"] = hb.DroppedTotal
	out["observed_total"] = hb.ObservedTotal
	out["dropped_unmapped"] = hb.DroppedUnmapped
	if len(hb.ObservedByKind) > 0 {
		out["observed_by_kind"] = hb.ObservedByKind
	}
	switch {
	case s.cfg.Now().Sub(ev.Time) > ebpfHeartbeatTTL:
		// Heartbeat stale: the sensor process itself has stalled/died.
		out["state"] = "degraded"
	case hb.ObservedTotal == 0:
		// Process alive and beating, but it has mapped ZERO kernel events: the
		// sensor is blind (Tetragon dead / wrong export path / no TracingPolicy)
		// or the run is genuinely idle. Either way there is no ground truth yet,
		// so report "idle" with a reason rather than the "healthy" overclaim.
		//
		// The two idle causes are NOT the same failure and must not read the
		// same: dropped_unmapped>0 means the sensor saw kernel events and could
		// not bind ANY of them to a run — a broken correlation, which no amount
		// of waiting fixes.
		out["state"] = "idle"
		out["reason"] = "no kernel events observed"
		if hb.DroppedUnmapped > 0 {
			out["reason"] = fmt.Sprintf("kernel events observed but none correlated to a run (%d dropped as unmapped)", hb.DroppedUnmapped)
		}
	default:
		// A single aggregate over every kernel event kind would let a sensor
		// seeing only process.exec (a mis-scoped TracingPolicy that never fires
		// for network.connect or file.write, say) report healthy identically to
		// one seeing all three. When the sensor publishes the per-kind breakdown, require
		// EVERY known kind to have arrived at least once; report "partial"
		// (not the "healthy" overclaim) and name what's missing otherwise. An
		// older sensor build that has not upgraded to publish
		// observed_by_kind at all (empty map) cannot be assessed this way —
		// fall back to the aggregate-only "healthy" rather than downgrading
		// on missing DATA rather than a missing EVENT KIND.
		if missing := missingGroundtruthKinds(hb.ObservedByKind); len(hb.ObservedByKind) > 0 && len(missing) > 0 {
			out["state"] = "partial"
			out["missing_kinds"] = missing
		} else {
			out["state"] = "healthy"
		}
	}
	return out
}

// ebpfGroundtruthPublicFields is what the ANONYMOUS /healthz may publish from
// the ground-truth block: the verdict, when it was last beaten, why it is not
// healthy, and which kernel event kinds are missing.
//
// The cumulative counters (observed_total, dropped_total, dropped_unmapped,
// observed_by_kind) are deliberately NOT here. /metrics is operator-gated with
// the reason "a member … would learn operational volumes", and this endpoint is
// reachable with no credential at all — publishing them here would hand the
// fleet's kernel event volume to anyone who could open the port. The counters stay on the
// gated scrape; the dropped_unmapped COUNT still reaches an operator here
// inside the idle `reason` sentence, which is the form deploy/compose/README.md
// points them at.
var ebpfGroundtruthPublicFields = []string{"state", "last_heartbeat", "reason", "missing_kinds"}

// ebpfGroundtruthPublic projects the full status down to the anonymous view.
// It is a projection rather than a second computation on purpose: one function
// decides what "healthy" means (ebpfGroundtruthStatus), and the two surfaces
// can never disagree about the verdict — only about how much of it they show.
func ebpfGroundtruthPublic(status map[string]any) map[string]any {
	out := make(map[string]any, len(ebpfGroundtruthPublicFields))
	for _, k := range ebpfGroundtruthPublicFields {
		if v, ok := status[k]; ok {
			out[k] = v
		}
	}
	return out
}

// writeEbpfGroundtruthCounters emits the sensor's cumulative counts on the
// OPERATOR-GATED scrape rather than the anonymous /healthz: publishing them
// there would be the fleet-volume disclosure /metrics is gated to prevent;
// keeping them here loses nobody anything, it just requires the credential
// every other volume series already requires.
//
// Read off the same heartbeat /healthz reads, so the two can never disagree.
// Omitted entirely when no sensor has ever beaten (state "unavailable"): a
// deployment with no eBPF sidecar carries no dead series.
func (s *Server) writeEbpfGroundtruthCounters(ctx context.Context, w io.Writer) {
	status := s.ebpfGroundtruthStatus(ctx)
	if status["state"] == "unavailable" {
		return
	}
	num := func(k string) uint64 {
		v, _ := status[k].(uint64)
		return v
	}
	fmt.Fprintf(w, "# HELP wardyn_groundtruth_observed_total Kernel events the eBPF sensor mapped to a run since it started.\n"+
		"# TYPE wardyn_groundtruth_observed_total counter\nwardyn_groundtruth_observed_total %d\n", num("observed_total"))
	fmt.Fprintf(w, "# HELP wardyn_groundtruth_dropped_total Kernel events the eBPF sensor dropped.\n"+
		"# TYPE wardyn_groundtruth_dropped_total counter\nwardyn_groundtruth_dropped_total %d\n", num("dropped_total"))
	fmt.Fprintf(w, "# HELP wardyn_groundtruth_dropped_unmapped_total Kernel events the sensor saw but could bind to no run — a broken correlation, not a blind sensor.\n"+
		"# TYPE wardyn_groundtruth_dropped_unmapped_total counter\nwardyn_groundtruth_dropped_unmapped_total %d\n", num("dropped_unmapped"))
	// Closed set, filtered BEFORE the header so the family is emitted only when
	// it has samples. This is a correctness guard, not tidiness: the map keys
	// come straight out of the sensor's heartbeat audit row — a component that
	// is not the control plane — and %q escapes a tab as \t, a control byte as
	// \xNN and invalid UTF-8 as a \u escape, none of which the Prometheus text
	// format accepts in a label value (only \\, \n and \"). ONE odd byte from
	// the sensor would be a parse error that fails the WHOLE scrape, taking
	// every wardyn_* series with it — including the store and auth gauges an
	// operator is paging on. It bounds label cardinality too, and
	// missingGroundtruthKinds already treats groundtruthKinds as closed.
	byKind, _ := status["observed_by_kind"].(map[string]uint64)
	kinds := make([]string, 0, len(groundtruthKinds))
	for _, k := range slices.Sorted(maps.Keys(byKind)) {
		if slices.Contains(groundtruthKinds, k) {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return
	}
	fmt.Fprint(w, "# HELP wardyn_groundtruth_observed_by_kind_total Kernel events mapped to a run, by event kind.\n"+
		"# TYPE wardyn_groundtruth_observed_by_kind_total counter\n")
	for _, kind := range kinds {
		fmt.Fprintf(w, "wardyn_groundtruth_observed_by_kind_total{kind=%q} %d\n", kind, byKind[kind])
	}
}

// groundtruthKinds is the set of kernel event kinds required at least once
// before ebpfGroundtruthStatus reports "healthy" when the sensor publishes a
// per-kind breakdown at all — exec and connect only. kernel.file.write (the
// sensor's third mapped kind, cmd/wardyn-tetragon-ingest/main.go's
// processLine) is DELIBERATELY excluded: sensitive.go's own allowlist filter
// means it fires only on a write to a narrow set of credential-shaped paths
// (~/.ssh, ~/.aws, ...), so a normal capture that never happens to touch one
// is not a coverage gap — requiring it made "healthy" chronically unreachable
// and stamped a spurious "partial coverage" caveat on nearly every Record
// Mode capture. observed_by_kind still reports its count when
// the sensor does see one; it just never gates the health verdict.
var groundtruthKinds = []string{
	groundtruth.ActionProcessExec,
	groundtruth.ActionNetworkConnect,
}

// missingGroundtruthKinds returns the subset of groundtruthKinds absent or
// zero in observedByKind, in a stable order.
func missingGroundtruthKinds(observedByKind map[string]uint64) []string {
	var missing []string
	for _, k := range groundtruthKinds {
		if observedByKind[k] == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}
