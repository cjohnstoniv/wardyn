// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type outputRecordingStore struct {
	recording.Store
	opens atomic.Int64
	open  func(context.Context, string) (io.ReadCloser, error)
}

func (s *outputRecordingStore) OpenCast(ctx context.Context, key string) (io.ReadCloser, error) {
	s.opens.Add(1)
	if s.open != nil {
		return s.open(ctx, key)
	}
	return s.Store.OpenCast(ctx, key)
}

func newRecordingOutputFixture(t *testing.T, shape ...func(*Config)) (*outputFixture, *outputRecordingStore) {
	t.Helper()
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rs := &outputRecordingStore{Store: fs}
	f := newOutputFixture(t, append([]func(*Config){func(c *Config) {
		c.RecordingStore = rs
		c.Runner.(*outputRunner).execOutputUncaptured = true
	}}, shape...)...)
	f.st.mu.Lock()
	f.st.state = types.RunCompleted
	f.st.mu.Unlock()
	f.run.State = types.RunCompleted
	return f, rs
}

func saveOutputCast(t *testing.T, rs recording.Store, id uuid.UUID, body string) {
	t.Helper()
	if err := rs.SaveCast(t.Context(), id.String(), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingOutput_MasksDecodedStreamAcrossEventsAndParts(t *testing.T) {
	reg := secretmask.NewRegistry()
	f, rs := newRecordingOutputFixture(t, func(c *Config) { c.MaskRegistry = reg; c.RunOutputTailBytes = 65 })
	reg.Add(f.run.ID, []byte("event-split-secret"))
	reg.Add(f.run.ID, []byte("part-split-secret"))
	reg.Add(f.run.ID, []byte("line\nsecret-value"))
	saveOutputCast(t, rs, f.run.ID, partsHeader+`[0,"o","old old old old old old old old old old old old old\n"]`+"\n"+
		`[1,"o","event-split-"]`+"\n"+`[2,"o","secret "]`+"\n"+`[3,"o","part-split-"]`+"\n")
	if err := rs.SaveCastNamed(t.Context(), f.run.ID.String(), recording.PartSuffix(2), strings.NewReader(partsHeader+
		`[4,"o","secret line\nsecret-value end\n"]`+"\n")); err != nil {
		t.Fatal(err)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	r := f.finalRow(t)
	want := "<secret-hidden> <secret-hidden> <secret-hidden> end\n"
	if !strings.HasSuffix(string(r.Output), want) || len(r.Output) != 65 || !r.Truncated || !r.Incomplete || r.CaptureGap || r.Source != recordingOutputSource {
		t.Fatalf("recovered row = %+v output=%q", r, r.Output)
	}
	for _, secret := range []string{"event-split", "part-split", "secret-value"} {
		if strings.Contains(string(r.Output), secret) {
			t.Fatalf("secret in stored bytes %q", r.Output)
		}
	}
	if f.srv.tailFor(f.run.ID) != nil {
		t.Fatal("recording recovery exposed a live stdout tail")
	}
}

func TestRecordingOutput_GapThenLateCommittedUpload(t *testing.T) {
	f, rs := newRecordingOutputFixture(t)
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	gap := f.finalRow(t)
	if !gap.CaptureGap || gap.Source != recordingOutputSource || !gap.Incomplete || len(gap.Output) != 0 {
		t.Fatalf("missing cast: %+v", gap)
	}
	reads := rs.opens.Load()
	if code, got := f.get(t); code != http.StatusOK || !got.CaptureGap || rs.opens.Load() != reads {
		t.Fatal("gap read ignored repair cooldown")
	}
	saveOutputCast(t, rs, f.run.ID, partsHeader+`[0,"o","late marker\n"]`+"\n")
	if err := f.srv.uploadedRecordingOutput(t.Context(), f.run.ID); err != nil {
		t.Fatal(err)
	}
	if code, got := f.get(t); code != http.StatusOK || got.Output != "late marker\n" || got.CaptureGap || !got.Incomplete || !got.Complete {
		t.Fatalf("late upload = %d %+v", code, got)
	}
	// A malformed later source never destroys a better previously kept result.
	saveOutputCast(t, rs, f.run.ID, "plaintext fallback log\n")
	if err := f.srv.uploadedRecordingOutput(t.Context(), f.run.ID); err != nil {
		t.Fatal(err)
	}
	if _, got := f.get(t); got.Output != "late marker\n" {
		t.Fatalf("invalid replacement lost %q", got.Output)
	}
}

func TestRecordingOutput_LostReceiptReadRepairAndDurableRetry(t *testing.T) {
	f, rs := newRecordingOutputFixture(t)
	saveOutputCast(t, rs, f.run.ID, partsHeader+`[0,"o","committed without receipt\n"]`+"\n")
	// Source commit succeeded, but the process died before Queue and the receipt.
	if code, got := f.get(t); code != http.StatusOK || got.Output != "committed without receipt\n" {
		t.Fatalf("repair = %d %+v", code, got)
	}
	f2, rs2 := newRecordingOutputFixture(t)
	saveOutputCast(t, rs2, f2.run.ID, partsHeader+`[0,"o","retry marker\n"]`+"\n")
	rs2.open = func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("temporary storage outage")
	}
	f2.srv.FinishRunOutput(t.Context(), f2.run.ID)
	f2.mem.mu.Lock()
	p := f2.mem.recording[f2.run.ID]
	if p.completed == p.requested {
		t.Error("transient source failure consumed durable work")
	}
	p.claimedAt = time.Now().Add(-2 * runOutputPendingStale)
	f2.mem.recording[f2.run.ID] = p
	f2.mem.mu.Unlock()
	rs2.open = nil
	if err := f2.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := f2.finalRow(t); string(r.Output) != "retry marker\n" {
		t.Fatalf("retry row = %+v", r)
	}
}

func TestRecordingOutput_InvalidAndEmptyRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name, cast string
		gap        bool
	}{
		{"plain log", "unstructured log\n", true},
		{"malformed event", partsHeader + `[0,"o",17]` + "\n", true},
		{"header only", partsHeader, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rs := newRecordingOutputFixture(t)
			saveOutputCast(t, rs, f.run.ID, tc.cast)
			f.srv.FinishRunOutput(t.Context(), f.run.ID)
			r := f.finalRow(t)
			if r.CaptureGap != tc.gap || len(r.Output) != 0 || !r.Incomplete {
				t.Fatalf("row = %+v", r)
			}
		})
	}
}

