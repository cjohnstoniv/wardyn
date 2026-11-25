// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The live streams against a real unit tree and real audit rows, served through the server's whole handler. A stream
// is read through a pipe, so a test sees its events as they are written and sees it end when the handler returns.

// wallsPipeWriter is the response writer a pipe stream is served into.
type wallsPipeWriter struct {
	header http.Header
	pw     *io.PipeWriter
	ready  chan struct{} // closed at the first header, write or flush
	once   sync.Once
	code   int
}

func (w *wallsPipeWriter) Header() http.Header { return w.header }

func (w *wallsPipeWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.code = code
		close(w.ready)
	})
}

func (w *wallsPipeWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

func (w *wallsPipeWriter) Flush()                           { w.WriteHeader(http.StatusOK) }
func (w *wallsPipeWriter) SetWriteDeadline(time.Time) error { return nil }

// wallsPipeStream is one open request: its status, and its body as events.
type wallsPipeStream struct {
	t      *testing.T
	code   int
	br     *bufio.Reader
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// openStream serves GET path as cookie's caller and returns once the handler has answered.
func (e *unitAuditEnv) openStream(path string, cookie *http.Cookie) *wallsPipeStream {
	e.t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	r.AddCookie(cookie)
	w := &wallsPipeWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		panicFails(e.t, e.srv.Handler()).ServeHTTP(w, r)
		w.WriteHeader(http.StatusOK)
	}()
	s := &wallsPipeStream{t: e.t, br: bufio.NewReader(pr), pr: pr, cancel: cancel, done: done}
	e.t.Cleanup(s.stop)
	select {
	case <-w.ready:
		s.code = w.code
	case <-time.After(10 * time.Second):
		e.t.Fatalf("GET %s: no answer in 10s", path)
	}
	return s
}

// stop ends the request from the client's side and waits for the handler to return.
func (s *wallsPipeStream) stop() {
	s.once.Do(func() {
		s.cancel()
		_ = s.pr.CloseWithError(io.ErrClosedPipe)
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			s.t.Error("a stream's handler did not return within 5s of its client going away")
		}
	})
}

// readUntil reads events until f says to stop; the test fails when the stream ends or timeout passes first.
func (s *wallsPipeStream) readUntil(timeout time.Duration, f func(wallsSSEEvent) bool) {
	s.t.Helper()
	timer := time.AfterFunc(timeout, func() { _ = s.pr.CloseWithError(io.ErrClosedPipe) })
	defer timer.Stop()
	for {
		ev, ok := wallsSSENext(s.br)
		if !ok {
			s.t.Fatalf("the stream ended or %v passed before the events the test waited for", timeout)
		}
		if f(ev) {
			return
		}
	}
}

// ended reads the stream to its end and reports whether it ended within timeout.
func (s *wallsPipeStream) ended(timeout time.Duration) bool {
	timer := time.AfterFunc(timeout, func() { _ = s.pr.CloseWithError(io.ErrClosedPipe) })
	defer timer.Stop()
	_, err := io.Copy(io.Discard, s.pr)
	return err == nil
}

// wallsPGFrame is the part of a frame these tests read.
type wallsPGFrame struct {
	Runs []struct {
		RunID    uuid.UUID          `json:"run_id"`
		Counters json.RawMessage    `json:"counters"`
		Samples  []WallsFrameSample `json:"samples"`
	} `json:"runs"`
}

// wallsStreamPGEnv is a unit audit environment whose sampler beats every 50 ms and stops soon after its last stream.
func wallsStreamPGEnv(t *testing.T) *unitAuditEnv {
	t.Helper()
	e := newUnitAuditEnv(t)
	sm := &e.srv.wallsLive.sampler
	sm.tick, sm.idle = 50*time.Millisecond, 300*time.Millisecond
	return e
}

// wallsPGSeedEgress stores one egress row of run.
func (e *unitAuditEnv) wallsPGSeedEgress(run uuid.UUID, action, host string, extra map[string]any) {
	e.t.Helper()
	data := map[string]any{"host": host, "port": 443, "rule_source": "policy:allowlist"}
	for k, v := range extra {
		data[k] = v
	}
	e.seedRow(&run, action, host, data)
}

