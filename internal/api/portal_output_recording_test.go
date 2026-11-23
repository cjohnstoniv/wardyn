// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pinOutputStore is the pinned issuer portal's store (portal_pin_test.go) with run outputs kept in memory.
type pinOutputStore struct {
	*pinPortalStore
	store.RunOutputStore
}

// issuerOutputFixture is a grant of a person of an issuer portal pinned to unit pin, behind the real
// authentication middleware, on a server with a file recording store and a runner that does not capture
// output unless captures is set: the Kubernetes runner under the session recorder.
type issuerOutputFixture struct {
	srv          *Server
	h            *exchHarness
	ast          *authzStore
	mem          *memRunOutputs
	rs           *outputRecordingStore
	reg          *secretmask.Registry
	tok, person  string
	via          types.DelegationVia
	pin, outside types.GovernanceUnit
	delegate     types.Delegate
}

func newIssuerOutputFixture(t *testing.T, captures bool) *issuerOutputFixture {
	t.Helper()
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &issuerOutputFixture{rs: &outputRecordingStore{Store: fs}, reg: secretmask.NewRegistry(),
		person: oidc.PortalPrincipal(portalTestIssuer, "uid-output")}
	var pps *pinPortalStore
	f.h = newExchHarness(t, func(c *Config) {
		pps = &pinPortalStore{portalStore: c.Store.(*portalStore)}
		f.mem = newMemRunOutputs(pps)
		c.Store = pinOutputStore{pinPortalStore: pps, RunOutputStore: f.mem}
		rr := newRecoveringRunner()
		rr.execOutputUncaptured = !captures
		c.Runner, c.RecordingStore, c.MaskRegistry = rr, f.rs, f.reg
	})
	f.srv, f.ast = f.h.srv, f.h.ps.authzStore
	f.pin = types.GovernanceUnit{ID: uuid.New(), Name: "pin"}
	f.outside = types.GovernanceUnit{ID: uuid.New(), Name: "outside"}
	pps.units = []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}, f.pin, f.outside}
	f.delegate, _ = f.h.seedPortal(func(d *types.Delegate) { d.RunInUnder = &f.pin.ID })
	f.tok, f.via = f.grant(f.delegate)
	return f
}

// grant seeds a live grant of the fixture's person through portal d.
func (f *issuerOutputFixture) grant(d types.Delegate) (string, types.DelegationVia) {
	g := types.PortalGrant{ID: uuid.New(), DelegateID: d.ID, Principal: f.person, UserType: d.UserType}
	return f.h.ps.seedGrant(g), types.DelegationVia{Delegate: d.ID, Grant: g.ID}
}

// seed stores a completed non-interactive run of the person, under the pin, created through the portal; mut
// edits it first.
func (f *issuerOutputFixture) seed(mut func(*types.AgentRun)) types.AgentRun {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: f.person, State: types.RunCompleted, Agent: "claude-code",
		UnitID: &f.pin.ID, CreatedVia: &f.delegate.ID}
	if mut != nil {
		mut(&run)
	}
	f.ast.mu.Lock()
	f.ast.runs[run.ID] = run
	f.ast.mu.Unlock()
	return run
}

func (f *issuerOutputFixture) read(t *testing.T, tok string, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/output", tok, "")
}

// storeRow writes row as id's stored output row.
func (f *issuerOutputFixture) storeRow(id uuid.UUID, row store.RunOutput) {
	now := time.Now()
	row.RunID, row.CapturedAt = id, &now
	f.mem.mu.Lock()
	f.mem.rows[id] = row
	f.mem.mu.Unlock()
}

