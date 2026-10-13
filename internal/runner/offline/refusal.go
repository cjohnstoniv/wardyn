// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxRefusalReason bounds the reason a refused result carries to the org.
const maxRefusalReason = 512

// actionRefusal marks a pending action this runner can never apply. It is
// answered with a terminal refused result, so the org closes it and the
// session comes up, instead of an error that fails every reconnect.
type actionRefusal struct{ err error }

func (r actionRefusal) Error() string { return r.err.Error() }
func (r actionRefusal) Unwrap() error { return r.err }

func refusal(err error) error { return actionRefusal{err} }

// refuseAction durably records a's refused result, so a lost ACK replays the
// same receipt. The caller holds m.mu.
func (m *Manager) refuseAction(ctx context.Context, a runnerwire.PendingAction, cause error) (runnerwire.ActionResult, error) {
	reason := cause.Error()
	if len(reason) > maxRefusalReason {
		reason = strings.ToValidUTF8(reason[:maxRefusalReason], "")
	}
	slog.WarnContext(ctx, "runner: refused a pending action", slog.String("action", a.ID.String()), slog.String("reason", reason))
	result := runnerwire.ActionResult{ID: a.ID, Outcome: types.RunnerActionRefused, Reason: reason, ObservedAt: m.now().UTC()}
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	return result, nil
}
