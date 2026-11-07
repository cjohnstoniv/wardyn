// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunMachineWireNames(t *testing.T) {
	b, err := json.Marshal(client.CreateRunRequest{Agent: "a", Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["machine"]; ok {
		t.Fatalf("a request with no machine sends one: %s", b)
	}
	b, err = json.Marshal(client.RunMachine{Keep: true, Name: "n", Resume: "r", Fresh: true})
	if err != nil || string(b) != `{"keep":true,"name":"n","resume":"r","fresh":true}` {
		t.Fatalf("RunMachine JSON %s, %v", b, err)
	}
	b, err = json.Marshal(client.RunMachine{})
	if err != nil || string(b) != `{}` {
		t.Fatalf("zero RunMachine JSON %s, %v", b, err)
	}
}

func TestKeptMachineReadsAndAdminRoutes(t *testing.T) {
	run, holder, mid := uuid.New(), uuid.New(), uuid.New()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+run.String():
			_, _ = w.Write([]byte(`{"id":"` + run.String() + `","machine":{"id":"` + mid.String() + `","name":"dev","mode":"resumed","keep_at_end":true,"state":"in_use","scrubbed":false,"runner_name":"studio","holder_run_id":"` + holder.String() + `","base":"macos-26","disk_bytes":42,"expires_at":"` + time.Now().UTC().Add(72*time.Hour).Format(time.RFC3339) + `","deleted":false}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+holder.String():
			_, _ = w.Write([]byte(`{"id":"` + holder.String() + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/machines/kept":
			_, _ = w.Write([]byte(`{"machines":[{"id":"` + mid.String() + `","name":"dev","owner_subject":"ada"}],"total":3}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/machines/kept/"+mid.String():
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"` + mid.String() + `","name":"dev","owner":"ada","runner_id":"` + uuid.Nil.String() + `","outcome":"deferred"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &client.Client{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client()}
	ctx := context.Background()

	m, err := c.GetRunMachine(ctx, run)
	if err != nil || m == nil || m.ID != mid || m.Mode != "resumed" || !m.KeepAtEnd || m.State != types.KeptMachineState("in_use") ||
		m.HolderRunID == nil || *m.HolderRunID != holder || m.DiskBytes != 42 || m.ExpiresAt == nil || m.Deleted {
		t.Fatalf("GetRunMachine = %+v, %v", m, err)
	}
	if m, err := c.GetRunMachine(ctx, holder); err != nil || m != nil {
		t.Fatalf("a run with no machine: %+v, %v", m, err)
	}
	list, err := c.ListKeptMachines(ctx, client.ListOpts{Limit: 1, Offset: 2})
	if err != nil || list.Total != 3 || len(list.Machines) != 1 || list.Machines[0].OwnerSubject != "ada" {
		t.Fatalf("ListKeptMachines = %+v, %v", list, err)
	}
	del, err := c.DeleteKeptMachine(ctx, mid)
	if err != nil || del.ID != mid || del.Outcome != "deferred" {
		t.Fatalf("DeleteKeptMachine = %+v, %v", del, err)
	}
	if calls[2] != "GET /api/v1/machines/kept?limit=1&offset=2" {
		t.Fatalf("list call %q", calls[2])
	}
}
