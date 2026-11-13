// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A reviewed download (desktop.download: review): the Files panel asks, the file_download deciders see the
// file's name, size and hash, and an approval releases that file's bytes, once, re-hashed before any is sent.

const reviewFile = "agent-report.csv"

// reviewSpendStore is the store's kind-bound spend over the fake approvals, mirroring its SQL: an APPROVED,
// once-decided, unspent file_download row of that run whose scope names that file, size and hash.
type reviewSpendStore struct {
	store.Store
	aps      *fakeApprovals
	spendErr error
	spends   int
}

func (s *reviewSpendStore) SpendFileDownloadApproval(_ context.Context, id, runID uuid.UUID, want types.FileDownloadScope, jti string) (bool, error) {
	s.spends++
	if s.spendErr != nil {
		return false, s.spendErr
	}
	if jti == "" {
		return false, errors.New("empty jti")
	}
	s.aps.mu.Lock()
	defer s.aps.mu.Unlock()
	ap, ok := s.aps.byID[id]
	sc, scOK := types.ParseFileDownloadScope(ap.RequestedScope)
	if !ok || !scOK || ap.RunID != runID || ap.Kind != types.ApprovalFileDownload || ap.State != types.ApprovalApproved ||
		ap.DecisionScope != types.ScopeOnce || ap.MintedJTI != "" || ap.GrantID != nil ||
		sc.Name != want.Name || sc.SHA256 != want.SHA256 || sc.Bytes != want.Bytes {
		return false, nil
	}
	ap.MintedJTI = jti
	s.aps.byID[id] = ap
	return true, nil
}

// getFailingApprovals answers every Get with err: the hold store is unreadable.
type getFailingApprovals struct {
	ApprovalService
	err error
}

func (a getFailingApprovals) Get(context.Context, uuid.UUID) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, a.err
}

type reviewEnv struct {
	*desktopFilesEnv
	aps   *fakeApprovals
	spend *reviewSpendStore
}

func newReviewEnv(t *testing.T, rule types.TransferRule) *reviewEnv {
	t.Helper()
	e := newDesktopFilesEnv(t, &types.DesktopControls{Download: rule})
	aps := newFakeApprovals()
	sp := &reviewSpendStore{Store: e.srv.cfg.Store, aps: aps}
	e.srv.cfg.Approvals, e.srv.cfg.Store = aps, sp
	return &reviewEnv{desktopFilesEnv: e, aps: aps, spend: sp}
}

func (e *reviewEnv) ask(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.srv, http.MethodPost, e.path(name)+"/review", e.ses["owner"], "")
}

func (e *reviewEnv) release(t *testing.T, name string, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.srv, http.MethodGet, e.path(name)+"?approval="+id.String(), e.ses["owner"], "")
}

// asked raises the review of name and returns its row.
func (e *reviewEnv) asked(t *testing.T, name string) types.ApprovalRequest {
	t.Helper()
	w := e.ask(t, name)
	if w.Code != http.StatusAccepted {
		t.Fatalf("ask %s: %d %s", name, w.Code, w.Body.String())
	}
	var ap types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &ap); err != nil || ap.ID == uuid.Nil {
		t.Fatalf("ask %s answered %s (%v)", name, w.Body.String(), err)
	}
	return e.row(t, ap.ID)
}

func (e *reviewEnv) row(t *testing.T, id uuid.UUID) types.ApprovalRequest {
	t.Helper()
	e.aps.mu.Lock()
	defer e.aps.mu.Unlock()
	ap, ok := e.aps.byID[id]
	if !ok {
		t.Fatalf("no approval %s", id)
	}
	return ap
}

// set moves approval id to state, decided once, as the decide path stores an approved review.
func (e *reviewEnv) set(id uuid.UUID, state types.ApprovalState) {
	e.aps.mu.Lock()
	defer e.aps.mu.Unlock()
	ap := e.aps.byID[id]
	now := time.Now().UTC()
	ap.State, ap.DecidedAt, ap.DecidedBy = state, &now, "sub-sec"
	if state == types.ApprovalApproved {
		ap.DecisionScope = types.ScopeOnce
	}
	e.aps.byID[id] = ap
}

