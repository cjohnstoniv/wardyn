// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every sidecar door binds the run from the verified token's claims. Reached with
// none (the auth middleware did not run), each refuses 401 and touches nothing.
func TestInternalDoors_RefuseWhenNoRunClaimsAreOnTheRequest(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"decisions":       h.srv.handlePostDecision,
		"raise approval":  h.srv.handleInternalRequestApproval,
		"get approval":    h.srv.handleInternalGetApproval,
		"expire approval": h.srv.handleInternalExpireApproval,
	} {
		w := httptest.NewRecorder()
		call(w, httptest.NewRequest(http.MethodPost, "/internal/x/"+id, strings.NewReader(`{}`)))
		if w.Code != http.StatusUnauthorized || errorReason(w) != reasonMissingRunClaims {
			t.Errorf("%s: %d %s, want 401 %s", name, w.Code, w.Body.String(), reasonMissingRunClaims)
		}
	}
	if got := len(h.audit.snapshot()); got != 0 {
		t.Errorf("a refused call wrote %d audit rows", got)
	}
}

func TestInternalDecisions_BadBodyIsRefusedAndRepeatIsAudited(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	token := h.mintRunToken(t, runID)

	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", token, `{"decision":`); w.Code != http.StatusBadRequest || errorReason(w) != reasonInternalDecisionLogInvalid {
		t.Errorf("a truncated body = %d %s, want 400 %s", w.Code, w.Body.String(), reasonInternalDecisionLogInvalid)
	}

	body, _ := json.Marshal(egress.DecisionLog{
		Request:  egress.Request{Host: "registry.example.com", Port: 443},
		Decision: egress.Deny, RuleSource: "policy", Repeat: 7,
	})
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", token, string(body)); w.Code >= 300 {
		t.Fatalf("a streak summary = %d %s, want success", w.Code, w.Body.String())
	}
	ev := findAudit(h.audit.snapshot(), runID, "egress.deny", "denied")
	if ev == nil {
		t.Fatalf("no egress.deny row for the run; rows=%v", h.audit.snapshot())
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["repeat"] != float64(7) || data["host"] != "registry.example.com" {
		t.Errorf("data = %v, want the host and the streak's repeat count", data)
	}
}

func TestInternalApprovals_StoreFailuresAndUnknownIDs(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	token := h.mintRunToken(t, runID)

	h.approvals.requestErr = errors.New("boom")
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", token, `{"kind":"egress_domain","requested_scope":{"host":"a.example.com"}}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a failing raise = %d %s, want 500", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("the 500 leaked the store error: %s", w.Body.String())
	}

	// The real approval service answers store.ErrNotFound for an id it does not hold, and any
	// other error for a read that failed; the fake's own sentinel is neither.
	h.srv.cfg.Approvals = lookupApprovals{fakeApprovals: h.approvals, err: store.ErrNotFound}
	missing := "/api/v1/internal/approvals/" + uuid.New().String()
	if w := do(t, h.srv, http.MethodGet, missing, token, ""); w.Code != http.StatusNotFound || errorReason(w) != reasonApprovalNotFound {
		t.Errorf("get of an unknown approval = %d %s, want 404 %s", w.Code, w.Body.String(), reasonApprovalNotFound)
	}
	if w := do(t, h.srv, http.MethodPost, missing+"/expire", token, ""); w.Code != http.StatusNotFound || errorReason(w) != reasonApprovalNotFound {
		t.Errorf("expire of an unknown approval = %d %s, want 404 %s", w.Code, w.Body.String(), reasonApprovalNotFound)
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals/not-a-uuid/expire", token, ""); w.Code != http.StatusBadRequest {
		t.Errorf("expire with a bad id = %d, want 400", w.Code)
	}

	h.srv.cfg.Approvals = lookupApprovals{fakeApprovals: h.approvals, err: errors.New("boom")}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"get":    do(t, h.srv, http.MethodGet, missing, token, ""),
		"expire": do(t, h.srv, http.MethodPost, missing+"/expire", token, ""),
	} {
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "boom") {
			t.Errorf("%s with a failing read = %d %s, want a 500 that does not leak the error", name, w.Code, w.Body.String())
		}
	}
}

// lookupApprovals is the harness's approval fake with the one read answering a fixed error.
type lookupApprovals struct {
	*fakeApprovals
	err error
}

func (a lookupApprovals) Get(context.Context, uuid.UUID) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, a.err
}

// groundStore answers GetRun with one fixed error, so the sensor batch's run_id
// validation sees a store that failed rather than a run that does not exist.
type groundStore struct {
	store.Store
	err error
}

func (s groundStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, s.err
}

func TestGroundtruthBatch_RefusalsWriteNothing(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Store = groundStore{err: errors.New("boom")}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.srv.handleGroundtruthEvents(w, httptest.NewRequest(http.MethodPost, "/internal/groundtruth", strings.NewReader(body)))
		return w
	}

	if w := post(`{"events":`); w.Code != http.StatusBadRequest || errorReason(w) != reasonGroundtruthBatchInvalid {
		t.Errorf("a truncated batch = %d %s, want 400 %s", w.Code, w.Body.String(), reasonGroundtruthBatchInvalid)
	}
	if w := post(`{"events":[]}`); w.Code != http.StatusAccepted {
		t.Errorf("an empty batch = %d, want 202", w.Code)
	}
	if w := post(`{"events":[` + strings.Repeat(`{"action":"kernel.exec"},`, 1000) + `{"action":"kernel.exec"}]}`); w.Code != http.StatusRequestEntityTooLarge || errorReason(w) != reasonGroundtruthBatchTooLarge {
		t.Errorf("1001 events = %d %s, want 413 %s", w.Code, w.Body.String(), reasonGroundtruthBatchTooLarge)
	}
	if w := post(`{"events":[{"action":"egress.allow"}]}`); w.Code != http.StatusBadRequest || errorReason(w) != reasonGroundtruthActionNotKernel {
		t.Errorf("a non-kernel action = %d %s, want 400 %s", w.Code, w.Body.String(), reasonGroundtruthActionNotKernel)
	}
	// A run that does not exist is downgraded to unmapped; a store that failed is not guessed at.
	if w := post(`{"events":[{"action":"kernel.exec","run_id":"` + uuid.New().String() + `"}]}`); w.Code != http.StatusInternalServerError {
		t.Errorf("a store failure while validating run_id = %d %s, want 500", w.Code, w.Body.String())
	}
	if got := len(h.audit.snapshot()); got != 0 {
		t.Errorf("refused batches wrote %d audit rows", got)
	}
}