// TestWallsStreamUnitCountersMatchAudit_PG: with every class seeded inside a unit admin's subtree and outside it,
// the sum of their frames' counters equals the egress rows GET /api/v1/audit returns them, less the control-plane
// host rows and the upstream start rows, and each sample's host, port and approval id equal that row's.
func TestWallsStreamUnitCountersMatchAudit_PG(t *testing.T) {
	e := wallsStreamPGEnv(t)
	ctx := context.Background()
	in := []uuid.UUID{e.runA, e.runA1, e.runOwnA}
	start := time.Now()
	stream := e.openStream("/api/v1/walls/stream", e.adminA)
	if stream.code != http.StatusOK {
		t.Fatalf("the stream answered %d", stream.code)
	}
	for _, run := range append(append([]uuid.UUID{}, in...), e.runB, e.runNone) {
		for _, s := range wallsTopoPGSeeds() {
			e.wallsPGSeedEgress(run, s.action, fmt.Sprint(s.data["host"]), s.data)
		}
		e.wallsPGSeedEgress(run, "egress.hold", "held.example", map[string]any{"rule_source": "approval:pending", "approval_id": "ap-pg-held"})
	}

	rows, _, err := e.pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: wallsTopoActions, Since: start, RunIDs: in, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	var want wallsTopoCount
	hidden := map[uuid.UUID]bool{}
	for _, r := range rows {
		if v := wallsClassify(r); v.controlPlaneHost() || v.Class == wallsConfig {
			hidden[r.ID] = true
		} else {
			want.add(v, 1, false)
		}
	}
	wantCounters := *want.wire(false).(*WallsUnitCounters)

	var sum WallsUnitCounters
	samples := map[uuid.UUID]WallsFrameSample{}
	stream.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
		var f wallsPGFrame
		if ev.Event != "frame" || json.Unmarshal([]byte(ev.Data), &f) != nil {
			return false
		}
		for _, run := range f.Runs {
			var c WallsUnitCounters
			if err := json.Unmarshal(run.Counters, &c); err != nil {
				t.Fatalf("decode counters: %v", err)
			}
			sum.sum(c)
			for _, s := range run.Samples {
				samples[s.ID] = s
			}
		}
		return sum.total() >= wantCounters.total()
	})
	if sum != wantCounters {
		t.Errorf("the frames' counters sum to %+v, want wallsClassify's %+v over the same rows", sum, wantCounters)
	}
	audited := map[uuid.UUID]types.AuditEvent{}
	for _, ev := range e.read(e.adminA, "/api/v1/audit", "?limit=500") {
		if strings.HasPrefix(ev.Action, "egress.") && !hidden[ev.ID] && !ev.Time.Before(start) {
			audited[ev.ID] = ev
		}
	}
	if int64(len(audited)) != sum.total() {
		t.Errorf("the frames count %d rows, GET /api/v1/audit serves %d", sum.total(), len(audited))
	}
	for id, s := range samples {
		ev, ok := audited[id]
		if !ok {
			t.Errorf("sample %s is a row GET /api/v1/audit does not serve this caller", id)
			continue
		}
		// The stream serves every sample in the unit view; the feed keeps the member view for a run the caller
		// created, so its row is read through the unit view too (a host with a space or past 128 characters is not served).
		var data map[string]any
		_ = json.Unmarshal(unitViewData(ev.Data), &data)
		host, _ := data["host"].(string)
		port, _ := data["port"].(float64)
		approval, _ := data["approval_id"].(string)
		if s.Host != host || s.Port != int(port) || s.ApprovalID != approval {
			t.Errorf("sample %s: host %q port %d approval %q, GET /api/v1/audit serves %q %v %q", id, s.Host, s.Port, s.ApprovalID, host, port, approval)
		}
	}
	if len(samples) == 0 {
		t.Error("no sample was streamed, so the comparison proved nothing about them")
	}
}

// TestWallsStreamNoCrossUnitLeak_PG: with two sibling subtrees, what admin A's two streams carry never names a run,
// host or approval id of subtree B, and admin A is refused B's run stream.
func TestWallsStreamNoCrossUnitLeak_PG(t *testing.T) {
	e := wallsStreamPGEnv(t)
	orgStream := e.openStream("/api/v1/walls/stream", e.adminA)
	runStream := e.openStream("/api/v1/runs/"+e.runA.String()+"/walls/stream", e.adminA)
	if orgStream.code != http.StatusOK || runStream.code != http.StatusOK {
		t.Fatalf("admin A's streams answered %d and %d", orgStream.code, runStream.code)
	}
	if foreign := e.openStream("/api/v1/runs/"+e.runB.String()+"/walls/stream", e.adminA); foreign.code != http.StatusNotFound {
		t.Errorf("admin A on subtree B's run stream: %d, want 404", foreign.code)
	}
	e.wallsPGSeedEgress(e.runB, "egress.hold", "b-secret.example", map[string]any{"rule_source": "approval:pending", "approval_id": "ap-b-secret"})
	e.wallsPGSeedEgress(e.runB, "egress.deny", "b-secret.example", map[string]any{"rule_source": "policy:denied"})
	e.seedRow(&e.runB, "approval.decide", "ap-b-secret", map[string]any{"approval_id": "ap-b-secret"})
	e.wallsPGSeedEgress(e.runA, "egress.hold", "a-visible.example", map[string]any{"rule_source": "approval:pending", "approval_id": "ap-a-visible"})
	for name, s := range map[string]*wallsPipeStream{"the organisation's stream": orgStream, "the run's stream": runStream} {
		var seen strings.Builder
		s.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
			seen.WriteString(ev.Event + " " + ev.Data + "\n")
			return strings.Contains(ev.Data, "a-visible.example")
		})
		for _, leak := range []string{e.runB.String(), "b-secret.example", "ap-b-secret"} {
			if strings.Contains(seen.String(), leak) {
				t.Errorf("%s carried %q of the other subtree", name, leak)
			}
		}
		if !strings.Contains(seen.String(), e.runA.String()) {
			t.Errorf("%s never named admin A's own run", name)
		}
	}
}

