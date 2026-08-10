// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/gitremote"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composerWorkspaceTarget is the in-sandbox path a local-directory workspace is
// bind-mounted at — the agent's working dir (matches the New Run wizard).
const composerWorkspaceTarget = "/home/agent/work"

// The compose wire DTOs (composeRequest, clarifyResponse, composeProposed,
// composeResponse, composeLLMAccess) and the composeModeSkip/composeModeAlways
// constants live in compose_types.go — split out to keep this file (the
// pipeline + its helpers) under the repo's file-size gate.

// handleComposeRun is the AI Run Composer endpoint. It is advisory only: it
// returns a PROPOSED run setup for a human to review and approve through the
// normal create-run path — it never creates a run or mints a credential.
//
// Security posture (see internal/composer): the analyzer input is UNTRUSTED, so
// (1) the proposal is clamped to the operator's DefaultPolicy ceiling, (2) the
// clamped spec is validated through the SAME chokepoint as an inline policy, and
// (3) risk is graded DETERMINISTICALLY from the clamped spec — a prompt-injected
// attachment can neither exceed operator policy nor lower its own graded risk.
func (s *Server) handleComposeRun(w http.ResponseWriter, r *http.Request) {
	if s.composerEnabledOrNotFound(w) {
		return
	}
	// Bound the request body before reading (defense-in-depth atop ValidateRequest).
	r.Body = http.MaxBytesReader(w, r.Body, composer.MaxTotalInputBytes+64*1024)
	var req composeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "compose request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid compose request: "+err.Error())
		return
	}

	// Normalize the workspace selection: Workspaces (multi-select) wins; else fall
	// back to the single legacy Workspace. After this, req.Workspace is the PRIMARY
	// (drives the analyzer, git-detect, and grounding) and req.Workspaces is the full
	// list (every entry mounted/cloned by applyWorkspaces).
	if len(req.Workspaces) == 0 {
		req.Workspaces = []composer.Workspace{req.Workspace}
	} else {
		req.Workspace = req.Workspaces[0]
	}

	// ALL pre-flush validation runs here so it returns a REAL HTTP status in both
	// transports (buffer and SSE): once SSE writes its 200 header, no later error
	// can change the status. Only backend (LLM) failures INSIDE the pipeline differ
	// by transport (buffer 5xx vs. an EvError frame).
	if err := composer.ValidateRequest(composer.ComposeRequest{
		Prompt: req.Prompt, Workspace: req.Workspace, Attachments: req.Attachments,
		Sources: req.Sources, Transcript: req.Transcript, Round: req.Round,
		SessionID: req.SessionID,
	}); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, composer.ErrInputTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeError(w, code, err.Error())
		return
	}
	// Unknown backend is a 400 that must surface as a real status in BOTH
	// transports, so resolve it BEFORE any SSE flush — a cheap registry lookup, no
	// backend/LLM call (the in-pipeline Propose/Clarify guard stays as belt-and-
	// suspenders and yields the same message).
	if !composerBackendKnown(s.cfg.Composer, req.Backend) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %q", composer.ErrUnknownBackend, req.Backend).Error())
		return
	}
	// Same eager integration_id check create-run applies (runs_create.go):
	// fail loud on a typo or a non-ai_provider id before any pipeline stage
	// runs, in BOTH transports.
	if req.IntegrationID != "" {
		if in, ok := s.resolveIntegrationRef(r.Context(), req.IntegrationID); !ok || in.Category != types.IntegrationAIProvider {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("integration_id %q does not name an ai_provider integration", req.IntegrationID))
			return
		}
	}

	ctx := r.Context()
	principalType, principal := actorFromRequest(r)

	// SSE transport (opt-in via Accept: text/event-stream): one `data: <json>\n\n`
	// frame per emitted event, flushed as the synchronous pipeline runs. All 4xx
	// validation above already ran, so the 200 header is safe; a pipeline error now
	// becomes a terminal EvError frame (the status can no longer change).
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming not supported by this server")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		// The pipeline is synchronous and BLOCKS SILENTLY during the model-analysis
		// stages (the "propose" step can take 15-20s with no events). A bare SSE
		// comment heartbeat every few seconds keeps the connection from being reaped
		// by an idle-read timeout in an intermediary (the WSL2 localhost relay, an
		// nginx/cloudflare proxy, ...) — otherwise the terminal result frame arrives
		// after the connection is already gone and the client sees "stream ended
		// without a result". Comment lines (": ...\n\n") are ignored by EventSource.
		// A mutex serialises the heartbeat goroutine's writes with emit()'s.
		var wmu sync.Mutex
		emit := func(ev composer.ComposeEvent) {
			b, _ := json.Marshal(ev)
			wmu.Lock()
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
			wmu.Unlock()
		}
		stop := make(chan struct{})
		var hbWG sync.WaitGroup
		hbWG.Add(1)
		go func() {
			defer hbWG.Done()
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					wmu.Lock()
					_, _ = w.Write([]byte(": keepalive\n\n"))
					flusher.Flush()
					wmu.Unlock()
				}
			}
		}()
		_, cerr := s.runComposePipeline(ctx, req, principalType, principal, emit)
		close(stop)
		hbWG.Wait() // no writes to w after the handler returns
		if cerr != nil {
			// Carry cerr.status too: the frame is post-flush so it cannot BE the
			// status, but it is the SAME status the buffer transport writes below,
			// so both transports report one failure identically instead of the
			// stream silently downgrading every refusal to a backend failure.
			emit(composer.ComposeEvent{Type: composer.EvError, Error: cerr.msg, Status: cerr.status})
		}
		return
	}

	// Buffer transport (default; CLI/tests): discard the progress events and write
	// the terminal payload EXACTLY as before — byte-identical response. A pipeline
	// error maps to the same HTTP error the pre-refactor handler returned.
	result, cerr := s.runComposePipeline(ctx, req, principalType, principal, func(composer.ComposeEvent) {})
	if cerr != nil {
		writeError(w, cerr.status, cerr.msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// composeError carries an HTTP status alongside a compose-pipeline failure. The
// buffer transport renders it as writeError(status, msg) — byte-identical to the
// pre-refactor handler; the SSE transport (post-flush, status already 200) turns
// it into an EvError frame.
type composeError struct {
	status int
	msg    string
}

// composerBackendKnown reports whether a backend name resolves in the registry
// ("" = the default). It lets handleComposeRun reject an unknown backend with a
// real 400 BEFORE any SSE flush, matching the buffer transport.
func composerBackendKnown(reg *composer.Registry, name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	return slices.ContainsFunc(reg.List(), func(b composer.BackendInfo) bool {
		return b.Name == name
	})
}

// runComposePipeline is the advisory compose pipeline, factored out of the
// handler so one body drives both transports. It emits an EvStage event before
// each stage and a terminal EvResult carrying the payload (clarify questions or
// the final proposal), records the advisory audit trail, and returns that same
// payload (for the buffer transport) or a *composeError on failure. The logic is
// a verbatim move of the old handler body; the only additions are the
// emit(EvStage) progress calls and a per-pipeline correlation id on the audit
// events.
//
// closure over the ResponseWriter (SSE) or a discard (buffer); no
// channel/goroutine — the pipeline is synchronous, so emit runs inline.
//
//nolint:funlen,gocyclo // Deliberate: the compose pipeline's stage order (compose → risk → reconcile → ceiling-clamp → review) IS the security contract; each stage already lives in its own helper and this function is the one place the sequencing can be audited top-to-bottom.
func (s *Server) runComposePipeline(ctx context.Context, req composeRequest, principalType types.ActorType, principal string, emit func(composer.ComposeEvent)) (any, *composeError) {
	// Correlation id stamped at pipeline start and echoed on every advisory audit
	// event so a compose can be tied together across rounds and to the eventual
	// run.create (time-to-launch). The client's SessionID wins when present (it
	// is the SAME id across every round of one describe-mode conversation, so
	// using it here — instead of minting a fresh id per round — is what makes the
	// audit feed reconstructable per Decision 1/7); a fallback mint keeps every
	// OLDER/non-session client (no SessionID sent) working exactly as before.
	correlationID := req.SessionID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "validate"})
	creq := composer.ComposeRequest{
		Prompt: req.Prompt, Workspace: req.Workspace, Attachments: req.Attachments,
		Sources: req.Sources, Transcript: req.Transcript, Round: req.Round,
		SessionID: req.SessionID,
	}

	// For a LOCAL workspace, DETERMINISTICALLY detect the directory's real git
	// remotes (read-only, no subprocess) so the GitHub grant is grounded on
	// reality, not an LLM guess. Detect once: inform the analyzer (so it doesn't
	// invent a repo) AND enforce on the proposal below.
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "detect"})
	var detectedGitHub, detectedOther []string
	if req.Workspace.Kind == composer.WorkspaceLocal && strings.TrimSpace(req.Workspace.Path) != "" {
		detectedGitHub, detectedOther = gitremote.DetectGitHubRepos(req.Workspace.Path)
		creq.WorkspaceGitHubRepos = detectedGitHub
		creq.WorkspaceOtherRemotes = detectedOther
	}

	// INTERACTIVE CLARIFY STEP (advisory, zero-authority): unless the operator chose
	// to skip — or we've hit the round cap — let the analyzer ask clarifying
	// questions first. If it returns questions, hand them back (discriminated
	// response) and stop; the UI collects answers and re-POSTs with the transcript.
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != composeModeSkip && req.Round < composer.MaxClarifyRounds {
		emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "clarify"})
		clarReq := creq
		clarReq.ClarifyAlways = mode == composeModeAlways && req.Round == 0
		cl, cerr := s.cfg.Composer.Clarify(ctx, req.Backend, clarReq)
		if cerr != nil {
			if errors.Is(cerr, composer.ErrUnknownBackend) {
				return nil, &composeError{http.StatusBadRequest, cerr.Error()}
			}
			return nil, &composeError{http.StatusBadGateway, "composer backend: " + cerr.Error()}
		}
		if !cl.Ready && len(cl.Questions) > 0 {
			backend := cmp.Or(req.Backend, s.cfg.Composer.Default())
			auditFields := map[string]any{
				"backend": backend, "round": req.Round, "questions": len(cl.Questions),
				"workspace": string(req.Workspace.Kind), "correlation_id": correlationID,
				"prompt":        composer.CapAuditText(req.Prompt),
				"question_list": auditQuestions(cl.Questions),
			}
			// Delta answers: the Q&A the operator has supplied SO FAR. Round 0 has
			// none (the transcript is empty on the first clarify call); this stays a
			// stateless pipeline (Decision 1: no session store), so there is no prior
			// round's event to diff against — from round 1 on, the resent transcript
			// IS the new information this round brought, and it's what gets audited.
			if req.Round > 0 && len(req.Transcript) > 0 {
				auditFields["answers"] = composer.CapAuditText(qaSummary(req.Transcript))
			}
			auditData, _ := json.Marshal(auditFields)
			s.recordAudit(ctx, s.auditEvent(nil, principalType, principal,
				"run.compose.clarify", "compose", "success", auditData))
			resp := clarifyResponse{
				Kind: "questions", Questions: cl.Questions, Assumptions: cl.Assumptions,
				Notes: cl.Notes, Round: req.Round,
			}
			emit(composer.ComposeEvent{Type: composer.EvResult, Result: resp})
			return resp, nil
		}
	}

	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "propose"})
	prop, err := s.cfg.Composer.Propose(ctx, req.Backend, creq)
	if err != nil {
		if errors.Is(err, composer.ErrUnknownBackend) {
			return nil, &composeError{http.StatusBadRequest, err.Error()}
		}
		// The analyzer failed (backend error / unparseable model output). This is
		// not a client error; surface it as a bad gateway so the operator retries.
		return nil, &composeError{http.StatusBadGateway, "composer backend: " + err.Error()}
	}

	// Clamp to the operator's policy ceiling (this strips any host mount the
	// model emitted), THEN apply the operator's trusted choices — the workspace
	// and, when the run's resolved integration asks for it, the ceiling-blessed
	// Claude credential mounts — THEN validate the final spec through the same
	// path inline policies take. Applying mounts after the clamp is what makes
	// operator-authored entries the ONLY mount sources — the LLM can never
	// introduce one.
	run := prop.Run
	// Ground GitHub grants on the LOCAL workspace's DETECTED git remotes BEFORE the
	// clamp (so the operator's repo ceiling still intersects): scope any
	// github_token to the detected repos, or drop it when the dir has no GitHub
	// remote ("no remote -> no token"). The LLM's repo guess never survives.
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "ground"})
	var groundWarns []string
	if req.Workspace.Kind == composer.WorkspaceLocal {
		groundWarns = groundGitHubGrants(&prop.InlinePolicy, detectedGitHub, detectedOther)
		groundWarns = append(groundWarns, groundGitPATGrants(&prop.InlinePolicy, detectedOther)...)
	}
	// ALL workspace kinds: api_key secret names must be storable, or the setup
	// checklist's add-secret fix and the launch gate both dead-end (see
	// groundAPIKeySecretNames).
	groundWarns = append(groundWarns, groundAPIKeySecretNames(&prop.InlinePolicy)...)
	// Model-access grounding (ALL workspace kinds): a composed run is api-key mode,
	// so an LLM-backed agent needs a brokered api_key grant for its provider or it
	// reaches no model. Add it BEFORE the clamp (secret-aware + non-breaking); the
	// truthful "did model access survive?" warning is emitted AFTER the clamp below.
	presentSecrets := s.presentSecretNames(ctx)
	// Resolve the run-level integration precedence (run-explicit
	// integration_id → the PRIMARY workspace's LLMCred.IntegrationRef → the
	// operator's DefaultFor:agent_runs default — resolveRunIntegration,
	// llmcred.go) BEFORE the legacy subscribed/managedSub computation below,
	// so an operator-pinned Integration steers the SAME two booleans that
	// already drive ensureLLMGrant/applyLLMCredMount/reconcileLLMAccess — no
	// new pipeline stage, no change to clamp ordering. primaryWorkspaceLLMRef
	// resolves req.Workspaces — raw source descriptors, never types.Workspace
	// ids — against the onboarded workspace list the same way
	// referencedWorkspaces resolves ITS OWN primary (see its doc comment for
	// the exact mirror), so a workspace pinned to a subscription previews here
	// exactly as launch will grant it (foldRunIntegration resolves the
	// identical ref from the real onboarded workspace). hasInteg is always
	// false with zero configured integrations and no workspace binding, and
	// subscriptionRequested/managedByChoice then stay false — so every line
	// below degrades to the api-key-only path whenever nothing is configured.
	integ, hasInteg := s.resolveRunIntegration(ctx, req.IntegrationID, s.primaryWorkspaceLLMRef(ctx, req.Workspaces))
	subscriptionRequested, managedByChoice := false, false
	if hasInteg {
		subscriptionRequested = integ.Type == "anthropic_subscription" && subscriptionLane(integ) == "resident_host"
		managedByChoice = integ.Type == "anthropic_subscription" && subscriptionLane(integ) != "resident_host"
	}
	// Subscription transport engages only when the run's resolved integration IS
	// a resident_host subscription AND a ceiling-blessed cred mount AND a Claude
	// agent; otherwise api-key (the more governed default: key never resident,
	// proxy-injected, 1h TTL).
	subscribed := subscriptionRequested && prop.Run.Agent == "claude-code" &&
		ceilingBlessesClaudeCreds(s.cfg.DefaultPolicy)
	// Managed subscription: the resolved integration is a managed (non-
	// resident_host) subscription, Claude, no ceiling-blessed mount, but a
	// Wardyn-managed setup-token IS connected — the compose-mode path (no host
	// ~/.claude to stage). Treated like subscription for grant purposes (egress
	// only, no api-key grant); dispatch injects it proxy-side.
	managedSub := managedByChoice && prop.Run.Agent == "claude-code" &&
		!subscribed && s.managedInjectReady(prop.Run.Agent)
	// MOUNT GATING and the MODEL-ACCESS VERDICT are different questions, and
	// conflating them produced a false blocker. managedSub stays gated above
	// because it also gates applyLLMCredMount (the MOUNT path). But a managed
	// subscription needs NO mount at all: dispatch's resolveLLMTransport
	// injects it proxy-side for ANY eligible claude-code run (precedence: host
	// mount > managed > bedrock > api-key). Gating the verdict on managedSub
	// alone made the Review checklist announce "no model access — this run will
	// do nothing" for a run dispatch would happily credential (observed with a
	// connected setup-token and no ceiling-blessed mount).
	managedForVerdict := managedSub ||
		(prop.Run.Agent == "claude-code" && !subscribed && s.managedInjectReady(prop.Run.Agent))
	var bedrockRef *types.WorkspaceBedrockRef
	var integCredKind string
	if hasInteg && integ.Type != "anthropic_subscription" {
		// Any pinned NON-subscription integration (api_key/openai_api_key/
		// bedrock/azure_openai) overrides ensureLLMGrant's provider-DEFAULT
		// secret with the INTEGRATION's own — applyIntegrationCreds is the
		// same grant + exact-host-egress coupling ensureLLMGrant uses for
		// api_key, unions the region-scoped Bedrock hosts for bedrock (instead
		// of falling through to a default Anthropic api-key grant that
		// discards the region/model override), and no-ops for azure_openai
		// (its own switch case: no sandbox lane exists). bedrockRef is threaded
		// into the proposal below instead of discarded — the same ref launch
		// carries to dispatch (dispatchParams.BedrockRef, runs.go) once the
		// operator approves and creates the real run from this preview.
		// integCredKind (also no longer discarded) is what the llm_access verdict
		// below uses to recognize a bedrock pin as provisioned — reconcileLLMAccess
		// only understands the anthropic/openai api_key shape, and the bedrock
		// fold deliberately REMOVES rather than adds one (llmcred.go's bedrock case).
		integCredKind, bedrockRef = s.applyIntegrationCreds(ctx, &prop.InlinePolicy, integ, prop.Run.Agent)
	} else {
		ensureLLMGrant(&prop.InlinePolicy, prop.Run.Agent, presentSecrets, subscribed || managedSub)
	}
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "clamp"})
	// Raise the operator ceiling's confinement floor to include the per-run compose
	// floor (the operator's Getting Started default tier), capped at the strongest
	// class this host's runner can enforce so a too-strong floor degrades instead of
	// 422ing at launch (runs.go confinement gate). The raise surfaces via Clamp's
	// EXISTING "confinement raised ..." warning; the ceiling copy overwrites only the
	// value field, leaving the rest of DefaultPolicy shared and untouched.
	ceiling := s.cfg.DefaultPolicy
	var runnerBest types.ConfinementClass
	if s.cfg.Runner != nil {
		if caps, cerr := s.cfg.Runner.Capabilities(ctx); cerr == nil {
			runnerBest = bestClass(caps.ConfinementClasses)
		}
	}
	ceiling.MinConfinementClass = composer.EffectiveConfinementFloor(
		s.cfg.DefaultPolicy.MinConfinementClass, req.ConfinementFloor, runnerBest)
	clamped, clampWarns := composer.Clamp(prop.InlinePolicy, ceiling)
	// Pre/post-clamp egress diff for the setup checklist's dropped-domain rows
	// (no Clamp signature change — Clamp already returns this as a joined prose
	// warning; this is the same cut, structured per-domain).
	postDomains := map[string]bool{}
	for _, d := range clamped.AllowedDomains {
		postDomains[d] = true
	}
	var droppedDomains []string
	for _, d := range prop.InlinePolicy.AllowedDomains {
		if !postDomains[d] {
			droppedDomains = append(droppedDomains, d)
		}
	}
	// Operator-blessed Claude credential mounts (post-clamp, like applyWorkspace;
	// gated on the run's resolved integration). Then the honest FINAL-state
	// model-access check (after the clamp may have stripped the grant or its
	// egress entry): one warning, always true to what dispatch will do. May
	// also drop an orphaned grant to keep the run from hard-failing at startup.
	// Managed subscription needs no mount (the token is injected proxy-side from
	// the store), so skip applyLLMCredMount entirely — otherwise it would emit the
	// misleading "stage credentials" warning for a run that IS credentialed.
	var injectedCreds bool
	var credWarns []string
	if !managedSub {
		injectedCreds, credWarns = applyLLMCredMount(&clamped, s.cfg.DefaultPolicy, prop.Run.Agent, subscriptionRequested)
		clampWarns = append(clampWarns, credWarns...)
	}
	// A managed-subscription PIN nothing can actually deliver (managedByChoice,
	// but no ceiling-blessed mount AND no connected Wardyn-managed setup-token)
	// used to disappear silently: Requested below stayed false, so
	// setupSubscriptionMountItem rendered no row at all while ensureLLMGrant
	// quietly fell back to api-key underneath. Name it instead of hiding it.
	if managedByChoice && !managedSub && !subscribed {
		credWarns = append(credWarns, "this run's resolved integration is a managed Claude subscription, but no "+
			"setup-token is connected — falling back to a brokered API key; connect it in Getting Started.")
	}
	// The subscription-request <-> credential-mount PAIR's reconciled verdict,
	// threaded into the setup checklist (setupSubscriptionMountItem) verbatim —
	// reused, never recomputed, so that row can never disagree with the Warnings
	// bullets above. Requested also covers a managed pin (managedByChoice) so the
	// degrade case above gets its own row too, not just the resident_host case.
	subState := composeSubscriptionState{
		Requested: subscriptionRequested || managedByChoice, Injected: injectedCreds, Managed: managedSub, Warnings: credWarns,
	}
	// Structured model-access verdict (not folded into Warnings): a no-access result
	// is a "this run will do nothing" blocker the review must gate on, not one bullet
	// among benign clamp notices. reconcileLLMAccess still mutates the spec (drops
	// orphaned grants) as a side effect.
	var llmAccess *composeLLMAccess
	if note, provisioned := reconcileLLMAccess(&clamped, prop.Run.Agent, presentSecrets, s.subscriptionInjectEnabled(), managedForVerdict); note != "" {
		llmAccess = &composeLLMAccess{Provisioned: provisioned, Note: note}
	}
	// reconcileLLMAccess only recognizes the anthropic/openai api_key shape, so a
	// pinned Bedrock integration — which deliberately carries NO api_key grant —
	// always reads as "no model access" there, contradicting the run this
	// proposal previews (launch re-resolves Bedrock auth from integration_id
	// independently of any grant). A non-empty, non-api_key fold kind means the
	// integration itself IS the provisioned credential; teach the verdict that
	// directly instead of asking reconcileLLMAccess to model a transport it has
	// no branch for.
	//
	// Bedrock ONLY when it actually resolves, though: a bedrock row is derived
	// from AWS creds/mount alone (SetupBedrock.configured) and validates with
	// region and model both EMPTY, and for that row resolveBedrockAuth returns
	// unready at launch and dispatch falls back to the api-key path whose grant
	// applyIntegrationCreds just removed — no model access at all. Same
	// effective-region/model rule bedrockCaps applies (integration config wins,
	// global config is the fallback), so this verdict can never contradict the
	// capability matrix for the same integration.
	if (llmAccess == nil || !llmAccess.Provisioned) &&
		integCredKind != "" && integCredKind != "anthropic_api_key" && integCredKind != "openai_api_key" &&
		(integCredKind != "bedrock" || s.bedrockResolves(bedrockRef)) {
		note := fmt.Sprintf("model access provisioned for agent %q: the pinned %s integration is applied", prop.Run.Agent, integCredKind)
		if bedrockRef != nil {
			note += fmt.Sprintf(" (region %s, model %s)", bedrockRef.Region, bedrockRef.Model)
		}
		note += " — dispatch resolves its credentials at launch; no per-run API key is needed."
		llmAccess = &composeLLMAccess{Provisioned: true, Note: note}
	}
	wsWarns, code, werr := applyWorkspaces(&run, &clamped, req.Workspaces)
	if werr != nil {
		return nil, &composeError{code, werr.Error()}
	}
	// Onboarding gate (fail-fast at compose time; the run-create chokepoint remains
	// the load-bearing gate): the composed run's workspace source must be onboarded.
	if wc, wserr := s.validateWorkspaceSources(ctx, clamped); wserr != nil {
		return nil, &composeError{wc, "workspace: " + wserr.Error()}
	}
	// Deterministic BLAST-RADIUS floor: a run holding POWERFUL credentials (write-
	// capable, or a third-party/production api_key) is a high-value compromise target
	// and MUST run in the strongest sandbox so an escape is contained — independent
	// of what the model proposed. Raise the policy floor to it BEFORE clamping the run
	// class, so the review shows Vault as the minimum and the picker/clamp keep the
	// operator from choosing weaker.
	if composer.RequiredConfinementFloor(clamped) == types.CC3 && clamped.MinConfinementClass != types.CC3 {
		clamped.MinConfinementClass = types.CC3
		clampWarns = append(clampWarns, "confinement floored to Vault (CC3): this run holds a write-capable or third-party production credential, whose blast radius requires the strongest sandbox — a sandbox escape stays contained in a hardware-isolated VM.")
	}
	// Raise the run's confinement class to the clamped policy floor: the clamp
	// tightened MinConfinementClass independently. A NON-EMPTY class weaker than
	// the floor is inconsistent (handleCreateRun would 422 it); an empty class is
	// launchable (it inherits the policy minimum) but is raised anyway so the
	// class the reviewing human sees is the explicit, actual floor.
	var confWarn string
	run.ConfinementClass, confWarn = composer.ClampRunConfinement(run.ConfinementClass, clamped.MinConfinementClass)
	// The operator's UPFRONT interactive/background choice is AUTHORITATIVE — it
	// overrides whatever mode the model guessed. An interactive run comes up idle
	// awaiting `wardyn attach`, so pair it with never-reap (-1) so the lifecycle
	// reaper can't stop the idle sandbox (matches the manual wizard's behavior).
	run.Interactive = req.Interactive
	if req.Interactive {
		clamped.AutoStopAfterSec = -1
	}

	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "check"})
	if err := validatePolicySpec(clamped); err != nil {
		return nil, &composeError{http.StatusUnprocessableEntity, "composer produced an invalid policy after clamping: " + err.Error()}
	}

	// Deterministic risk grade of the FINAL run + spec (incl. the operator
	// workspace — e.g. a read-write local mount grades HIGH). Computed BEFORE
	// the requirements fold just below: a workspace's contract additions were
	// already outside Grade's scope before this fix (composer.Grade only ever
	// graded the operator's own choices, never a workspace's fine print), and
	// folding first would risk shifting graded risk levels for reasons unrelated
	// to this batch.
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "grade"})
	items := composer.Grade(run, clamped)
	overall := composer.OverallLevel(items)

	// Fold each referenced workspace's requirements contract into clamped — the
	// SAME applyWorkspaceRequirements call the manual wizard's preflight
	// (preflight.go) and the real launch (runs.go) both make before THEIR
	// setup-checklist/response is built. Without this, a required egress/secret/
	// write term a workspace's contract declares was invisible on Review (both
	// in the "N allowed" envelope below, since clamped becomes
	// proposed.InlinePolicy, and in the egress checklist row's "no additional
	// egress needed" verdict) yet applied anyway once the operator clicked
	// Launch — two derivations of "what egress does this run get" that could
	// disagree. Reusing the launch fold verbatim (not a second, egress-only
	// reimplementation) is what makes them agree. Discarded, not audited: like
	// preflight's identical call, this is a preview — it persists nothing.
	wsRefs := s.referencedWorkspaces(ctx, clamped)
	_ = s.applyWorkspaceRequirements(ctx, &clamped, run.Agent, wsRefs, selectionsByWorkspaceID(req.WorkspaceSelections))

	// Setup checklist: what this proposal needs configured (secrets, onboarded
	// workspaces, repo credentials, egress) vs. what actually is, derived from
	// this SAME final clamped spec (now INCLUDING the fold just above) — never
	// gates the proposal (Decision 4).
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "setup"})
	setupItems := s.deriveSetupItems(ctx, run, clamped, presentSecrets, llmAccess, droppedDomains, subState)

	// Advisory audit: a proposal was generated (no run, no mint). Record the
	// backend used and the overall graded risk for the trail.
	emit(composer.ComposeEvent{Type: composer.EvStage, Stage: "assemble"})
	backend := cmp.Or(req.Backend, s.cfg.Composer.Default())
	// Built ONCE and reused below for both the audit blob and the response, so
	// the two can never drift apart (echoes req.WorkspaceSelections verbatim —
	// see composeRequest.WorkspaceSelections' doc comment).
	proposed := composeProposed{Run: run, InlinePolicy: clamped, BedrockRef: bedrockRef, WorkspaceSelections: req.WorkspaceSelections}
	// The proposal is serialized+capped as ONE text blob (not embedded as nested
	// JSON): CapAuditText can cut mid-object, so storing the possibly-truncated
	// result as a plain string is what keeps the OUTER audit Data valid JSON.
	proposedJSON, _ := json.Marshal(proposed)
	auditData, _ := json.Marshal(map[string]any{
		"backend": backend, "overall_risk": string(overall),
		"workspace": string(req.Workspace.Kind), "correlation_id": correlationID,
		"prompt":      composer.CapAuditText(req.Prompt),
		"transcript":  composer.CapAuditText(qaSummary(req.Transcript)),
		"proposed":    composer.CapAuditText(string(proposedJSON)),
		"setup_items": setupItems,
		"workspaces":  auditWorkspaceRefs(req.Workspaces),
	})
	s.recordAudit(ctx, s.auditEvent(nil, principalType, principal,
		"run.compose", run.Repo, "success", auditData))

	// Deterministic policy actions only (M7): the LLM's own advisory prose
	// (prop.Warnings) is carried in a SEPARATE ModelNotes field so untrusted model
	// text is never rendered as an enforced "Tightened by policy:" action.
	warnings := append(append(append([]string{}, groundWarns...), clampWarns...), wsWarns...)
	if confWarn != "" {
		warnings = append(warnings, confWarn)
	}
	resp := composeResponse{
		Kind:           "proposal",
		Proposed:       proposed,
		RiskAssessment: items,
		OverallRisk:    overall,
		Summary:        prop.Summary,
		Warnings:       warnings,
		ModelNotes:     prop.Warnings,
		LLMAccess:      llmAccess,
		SetupItems:     setupItems,
	}
	emit(composer.ComposeEvent{Type: composer.EvResult, Result: resp})
	return resp, nil
}