// seed stores a row of kind on runID whose scope names name with the folder's bytes, already in state.
func (e *reviewEnv) seed(kind types.ApprovalKind, runID uuid.UUID, name string, state types.ApprovalState) uuid.UUID {
	content := string(e.folder.files[name])
	raw, _ := json.Marshal(types.FileDownloadScope{Name: name, Bytes: int64(len(content)), SHA256: digest(content), RequestedBy: entryOwnerSub})
	id := uuid.New()
	e.aps.mu.Lock()
	e.aps.byID[id] = types.ApprovalRequest{ID: id, RunID: runID, Kind: kind, RequestedScope: raw, State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	e.aps.mu.Unlock()
	e.set(id, state)
	return id
}

// reviewRows is each desktop.file.transfer download row's review value and outcome, in order.
func (e *reviewEnv) reviewRows() []string {
	var out []string
	for _, ev := range e.transferRows("download") {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		out = append(out, fmt.Sprintf("%v/%s", d["review"], ev.Outcome))
	}
	return out
}

// wantNothingSent fails when w carried any of the file's bytes or a download's headers.
func wantNothingSent(t *testing.T, name string, w *httptest.ResponseRecorder, content string) {
	t.Helper()
	if strings.Contains(w.Body.String(), content) || strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("%s: the file was sent: %d %q %v", name, w.Code, w.Body.String(), w.Header())
	}
}

// TestDesktopReview_ARunThatEndsAtItsLockTakesNoExec: the two review doors, an ask and a reviewed download, admit
// their exec as the Files panel's other doors do: a run that ends while the door waits for its lock (an end that
// keeps the sandbox for its computer folder's pass back holds it) answers 409 attach_not_running, and nothing
// reaches the sandbox or leaves it. Catches: a review door that thaws and execs on desktopRun's read alone.
func TestDesktopReview_ARunThatEndsAtItsLockTakesNoExec(t *testing.T) {
	for _, door := range []string{"ask", "release"} {
		e := newReviewEnv(t, types.TransferReview)
		id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
		ast := e.spend.Store.(*envelopeStore).Store.(*touchCountingStore).authzStore
		lockEndsRun(e.srv, e.run.ID, func() { endRun(ast, e.run.ID, func(r *types.AgentRun) { r.State = types.RunCompleted }) })
		var w *httptest.ResponseRecorder
		if door == "ask" {
			w = e.ask(t, reviewFile)
		} else {
			w = e.release(t, reviewFile, id)
		}
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonAttachNotRunning || e.folder.execs != 0 {
			t.Errorf("%s on a run that ended at its lock: %d %s, %d execs; want 409 %s and no exec",
				door, w.Code, w.Body.String(), e.folder.execs, reasonAttachNotRunning)
		}
		wantNothingSent(t, door, w, string(e.folder.files[reviewFile]))
	}
}

