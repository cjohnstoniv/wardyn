// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestNoteHeldChanges: `governance set --reason/--ticket` writes the note on each held change, sends
// nothing when neither flag is set, and says so when nothing was held.
func TestNoteHeldChanges(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = string(body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(sdk.GovernanceChange{})
	}))
	defer srv.Close()
	c := sdk.New(srv.URL, "tok")
	a, b := uuid.New(), uuid.New()
	held := []sdk.GovernanceChange{{ID: a}, {ID: b}}

	run := func(held []sdk.GovernanceChange, reason, ticket string) (string, error) {
		cmd := &cobra.Command{}
		stderr := &strings.Builder{}
		cmd.SetErr(stderr)
		cmd.SetContext(t.Context())
		err := noteHeldChanges(cmd, c, held, reason, ticket)
		return stderr.String(), err
	}

	if _, err := run(held, "", ""); err != nil || len(got) != 0 {
		t.Fatalf("no flags: err = %v, requests = %v, want none", err, got)
	}
	if msg, err := run(nil, "why", ""); err != nil || !strings.Contains(msg, "no change was held") || len(got) != 0 {
		t.Fatalf("nothing held: err = %v, stderr = %q, requests = %v", err, msg, got)
	}
	if _, err := run(held, "needed for launch", "INC-9"); err != nil {
		t.Fatalf("held changes: %v", err)
	}
	for _, id := range []uuid.UUID{a, b} {
		body, ok := got["PUT /api/v1/governance/changes/"+id.String()+"/note"]
		if !ok || !strings.Contains(body, `"reason":"needed for launch"`) || !strings.Contains(body, `"ticket":"INC-9"`) {
			t.Errorf("note on %s = %q (sent: %v), want the reason and ticket", id, body, ok)
		}
	}
}