// auditQuestionEntry is the id+text shape a clarify round's audit event records
// for each asked question — enough to reconstruct the interview from the audit
// feed (Decision 7: audit-feed-only history) without carrying the Options/Why/
// Help enrichment fields, which are display-only and add nothing to the trail.
type auditQuestionEntry struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// auditQuestions reshapes a clarify round's questions into the audited id+text
// shape.
func auditQuestions(qs []composer.Question) []auditQuestionEntry {
	out := make([]auditQuestionEntry, len(qs))
	for i, q := range qs {
		out[i] = auditQuestionEntry{ID: q.ID, Text: q.Question}
	}
	return out
}

// qaSummary flattens a clarify transcript into one "Q: ...\nA: ...\n\n"-joined
// string for the audit trail's transcript/answers fields. The caller caps the
// result with composer.CapAuditText — this just does the flattening.
func qaSummary(qas []composer.QA) string {
	var b strings.Builder
	for _, qa := range qas {
		b.WriteString("Q: ")
		b.WriteString(qa.Question)
		b.WriteString("\nA: ")
		b.WriteString(qa.Answer)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// auditWorkspaceRef is the kind+source shape the run.compose audit event
// records per referenced workspace — enough to see WHAT was operated on
// (a local directory, a repo, ...) without embedding the full Workspace struct
// (ReadWrite is not audited: it is not identifying/free-text, and the launched
// run's own workspace_mount audit already carries it).
type auditWorkspaceRef struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

// auditWorkspaceRefs reshapes the request's workspace selections into the
// audited kind+source shape. Source is the local path or the git repo slug/URL;
// empty for ephemeral (nothing to name).
func auditWorkspaceRefs(wss []composer.Workspace) []auditWorkspaceRef {
	out := make([]auditWorkspaceRef, len(wss))
	for i, ws := range wss {
		ref := auditWorkspaceRef{Kind: string(ws.Kind)}
		switch ws.Kind {
		case composer.WorkspaceLocal:
			ref.Source = ws.Path
		case composer.WorkspaceGit:
			ref.Source = ws.Repo
		}
		out[i] = ref
	}
	return out
}

// primaryWorkspaceLLMRef mirrors referencedWorkspaces' PRIMARY selection
// (workspace_run.go) one step earlier, against the compose request's RAW
// source descriptors (wss) instead of the already-applied spec. Launch's
// primary is referencedWorkspaces[0], which resolves EVERY spec.WorkspaceMounts
// entry (in order) before ANY spec.WorkspaceRepos entry; applyWorkspaces
// (compose.go) buckets each wss entry into Mounts (local) or Repos (git) by
// kind, preserving wss' relative order WITHIN each bucket. So the primary here
// is NOT wss[0] — it is the first LOCAL descriptor that resolves to an
// onboarded workspace, or (only when no local descriptor resolves) the first
// GIT descriptor that resolves. Descriptors are matched against the onboarded
// workspace list via the same workspaceSourceIndex referencedWorkspaces uses
// (workspace_refs.go); an empty Path/Repo (a multi-source workspace's
// single-source mirror fields are blank once it has more than one source) is
// skipped rather than looked up as "". Returns "" when the store is unset,
// nothing resolves, or the resolved workspace carries no LLMCred binding —
// resolveRunIntegration then falls through to the operator's site-wide default
// exactly like an unbound workspace always has.
func (s *Server) primaryWorkspaceLLMRef(ctx context.Context, wss []composer.Workspace) string {
	if s.cfg.Store == nil {
		return ""
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return ""
	}
	idx := indexWorkspacesBySource(all)
	firstMatch := func(kind composer.WorkspaceKind, key func(composer.Workspace) string, lookup map[string]types.Workspace) (types.Workspace, bool) {
		for _, ws := range wss {
			if ws.Kind != kind {
				continue
			}
			k := key(ws)
			if k == "" {
				continue
			}
			if match, ok := lookup[k]; ok {
				return match, true
			}
		}
		return types.Workspace{}, false
	}
	match, ok := firstMatch(composer.WorkspaceLocal, func(w composer.Workspace) string { return w.Path }, idx.localDir)
	if !ok {
		match, ok = firstMatch(composer.WorkspaceGit, func(w composer.Workspace) string { return w.Repo }, idx.repo)
	}
	if !ok || match.LLMCred == nil {
		return ""
	}
	return match.LLMCred.IntegrationRef
}

// applyWorkspace applies the OPERATOR's trusted workspace choice to the proposed
// run + clamped policy. It runs AFTER the clamp (which strips any model-emitted
// mount), so a host directory enters a composed run ONLY via this operator
// choice. Returns (warnings, httpStatus, error); a nil error means applied.
//
// This is the compose path's counterpart to runs_create.go's
// seedRequestWorkspace, and covers the SAME core job — mount/repo entries land
// on spec.WorkspaceMounts/WorkspaceRepos either way — but from a narrower
// input: wss is the proposal's raw kind+source descriptor (composer.Workspace),
// never a resolved types.Workspace record, so unlike seedRequestWorkspace it
// cannot set a base_image (composer.RunInput has no field to carry one to the
// create-run request even if it could) or surface an ephemeral source's target
// as WARDYN_EPHEMERAL_DIRS. Deliberate, not an oversight: see
// seedRequestWorkspace's own doc comment for why routing a composed launch back
// through it instead (e.g. by deriving a workspace_id for the primary
// selection) would double-apply the sources this function already seeded. See
// reconcile-workspace-first.md item 2.
func applyWorkspaces(run *composer.RunInput, spec *types.RunPolicySpec, wss []composer.Workspace) ([]string, int, error) {
	var warns []string
	localCount, gitCount := 0, 0
	for i, ws := range wss {
		switch ws.Kind {
		case composer.WorkspaceLocal:
			ro := !ws.ReadWrite
			// First local dir gets the canonical /home/agent/work; additional dirs
			// get a distinct subdir so targets never collide (the unique-target
			// validator rejects a genuine duplicate basename).
			target := composerWorkspaceTarget
			if localCount > 0 {
				target = composerWorkspaceTarget + "/" + path.Base(ws.Path)
			}
			m := runner.Mount{Source: ws.Path, Target: target, ReadOnly: ro}
			if err := runner.ValidateMount(m); err != nil {
				return nil, http.StatusBadRequest, errors.New("invalid local workspace: " + err.Error())
			}
			spec.WorkspaceMounts = append(spec.WorkspaceMounts, types.WorkspaceMount{
				Source: ws.Path, Target: target, ReadOnly: &ro,
			})
			if i == 0 {
				run.Repo = "local:" + path.Base(ws.Path)
				run.DevcontainerRepo = ""
			}
			if ws.ReadWrite {
				warns = append(warns, "read-WRITE local workspace: the agent's changes persist to the host directory "+ws.Path)
			}
			localCount++
		case composer.WorkspaceGit:
			repo := ws.Repo
			if !repoFieldSafe(repo) {
				return nil, http.StatusBadRequest, errors.New("invalid git repo (contains control/whitespace characters)")
			}
			// EVERY git repo — index 0 included — rides WorkspaceRepos, mirroring
			// seedRequestWorkspace's identical convention for the manual/workspace_id
			// path (runs_create.go: "the FIRST repo source also sets req.Repo ...
			// the gate + wsRefs read WorkspaceRepos"). Without this the PRIMARY git
			// workspace was invisible to referencedWorkspaces (only spec.WorkspaceMounts/
			// WorkspaceRepos are scanned, never run.Repo) and to validateWorkspaceSources'
			// onboarding gate — both bugs, not just the checklist's. The first repo
			// ADDITIONALLY drives the legacy run.Repo label (run-row display only);
			// buildRepoRecords (runs_scm.go) dedupes the resulting legacyRepo +
			// WorkspaceRepos[0] pair by their shared default clone dest, so this is
			// still exactly ONE clone, not two.
			if i == 0 {
				run.Repo = repo
			}
			spec.WorkspaceRepos = append(spec.WorkspaceRepos, types.WorkspaceRepo{Repo: repo})
			gitCount++
		case composer.WorkspaceEphemeral:
			// Ephemeral only defines the workspace when it is the SOLE selection;
			// mixed with real dirs/repos it is ignored (they define the workspace).
			if len(wss) == 1 {
				run.Repo = "ephemeral"
				run.DevcontainerRepo = ""
				warns = append(warns, "ephemeral workspace: the sandbox working dir is wiped on teardown (nothing persists)")
			}
		default:
			return nil, http.StatusBadRequest, errors.New("a workspace is required (local | git | ephemeral)")
		}
	}
	return warns, 0, nil
}

// groundGitHubGrants makes the proposal's GitHub access reflect the LOCAL
// workspace's ACTUAL git remotes (detectedGitHub), not the model's guess:
//   - detected repos exist -> set each github_token grant's scope.repos to them
//     (keeping the clamped, task-driven permissions + requires_approval);
//   - no GitHub remote -> REMOVE every github_token grant (no remote, no token);
//   - non-GitHub remotes -> a warning (Wardyn brokers GitHub tokens only).
//
// It never fabricates a grant the model didn't request (least privilege).
func groundGitHubGrants(spec *types.RunPolicySpec, detectedGitHub, detectedOther []string) []string {
	var warns []string
	kept := spec.EligibleGrants[:0]
	dropped := 0
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantGitHubToken {
			kept = append(kept, g)
			continue
		}
		if len(detectedGitHub) == 0 {
			dropped++
			continue // no remote -> drop the github token entirely
		}
		var sc struct {
			Repos       []string          `json:"repos"`
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.Unmarshal(g.Scope, &sc)
		sc.Repos = detectedGitHub // override the guess with detected reality
		if b, err := json.Marshal(sc); err == nil {
			g.Scope = b
		}
		kept = append(kept, g)
	}
	spec.EligibleGrants = kept
	if dropped > 0 {
		warns = append(warns, "no GitHub git remote detected in the workspace; dropped the proposed github_token grant (nothing to scope it to)")
	} else if len(detectedGitHub) > 0 {
		warns = append(warns, "scoped github_token to the workspace's detected remote(s): "+strings.Join(detectedGitHub, ", "))
	}
	if len(detectedOther) > 0 {
		warns = append(warns, "non-GitHub remote host(s) detected ("+strings.Join(detectedOther, ", ")+"); add a git_pat grant with a stored PAT to broker credentials for these hosts")
	}
	return warns
}

// groundGitPATGrants makes the proposal's non-GitHub PAT access reflect the
// LOCAL workspace's ACTUAL non-github remotes (detectedOther): a git_pat grant
// is KEPT only when its host matches a detected remote host, and DROPPED with a
// warning otherwise (a stored PAT brokered for a host the workspace never uses
// is needless standing access). It never fabricates a grant the model didn't
// request (least privilege), mirroring groundGitHubGrants.
func groundGitPATGrants(spec *types.RunPolicySpec, detectedOther []string) []string {
	if len(spec.EligibleGrants) == 0 {
		return nil
	}
	detected := make(map[string]bool, len(detectedOther))
	for _, h := range detectedOther {
		detected[strings.ToLower(h)] = true
	}
	var warns []string
	kept := spec.EligibleGrants[:0]
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantGitPAT {
			kept = append(kept, g)
			continue
		}
		host, _, _, derr := gitPATScopeFields(g.Scope)
		if derr != nil || !detected[strings.ToLower(host)] {
			warns = append(warns, "dropped a git_pat grant (host "+host+"): no matching non-GitHub remote detected in the workspace")
			continue
		}
		warns = append(warns, "kept git_pat grant for detected non-GitHub remote host "+host)
		kept = append(kept, g)
	}
	spec.EligibleGrants = kept
	return warns
}

// groundAPIKeySecretNames rewrites LLM-proposed api_key secret names that can
// never exist: the model may invent env-var-style names (e.g.
// "ANTHROPIC_API_KEY") that secretNameRE rejects, so the setup checklist's
// add-secret fix would dead-end in the dialog's name validation and the launch
// would 422 on a secret nobody can create. A name that fails secretNameRE gets
// the provider's canonical name when the host is a known LLM provider (same
// source of truth as agentLLMProvider), else a mechanical sanitize. Names the
// store accepts are left alone — the model's valid choice stands. Mirrors the
// deterministic-grounding rule: the model never invents unusable refs.
func groundAPIKeySecretNames(spec *types.RunPolicySpec) []string {
	var warns []string
	for i, g := range spec.EligibleGrants {
		if g.Kind != types.GrantAPIKey {
			continue
		}
		var scope map[string]any
		if err := json.Unmarshal(g.Scope, &scope); err != nil {
			continue // validation rejects undecodable scopes later
		}
		name, _ := scope["secret_name"].(string)
		host, _ := scope["host"].(string)
		if name == "" || secretNameRE.MatchString(name) {
			continue
		}
		fixed, ok := canonicalSecretForHost(host)
		if !ok {
			fixed = sanitizeSecretName(name)
		}
		if fixed == "" || fixed == name {
			continue
		}
		scope["secret_name"] = fixed
		raw, err := json.Marshal(scope)
		if err != nil {
			continue
		}
		spec.EligibleGrants[i].Scope = raw
		warns = append(warns, fmt.Sprintf("normalized api_key secret name %q to storable %q (host %s)", name, fixed, host))
	}
	return warns
}

// canonicalSecretForHost maps a known LLM provider host to its canonical secret
// name via the agentLLMProvider table (single source of truth).
func canonicalSecretForHost(host string) (string, bool) {
	for _, agent := range []string{"claude-code", "codex-cli"} {
		if p, ok := agentLLMProvider(agent); ok && p.host == host {
			return p.secret, true
		}
	}
	return "", false
}

// sanitizeSecretName lowercases and maps a proposed name onto secretNameRE's
// alphabet ('_' and spaces become '-', other invalid runes drop, edge
// punctuation trims); returns "" when nothing storable remains.
func sanitizeSecretName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case r == '_', r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if !secretNameRE.MatchString(out) {
		return ""
	}
	return out
}

// handleListComposerBackends returns the configured composer backends (no
// secrets) for the UI provider dropdown. 404 when the composer is disabled.
func (s *Server) handleListComposerBackends(w http.ResponseWriter, r *http.Request) {
	if s.composerEnabledOrNotFound(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": s.cfg.Composer.List()})
}
