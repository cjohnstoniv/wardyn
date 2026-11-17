// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// revokeFailBroker is fakeBroker whose revocation fails: the injected failure.
type revokeFailBroker struct{ *fakeBroker }

func (revokeFailBroker) RevokeRun(context.Context, uuid.UUID) error {
	return errors.New("injected: the broker could not revoke")
}

// RevokeStoppedRun runs every cascade step even past a failed one, and hands the
// failure back keyed as the cascade's run.revoke row is, without writing that
// row: the idle reaper audits what it is handed, so a row here would be a second.
func TestRevokeStoppedRun_RunsEveryStepAndLeavesTheAuditToTheReaper(t *testing.T) {
	logs := captureSlog(t)
	srv, ps, run := dispatchWithProxyConfigStore(t, &killRaceRunner{fakeRunner: &fakeRunner{}})
	audit := &recRecorder{}
	srv.cfg.Audit = audit
	srv.cfg.Broker = revokeFailBroker{&fakeBroker{}}
	ps.sealed[run.ID] = []byte("sealed")

	got := srv.RevokeStoppedRun(context.Background(), run.ID)

	// The key holds the failed step; the broker's own error is in the server log.
	if got["broker_error"] != auditStepRevokeBroker || !strings.Contains(logs.String(), "injected") {
		t.Errorf("RevokeStoppedRun = %v, log %q; want the broker's step under broker_error and its failure logged", got, logs.String())
	}
	if ps.has(run.ID) {
		t.Error("the proxy config is still stored after a failed broker revoke; every step must run")
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.revoke", "failure"); ev != nil {
		t.Errorf("RevokeStoppedRun wrote a run.revoke row (%s); the reaper writes it", ev.Data)
	}
}
