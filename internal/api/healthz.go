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
	"net/http"
	"strings"
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
		// anonymous /healthz (unlike the capability enumeration below, which is
		// admin-gated on /setup/status): a support issue or a CLI/server skew after
		// a rolling upgrade has to be answerable without a credential, and the
		// sign-in screen reads /healthz before anyone is authenticated.
		"version": version.Version,
		// sso reports whether the OIDC login flow is mounted (/auth/login). The
		// sign-in screen reads it BEFORE anyone is authenticated to decide whether to
		// offer the SSO link — without it the console has no usable sign-in at all in
		// the OIDC-configured, admin-token-empty deployment wardynd itself suggests
		// (cmd/wardynd: "Set WARDYN_ADMIN_TOKEN, enable OIDC, or use -local-mode").
		// One bit, no configuration detail: it discloses nothing /auth/login's own
		// presence does not.
		"sso":               s.cfg.OIDC != nil,
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
		"ebpf_groundtruth": s.ebpfGroundtruthStatus(r.Context()),
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
		// host key's SHA256 fingerprint ("verify on first connect" — PUBLIC BY
		// DESIGN, it identifies the server, it authenticates no one; see
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
		// not bind ANY of them to a run (correlation broken — the 0.6 frozen-
		// counter defect), which no amount of waiting fixes.
		out["state"] = "idle"
		out["reason"] = "no kernel events observed"
		if hb.DroppedUnmapped > 0 {
			out["reason"] = fmt.Sprintf("kernel events observed but none correlated to a run (%d dropped as unmapped)", hb.DroppedUnmapped)
		}
	default:
		// W20-W20-groundtruth-mapper-4: "healthy" used to be one aggregate over
		// every kernel event kind — a sensor seeing only process.exec (a
		// mis-scoped TracingPolicy that never fires for network.connect or
		// file.write, say) reported healthy identically to one seeing all
		// three. When the sensor publishes the per-kind breakdown, require
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

// ebpfGroundtruthCaveat is the one-line, human-readable note a Record Mode
// capture (RecordTaskResult.Caveats, reconcileRecordRun) and a synthesized
// profile (profileResponse.Warnings, handleSynthesizeProfile) each stamp for
// every non-healthy sensor state — the SAME state ebpfGroundtruthStatus
// reports on /healthz, read through the one function so the two surfaces can
// never disagree about what "healthy" means (W20-W20-groundtruth-mapper-4:
// before this, the per-kind coverage state existed ONLY on the admin-only
// /healthz endpoint — nowhere an operator reviewing a capture or a
// synthesized profile would ever see it). "" (no caveat) when the sensor is
// fully healthy — there is nothing to warn about.
func (s *Server) ebpfGroundtruthCaveat(ctx context.Context) string {
	status := s.ebpfGroundtruthStatus(ctx)
	switch status["state"] {
	case "unavailable":
		return "kernel ground truth: unavailable — no eBPF sensor heartbeat was ever observed for this run; " +
			"proxy egress decisions are the only signal behind this capture"
	case "degraded":
		return "kernel ground truth: degraded — the eBPF sensor's heartbeat is stale; kernel-level coverage " +
			"for this capture may be incomplete"
	case "idle":
		return "kernel ground truth: idle — the eBPF sensor is alive but mapped zero kernel events; " +
			"this capture has no kernel-level corroboration"
	case "partial":
		missing, _ := status["missing_kinds"].([]string)
		return "kernel ground truth: partial — the eBPF sensor never observed " + strings.Join(missing, ", ") +
			"; this capture's kernel-level corroboration is incomplete"
	default: // "healthy", or absent (tests with no Store — same as unavailable, but there's no run to caveat)
		return ""
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
// Mode capture (bug-audit-1). observed_by_kind still reports its count when
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
