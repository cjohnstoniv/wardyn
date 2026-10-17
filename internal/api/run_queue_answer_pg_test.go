// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A launch that waits answers what a launch that starts at once answers: the 201 carries the resources
// the run asked for and got, and a stored AWS SSO credential under CC3 carries the same advisory in the
// answer and in the run.create row.
func TestPG_QueuedLaunchAnswersWhatAnImmediateLaunchAnswers(t *testing.T) {
	e := newQueueEnv(t)
	e.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	e.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	e.srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	storeSSOBlobFor(t, e.srv, adminTokenPrincipal, putAWSSSOBlob(t, e.srv, awsSSOTestFixedNow.Add(time.Hour)))
	if _, err := e.srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{
		Runners: &types.RunnerSettings{Enabled: true}, ModelProviders: providerBlock(awsSSOTestProvider()),
	}); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Shared", 1)
	occupant := e.occupy(pool)
	defer e.free(occupant)

	_, w := e.launch(nil, e.reread(pool), `,"confinement_class":"CC1"`)
	if w.Code != http.StatusCreated {
		t.Fatalf("launch into a full pool = %d %s; want 201", w.Code, w.Body)
	}
	var out struct {
		types.AgentRun
		Warnings         []string        `json:"warnings"`
		ResourcesApplied json.RawMessage `json:"resources_applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.State != types.RunQueued {
		t.Fatalf("answer = %s (%v); want a QUEUED run", w.Body, err)
	}
	if len(out.ResourcesApplied) == 0 || string(out.ResourcesApplied) == "null" {
		t.Errorf("the queued 201 has no resources_applied: %s", w.Body)
	}
	if !warningsNameConfinementAdvisory(out.Warnings) {
		t.Errorf("the queued 201 warnings = %v; want the credential-confinement advisory", out.Warnings)
	}
	rows := e.audits(out.ID, "run.create")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"credential_confinement":"below_floor"`) {
		t.Errorf("run.create rows = %+v; want one with credential_confinement below_floor", rows)
	}
}