// TestDesktopReviewHoldThenReleasesExactlyThatFile: under review the folder is listed; a plain download is
// refused; the ask raises one file_download row naming the file's name, size and hash and who asked; nothing is
// released before the approval; the approval releases the file once and the second try is refused.
func TestDesktopReviewHoldThenReleasesExactlyThatFile(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	content := string(e.folder.files[reviewFile])
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"`+reviewFile+`"`) || !strings.Contains(w.Body.String(), `"download":"review"`) {
		t.Fatalf("list under review: %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path(reviewFile), e.ses["owner"], "")
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewRequired {
		t.Fatalf("a download with no review: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "no review", w, content)

	ap := e.asked(t, reviewFile)
	sc, ok := types.ParseFileDownloadScope(ap.RequestedScope)
	if ap.Kind != types.ApprovalFileDownload || ap.RunID != e.run.ID || ap.State != types.ApprovalPending || !ok ||
		sc != (types.FileDownloadScope{Name: reviewFile, Bytes: int64(len(content)), SHA256: digest(content), RequestedBy: entryOwnerSub}) {
		t.Fatalf("the raised row = %+v (scope %s)", ap, ap.RequestedScope)
	}
	w = e.release(t, reviewFile, ap.ID)
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewNotApproved {
		t.Fatalf("a release before the approval: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "pending", w, content)

	e.set(ap.ID, types.ApprovalApproved)
	w = e.release(t, reviewFile, ap.ID)
	if w.Code != http.StatusOK || w.Body.String() != content || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("the approved release: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if e.row(t, ap.ID).MintedJTI == "" {
		t.Fatal("the release did not spend the approval")
	}
	w = e.release(t, reviewFile, ap.ID)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonDesktopReviewSpent {
		t.Fatalf("a second release of one approval: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "second release", w, content)
	if got := e.reviewRows(); !slices.Equal(got, []string{"<nil>/denied", "requested/success", "released/success"}) {
		t.Errorf("download rows = %v", got)
	}
}

// TestDesktopReviewReleaseBoundToFileAndKind: an approval releases only the file it names, on its own run; an
// approved row of another kind, even one whose scope names the same file, releases nothing.
func TestDesktopReviewReleaseBoundToFileAndKind(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	e.folder.files["other.csv"] = []byte("other,file\n")
	reviewed := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
	for name, c := range map[string]struct {
		file string
		id   uuid.UUID
	}{
		"another file":           {"other.csv", reviewed},
		"a tool_call row":        {reviewFile, e.seed(types.ApprovalToolCall, e.run.ID, reviewFile, types.ApprovalApproved)},
		"an egress row":          {reviewFile, e.seed(types.ApprovalEgressDomain, e.run.ID, reviewFile, types.ApprovalApproved)},
		"another run's approval": {reviewFile, e.seed(types.ApprovalFileDownload, uuid.New(), reviewFile, types.ApprovalApproved)},
	} {
		w := e.release(t, c.file, c.id)
		if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewNotApproved {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
		wantNothingSent(t, name, w, string(e.folder.files[c.file]))
	}
	if e.row(t, reviewed).MintedJTI != "" {
		t.Fatal("a refused release spent the approval")
	}
	if w := e.release(t, reviewFile, reviewed); w.Code != http.StatusOK || w.Body.String() != string(e.folder.files[reviewFile]) {
		t.Fatalf("the approval's own file after the refusals: %d %s", w.Code, w.Body.String())
	}
}

// TestDesktopReviewRefusedNeverReleases: a denied, pending, expired or cancelled review releases nothing.
func TestDesktopReviewRefusedNeverReleases(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	for _, st := range []types.ApprovalState{types.ApprovalDenied, types.ApprovalPending, types.ApprovalExpired, types.ApprovalCancelled} {
		id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, st)
		w := e.release(t, reviewFile, id)
		if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewNotApproved {
			t.Errorf("%s: %d %s", st, w.Code, w.Body.String())
		}
		wantNothingSent(t, string(st), w, string(e.folder.files[reviewFile]))
	}
	if e.spend.spends != 0 {
		t.Fatalf("%d spends were tried on refused reviews", e.spend.spends)
	}
}

// TestDesktopReviewChangedFileRefused: a file whose bytes changed after the approval is refused, nothing is sent,
// and the approval stays unspent; the refusal is audited with the new size and hash.
func TestDesktopReviewChangedFileRefused(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
	e.folder.files[reviewFile] = []byte("agent,wrote,THIS\n") // same size, other bytes
	w := e.release(t, reviewFile, id)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonFileChangedSinceReview {
		t.Fatalf("a changed file: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "changed", w, "agent,wrote")
	if e.row(t, id).MintedJTI != "" || e.spend.spends != 0 {
		t.Fatal("a changed file spent the approval")
	}
	rows := e.transferRows("download")
	if len(rows) != 1 || rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), `"review":"changed"`) ||
		!strings.Contains(string(rows[0].Data), digest("agent,wrote,THIS\n")) {
		t.Fatalf("the changed-file row = %+v", rows)
	}
}

// TestDesktopReviewOverSizeRefused: a file over the review limit is never raised, and one that grew past it
// after the approval is refused with nothing sent.
func TestDesktopReviewOverSizeRefused(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	prev := desktopReviewMaxBytes
	desktopReviewMaxBytes = 20
	t.Cleanup(func() { desktopReviewMaxBytes = prev })
	e.folder.files["big.bin"] = bytes.Repeat([]byte("x"), 21)
	if w := e.ask(t, "big.bin"); w.Code != http.StatusRequestEntityTooLarge || wireReason(t, w) != reasonDesktopTransferTooLarge {
		t.Fatalf("an ask over the limit: %d %s", w.Code, w.Body.String())
	}
	if len(e.aps.requested) != 0 {
		t.Fatal("an ask over the limit raised a row")
	}
	id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
	e.folder.files[reviewFile] = bytes.Repeat([]byte("y"), 40)
	w := e.release(t, reviewFile, id)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonFileChangedSinceReview {
		t.Fatalf("a file grown past the limit: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "grown", w, "yyyy")
	if e.row(t, id).MintedJTI != "" {
		t.Fatal("a grown file spent the approval")
	}
}

// TestDesktopReviewPendingDuplicate409: one review of a file name waits at a time, whether the second ask sees
// the same bytes (the same row) or the store's one-per-name index refuses it.
func TestDesktopReviewPendingDuplicate409(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	e.asked(t, reviewFile)
	if w := e.ask(t, reviewFile); w.Code != http.StatusConflict || wireReason(t, w) != reasonDesktopReviewPending {
		t.Fatalf("a second ask of the same file: %d %s", w.Code, w.Body.String())
	}
	e.aps.requestErr = fmt.Errorf("approval: create: %w", types.ErrDuplicatePendingApproval)
	if w := e.ask(t, reviewFile); w.Code != http.StatusConflict || wireReason(t, w) != reasonDesktopReviewPending {
		t.Fatalf("an ask the index refuses: %d %s", w.Code, w.Body.String())
	}
	if len(e.aps.requested) != 1 {
		t.Fatalf("%d rows raised, want 1", len(e.aps.requested))
	}
}

// TestDesktopReviewDecidersSeeNameSizeHashOnly: the row the deciders read carries the name, size, hash and who
// asked, never the content; no audit row carries the name or the content.
func TestDesktopReviewDecidersSeeNameSizeHashOnly(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	e.folder.files["notes.txt"] = []byte(fileSentinel)
	ap := e.asked(t, "notes.txt")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ap.RequestedScope, &m); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(m)); !slices.Equal(keys, []string{"bytes", "name", "requested_by", "sha256"}) {
		t.Fatalf("the deciders' scope keys = %v", keys)
	}
	if bytes.Contains(ap.RequestedScope, []byte(fileSentinel)) || ap.Reason != "" {
		t.Fatalf("the row carries the content: %+v", ap)
	}
	e.set(ap.ID, types.ApprovalApproved)
	if w := e.release(t, "notes.txt", ap.ID); w.Code != http.StatusOK {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	for _, ev := range e.audit.snapshot() {
		if bytes.Contains(ev.Data, []byte(fileSentinel)) || bytes.Contains(ev.Data, []byte("notes.txt")) || strings.Contains(ev.Target, "notes.txt") {
			t.Fatalf("a file's content or name reached an audit row: %s %s", ev.Action, ev.Data)
		}
	}
}

// TestDesktopReviewHoldStoreUnreadableFailsClosed: when the approvals cannot be read, counted, raised or spent,
// or the store has no spend for a review, nothing is raised and nothing is sent.
func TestDesktopReviewHoldStoreUnreadableFailsClosed(t *testing.T) {
	content := "agent,wrote,this\n"
	t.Run("get", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
		e.srv.cfg.Approvals = getFailingApprovals{ApprovalService: e.aps, err: errors.New("approvals unavailable")}
		w := e.release(t, reviewFile, id)
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("an unreadable approval: %d %s", w.Code, w.Body.String())
		}
		wantNothingSent(t, "get", w, content)
	})
	t.Run("not found", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		e.srv.cfg.Approvals = getFailingApprovals{ApprovalService: e.aps, err: store.ErrNotFound}
		w := e.release(t, reviewFile, uuid.New())
		if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewNotApproved {
			t.Fatalf("an approval that does not exist: %d %s", w.Code, w.Body.String())
		}
		wantNothingSent(t, "not found", w, content)
	})
	t.Run("spend", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
		e.spend.spendErr = errors.New("approvals unavailable")
		w := e.release(t, reviewFile, id)
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("a failed spend: %d %s", w.Code, w.Body.String())
		}
		wantNothingSent(t, "spend", w, content)
	})
	t.Run("no spend", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
		e.srv.cfg.Store = e.spend.Store
		w := e.release(t, reviewFile, id)
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("a store with no review spend: %d %s", w.Code, w.Body.String())
		}
		wantNothingSent(t, "no spend", w, content)
	})
	t.Run("no approvals", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		e.srv.cfg.Approvals = nil
		if w := e.ask(t, reviewFile); w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("an ask with no approvals: %d %s", w.Code, w.Body.String())
		}
		w := e.release(t, reviewFile, uuid.New())
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("a release with no approvals: %d %s", w.Code, w.Body.String())
		}
		wantNothingSent(t, "no approvals", w, content)
	})
	t.Run("count and raise", func(t *testing.T) {
		e := newReviewEnv(t, types.TransferReview)
		e.aps.countErr = errors.New("approvals unavailable")
		if w := e.ask(t, reviewFile); w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("an ask whose count failed: %d %s", w.Code, w.Body.String())
		}
		e.aps.countErr, e.aps.requestErr = nil, errors.New("approvals unavailable")
		if w := e.ask(t, reviewFile); w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonDesktopReviewUnavailable {
			t.Fatalf("an ask whose raise failed: %d %s", w.Code, w.Body.String())
		}
		if len(e.aps.requested) != 0 {
			t.Fatal("a failed ask raised a row")
		}
	})
}

// TestDesktopReviewAskRefusals: an ask is refused, raising nothing, when download is not held for review, for a
// name that is not plain, and for a file the folder does not have.
func TestDesktopReviewAskRefusals(t *testing.T) {
	for _, c := range []struct {
		rule   types.TransferRule
		name   string
		code   int
		reason string
	}{
		{types.TransferAllowed, reviewFile, http.StatusConflict, reasonDesktopReviewNotHeld},
		{types.TransferBlocked, reviewFile, http.StatusForbidden, reasonDesktopTransferBlocked},
		{types.TransferReview, ".hidden", http.StatusBadRequest, reasonDesktopTransferBadRequest},
		{types.TransferReview, "missing.txt", http.StatusNotFound, reasonDesktopTransferNotFound},
	} {
		e := newReviewEnv(t, c.rule)
		if w := e.ask(t, c.name); w.Code != c.code || wireReason(t, w) != c.reason {
			t.Errorf("%s %s: %d %s, want %d %s", c.rule, c.name, w.Code, w.Body.String(), c.code, c.reason)
		}
		if len(e.aps.requested) != 0 {
			t.Errorf("%s %s raised a row", c.rule, c.name)
		}
	}
}

// TestDesktopDownloadBlockedAndAllowedUnchanged: blocked still refuses a download with an approved review in
// hand, and allowed still streams it with no review.
func TestDesktopDownloadBlockedAndAllowedUnchanged(t *testing.T) {
	e := newReviewEnv(t, types.TransferBlocked)
	id := e.seed(types.ApprovalFileDownload, e.run.ID, reviewFile, types.ApprovalApproved)
	w := e.release(t, reviewFile, id)
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopTransferBlocked {
		t.Fatalf("blocked with an approved review: %d %s", w.Code, w.Body.String())
	}
	wantNothingSent(t, "blocked", w, "agent,wrote")
	if e.row(t, id).MintedJTI != "" {
		t.Fatal("a blocked download spent the approval")
	}
	e = newReviewEnv(t, types.TransferAllowed)
	if w := doSSO(t, e.srv, http.MethodGet, e.path(reviewFile), e.ses["owner"], ""); w.Code != http.StatusOK || w.Body.String() != "agent,wrote,this\n" {
		t.Fatalf("allowed: %d %s", w.Code, w.Body.String())
	}
}
