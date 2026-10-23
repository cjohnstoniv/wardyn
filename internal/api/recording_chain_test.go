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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	srv.cfg.Store = base
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("a store with no chain: %d %s, want 501", w.Code, w.Body.String())
	}
}