// outputAuditRows is every recorded row with action, in order.
func outputAuditRows(srv *Server, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// wantRefuseRow fails unless exactly one run.output.refuse row was written, a failure on run carrying only the
// answer's reason and status, stamped with the portal that asked.
func wantRefuseRow(t *testing.T, srv *Server, run uuid.UUID, reason string, status int, via types.DelegationVia) {
	t.Helper()
	rows := outputAuditRows(srv, auditRunOutputRefuse)
	if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].RunID == nil || *rows[0].RunID != run || rows[0].Target != run.String() {
		t.Fatalf("run.output.refuse rows = %+v, want one failure row on run %s", rows, run)
	}
	var keys map[string]json.RawMessage
	var d struct {
		Reason string              `json:"reason"`
		Status int                 `json:"status"`
		Via    types.DelegationVia `json:"via"`
	}
	if json.Unmarshal(rows[0].Data, &keys) != nil || json.Unmarshal(rows[0].Data, &d) != nil {
		t.Fatalf("run.output.refuse data %s does not decode", rows[0].Data)
	}
	if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, []string{"reason", "status", "via"}) ||
		d.Reason != reason || d.Status != status || d.Via.Delegate != via.Delegate {
		t.Fatalf("run.output.refuse data = %s, want only reason %q, status %d and the portal %s", rows[0].Data, reason, status, via.Delegate)
	}
}

// wantNoRows fails if any row with one of actions was written.
func wantNoRows(t *testing.T, srv *Server, actions ...string) {
	t.Helper()
	for _, a := range actions {
		if rows := outputAuditRows(srv, a); len(rows) != 0 {
			t.Fatalf("%s rows = %+v, want none", a, rows)
		}
	}
}

// The first read of a completed run with a committed main cast and no derived row recovers it for the issuer
// portal's own person: masked output events only, never what was typed or an attach session's cast, and the
// recovered row is stored. A cast with no output events is a successful, empty recovery.
func TestPortalOutputRecording_FirstReadRecovers(t *testing.T) {
	f := newIssuerOutputFixture(t, false)
	run := f.seed(nil)
	f.reg.Add(run.ID, []byte("portal-secret-value"))
	saveOutputCast(t, f.rs, run.ID, partsHeader+`[0,"o","built portal-secret-value ok\n"]`+"\n"+`[1,"i","TYPED-INPUT"]`+"\n"+
		`[2,"o","done\n"]`+"\n")
	if err := f.rs.SaveCastNamed(t.Context(), run.ID.String(), uuid.NewString(),
		strings.NewReader(partsHeader+`[0,"o","ATTACH-MARKER\n"]`+"\n")); err != nil {
		t.Fatal(err)
	}
	if _, found := f.mem.row(run.ID); found {
		t.Fatal("a derived row exists before the first read")
	}
	w := f.read(t, f.tok, run.ID)
	var got runOutputResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Source != recordingOutputSource ||
		!strings.HasPrefix(got.Output, "built ") || !strings.Contains(got.Output, "<secret-hidden>") ||
		!strings.HasSuffix(got.Output, "done\n") || got.CaptureGap {
		t.Fatalf("first read = %d %s, want 200 with the masked recovered output", w.Code, w.Body)
	}
	for _, leak := range []string{"TYPED-INPUT", "ATTACH-MARKER", "portal-secret-value"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("first read carried %q: %s", leak, w.Body)
		}
	}
	row, found, err := f.srv.readStoredRunOutput(t.Context(), run.ID)
	if err != nil || !found || row.Source != recordingOutputSource || row.CapturedAt == nil || string(row.Output) != got.Output {
		t.Fatalf("stored row after the first read = %+v found=%v err=%v, want the recovered row", row, found, err)
	}
	wantNoRows(t, f.srv, authz.AuditAction, auditRunOutputRefuse)

	empty := f.seed(nil)
	saveOutputCast(t, f.rs, empty.ID, partsHeader)
	w = f.read(t, f.tok, empty.ID)
	got = runOutputResponse{}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Source != recordingOutputSource ||
		got.Output != "" || got.CaptureGap {
		t.Fatalf("empty recovery = %d %s, want 200 with empty output and no capture gap", w.Code, w.Body)
	}
	wantNoRows(t, f.srv, authz.AuditAction, auditRunOutputRefuse)
}