func TestRecordingOutput_SourceReadCancellationLeavesWorkPending(t *testing.T) {
	f, rs := newRecordingOutputFixture(t)
	f.srv.runOutputRecoverWaitOverride = 20 * time.Millisecond
	ended := make(chan struct{})
	rs.open = func(ctx context.Context, _ string) (io.ReadCloser, error) {
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	awaitRecordingSignal(t, ended)
	if _, found := f.mem.row(f.run.ID); found {
		t.Fatal("canceled read finalized a clean result")
	}
	f.mem.mu.Lock()
	defer f.mem.mu.Unlock()
	p := f.mem.recording[f.run.ID]
	if p.requested == p.completed {
		t.Fatal("canceled read lost restart work")
	}
}

func TestRecordingOutput_PreservesIndependentFinalRowsAndOptOuts(t *testing.T) {
	for _, source := range []string{"stdout", paneSnapshotSource} {
		t.Run(source, func(t *testing.T) {
			f, rs := newRecordingOutputFixture(t)
			if err := f.mem.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: f.run.ID, Source: source, Output: []byte("independent")}); err != nil {
				t.Fatal(err)
			}
			f.srv.FinishRunOutput(t.Context(), f.run.ID)
			if r := f.finalRow(t); string(r.Output) != "independent" || r.Source != source || rs.opens.Load() != 0 {
				t.Fatalf("changed independent row %+v", r)
			}
		})
	}
	for _, mode := range []string{"output off", "persistence off", "recording off", "interactive", "sign in", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f, rs := newRecordingOutputFixture(t)
			switch mode {
			case "output off":
				f.srv.cfg.ExecOutputTailOff = true
			case "persistence off":
				f.srv.cfg.RunOutputPersistOff = true
			case "recording off":
				f.srv.cfg.RecordingStore = nil
			case "interactive":
				f.st.run.Interactive = true
			case "sign in":
				f.st.run.Task = harnessLoginTask
			case "expired":
				ago := time.Now().Add(-2 * time.Hour)
				f.st.run.EndedAt = &ago
				f.srv.cfg.RunOutputRetention = time.Hour
			}
			f.srv.FinishRunOutput(t.Context(), f.run.ID)
			if _, found := f.mem.row(f.run.ID); found || rs.opens.Load() != 0 {
				t.Fatal("excluded run read a recording or wrote output")
			}
		})
	}
}