// TestWallsStreamNullUnitRunIsInNoUnitView_PG: a run with no unit has its rows in no unit admin's organisation
// stream, a Default-unit admin's included, and its owner reads them on the run's stream.
func TestWallsStreamNullUnitRunIsInNoUnitView_PG(t *testing.T) {
	e := wallsStreamPGEnv(t)
	const defaultSub = "sub-walls-default-admin"
	e.grant(types.GovernanceUnit{ID: store.DefaultUnitID, Name: "Default"}, types.CapabilitySubjectUser, defaultSub)
	defaultAdmin := uaSession(t, defaultSub, []string{}, false)
	owner := uaSession(t, uaRunOwner, []string{}, false)
	adminStream := e.openStream("/api/v1/walls/stream", e.adminA)
	defaultStream := e.openStream("/api/v1/walls/stream", defaultAdmin)
	ownerStream := e.openStream("/api/v1/runs/"+e.runNone.String()+"/walls/stream", owner)
	for _, s := range []*wallsPipeStream{adminStream, defaultStream, ownerStream} {
		if s.code != http.StatusOK {
			t.Fatalf("a stream answered %d, want 200", s.code)
		}
	}
	e.wallsPGSeedEgress(e.runNone, "egress.allow", "no-unit.example", nil)
	e.wallsPGSeedEgress(e.runNone, "egress.deny", "no-unit.example", map[string]any{"rule_source": "policy:denied"})
	e.wallsPGSeedEgress(e.runA, "egress.allow", "sentinel.example", nil)
	for name, s := range map[string]*wallsPipeStream{"a unit admin": adminStream, "a Default-unit admin": defaultStream} {
		// The run's rows were written before the sentinel's, so any frame that named them is read before this one ends.
		var seen strings.Builder
		s.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
			seen.WriteString(ev.Data)
			return strings.Contains(ev.Data, e.runA.String())
		})
		if strings.Contains(seen.String(), e.runNone.String()) {
			t.Errorf("%s was streamed the rows of a run with no unit", name)
		}
	}
	var counts WallsCounters
	ownerStream.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
		var f wallsPGFrame
		if ev.Event != "frame" || json.Unmarshal([]byte(ev.Data), &f) != nil {
			return false
		}
		for _, run := range f.Runs {
			var c WallsCounters
			if run.RunID == e.runNone && json.Unmarshal(run.Counters, &c) == nil {
				counts.sum(c)
			}
		}
		return counts.Allowed >= 1 && counts.Refused >= 1
	})
}

// TestWallsStreamRevocationEndsTheStream_PG: revoking a unit admin's grant ends both their streams within one
// refresh, and the production refresh is the 15 seconds that bounds how long a revoked grant keeps serving.
func TestWallsStreamRevocationEndsTheStream_PG(t *testing.T) {
	e := wallsStreamPGEnv(t)
	e.srv.runEvents.beat = 200 * time.Millisecond
	const sub = "sub-walls-revocation"
	g := e.grant(e.a1, types.CapabilitySubjectUser, sub)
	cookie := uaSession(t, sub, []string{}, false)
	inside := e.seedRun(&e.a1.ID, uaRunOwner)
	orgStream := e.openStream("/api/v1/walls/stream", cookie)
	runStream := e.openStream("/api/v1/runs/"+inside.String()+"/walls/stream", cookie)
	if orgStream.code != http.StatusOK || runStream.code != http.StatusOK {
		t.Fatalf("the streams answered %d and %d before the revoke, want 200", orgStream.code, runStream.code)
	}
	if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, e.a1.ID, g.ID); err != nil {
		t.Fatalf("revoke the grant: %v", err)
	}
	if !orgStream.ended(5 * time.Second) {
		t.Error("the organisation stream was still open 5s after the revoke, with a 200 ms refresh")
	}
	if !runStream.ended(5 * time.Second) {
		t.Error("the run's stream was still open 5s after the revoke, with a 200 ms refresh")
	}
	if production := runEventsBeat; production != 15*time.Second {
		t.Errorf("the production refresh is %v, want 15s: that is the bound on how long a revoked grant keeps serving", production)
	}
}