// A missing cast (recovery writes a gap row), a recovered row erased through the recording-only erase, and a
// recovery still pending on an unreadable store answer the issuer portal's grant byte for byte alike, 409
// run_output_not_captured with no source fields, and write the same one audit row. None is 410 recording_erased.
func TestPortalOutputRecording_UnavailableCasesAreIdentical(t *testing.T) {
	f := newIssuerOutputFixture(t, false)
	st := f.srv.runOutputStore()
	missing := f.seed(nil)
	if err := f.srv.finalizeRecordingOutput(t.Context(), st, missing); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.mem.row(missing.ID); !r.CaptureGap || r.Source != recordingOutputSource {
		t.Fatalf("missing cast row = %+v, want a recording gap", r)
	}
	erased := f.seed(nil)
	saveOutputCast(t, f.rs, erased.ID, partsHeader+`[0,"o","erased marker\n"]`+"\n")
	if err := f.srv.finalizeRecordingOutput(t.Context(), st, erased); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mem.EraseRecordingRunOutputs(t.Context(), []uuid.UUID{erased.ID}); err != nil {
		t.Fatal(err)
	}
	if r, found, err := f.srv.readStoredRunOutput(t.Context(), erased.ID); err != nil || found || !r.RecordingErased {
		t.Fatalf("erased row = %+v found=%v err=%v, want RecordingErased and no row", r, found, err)
	}
	pending := f.seed(nil)
	saveOutputCast(t, f.rs, pending.ID, partsHeader+`[0,"o","pending marker\n"]`+"\n")
	f.rs.open = func(ctx context.Context, key string) (io.ReadCloser, error) {
		if strings.HasPrefix(key, pending.ID.String()) {
			return nil, errors.New("temporary storage outage")
		}
		return f.rs.Store.OpenCast(ctx, key)
	}

	type answer struct {
		code int
		body string
		rows []string
	}
	ask := func(id uuid.UUID) answer {
		before := len(f.srv.cfg.Audit.(*recRecorder).snapshot())
		w := f.read(t, f.tok, id)
		var rows []string
		for _, ev := range f.srv.cfg.Audit.(*recRecorder).snapshot()[before:] {
			rows = append(rows, ev.Action+" "+ev.Outcome+" "+string(ev.Data))
		}
		return answer{w.Code, w.Body.String(), rows}
	}
	want := ask(missing.ID)
	if want.code != http.StatusConflict || !strings.Contains(want.body, `"reason":"`+reasonRunOutputNotCaptured+`"`) ||
		len(want.rows) != 1 || !strings.HasPrefix(want.rows[0], auditRunOutputRefuse+" failure ") {
		t.Fatalf("missing cast = %+v, want 409 %s and one %s row", want, reasonRunOutputNotCaptured, auditRunOutputRefuse)
	}
	for _, field := range []string{`"source"`, `"capture_gap"`, reasonRecordingErased, "marker"} {
		if strings.Contains(want.body, field) {
			t.Errorf("missing cast answer carries %s: %s", field, want.body)
		}
	}
	for name, id := range map[string]uuid.UUID{"recording erased": erased.ID, "recovery pending": pending.ID} {
		if got := ask(id); got.code != want.code || got.body != want.body || !slices.Equal(got.rows, want.rows) {
			t.Errorf("%s = %+v, want the missing cast's answer %+v", name, got, want)
		}
	}
	if _, found := f.mem.row(pending.ID); found {
		t.Fatal("the pending recovery stored a row")
	}
	wantNoRows(t, f.srv, authz.AuditAction)
}

// A gap row stored beside a live stdout tail this process holds is withheld without a refusal: the tail is served
// and neither a refusal nor a run.output.refuse row is written.
func TestPortalOutputRecording_GapRowFallsThroughToStdout(t *testing.T) {
	f := newIssuerOutputFixture(t, true)
	run := f.seed(func(r *types.AgentRun) { r.State = types.RunRunning })
	writeExecOutput(t, f.srv.openExecOutput(run, false), "direct stdout\n")
	f.storeRow(run.ID, store.RunOutput{Source: recordingOutputSource, Incomplete: true, CaptureGap: true})
	w := f.read(t, f.tok, run.ID)
	var got runOutputResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Source != "stdout" || got.Output != "direct stdout\n" {
		t.Fatalf("gap row beside a live tail = %d %s, want 200 stdout", w.Code, w.Body)
	}
	wantNoRows(t, f.srv, authz.AuditAction, auditRunOutputRefuse)
}

