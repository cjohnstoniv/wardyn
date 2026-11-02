// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rangeStore answers the recording range check with a fixed verdict and records what it was asked.
type rangeStore struct {
	store.Store
	rng   store.AuditChainRange
	found bool
	calls int
	run   uuid.UUID
	key   string
	from  time.Time
}

func (s *rangeStore) VerifyRecordingAuditRange(_ context.Context, run uuid.UUID, key string, from time.Time) (store.AuditChainRange, bool, error) {
	s.calls++
	s.run, s.key, s.from = run, key, from
	return s.rng, s.found, nil
}

// recordingReadAnswer is what the recording read (recording.Handler behind recordingAuthorizer, the
// gate GET /runs/{id}/recording/{key} uses) answers cookie for runID's own cast.
func recordingReadAnswer(t *testing.T, srv *Server, runID string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := chi.NewRouter()
	rt.Group(func(g chi.Router) {
		g.Use(srv.humanOrAdminAuth)
		g.Mount("/api/v1/runs/{id}/recording", recording.Handler(fs, srv.recordingAuthorizer))
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/recording/"+runID, nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}

func sameAnswer(a, b *httptest.ResponseRecorder) bool {
	return a.Code == b.Code && a.Body.String() == b.Body.String() && a.Header().Get("Content-Type") == b.Header().Get("Content-Type")
}

// TestRecordingChain_ReadersOnly: the range check answers whoever may read the run's recording (its
// owner, a super admin). Everyone else gets the recording read's own refusal, the same status, type
// and body, and so does a run that does not exist and a key with no recording, so the check tells a
// prober nothing the recording read does not. A refused caller on a real run leaves a not_owner row,
// and the store is never asked.
func TestRecordingChain_ReadersOnly(t *testing.T) {
	srv, run, rec, ses := entryServer(t, false)
	rs := &rangeStore{Store: srv.cfg.Store, found: true,
		rng: store.AuditChainRange{OK: true, FirstSeq: 4801, LastSeq: 4838, Checked: 38, Marks: []store.AuditChainMark{{ID: uuid.New(), Seq: 4815}}}}
	srv.cfg.Store = rs
	key := run.ID.String() + "~desktop-" + uuid.NewString()
	chain := func(runID string) string {
		return "/api/v1/runs/" + runID + "/recording-chain?key=" + url.QueryEscape(key) + "&from=2026-10-20T10:40:00Z"
	}
	path := chain(run.ID.String())

	w := doSSO(t, srv, http.MethodGet, path, ses["owner"], "")
	if w.Code != http.StatusOK {
		t.Fatalf("the owner: %d %s, want 200", w.Code, w.Body.String())
	}
	var got store.AuditChainRange
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.OK || got.FirstSeq != 4801 || got.LastSeq != 4838 || len(got.Marks) != 1 {
		t.Fatalf("the owner's verdict = %+v (%v), want the store's", got, err)
	}
	if rs.run != run.ID || rs.key != key || !rs.from.Equal(time.Date(2026, 10, 20, 10, 40, 0, 0, time.UTC)) {
		t.Fatalf("the store was asked run=%s key=%q from=%s", rs.run, rs.key, rs.from)
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["admin"], ""); w.Code != http.StatusOK {
		t.Fatalf("a super admin: %d %s, want 200 (an operator reads every recording)", w.Code, w.Body.String())
	}

	before := rs.calls
	other := ssoSession(t, "sub-other-member", "other@corp.example", oidc.RoleUser)
	refused := recordingReadAnswer(t, srv, run.ID.String(), other)
	if refused.Code != http.StatusNotFound {
		t.Fatalf("the recording read itself answered another member %d %s; the comparison needs its refusal", refused.Code, refused.Body.String())
	}
	for name, c := range map[string]*http.Cookie{"another member": other, "a security admin": ses["security"]} {
		if read := recordingReadAnswer(t, srv, run.ID.String(), c); !sameAnswer(read, refused) {
			t.Fatalf("%s: the recording read answered %d %q", name, read.Code, read.Body.String())
		}
		if w := doSSO(t, srv, http.MethodGet, path, c, ""); !sameAnswer(w, refused) {
			t.Fatalf("%s: %d %q (%s), want the recording read's refusal %d %q (%s)", name, w.Code, w.Body.String(),
				w.Header().Get("Content-Type"), refused.Code, refused.Body.String(), refused.Header().Get("Content-Type"))
		}
	}
	if rs.calls != before {
		t.Fatalf("a refused caller reached the store (%d calls, want %d)", rs.calls, before)
	}
	if ev := findAudit(rec.snapshot(), run.ID, "authz.denied", "denied"); ev == nil || !strings.Contains(string(ev.Data), "not_owner") {
		t.Fatalf("the refusal was not audited as not_owner: %+v", ev)
	}
	missing := uuid.NewString()
	if w := doSSO(t, srv, http.MethodGet, chain(missing), ses["owner"], ""); !sameAnswer(w, recordingReadAnswer(t, srv, missing, ses["owner"])) || !sameAnswer(w, refused) {
		t.Fatalf("a run that does not exist: %d %q, want the same refusal as a forbidden one", w.Code, w.Body.String())
	}
	rs.found = false
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); !sameAnswer(w, refused) {
		t.Fatalf("a key with no recording: %d %q, want the same refusal", w.Code, w.Body.String())
	}
}

// TestRecordingChain_Answers: a broken range is a 200 with ok=false; a missing from is a 400; a
// store with no chain is a 501.
func TestRecordingChain_Answers(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	base := srv.cfg.Store
	rs := &rangeStore{Store: base, found: true, rng: store.AuditChainRange{OK: false, BrokenSeq: 4820, Reason: "edited"}}
	srv.cfg.Store = rs
	path := "/api/v1/runs/" + run.ID.String() + "/recording-chain?key=k&from=2026-10-20T10:40:00Z"

	w := doSSO(t, srv, http.MethodGet, path, ses["owner"], "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), `"broken_seq":4820`) {
		t.Fatalf("a broken range: %d %s, want 200 ok=false naming the row", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, strings.TrimSuffix(path, "&from=2026-10-20T10:40:00Z"), ses["owner"], ""); w.Code != http.StatusBadRequest {
		t.Fatalf("no from: %d %s, want 400", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, strings.Replace(path, "key=k", "key=", 1), ses["owner"], ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"reason":"`+reasonRecordingChainKeyRequired+`"`) {
		t.Fatalf("no key: %d %s, want 400 %s", w.Code, w.Body.String(), reasonRecordingChainKeyRequired)
	}
	srv.cfg.Store = base
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("a store with no chain: %d %s, want 501", w.Code, w.Body.String())
	}
}

// heldRangeStore holds its first range check until released, so a second can arrive while it walks. The
// first check waits for the test to take its signal: a send that gave up when the test was not yet
// listening let the first check finish unheld, and the test then waited out its bound (#3600).
type heldRangeStore struct {
	store.Store
	walking chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func (s *heldRangeStore) VerifyRecordingAuditRange(context.Context, uuid.UUID, string, time.Time) (store.AuditChainRange, bool, error) {
	if s.held.CompareAndSwap(false, true) {
		s.walking <- struct{}{}
		<-s.release
	}
	return store.AuditChainRange{OK: true}, true, nil
}

// TestRecordingChain_OneWalkAtATime: the range walk hashes the org's log over the run's lifetime, so
// while one check walks, a second is refused with a 503 recording_chain_busy and a Retry-After, never
// queued; once the first ends the next one walks.
func TestRecordingChain_OneWalkAtATime(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	hs := &heldRangeStore{Store: srv.cfg.Store, walking: make(chan struct{}), release: make(chan struct{})}
	srv.cfg.Store = hs
	path := "/api/v1/runs/" + run.ID.String() + "/recording-chain?key=k&from=2026-10-20T10:40:00Z"

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- doSSO(t, srv, http.MethodGet, path, ses["owner"], "") }()
	select {
	case <-hs.walking:
	case <-time.After(10 * time.Second):
		t.Fatal("the first check never reached the walk")
	}
	w := doSSO(t, srv, http.MethodGet, path, ses["owner"], "")
	close(hs.release)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"reason":"`+reasonRecordingChainBusy+`"`) || w.Header().Get("Retry-After") == "" {
		t.Errorf("a second check while one walks: %d %q Retry-After=%q, want 503 %s with Retry-After", w.Code, w.Body.String(), w.Header().Get("Retry-After"), reasonRecordingChainBusy)
	}
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("the first check: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("a check after the walk ended: %d %s, want 200", w.Code, w.Body.String())
	}
}

// errRangeStore fails the recording range check.
type errRangeStore struct{ store.Store }

func (errRangeStore) VerifyRecordingAuditRange(context.Context, uuid.UUID, string, time.Time) (store.AuditChainRange, bool, error) {
	return store.AuditChainRange{}, false, context.DeadlineExceeded
}

// chainReadRows is the recording.read rows the run's audit holds.
func chainReadRows(rec *sshTestRecorder, runID uuid.UUID) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == auditRecordingRead && ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}

// TestRecordingChain_AnOperatorsCheckOfAnotherPersonsRunIsAudited: the check is a metadata read, and an
// operator's successful one of a run another person created writes the recording read's row, marked
// chain_check, with the recording's key as its target. The owner's check of their own run writes none.
func TestRecordingChain_AnOperatorsCheckOfAnotherPersonsRunIsAudited(t *testing.T) {
	srv, run, rec, ses := entryServer(t, false)
	srv.cfg.Store = &rangeStore{Store: srv.cfg.Store, found: true, rng: store.AuditChainRange{OK: true}}
	key := run.ID.String() + "~desktop-" + uuid.NewString()
	path := "/api/v1/runs/" + run.ID.String() + "/recording-chain?key=" + url.QueryEscape(key) + "&from=2026-10-20T10:40:00Z"

	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("the owner: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := chainReadRows(rec, run.ID); len(got) != 0 {
		t.Fatalf("an owner's check of their own run was audited: %+v", got)
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["admin"], ""); w.Code != http.StatusOK {
		t.Fatalf("a super admin: %d %s, want 200", w.Code, w.Body.String())
	}
	got := chainReadRows(rec, run.ID)
	if len(got) != 1 || got[0].Target != key || got[0].Outcome != "success" || got[0].Actor == "" || !strings.Contains(string(got[0].Data), `"operation":"chain_check"`) {
		t.Fatalf("recording.read rows = %+v, want one success row for the key marked chain_check", got)
	}
}

// TestRecordingChain_OnlyASuccessfulCheckIsAudited: a refused caller, a missing key or time, a store with no
// chain, a key with no recording, a failed walk and a check turned away as busy write no recording.read row.
func TestRecordingChain_OnlyASuccessfulCheckIsAudited(t *testing.T) {
	srv, run, rec, ses := entryServer(t, false)
	base := srv.cfg.Store
	path := "/api/v1/runs/" + run.ID.String() + "/recording-chain?key=k&from=2026-10-20T10:40:00Z"
	none := func(what string, code int, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != code {
			t.Fatalf("%s: %d %s, want %d", what, w.Code, w.Body.String(), code)
		}
		if got := chainReadRows(rec, run.ID); len(got) != 0 {
			t.Fatalf("%s wrote a recording.read row: %+v", what, got)
		}
	}

	srv.cfg.Store = &rangeStore{Store: base, found: true, rng: store.AuditChainRange{OK: true}}
	none("a security admin", http.StatusNotFound, doSSO(t, srv, http.MethodGet, path, ses["security"], ""))
	none("another member", http.StatusNotFound, doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sub-other-member", "other@corp.example", oidc.RoleUser), ""))
	none("no key", http.StatusBadRequest, doSSO(t, srv, http.MethodGet, strings.Replace(path, "key=k", "key=", 1), ses["admin"], ""))
	none("no from", http.StatusBadRequest, doSSO(t, srv, http.MethodGet, strings.TrimSuffix(path, "&from=2026-10-20T10:40:00Z"), ses["admin"], ""))
	srv.cfg.Store = &rangeStore{Store: base, found: false}
	none("a key with no recording", http.StatusNotFound, doSSO(t, srv, http.MethodGet, path, ses["admin"], ""))
	srv.cfg.Store = errRangeStore{Store: base}
	none("a failed walk", http.StatusInternalServerError, doSSO(t, srv, http.MethodGet, path, ses["admin"], ""))
	srv.cfg.Store = base
	none("a store with no chain", http.StatusNotImplemented, doSSO(t, srv, http.MethodGet, path, ses["admin"], ""))

	hs := &heldRangeStore{Store: base, walking: make(chan struct{}), release: make(chan struct{})}
	srv.cfg.Store = hs
	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- doSSO(t, srv, http.MethodGet, path, ses["admin"], "") }()
	select {
	case <-hs.walking:
	case <-time.After(10 * time.Second):
		t.Fatal("the first check never reached the walk")
	}
	none("a check turned away as busy", http.StatusServiceUnavailable, doSSO(t, srv, http.MethodGet, path, ses["admin"], ""))
	close(hs.release)
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("the check that held the walk: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := chainReadRows(rec, run.ID); len(got) != 1 {
		t.Fatalf("recording.read rows after the one successful check = %+v, want exactly one", got)
	}
}
