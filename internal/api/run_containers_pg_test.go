// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Postgres-backed: a run's extra containers go from POST /runs into run_containers, onto the run.create row
// without their command, and into the sandbox spec dispatch hands the runner.

const extrasRunBody = `{"agent":"claude-code","repo":"acme/widgets","task":"t","resources":{"cpu_millis":2000,"memory_mib":4096},` +
	`"containers":[{"name":"kernel","image":"` + kernelImage + `","command":["serve","--token=x"],"ports":[8888],` +
	`"resources":{"cpu_millis":100,"memory_mib":128}}]}`

// containersReadFails is the Postgres store with a run_containers read that fails.
type containersReadFails struct{ store.PG }

func (containersReadFails) RunContainers(context.Context, uuid.UUID) ([]types.RunExtraContainer, error) {
	return nil, errors.New("db down")
}

func createExtrasRun(t *testing.T, srv *Server) createRunResponse {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, extrasRunBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: %d %s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

// TestDispatchCarriesContainers: the stored extras reach SandboxSpec.Containers; a read that fails fails the run.
func TestDispatchCarriesContainers(t *testing.T) {
	fr := &fakeRunner{}
	srv, pool := pgHarnessWithRunner(t, extrasRunner{fr})
	created := createExtrasRun(t, srv)
	if len(created.Containers) != 1 || created.Containers[0].Name != "kernel" ||
		!reflect.DeepEqual(created.Containers[0].Command, []string{"serve", "--token=x"}) {
		t.Fatalf("the 201 carries %+v, want what the request sent", created.Containers)
	}
	stored, err := store.NewPG(pool).RunContainers(context.Background(), created.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored extras %+v, %v", stored, err)
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	got := fr.lastSpec.Containers
	fr.mu.Unlock()
	want := []runner.ExtraContainer{{Name: "kernel", Image: stored[0].Image, Command: stored[0].Command, Ports: stored[0].Ports,
		CPUMillis: int64(stored[0].CPUMillis), MemoryMiB: int64(stored[0].MemoryMiB)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SandboxSpec.Containers = %+v, want the stored %+v", got, want)
	}

	t.Run("a read that fails", func(t *testing.T) {
		fr := &fakeRunner{}
		srv, pool := pgHarnessWithRunner(t, extrasRunner{fr})
		srv.cfg.Store = containersReadFails{store.NewPG(pool)}
		created := createExtrasRun(t, srv)
		const hint = "the run's extra containers could not be read"
		waitFor(t, "the run failed with its hint", func() bool {
			run, err := srv.cfg.Store.GetRun(context.Background(), created.ID)
			return err == nil && run.State == types.RunFailed && run.FailureHint == hint
		})
		fr.mu.Lock()
		calls := fr.createCalls
		fr.mu.Unlock()
		if calls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0: a run whose extras cannot be read never starts", calls)
		}
	})
}

// TestRunCreateAuditRowHasNoContainerCommand: the run.create row names each extra, its image, sizes, ports and
// verdict, and never its command.
func TestRunCreateAuditRowHasNoContainerCommand(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, extrasRunner{fr})
	created := createExtrasRun(t, srv)
	var row *types.AuditEvent
	waitFor(t, "the run.create row", func() bool {
		for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
			if ev.Action == "run.create" && ev.Outcome == "success" && ev.RunID != nil && *ev.RunID == created.ID {
				row = &ev
				return true
			}
		}
		return false
	})
	if strings.Contains(string(row.Data), "--token=x") {
		t.Fatalf("the run.create row carries the command: %s", row.Data)
	}
	var data struct {
		Containers []map[string]any `json:"containers"`
	}
	if err := json.Unmarshal(row.Data, &data); err != nil || len(data.Containers) != 1 {
		t.Fatalf("row data %s (%v), want one container", row.Data, err)
	}
	c := data.Containers[0]
	for _, k := range []string{"name", "image", "cpu_millis", "memory_mib", "ports", "image_admission"} {
		if _, ok := c[k]; !ok {
			t.Errorf("the row's container has no %q: %v", k, c)
		}
	}
	if _, ok := c["command"]; ok {
		t.Errorf("the row's container has a command: %v", c)
	}
}