// An interactive run's terminal stays closed to the issuer portal: a pane snapshot, a stored recording row and a
// committed cast with no derived row all answer 409 run_output_interactive with none of their bytes, the read
// stores nothing, and no refusal row is written.
func TestPortalOutputRecording_InteractiveRunStaysClosed(t *testing.T) {
	cases := map[string]func(*testing.T, *issuerOutputFixture, uuid.UUID){
		"pane snapshot": func(_ *testing.T, f *issuerOutputFixture, id uuid.UUID) {
			f.storeRow(id, store.RunOutput{Source: paneSnapshotSource, Output: []byte("SCREEN-MARKER\n")})
		},
		"stored recording row": func(_ *testing.T, f *issuerOutputFixture, id uuid.UUID) {
			f.storeRow(id, store.RunOutput{Source: recordingOutputSource, Output: []byte("SCREEN-MARKER\n"), Incomplete: true})
		},
		"committed cast, no row": func(t *testing.T, f *issuerOutputFixture, id uuid.UUID) {
			saveOutputCast(t, f.rs, id, partsHeader+`[0,"o","SCREEN-MARKER\n"]`+"\n")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newIssuerOutputFixture(t, false)
			run := f.seed(func(r *types.AgentRun) { r.Interactive = true })
			setup(t, f, run.ID)
			before, _ := f.mem.row(run.ID)
			w := f.read(t, f.tok, run.ID)
			if w.Code != http.StatusConflict || errorReason(w) != reasonRunOutputInteractive || strings.Contains(w.Body.String(), "SCREEN-MARKER") {
				t.Fatalf("interactive run = %d %s, want 409 %s and none of its terminal", w.Code, w.Body, reasonRunOutputInteractive)
			}
			if after, _ := f.mem.row(run.ID); after.Source != before.Source || string(after.Output) != string(before.Output) {
				t.Fatalf("the read changed the stored row: %+v -> %+v", before, after)
			}
			if f.rs.opens.Load() != 0 {
				t.Fatal("the read opened the recording")
			}
			wantNoRows(t, f.srv, authz.AuditAction)
			wantRefuseRow(t, f.srv, run.ID, reasonRunOutputInteractive, http.StatusConflict, f.via)
		})
	}
}

// Genuine denials keep their audit: another person's existing run is the concealed 404, byte for byte a missing
// run's, with one not_owner row; the missing run writes nothing. Neither starts a recovery.
func TestPortalOutputRecording_ForeignAndMissingRun(t *testing.T) {
	f := newIssuerOutputFixture(t, false)
	foreign := f.seed(func(r *types.AgentRun) { r.CreatedBy = oidc.PortalPrincipal(portalTestIssuer, "uid-other") })
	saveOutputCast(t, f.rs, foreign.ID, partsHeader+`[0,"o","FOREIGN-MARKER\n"]`+"\n")

	missing := f.read(t, f.tok, uuid.New())
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing run = %d %s, want 404", missing.Code, missing.Body)
	}
	wantNoRows(t, f.srv, authz.AuditAction, auditRunOutputRefuse)

	w := f.read(t, f.tok, foreign.ID)
	if w.Code != missing.Code || w.Body.String() != missing.Body.String() {
		t.Fatalf("foreign run = %d %s, want the missing run's %d %s", w.Code, w.Body, missing.Code, missing.Body)
	}
	if reasons := auditReasons(t, f.srv, authz.AuditAction); len(reasons) != 1 || reasons[0] != string(authz.ReasonNotOwner) {
		t.Fatalf("refusal reasons = %v, want exactly [not_owner]", reasons)
	}
	wantNoRows(t, f.srv, auditRunOutputRefuse)
	if _, found := f.mem.row(foreign.ID); found || f.rs.opens.Load() != 0 {
		t.Fatal("a foreign read started a recovery")
	}
}

