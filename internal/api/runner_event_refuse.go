// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// errRunnerEventShape marks a handler's refusal of an event's own shape, before any store call; each kind's
// shape error also carries the sentinel that kind refused with before.
var (
	errRunnerEventShape  = errors.New("runner event shape refused")
	errActionResultShape = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerActionRefused)
	errAgentExitShape    = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerExecutionRefused)
	errSpoolShape        = fmt.Errorf("%w: %w", errRunnerEventShape, store.ErrRunnerSpoolRefused)
	errReportShape       = fmt.Errorf("%w: %w", errRunnerEventShape, errRunnerEventRefused)
)

// runnerEventRefuser is the store side of a refusal (store.PG).
type runnerEventRefuser interface {
	RefuseRunnerEvent(ctx context.Context, runnerID uuid.UUID, r store.RunnerEventRefusal, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error)
}

// runnerEventRefusalCodes are the permanent refusals, in the order they are matched; every other error is transient.
var runnerEventRefusalCodes = []struct {
	sentinel error
	code     string
}{
	{errRunnerEventShape, runnerwire.RefusalShape},
	{errRunnerEventUnsupported, runnerwire.RefusalUnsupportedKind},
	{errRunnerUnknownRun, runnerwire.RefusalUnknownRun},
	{store.ErrRunnerExecutionRefused, runnerwire.RefusalStoreRefused},
	{store.ErrRunnerActionRefused, runnerwire.RefusalStoreRefused},
	{store.ErrRunnerSpoolRefused, runnerwire.RefusalStoreRefused},
	{errRunnerEventRefused, runnerwire.RefusalStoreRefused},
}

// classifyRunnerEvent turns err, an event handler's error, into the peer's verdict. A permanent refusal (a sentinel
// that names the event itself, never a bare not-found or a database error) is recorded first, a runner.event.refuse
// row within the store's per-runner bound and an action_result's open action settled as refused, and only once that commits is it an
// EventRefusal, which the peer acknowledges. Anything else is returned unchanged, so the link fails and the event is
// replayed; so is a refusal whose record failed.
func (s *Server) classifyRunnerEvent(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event, err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	code := ""
	for _, c := range runnerEventRefusalCodes {
		if errors.Is(err, c.sentinel) {
			sentinel, code = c.sentinel, c.code
			break
		}
	}
	if sentinel == nil {
		return err
	}
	st, ok := s.cfg.Store.(runnerEventRefuser)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return fmt.Errorf("%w: %w", errRunnerIngestUnavailable, err)
	}
	sum := sha256.Sum256(ev.Data)
	r := store.RunnerEventRefusal{Kind: ev.Kind, Code: code, Sentinel: sentinel.Error(), RunID: ev.RunID,
		PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: len(ev.Data)}
	if ev.Kind == runnerwire.EventActionResult {
		// Lenient: a result refused for its shape still names the action it answers, if it names one at all.
		var named struct {
			ID uuid.UUID `json:"id"`
		}
		if json.Unmarshal(ev.Data, &named) == nil && named.ID != uuid.Nil {
			r.ActionID = &named.ID
		}
	}
	if ev.Kind == runnerwire.EventSpool && ev.RunID != nil {
		// Lenient too: a record refused for its shape or its run still says where it sits in its run's stream, and
		// the store moves the run's cursor past it only when it is next there.
		var named struct {
			SpoolID uuid.UUID `json:"spool_id"`
			Seq     int64     `json:"seq"`
			Hash    string    `json:"hash"`
		}
		if json.Unmarshal(ev.Data, &named) == nil && named.SpoolID != uuid.Nil {
			r.Spool = &store.RunnerEventRefusalSpool{RunID: *ev.RunID, SpoolID: named.SpoolID, Seq: named.Seq, Hash: named.Hash}
		}
	}
	settled, werr := st.RefuseRunnerEvent(ctx, runnerID, r, s.cfg.RunnerAuditPrepare)
	if werr != nil {
		slog.WarnContext(ctx, "runner event refusal not recorded; the event stays unacknowledged", "runner_id", runnerID,
			"kind", ev.Kind, "code", code, "error", werr)
		return err
	}
	slog.WarnContext(ctx, "runner event refused for good and acknowledged", "runner_id", runnerID, "kind", ev.Kind,
		"run_id", ev.RunID, "code", code, "settled", settled)
	return &runnerwire.EventRefusal{Code: code, Err: err}
}