// TestWallsStreamOwnRunOutsideSubtree_PG: a unit admin on their own run outside their subtree gets the full view,
// whose samples carry the refusal's reason.
func TestWallsStreamOwnRunOutsideSubtree_PG(t *testing.T) {
	e := wallsStreamPGEnv(t)
	own := e.seedRun(&e.b.ID, uaSubAdminA)
	stream := e.openStream("/api/v1/runs/"+own.String()+"/walls/stream", e.adminA)
	if stream.code != http.StatusOK {
		t.Fatalf("the stream answered %d, want 200", stream.code)
	}
	e.wallsPGSeedEgress(own, "egress.deny", "own.example", map[string]any{"rule_source": "policy:denied", "refusal_reason": "the-owners-reason"})
	stream.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
		return ev.Event == "frame" && strings.Contains(ev.Data, `"refusal_reason":"the-owners-reason"`) && strings.Contains(ev.Data, `"control":`)
	})
}

// TestWallsStreamNoPlantedSecret_PG: a secret value and a secret name planted in the run's rows and description
// never appear in an event of either view.
func TestWallsStreamNoPlantedSecret_PG(t *testing.T) {
	const name, value = "prod-db-password", "s3cr3t-planted-value"
	e := wallsStreamPGEnv(t)
	secret := map[string]any{"secret_name": name, "secret_value": value, "grants": map[string]any{name: value}, "env": map[string]any{"TOKEN": value}}
	ws := e.openStream("/api/v1/walls/stream", e.super)
	us := e.openStream("/api/v1/walls/stream", e.adminA)
	e.wallsPGSeedEgress(e.runA, "egress.allow", "planted.example", secret)
	e.seedRow(&e.runA, "credential.mint", name, secret)
	e.seedRow(&e.runA, "approval.decide", name, map[string]any{"approval_id": "ap-planted", "secret_name": name, "secret_value": value})
	e.wallsPGSeedEgress(e.runA, "egress.allow", "sentinel.example", nil)
	for who, s := range map[string]*wallsPipeStream{"a super admin": ws, "a unit admin": us} {
		var seen strings.Builder
		s.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
			seen.WriteString(ev.Event + " " + ev.Data + "\n")
			return strings.Contains(ev.Data, "sentinel.example")
		})
		for _, leak := range []string{name, value} {
			if strings.Contains(seen.String(), leak) {
				t.Errorf("%s was streamed %q", who, leak)
			}
		}
	}
}

// TestWallsStreamSamplerEndToEnd_PG: the real loop, beating every 50 ms, reads seeded rows written across several
// beats, and every row lands in exactly one beat: the counts sum to the rows seeded and the frame numbers only rise.
func TestWallsStreamSamplerEndToEnd_PG(t *testing.T) {
	const batches, perBatch = 4, 10
	e := wallsStreamPGEnv(t)
	stream := e.openStream("/api/v1/walls/stream", e.super)
	if stream.code != http.StatusOK {
		t.Fatalf("the stream answered %d, want 200", stream.code)
	}
	for range batches {
		for range perBatch {
			e.wallsPGSeedEgress(e.runA, "egress.allow", "e2e.example", nil)
		}
		time.Sleep(80 * time.Millisecond)
	}
	var sum WallsCounters
	var last uint64
	read := func(ev wallsSSEEvent) {
		if ev.Event != "frame" {
			return
		}
		_, num, _ := strings.Cut(ev.ID, ".")
		n, err := strconv.ParseUint(num, 10, 64)
		if err != nil || n <= last {
			t.Errorf("frame id %q after number %d: the numbers must only rise", ev.ID, last)
		}
		last = n
		var f wallsPGFrame
		if err := json.Unmarshal([]byte(ev.Data), &f); err != nil {
			t.Fatalf("decode a frame: %v", err)
		}
		for _, run := range f.Runs {
			var c WallsCounters
			if err := json.Unmarshal(run.Counters, &c); err != nil {
				t.Fatalf("decode counters: %v", err)
			}
			sum.sum(c)
		}
	}
	stream.readUntil(20*time.Second, func(ev wallsSSEEvent) bool {
		read(ev)
		return sum.Allowed >= batches*perBatch
	})
	// A row read twice would push the sum past the rows seeded within a second more of beats.
	timer := time.AfterFunc(time.Second, func() { _ = stream.pr.CloseWithError(io.ErrClosedPipe) })
	defer timer.Stop()
	for {
		ev, ok := wallsSSENext(stream.br)
		if !ok {
			break
		}
		read(ev)
	}
	if sum.Allowed != batches*perBatch {
		t.Errorf("the frames count %d allowed rows, want exactly %d", sum.Allowed, batches*perBatch)
	}
}
