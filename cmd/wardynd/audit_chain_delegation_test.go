// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type auditSink struct{ got []types.AuditEvent }

func (a *auditSink) Record(_ context.Context, ev types.AuditEvent) error {
	a.got = append(a.got, ev)
	return nil
}

// TestAuditChainStampsAndCoalescesDryRunDenials: below the delegation stamp the
// chain carries the dry-run stamp, and only the serve chain adds the coalescer,
// above masking so a summary row is masked like any other (a secret in its
// reason never reaches the recorder below).
func TestAuditChainStampsAndCoalescesDryRunDenials(t *testing.T) {
	plain, _, _, _, err := buildAuditChain(context.Background(), "", "", "", nil, secretmask.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	stamp, ok := plain.(audit.DelegationRecorder).Inner.(audit.DryRunRecorder)
	if !ok {
		t.Fatalf("below the delegation stamp = %T, want audit.DryRunRecorder", plain.(audit.DelegationRecorder).Inner)
	}
	if _, ok := stamp.Inner.(maskingRecorder); !ok {
		t.Errorf("a chain built without a coalescer has %T below the stamp, want maskingRecorder", stamp.Inner)
	}

	reg := secretmask.NewRegistry()
	reg.AddGlobal("test", "dry-run-reason", time.Now(), []byte("s3cr3t-value"))
	denials := &audit.DenialCoalescer{}
	head, _, _, _, err := buildAuditChain(context.Background(), "", "", "", nil, reg, serveChain{denials: denials})
	if err != nil {
		t.Fatal(err)
	}
	if got := head.(audit.DelegationRecorder).Inner.(audit.DryRunRecorder).Inner; got != audit.Recorder(denials) {
		t.Fatalf("below the stamp = %T, want the coalescer", got)
	}
	masking, ok := denials.Inner.(maskingRecorder)
	if !ok {
		t.Fatalf("below the coalescer = %T, want maskingRecorder", denials.Inner)
	}
	// The masking recorder's inner chain needs a database; swap in a sink.
	below := &auditSink{}
	masking.inner = below
	denials.Inner = masking

	ctx := audit.WithDryRun(context.Background())
	for range 2 {
		data, _ := json.Marshal(map[string]string{"reason": "s3cr3t-value"})
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "sub-a",
			Action: audit.DenialAction, Target: "runs.image", Outcome: audit.DenialOutcome, Data: data}
		if err := head.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	denials.Flush(context.Background())
	if len(below.got) != 2 || below.got[1].Action != audit.CoalesceAction {
		t.Fatalf("recorded %d rows, want the first denial and a summary", len(below.got))
	}
	for _, ev := range below.got {
		if strings.Contains(string(ev.Data), "s3cr3t-value") {
			t.Errorf("%s row reached the recorder below masking unmasked: %s", ev.Action, ev.Data)
		}
	}
}

// TestAuditChainStampsDelegation: the chain every writer shares is headed by
// audit.DelegationRecorder, so a row the identity provider or the broker
// records under a portal's delegated request names the portal too (#1142),
// not only the rows the API writes itself.
func TestAuditChainStampsDelegation(t *testing.T) {
	rec, _, _, _, err := buildAuditChain(context.Background(), "", "", "", nil, secretmask.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.(audit.DelegationRecorder); !ok {
		t.Fatalf("audit chain head = %T, want audit.DelegationRecorder", rec)
	}
}
