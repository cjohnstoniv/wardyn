// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Pagination defaults for the public list endpoints. defaultListLimit is what an
// unparameterised caller (every existing SDK/CLI/UI list call) gets; maxListLimit
// hard-caps a client-supplied ?limit so a long-lived daemon's /runs, the Fleet
// view, and the compose healthcheck can never pull down an unbounded payload
// . A ?limit of 0 or above the max clamps to the max — never to
// "unbounded", which only internal store callers get.
const (
	defaultListLimit = 200
	maxListLimit     = 1000
)

// parseListPage reads ?limit=&offset= into a store.Page. An absent ?limit uses
// defaultLimit (each endpoint supplies its own — the list endpoints use
// defaultListLimit, the audit trail keeps its historical caps); a present one is
// hard-capped at maxListLimit. It writes a 400 and returns ok=false on a
// malformed value; callers must return immediately when ok is false.
func parseListPage(w http.ResponseWriter, r *http.Request, defaultLimit int) (store.Page, bool) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return store.Page{}, false
		}
		limit = n
	}
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return store.Page{}, false
		}
		offset = n
	}
	return store.Page{Limit: limit, Offset: offset}, true
}

// pageWindow returns items[offset : offset+limit] (clamped) and whether the full
// slice extended past that window. It is the fetch-all fallback path (test fakes
// and the approvals lister, which is not a store.Pager) — the store already
// applies LIMIT/OFFSET on the pager path.
func pageWindow[T any](items []T, offset, limit int) ([]T, bool) {
	if offset >= len(items) {
		return []T{}, false
	}
	end := offset + limit
	if end >= len(items) {
		return items[offset:], false
	}
	return items[offset:end], true
}

// servePage writes one page of a list endpoint. When pageFn is non-nil (the
// store implements store.Pager — production PG) it fetches page.Limit+1 rows at
// the DB so truncation is exact and the payload is bounded there; otherwise it
// falls back to allFn (fetch-all) + in-Go windowing for test doubles and the
// approvals lister, which is not a store.Pager. X-Wardyn-Truncated is set when a
// further page exists.
func servePage[T any](w http.ResponseWriter, page store.Page, pageFn func(store.Page) ([]T, error), allFn func() ([]T, error)) {
	var items []T
	var truncated bool
	if pageFn != nil {
		got, err := pageFn(store.Page{Limit: page.Limit + 1, Offset: page.Offset})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list: "+err.Error())
			return
		}
		items = got
		if truncated = len(items) > page.Limit; truncated {
			items = items[:page.Limit]
		}
	} else {
		got, err := allFn()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list: "+err.Error())
			return
		}
		items, truncated = pageWindow(got, page.Offset, page.Limit)
	}
	if truncated {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, items)
}

// handleListRuns returns runs in reverse creation order, paginated by
// ?limit=&offset= (see parseListPage). A member sees only runs they created
// (store.RunsByCreatorPager); an admin sees every run, unchanged.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	if !s.isOperator(r.Context()) {
		creatorPager, capable := s.cfg.Store.(store.RunsByCreatorPager)
		if !capable {
			// Fail CLOSED (never fall back to the unscoped admin listing below —
			// that would serve every run to a member): a store that cannot scope
			// by creator cannot safely answer this request at all.
			writeError(w, http.StatusInternalServerError, "run listing is not scoped for members on this store backend")
			return
		}
		principal := principalFromRequest(r)
		servePage(w, page, func(p store.Page) ([]types.AgentRun, error) {
			return creatorPager.ListRunsPageByCreator(r.Context(), principal, p)
		}, nil)
		return
	}
	var pageFn func(store.Page) ([]types.AgentRun, error)
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		pageFn = func(p store.Page) ([]types.AgentRun, error) { return pg.ListRunsPage(r.Context(), p) }
	}
	servePage(w, page, pageFn, func() ([]types.AgentRun, error) { return s.cfg.Store.ListRuns(r.Context()) })
}

// handleGetRun returns one run by id. Owner-or-admin (getRunAuthorized): a
// member reading a run they did not create gets the same 404 a missing run
// would.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	// ui_apps is a READ-ONLY denormalization of the run's EFFECTIVE policy onto
	// the run payload — the console's UI-apps lane needs it, and the run row
	// cannot answer it (agent_runs carries policy_id only, and an inline or
	// default policy has no row to fetch). Resolved from the same
	// run.policy.effective envelope the UI gateway itself trusts. A store
	// failure is logged and the field omitted rather than failing the whole run
	// read: every other field is already loaded and correct, and a missing
	// ui_apps renders the lane's "no apps declared" state, which is the
	// closed direction.
	apps, err := s.effectiveUIApps(r.Context(), id)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: effective ui_apps lookup failed", "run_id", id, "err", err)
	}
	writeJSON(w, http.StatusOK, struct {
		types.AgentRun
		UIApps []types.UIApp `json:"ui_apps,omitempty"`
	}{AgentRun: run, UIApps: apps})
}