// A recordings erasure fences the derived copy only: direct stdout this process
// still holds, with no stored row beside it, stays readable.
func TestRecordingOutput_ErasedSourceLeavesIndependentLiveTailReadable(t *testing.T) {
	f := newOutputFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
	writeExecOutput(t, f.open(t), "direct stdout\n")
	if _, err := f.mem.EraseRecordingRunOutputs(t.Context(), []uuid.UUID{f.run.ID}); err != nil {
		t.Fatal(err)
	}
	if code, got := f.get(t); code != http.StatusOK || got.Output != "direct stdout\n" || got.Source != "stdout" {
		t.Fatalf("independent tail after a recordings erasure = %d %+v", code, got)
	}
}

func TestRecordingOutput_PrivacyIncludesGapsErasureAndDisabledBackend(t *testing.T) {
	var mem *memRunOutputs
	rr := newRecoveringRunner()
	rr.execOutputUncaptured = true
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spy := &outputRecordingStore{Store: fs}
	srv, _, seed := runOutputServer(t, func(c *Config) { mem = newMemRunOutputs(c.Store); c.Store = mem; c.Runner = rr; c.RecordingStore = spy })
	id := seed(types.AgentRun{})
	sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	wantCode, _, want := getRunOutput(t, srv, id, "", sec)
	if spy.opens.Load() != 0 {
		t.Fatal("foreign security admin triggered missing-row repair")
	}
	for _, gap := range []bool{false, true} {
		now := time.Now()
		mem.rows[id] = store.RunOutput{RunID: id, Source: recordingOutputSource, Output: []byte("private marker"), CaptureGap: gap, Incomplete: true, CapturedAt: &now}
		for _, configured := range []recording.Store{spy, nil} {
			srv.cfg.RecordingStore = configured
			reads := spy.opens.Load()
			code, _, refusal := getRunOutput(t, srv, id, "", sec)
			if code != wantCode || refusal != want || reads != spy.opens.Load() {
				t.Fatalf("foreign security admin learned recording presence: %d %+v vs %d %+v", code, refusal, wantCode, want)
			}
		}
		if code, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t)); code != http.StatusOK || got.Output != "private marker" {
			t.Fatalf("owner = %d %+v", code, got)
		}
	}
	operator := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if code, got, _ := getRunOutput(t, srv, id, "", operator); code != http.StatusOK || got.Output != "private marker" {
		t.Fatalf("operator = %d %+v", code, got)
	}
	other := ssoSession(t, "other-member", "other@example.com", oidc.RoleUser)
	if code, _, _ := getRunOutput(t, srv, id, "", other); code != http.StatusNotFound {
		t.Fatalf("foreign member = %d", code)
	}
	owned := seed(types.AgentRun{CreatedBy: secAdminSub})
	ownedRow := mem.rows[id]
	ownedRow.RunID = owned
	mem.rows[owned] = ownedRow
	if code, got, _ := getRunOutput(t, srv, owned, "", sec); code != http.StatusOK || got.Output != "private marker" {
		t.Fatalf("security admin's own run = %d %+v", code, got)
	}
	if _, err := mem.EraseRecordingRunOutputs(t.Context(), []uuid.UUID{id}); err != nil {
		t.Fatal(err)
	}
	if code, _, refusal := getRunOutput(t, srv, id, "", sec); code != wantCode || refusal != want {
		t.Fatal("foreign security admin learned source erasure")
	}
	if code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t)); code != http.StatusGone || refusal.Reason != reasonRecordingErased {
		t.Fatalf("owner erase = %d %+v", code, refusal)
	}
}
