// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tailLabResumes is tailLab with the resume read a record that does not decode is decided by (unreadOwed): the
// stored record holds a resume whatever statusErr says of its decode.
type tailLabResumes struct{ *tailLab }

func (l tailLabResumes) RunLocalFolderHoldsResume(_ context.Context, id uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rows[id]
	return r != nil && r.status.Resume != nil, nil
}

// tailStepOf's value list for a record that holds a resume no pass will run for: past the keep it is given up as an
// owed pass is, whether the record reads or not; inside the keep, and with no resume, the step is the one it was. A
// run that owes a pass is decided as before. Catches: a give-up past the keep narrower than the keep
// (keepsSandboxForFolder keeps on any resume), a resume given up inside the keep, and every row past the keep given
// up whatever it holds.
func TestTailStepOf_AResumeNoPassWillRunForValueList(t *testing.T) {
	killed := func(_ *tailRig, r *tailRow) { r.run.State = types.RunKilled }
	copiedIn := func(_ *tailRig, r *tailRow) { r.folder.Direction = types.ComputerFolderIn }
	blocked := func(g *tailRig, _ *tailRow) { g.lab.dataOut = types.TransferBlocked }
	owed := func(*tailRig, *tailRow) {}
	for _, tc := range []struct {
		name                     string
		change                   func(*tailRig, *tailRow)
		resume, unreadable, past bool
		want                     tailStep
		note                     string
		wantErr                  bool
	}{
		{"a killed run's resume, past the keep", killed, true, false, true, tailGiveUp, "", false},
		{"a killed run's resume, inside the keep", killed, true, false, false, tailHistory, "", false},
		{"a killed run with no resume, past the keep", killed, false, false, true, tailHistory, "", false},
		{"a killed run's unreadable record holding a resume, past the keep", killed, true, true, true, tailGiveUp, folderStatusUnreadable, false},
		{"a killed run's unreadable record holding no resume, past the keep", killed, false, true, true, tailHistory, "", false},
		{"a killed run's unreadable record, inside the keep", killed, true, true, false, tailNone, "", true},
		{"a folder copied in only, holding a resume, past the keep", copiedIn, true, false, true, tailGiveUp, "", false},
		{"a folder copied in only, no resume, past the keep", copiedIn, false, false, true, tailHistory, "", false},
		{"data out not allowed, holding a resume, past the keep", blocked, true, false, true, tailGiveUp, "", false},
		{"data out not allowed, holding a resume, inside the keep", blocked, true, false, false, tailHistory, "", false},
		{"a pass owed, past the keep", owed, true, false, true, tailGiveUp, "", false},
		{"a pass owed, inside the keep", owed, true, false, false, tailFolder, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTailRig(t)
			g.s.cfg.Store = tailLabResumes{g.lab}
			row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
			if !tc.resume {
				row.status = types.RunLocalFolderStatus{Gen: 3, Settled: 3}
			}
			tc.change(g, row)
			if tc.unreadable {
				g.lab.statusErr = errors.New("the record does not decode")
			}
			_, step, note, err := g.s.tailStepOf(context.Background(), row.run, store.RunDue{Due: true, CapPassed: tc.past})
			if step != tc.want || note != tc.note || (err != nil) != tc.wantErr {
				t.Errorf("step %v, note %q, err %v; want %v, %q, error %v", step, note, err, tc.want, tc.note, tc.wantErr)
			}
		})
	}
}

// A killed run on a runner that is away, whose record holds a resume, is given up once its keep has run out, its
// record read or not: one statement, its given_up row, no runner call and no tail, the sandbox left for the runner's
// return; then nothing keeps the sandbox for its folder. Inside the keep the row takes no lock and writes nothing.
// Catches: an away row given up only for an owed pass, which keeps a killed run's sandbox, and its runner's
// retirement, for good.
func TestTailOwner_AnAwayRunnersKilledRunHoldingAResumeIsGivenUpPastTheKeep(t *testing.T) {
	for name, unreadable := range map[string]bool{"a record that reads": false, "a record that does not decode": true} {
		t.Run(name, func(t *testing.T) {
			g := newTailRig(t)
			g.s.cfg.Store = tailLabResumes{g.lab}
			g.lab.giveUpOK = true
			id := g.lab.runner.ID
			past := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
			inside := g.owedRow(g.clock.now.Add(-time.Hour))
			for _, r := range []*tailRow{past, inside} {
				r.run.State, r.run.RunnerID = types.RunKilled, &id
			}
			g.online.Store(false)
			if unreadable {
				g.lab.statusErr = errors.New("the record does not decode")
			}
			g.tick()
			g.tick()
			if g.lab.giveUpCount() != 1 || transferAudit(g.audit) != 1 || g.total() != 0 || g.locks.n.Load() != 1 {
				t.Fatalf("%d give-ups, %d given_up rows, %d tails, %d lock attempts over two ticks; want 1, 1, 0, 1",
					g.lab.giveUpCount(), transferAudit(g.audit), g.total(), g.locks.n.Load())
			}
			g.lab.mu.Lock()
			st, in, ref := g.lab.rows[past.run.ID].status, g.lab.rows[inside.run.ID].status, g.lab.rows[past.run.ID].run.SandboxRef
			g.lab.statusErr = nil
			g.lab.mu.Unlock()
			if st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil || ref == "" {
				t.Errorf("record %+v, ref %q; want given up and the sandbox still there", st, ref)
			}
			if in.Resume == nil || in.Back == nil || in.Back.GivenUpAt != nil {
				t.Errorf("the row inside the keep: record %+v; want it as it was", in)
			}
			if kept, err := g.s.keepsSandboxForFolder(context.Background(), past.run); kept || err != nil {
				t.Errorf("keep = %v, %v after the give-up; want false", kept, err)
			}
		})
	}
}