// effectivePolicyAuditScan bounds how many of a run's earliest audit events are
// scanned for its run.policy.effective envelope. Dispatch writes that event
// before the sandbox exists, so it is always among a run's first events; the
// bound keeps a long-lived run's audit tail out of the query.
const effectivePolicyAuditScan = 200

// effectiveUIApps returns the ui_apps of the run's EFFECTIVE policy — the
// authorization envelope dispatch recorded as run.policy.effective
// (runs_dispatch.go), which is the ONLY post-hoc source of a run's real spec:
// agent_runs.policy_id has no spec column, run_policies.spec is overwritten in
// place, and an inline/default policy has no stored row at all. Resolving
// through policy_id instead would hand a run created with an INLINE policy the
// DEFAULT policy's apps — a widening this must never do.
//
// A run with no such event (never dispatched, or the audit store unavailable)
// yields no apps, so every caller fails closed on it.
func (s *Server) effectiveUIApps(ctx context.Context, runID uuid.UUID) ([]types.UIApp, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, effectivePolicyAuditScan)
	if err != nil {
		return nil, err
	}
	var apps []types.UIApp
	for _, ev := range events {
		if ev.Action != "run.policy.effective" || len(ev.Data) == 0 {
			continue
		}
		var spec types.RunPolicySpec
		if uerr := json.Unmarshal(ev.Data, &spec); uerr != nil {
			continue
		}
		apps = spec.UIApps // last wins: a re-dispatch supersedes an earlier envelope
	}
	return apps, nil
}

// resolvePolicy returns the spec + policy id to attach. When policyID is nil it
// returns the configured default with a nil id (the default is not a stored row).
func (s *Server) resolvePolicy(ctx context.Context, policyID *uuid.UUID) (types.RunPolicySpec, *uuid.UUID, error) {
	// Clone before handing the spec out. cfg.DefaultPolicy is a process-global
	// shared by every run; a shallow struct copy still shares its slice backing
	// arrays, so a caller's `append` to AllowedDomains (unionAllowedDomains, the
	// SCM/workspace egress unions) wrote into the global's spare capacity — two
	// concurrent create-runs then raced the same element, one run's egress domain
	// replacing another's in the allowlist passed to its proxy sidecar, and any
	// in-place edit (e.g. the preflight dry-run's grant filter) leaked into every
	// subsequent run. Cloning at this single seam fixes every caller at once.
	if policyID == nil {
		return s.cfg.DefaultPolicy.Clone(), nil, nil
	}
	p, err := s.cfg.Store.GetPolicy(ctx, *policyID)
	if err != nil {
		return types.RunPolicySpec{}, nil, err
	}
	pid := p.ID
	return p.Spec.Clone(), &pid, nil
}

// bestClass returns the strongest class a runner declares (slice is
// strongest-last per the Capabilities contract), or "" if none.
func bestClass(classes []types.ConfinementClass) types.ConfinementClass {
	best := types.ConfinementClass("")
	for _, c := range classes {
		if c.Rank() > best.Rank() {
			best = c
		}
	}
	return best
}

