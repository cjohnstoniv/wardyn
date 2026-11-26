// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type readSink struct{ got []types.AuditEvent }

func (r *readSink) Record(_ context.Context, ev types.AuditEvent) error {
	r.got = append(r.got, ev)
	return nil
}

// TestRunIdentity_SecretReadOwnerKept: the real secret.read writer, run through the run identity recorder with an
// identity on the context, keeps data.owner equal to the namespace read for and adds only the run keys.
func TestRunIdentity_SecretReadOwnerKept(t *testing.T) {
	sink := &readSink{}
	ctx := audit.WithRunIdentity(context.Background(), "alice@example.com", "", "spiffe://t/agent-run/1")
	RecordRead(ctx, audit.RunIdentityRecorder{Inner: sink}, PurposeDispatch, "team-a", Row{Store: "pg", Name: "x", Found: true, Owner: "team-a"}, nil)
	if len(sink.got) != 1 {
		t.Fatalf("recorded %d events, want 1", len(sink.got))
	}
	var m map[string]string
	if err := json.Unmarshal(sink.got[0].Data, &m); err != nil {
		t.Fatal(err)
	}
	if m["owner"] != "team-a" || m["run_owner"] != "alice@example.com" || m["run_workload"] != "spiffe://t/agent-run/1" {
		t.Errorf("data = %s, want owner team-a beside run_owner and run_workload", sink.got[0].Data)
	}
}
