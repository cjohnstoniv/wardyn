// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errRunnerEventRefused refuses a posture or resident_erase event that is
// malformed or names something this runner does not hold.
var errRunnerEventRefused = errors.New("runner event refused")

// maxPostureField bounds the posture's free-text fields.
const maxPostureField = 64

// decodeRunnerEventData strictly decodes one bounded event body into T.
func decodeRunnerEventData[T any](data []byte) (T, error) {
	var v T
	if len(data) > maxActionResultBytes {
		return v, errRunnerEventRefused
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, errRunnerEventRefused
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return v, errRunnerEventRefused
	}
	return v, nil
}

// PostureFromRunner stores a claimed runner's self-reported posture and, only
// when it changed, writes runner.posture.record (design §7.4).
func (s *Server) PostureFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) error {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return errRunnerEventRefused
	}
	p, err := decodeRunnerEventData[types.RunnerPosture](ev.Data)
	if err != nil || ev.RunID != nil || p.OS == "" || len(p.OS) > maxPostureField || len(p.OSVersion) > maxPostureField ||
		!slices.Contains([]string{types.PostureEvidenceMDMFile, types.PostureEvidenceOSProbe}, p.Evidence) {
		return errRunnerEventRefused
	}
	changed, err := st.RecordRunnerPosture(ctx, runnerID, p, time.Now())
	if err != nil || !changed {
		return err
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+runnerID.String(),
		placement.ActionRunnerPostureRecord, runnerID.String(), "success",
		mustJSON(map[string]any{"posture": p, "posture_source": types.RunnerPostureSourceRunnerAsserted})))
	return nil
}

// ResidentEraseFromRunner writes runner.resident.erase for one runner_resident
// grant of a run bound to this runner, as the runner reported its erase.
func (s *Server) ResidentEraseFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) error {
	e, err := decodeRunnerEventData[runnerwire.ResidentErase](ev.Data)
	if err != nil || ev.RunID == nil || (e.Outcome != runnerwire.ResidentErased && e.Outcome != runnerwire.ResidentEraseFailed) {
		return errRunnerEventRefused
	}
	run, err := s.cfg.Store.GetRun(ctx, *ev.RunID)
	if err != nil {
		return err
	}
	if run.RunnerID == nil || *run.RunnerID != runnerID {
		return errRunnerEventRefused
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(grants, func(g types.CredentialGrant) bool {
		return g.ID == e.GrantID && g.Delivery == types.GrantDeliveryResident
	}) {
		return errRunnerEventRefused
	}
	outcome := "success"
	if e.Outcome != runnerwire.ResidentErased {
		outcome = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "runner:"+runnerID.String(),
		placement.ActionRunnerResidentErase, e.GrantID.String(), outcome,
		mustJSON(map[string]any{"grant_id": e.GrantID, "outcome": e.Outcome, "runner_id": runnerID})))
	return nil
}