// classesOrNone renders an advertised confinement set for error messages, or
// "none" when the runner advertises no class.
func classesOrNone(classes []types.ConfinementClass) string {
	if len(classes) == 0 {
		return "none"
	}
	parts := make([]string, len(classes))
	for i, c := range classes {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

// agentImage resolves an agent name to its OCI image. images is consulted first
// (operator-provided map from WARDYN_AGENT_IMAGES); when the agent is absent or
// the map is nil, the ghcr convention image is used as fallback, keyed by the
// harness catalog's ImageKey when agent names a known harness (harness.go) or
// by the raw agent string otherwise — identical to the convention every agent
// used before the catalog existed, since every shipped row's ImageKey equals
// its ID.
func agentImage(agent string, images map[string]string) string {
	if ref, ok := images[agent]; ok {
		return ref
	}
	key := agent
	if def, ok := harnessByID(agent); ok && def.ImageKey != "" {
		key = def.ImageKey
	}
	return "ghcr.io/cjohnstoniv/agent-" + key + ":" + agentImageTag(version.Version)
}

// agentImageTag picks the tag for a convention agent image: the daemon's OWN
// version (images are published per-semver, so a 0.6 daemon pulls a 0.6 agent)
// rather than a floating ":latest" that a later release re-points under a
// version-pinned fleet's feet. ":latest" is the last resort only when the build
// carries no version string.
func agentImageTag(v string) string {
	if v == "" {
		return "latest"
	}
	return v
}

// primaryWorkspacePath returns the run's first local host workspace mount source
// (the directory the agent operates in), or "" for a git-clone / ephemeral run.
func primaryWorkspacePath(spec types.RunPolicySpec) string {
	for _, m := range spec.WorkspaceMounts {
		if strings.TrimSpace(m.Source) != "" {
			return m.Source
		}
	}
	return ""
}

// resourceLimitsToRunner maps a policy's optional ResourceLimits onto the runner
// spec. A nil policy block (or a zero field) yields the zero value, which the
// docker driver fills with conservative platform defaults — so EVERY run is
// CPU/memory/PID capped even when a policy sets nothing (C5: fleet safety).
func resourceLimitsToRunner(rl *types.ResourceLimits) runner.Resources {
	if rl == nil {
		return runner.Resources{}
	}
	return runner.Resources{
		CPUMillis: int64(rl.CPUMillis),
		MemoryMiB: int64(rl.MemoryMiB),
		PidsLimit: int64(rl.PidsLimit),
		DiskMiB:   int64(rl.DiskMiB),
	}
}

// adminTokenPrincipal is the actor recorded for an admin-bearer-token caller
// with no verified human identity (no LocalMode operator, no OIDC session). It
// is a NON-HUMAN, mechanism-named principal so the audit never implies a named
// person acted when only the shared token did.
const adminTokenPrincipal = "admin-token"

// principalFromRequest derives the actor name for an admin-gated action. It is
// the name half of actorFromRequest; callers that also record actor_type should
// prefer actorFromRequest so a token action is not mislabeled as human.
func principalFromRequest(r *http.Request) string {
	_, name := actorFromRequest(r)
	return name
}

// actorTypeFromRequest is the actor-type half of actorFromRequest, for audit
// sites that already pass principalFromRequest(r) for the name. Pairing them as
// (actorTypeFromRequest(r), principalFromRequest(r)) records a bare admin-token
// caller as system, not a forged human, without threading a local through every
// handler.
func actorTypeFromRequest(r *http.Request) types.ActorType {
	t, _ := actorFromRequest(r)
	return t
}

// actorFromRequest resolves the audit actor (type + name) for an admin-gated
// public-API action. Resolution order, strongest attribution first (invariant 4):
//
//  1. LOCAL HOST MODE — the operator injected by humanOrAdminAuth on the trusted
//     single-dev machine. Here (and ONLY here, off SSO) the X-Wardyn-Principal
//     DEV-ONLY override (docs/sdk.md) is honored, since the machine is trusted;
//     otherwise the configured operator (e.g. "local:alice") is used.
//  2. A VERIFIED OIDC SSO session (the IdP "sub"), published by humanOrAdminAuth.
//     A real human already won, so the X-Wardyn-Principal header is moot.
//  3. Admin bearer token, NO verified human. Attributed to a non-human
//     system actor ("admin-token").
//
// SECURITY (FIX #10 — human-attribution forgery): X-Wardyn-Principal is
// attacker-controllable and is documented as a DEV-ONLY override. It is
// therefore trusted ONLY in LocalMode (case 1). For a plain admin-token caller
// (case 3) the header is IGNORED: honoring it would let any WARDYN_ADMIN_TOKEN
// bearer forge a named human as decided_by / sub / sponsor in the append-only
// audit — recording that "alice@example.com approved" a credential that was in
// fact never human-gated (breaks invariant 4 per-run identity and invariant 6
// non-repudiation). A token action is recorded as system/admin-token, not human.
func actorFromRequest(r *http.Request) (types.ActorType, string) {
	// Attach-ticket auth (ticketOrHumanAuth): the ticket carries the actor that
	// MINTED it through the normal authenticated surface — strongest available
	// attribution for a WS handshake that cannot carry a credential itself.
	if ta, ok := ticketActorFromContext(r.Context()); ok {
		return ta.actorType, ta.principal
	}
	if op := localPrincipalFromContext(r.Context()); op != "" {
		if h := r.Header.Get("X-Wardyn-Principal"); h != "" {
			return types.ActorHuman, h
		}
		return types.ActorHuman, op
	}
	if sub := oidcHumanFromContext(r.Context()); sub != "" {
		return types.ActorHuman, sub
	}
	return types.ActorSystem, adminTokenPrincipal
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// withUnpublishedImageWarning appends an explanation when the agent's image
// resolves to the ghcr convention ref for something Wardyn does not publish.
//
// agentImage() falls back to ghcr.io/cjohnstoniv/agent-<key>:<version> for any
// agent with no WARDYN_AGENT_IMAGES entry. That is right for every image we ship,
// but agent-claude-code bundles Anthropic's Claude Code CLI — not open-source,
// and its terms are not even readable from inside the image — so this project does
// not redistribute it. Without this the operator's first symptom is a registry 404
// on a tag that looks like it should exist.
//
// An explicit override means the operator supplied their own image; say nothing.
func withUnpublishedImageWarning(warnings []string, agent string, images map[string]string) []string {
	if _, explicit := images[agent]; explicit {
		return warnings
	}
	key := agent
	if def, ok := harnessByID(agent); ok && def.ImageKey != "" {
		key = def.ImageKey
	}
	if key != "claude-code" {
		return warnings
	}
	return append(warnings, "Wardyn does not publish an agent-claude-code image: it bundles Anthropic's Claude Code CLI, "+
		"which is not open-source and whose terms are not readable from the image. "+
		"Build it locally with `make agent-images` — you then install that CLI under your own agreement with Anthropic — "+
		"or set WARDYN_AGENT_IMAGES to an image you built. See deploy/images/THIRD-PARTY-TERMS.md")
}
