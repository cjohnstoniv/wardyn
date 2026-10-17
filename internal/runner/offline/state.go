// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type localRun struct {
	Retirement      *types.RunnerRetirement      `json:"retirement,omitempty"`
	Retired         bool                         `json:"retired,omitempty"`
	Containment     *types.RunnerMutationFailure `json:"containment,omitempty"`
	LeaseAfter      time.Time                    `json:"lease_after,omitempty"`
	Quarantine      *types.RunnerQuarantine      `json:"quarantine,omitempty"`
	QuarantinedAt   *time.Time                   `json:"quarantined_at,omitempty"`
	EvidenceErased  bool                         `json:"evidence_erased,omitempty"`
	ContainmentOnly bool                         `json:"containment_only,omitempty"`
	Generation      uuid.UUID                    `json:"generation,omitempty"`
	Executions      map[uuid.UUID]string         `json:"executions,omitempty"`
	MutationFailure *types.RunnerMutationFailure `json:"mutation_failure,omitempty"`
	FailedCreation  bool                         `json:"failed_creation,omitempty"`
	ExecID          string                       `json:"exec_id,omitempty"`
	RunID           uuid.UUID                    `json:"run_id"`
	Ref             string                       `json:"ref"`
	Lease           runnerwire.Lease             `json:"lease"`
	ProxyStoppedAt  *time.Time                   `json:"proxy_stopped_at,omitempty"`
	Exit            *runnerwire.AgentExit        `json:"exit,omitempty"`
	ExitAcked       bool                         `json:"exit_acked,omitempty"`
	// OutputCapture records that the create handed the sandbox an output
	// writer; a retirement that says the run had no output is refused when set.
	OutputCapture bool `json:"output_capture,omitempty"`
}
type localState struct {
	RunnerID uuid.UUID                   `json:"runner_id"`
	Revoked  bool                        `json:"revoked"`
	Runs     map[uuid.UUID]localRun      `json:"runs"`
	Actions  map[uuid.UUID]appliedAction `json:"actions"`
}
type appliedAction struct {
	Action runnerwire.PendingAction `json:"action"`
	Result runnerwire.ActionResult  `json:"result"`
}

const stateLimit = 64 << 20

func (m *Manager) load() error {
	f, err := m.root.Open("state.json")
	if errors.Is(err, os.ErrNotExist) {
		m.state = localState{RunnerID: m.runnerID, Runs: map[uuid.UUID]localRun{}, Actions: map[uuid.UUID]appliedAction{}}
		return m.save()
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	link, err := m.root.Lstat("state.json")
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) || st.Size() > stateLimit {
		return errors.New("unsafe runner lifecycle state")
	}
	dec := json.NewDecoder(io.LimitReader(f, stateLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m.state); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing runner lifecycle state")
	}
	if m.state.RunnerID != m.runnerID || m.state.Runs == nil || m.state.Actions == nil {
		return errors.New("runner lifecycle identity mismatch")
	}
	for id, r := range m.state.Runs {
		if id == uuid.Nil || id != r.RunID || r.Lease.RunID != id || r.Ref != m.expectedRef(id) {
			return errors.New("runner lifecycle run mismatch")
		}
		if err := validateRetirementState(r); err != nil {
			return err
		}
		if r.Retirement == nil && r.Generation != uuid.Nil && r.Executions[r.Generation] != "" && r.ExecID == "" {
			// A previous daemon may have executed before losing its durable result.
			// Fresh leases cannot authorize more work or discharge this uncertainty.
			if r.MutationFailure == nil {
				r.MutationFailure = &types.RunnerMutationFailure{ID: uuid.New(), Generation: r.Generation, Operation: r.Executions[r.Generation]}
				m.state.Runs[id] = r
				m.dirty = true
			}
		}
	}
	return nil
}

// ensureDurable is called under mu before publishing lifecycle facts or
// admitting governed work. Failed writes retain safety intent in memory.
func (m *Manager) ensureDurable() error {
	if !m.dirty {
		return nil
	}
	return m.save()
}

func (m *Manager) save() (err error) {
	m.dirty = true
	defer func() {
		if err == nil {
			m.dirty = false
		}
	}()

	b, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	if len(b) > stateLimit {
		return errors.New("runner lifecycle state capacity reached")
	}
	if err := m.root.Remove("state.tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := m.root.OpenFile("state.tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := io.Copy(f, bytes.NewReader(b))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := m.root.Rename("state.tmp", "state.json"); err != nil {
		return err
	}
	dir, err := m.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
