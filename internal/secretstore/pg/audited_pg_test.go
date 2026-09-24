// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The pg store under the secretstore.Audited decorator wardynd wraps it in:
// the seam contract still holds, and each read records the row it actually
// opened — the operator's on a fallback, a refused one as a failure.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type auditLog struct{ got []types.AuditEvent }

func (l *auditLog) Record(_ context.Context, ev types.AuditEvent) error {
	l.got = append(l.got, ev)
	return nil
}

func TestPG_AuditedConformance(t *testing.T) {
	secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store {
		s, _, _ := newPGStore(t)
		return secretstore.Audited(s, &auditLog{})
	})
}

func TestPG_AuditedRecordsTheRowEachReadOpened(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	log := &auditLog{}
	st := secretstore.Audited(s, log)
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	name := uniqueName("git-pat")
	t.Cleanup(func() {
		_ = s.Delete(ctx, name)
		_ = s.For(alice).Delete(ctx, name)
		_ = s.For(bob).Delete(ctx, name)
	})
	for _, owner := range []string{"", alice, bob} {
		if err := st.For(owner).Put(ctx, name, []byte("value-of-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	data := func(i int) map[string]string {
		var d map[string]string
		if err := json.Unmarshal(log.got[i].Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	rctx := secretstore.WithPurpose(ctx, secretstore.PurposeBrokerMint)
	carol := "carol-" + uuid.NewString()
	if _, err := st.For(carol).Get(rctx, name); err != nil {
		t.Fatal(err)
	}
	if len(log.got) != 1 || log.got[0].Outcome != "success" {
		t.Fatalf("recorded %+v, want one success", log.got)
	}
	if d := data(0); d["owner"] != carol || d["row_owner"] != "" || d["ref"] != s.kek.ID() || d["store"] != "pg" || d["purpose"] != "broker-mint" {
		t.Errorf("fallback read recorded %v; want carol's read of the operator's row under %s", d, s.kek.ID())
	}

	swapEnvelopes(t, pool, alice, name, bob, name)
	got, err := st.For(alice).Get(rctx, name)
	assertRefused(t, "swapped row through the decorator", got, err, alice, name, "value-of-"+bob)
	if len(log.got) != 2 || log.got[1].Outcome != "failure" {
		t.Fatalf("recorded %+v, want the refusal as a second, failed read", log.got)
	}
	if d := data(1); d["row_owner"] != alice || d["ref"] == "" {
		t.Errorf("refused read recorded %v; want alice's row and its ref", d)
	}
}
