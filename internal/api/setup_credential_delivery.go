// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// credentialDeliveryCheck grades the credential_delivery row: stored rows of the local credential delivery
// policy that name a mode the server cannot build for their class (an operator set them before PUT refused
// them). Each is named with what the server can build for it. ok is false when every stored row is buildable.
// It never sets Blocking and changes nothing: those rows already refuse at every door.
func credentialDeliveryCheck(rows []placement.UnbuildableRow) (SetupCheck, bool) {
	if len(rows) == 0 {
		return SetupCheck{}, false
	}
	named := make([]string, len(rows))
	for i, r := range rows {
		named[i] = r.String()
	}
	noun := "rows name"
	if len(rows) == 1 {
		noun = "row names"
	}
	return SetupCheck{ID: "credential_delivery", Label: "Runner credential delivery", Status: "warn",
		Detail: fmt.Sprintf("%d stored credential delivery %s a mode the server cannot build, and a run needing it is refused: %s.",
			len(rows), noun, strings.Join(named, "; ")),
		Fix: "In Governance, Runner credentials, set each to a mode the server can build, or to refuse. This check changes nothing."}, true
}

// credentialDeliveryRow reads the stored document and grades it. A store without the document, or one that
// cannot be read or decoded, leaves the row out: nothing is guessed, and GET /governance/credential-delivery
// shows the same list.
func (s *Server) credentialDeliveryRow(ctx context.Context) (SetupCheck, bool) {
	st, ok := s.cfg.Store.(interface {
		GetCredentialDeliveryPolicy(context.Context) (types.CredentialDeliveryPolicyDoc, error)
	})
	if !ok {
		return SetupCheck{}, false
	}
	doc, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: setup status: get credential delivery policy", slog.Any("err", err))
		return SetupCheck{}, false
	}
	p, err := deliveryPolicyOf(doc.Policy)
	if err != nil {
		return SetupCheck{}, false
	}
	return credentialDeliveryCheck(p.Unbuildable())
}

// runnerPolicyRows are the operator rows about what a runner is set to take: runner_limits and credential_delivery.
func (s *Server) runnerPolicyRows(ctx context.Context) []SetupCheck {
	var rows []SetupCheck
	for _, row := range []func(context.Context) (SetupCheck, bool){s.runnerLimitsRow, s.credentialDeliveryRow} {
		if chk, ok := row(ctx); ok {
			rows = append(rows, chk)
		}
	}
	return rows
}