// The pin and the portal-drive scope still gate the read; a second issuer portal on the same issuer and pin
// serves the same person and reads the same recovered output; a console portal's grant never gets it.
func TestPortalOutputRecording_Boundaries(t *testing.T) {
	f := newIssuerOutputFixture(t, false)
	outside := f.seed(func(r *types.AgentRun) { r.UnitID = &f.outside.ID })
	if w := f.read(t, f.tok, outside.ID); w.Code != http.StatusForbidden || errorReason(w) != reasonPortalUnitScope {
		t.Errorf("own run outside the pin = %d %s, want 403 %s", w.Code, w.Body, reasonPortalUnitScope)
	}
	other := uuid.New()
	driveRun := f.seed(func(r *types.AgentRun) { r.CreatedVia, r.PortalDrives = &other, true })
	if w := f.read(t, f.tok, driveRun.ID); w.Code != http.StatusForbidden || errorReason(w) != reasonPortalRunScope {
		t.Errorf("another portal's drive-bound run = %d %s, want 403 %s", w.Code, w.Body, reasonPortalRunScope)
	}
	if f.rs.opens.Load() != 0 {
		t.Fatal("a refused read opened the recording")
	}

	run := f.seed(nil)
	saveOutputCast(t, f.rs, run.ID, partsHeader+`[0,"o","second portal marker\n"]`+"\n")
	second, _ := f.h.seedPortal(func(d *types.Delegate) { d.RunInUnder = &f.pin.ID })
	tok2, _ := f.grant(second)
	if w := f.read(t, tok2, run.ID); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "second portal marker") {
		t.Errorf("a second issuer portal of the same person = %d %s, want 200 with the recovered output", w.Code, w.Body)
	}

	const person = "sub-person"
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spy := &outputRecordingStore{Store: fs}
	srv, ast, mem, tok, via := newDelegatedOutputServer(t, person, func(c *Config) {
		rr := newRecoveringRunner()
		rr.execOutputUncaptured = true
		c.Runner, c.RecordingStore = rr, spy
	})
	id := seedOutputRun(ast, mem, person, "stdout", "")
	delete(mem.rows, id)
	saveOutputCast(t, spy, id, partsHeader+`[0,"o","console portal marker\n"]`+"\n")
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/output", tok, "")
	if w.Code != http.StatusConflict || errorReason(w) != reasonRunOutputNotCaptured || strings.Contains(w.Body.String(), "marker") {
		t.Fatalf("console portal with a committed cast = %d %s, want 409 %s", w.Code, w.Body, reasonRunOutputNotCaptured)
	}
	if _, found := mem.row(id); found || spy.opens.Load() != 0 {
		t.Fatal("a console portal's read recovered the recording")
	}
	wantNoRows(t, srv, authz.AuditAction)
	wantRefuseRow(t, srv, id, reasonRunOutputNotCaptured, http.StatusConflict, via)
}

// A live run with no row answers the issuer portal 409 run_output_not_captured, with no refusal row and one
// run.output.refuse row carrying that reason.
func TestPortalOutputRecording_LiveRunNotCaptured(t *testing.T) {
	f := newIssuerOutputFixture(t, false)
	run := f.seed(func(r *types.AgentRun) { r.State = types.RunRunning })
	if w := f.read(t, f.tok, run.ID); w.Code != http.StatusConflict || errorReason(w) != reasonRunOutputNotCaptured {
		t.Fatalf("live run = %d %s, want 409 %s", w.Code, w.Body, reasonRunOutputNotCaptured)
	}
	wantNoRows(t, f.srv, authz.AuditAction)
	wantRefuseRow(t, f.srv, run.ID, reasonRunOutputNotCaptured, http.StatusConflict, f.via)
}
