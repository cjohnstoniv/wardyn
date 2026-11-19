// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// POST /runs Idempotency-Key. A client whose 201 was lost retries with the same key and body and gets the
// first answer back instead of a second run. The key is scoped to the caller (principal and the delegate
// acting for it), kept 24 hours as its SHA-256, and bound to its run in the run insert's own transaction,
// so a key never names a run twice. It is on with no switch: it acts only when a key is sent, and a key
// can only stop a create from happening twice, never widen one.
const (
	idempotencyKeyHeader     = "Idempotency-Key"
	idempotentReplayedHeader = "Idempotent-Replayed"
	// runCreateKeyLease is how long one create holds its key before a retry may take it over. It outlasts
	// the create's foreground work up to the answer; the build and dispatch run after it.
	runCreateKeyLease = 5 * time.Minute
	maxIdempotencyKey = 255
)

type runCreateKeyAuditKey struct{}

// handleCreateRunRoute is POST /runs: handleCreateRun, under the request's Idempotency-Key when it sends
// one. Internal callers (a workload launch) call handleCreateRun directly and never consult a key.
func (s *Server) handleCreateRunRoute(w http.ResponseWriter, r *http.Request) {
	values, sent := r.Header[idempotencyKeyHeader]
	if !sent {
		s.handleCreateRun(w, r)
		return
	}
	key, ok := parseIdempotencyKey(values)
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonIdempotencyKeyInvalid,
			"Idempotency-Key must be one value of 1 to 255 visible ASCII characters.")
		return
	}
	keys, ok := s.cfg.Store.(store.RunCreateKeyStore)
	if !ok { // /healthz says idempotent_run_create false
		s.handleCreateRun(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil || len(body) > maxJSONBody {
		// The create path reads the rest and refuses the body before any insert.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		s.handleCreateRun(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	id := store.RunCreateKeyID{Principal: principalFromRequest(r), KeySHA256: idempotencyKeySHA256(key)}
	if via, ok := audit.DelegationFrom(r.Context()); ok {
		id.Via = via.Delegate.String()
	}
	k := store.RunCreateKey{RunCreateKeyID: id, RequestSHA256: runCreateFingerprint(body, r.Header.Get(breakGlassReasonHeader))}
	claim, err := keys.ClaimRunCreateKey(r.Context(), k, runCreateKeyLease)
	if err != nil {
		writeServerError(w, r, "claim idempotency key", err)
		return
	}
	switch claim.State {
	case store.RunCreateKeyMismatch:
		actorType, actor := actorFromRequest(r)
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "run.create.replay", "", "denied",
			mustJSON(map[string]any{"reason": reasonIdempotencyKeyReused})))
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonIdempotencyKeyReused,
			"This Idempotency-Key was used with a different request in the last 24 hours. Use a new key for a new run.")
	case store.RunCreateKeyInFlight:
		writeRunCreateKeyInFlight(w)
	case store.RunCreateKeyDone:
		s.replayRunCreate(w, r, claim)
	default:
		s.createRunUnderKey(w, r, keys, id, claim)
	}
}

// keepsRunCreateKeys is whether the store keeps Idempotency-Keys (/healthz idempotent_run_create).
func (s *Server) keepsRunCreateKeys() bool {
	_, ok := s.cfg.Store.(store.RunCreateKeyStore)
	return ok
}

// parseIdempotencyKey reads the header's one value: 1-255 visible ASCII characters, or an RFC 8941 string
// of them in quotes, which is read without its quotes.
func parseIdempotencyKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	v := values[0]
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
		if bytes.ContainsAny([]byte(v), `"\`) {
			return "", false
		}
	}
	if v == "" || len(v) > maxIdempotencyKey {
		return "", false
	}
	for i := range len(v) {
		if v[i] < 0x21 || v[i] > 0x7e {
			return "", false
		}
	}
	return v, true
}

// idempotencyKeySHA256 is the form a key is kept and audited in; the key itself never is.
func idempotencyKeySHA256(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// runCreateFingerprint is what a key is held to: the body's bytes, then the break-glass reason, which
// changes the run. A retry resends the same bytes.
func runCreateFingerprint(body []byte, breakGlass string) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0})
	h.Write([]byte(breakGlass))
	return hex.EncodeToString(h.Sum(nil))
}

func writeRunCreateKeyInFlight(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeErrorReason(w, http.StatusConflict, reasonIdempotencyKeyInFlight,
		"A request with this Idempotency-Key is still being answered. Retry in a moment.")
}

// createRunUnderKey runs the create holding the key's claim, keeps its answer when the key names the run
// it made, and releases the key when no run was made. The answer is kept before the caller sees it, so a
// retry gets the same bytes, a refusal after the insert included. A refusal before the insert keeps
// nothing: a retry once its cause is fixed is judged afresh.
func (s *Server) createRunUnderKey(w http.ResponseWriter, r *http.Request, keys store.RunCreateKeyStore, id store.RunCreateKeyID, claim store.RunCreateKeyClaim) {
	ctx := context.WithValue(store.WithRunCreateKey(r.Context(), id, claim.Token), runCreateKeyAuditKey{}, id.KeySHA256)
	held := &heldResponse{w: w}
	s.handleCreateRun(held, r.WithContext(ctx))
	// The answer is kept even when the caller has gone: that is the retry this exists for.
	keep := context.WithoutCancel(ctx)
	bound, err := keys.SaveRunCreateResponse(keep, id, claim.Token, held.status, w.Header().Get("Location"), held.body.Bytes())
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: keep the idempotency key's answer", "err", err)
	}
	if err == nil && !bound {
		if err := keys.ReleaseRunCreateKey(keep, id, claim.Token); err != nil {
			slog.ErrorContext(ctx, "wardynd: release the idempotency key", "err", err)
		}
	}
	held.send()
}

// replayRunCreate answers a key that names a run: the kept answer byte for byte, or, when none was kept
// (a crash before it was, a body over the size cap, or an erasure of the person's run tasks), a 201
// rebuilt from the run without the first answer's warnings. A key whose first request may still be
// answering is in flight, unless an erasure cleared its answer.
func (s *Server) replayRunCreate(w http.ResponseWriter, r *http.Request, claim store.RunCreateKeyClaim) {
	rebuilt := claim.Response == nil
	if rebuilt && claim.LeaseLive && !claim.ResponseErased {
		writeRunCreateKeyInFlight(w)
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, claim.RunID, s.ownsRunOrAdmin)
	if !ok {
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.create.replay", run.ID.String(), "success",
		mustJSON(map[string]any{"original_at": claim.CreatedAt, "reconstructed": rebuilt})))
	w.Header().Set(idempotentReplayedHeader, "true")
	if rebuilt {
		runs := []types.AgentRun{run}
		s.projectStatusDetailFor(r, runs)
		s.projectRunnerFacts(r, runs)
		redactRunRepos(runs)
		w.Header().Set("Location", s.cfg.BasePath+"/api/v1/runs/"+run.ID.String())
		writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: runs[0]})
		return
	}
	kept := claim.Response
	if kept.Location != "" {
		w.Header().Set("Location", kept.Location)
	}
	// Every create answer is JSON; the type is fixed here rather than carried with the kept bytes.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(kept.Status)
	_, _ = w.Write(kept.Body) // the caller may have gone; the answer stays kept
}

// stampIdempotencyKey adds the key's SHA-256 to run.create's data when the create came with a key, so a
// run.create.replay row (same run) and the key row link to it.
func stampIdempotencyKey(ctx context.Context, data map[string]any) {
	if sum, ok := ctx.Value(runCreateKeyAuditKey{}).(string); ok {
		data["idempotency_key_sha256"] = sum
	}
}

// heldResponse holds the create's answer until the key has kept it. Headers go straight to the real
// writer's map (nothing is sent before WriteHeader); the status and body wait for send.
type heldResponse struct {
	w      http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header { return h.w.Header() }

func (h *heldResponse) WriteHeader(status int) {
	if h.status == 0 {
		h.status = status
	}
}

func (h *heldResponse) Write(b []byte) (int, error) {
	if h.status == 0 {
		h.status = http.StatusOK
	}
	return h.body.Write(b)
}

// send writes the held answer as JSON, whatever type the create set: every create answer is JSON, and the
// same bytes are what a replay sends.
func (h *heldResponse) send() {
	if h.status == 0 {
		h.status = http.StatusOK
	}
	w := h.w
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(h.status)
	_, _ = w.Write(h.body.Bytes()) // the caller may have gone; the answer stays kept
}
