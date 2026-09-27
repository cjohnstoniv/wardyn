// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// Store mode under the secretstore.Audited decorator wardynd wraps every store
// in (#647): a read from Key Vault is one secret.read naming the store and the
// pointer it followed, an unmarked read never reaches the vault, and a read
// whose call site records its own event is not counted twice.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type auditLog struct{ got []types.AuditEvent }

func (l *auditLog) Record(_ context.Context, ev types.AuditEvent) error {
	l.got = append(l.got, ev)
	return nil
}

func TestStoreMode_AuditedRecordsEachReadOnce(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	log := &auditLog{}
	st := secretstore.Audited(storeMode(t, pool, newFakeStore(t, f), nil), log)
	ctx := t.Context()
	if err := st.For("alice").Put(ctx, "pat", []byte("alice-secret")); err != nil {
		t.Fatal(err)
	}
	ref := kekID(t, pool, "alice", "pat")
	data := func(i int) map[string]string {
		var d map[string]string
		if err := json.Unmarshal(log.got[i].Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	v, err := st.For("alice").Get(secretstore.WithPurpose(ctx, secretstore.PurposeBrokerMint), "pat")
	if err != nil || string(v) != "alice-secret" {
		t.Fatalf("marked Get = (%q, %v)", v, err)
	}
	if len(log.got) != 1 || log.got[0].Action != "secret.read" || log.got[0].Outcome != "success" {
		t.Fatalf("recorded %+v, want one secret.read success", log.got)
	}
	if d := data(0); d["store"] != Name || d["ref"] != ref || d["owner"] != "alice" || d["row_owner"] != "alice" || d["purpose"] != "broker-mint" {
		t.Errorf("read recorded %v; want store azurekv, ref %s, alice's row, purpose broker-mint", d, ref)
	}
	if strings.Contains(string(log.got[0].Data), "alice-secret") {
		t.Fatal("the secret.read carries the value")
	}

	// No purpose: refused before the store is read, and recorded as such.
	gets := f.count("GET")
	if _, err := st.For("alice").Get(ctx, "pat"); err == nil {
		t.Fatal("an unmarked Get read the value")
	}
	if f.count("GET") != gets {
		t.Fatal("an unmarked Get reached Key Vault")
	}
	if len(log.got) != 2 || log.got[1].Outcome != "failure" || data(1)["purpose"] != string(secretstore.PurposeUnmarked) {
		t.Fatalf("unmarked Get recorded %+v, want a second, failed read with purpose unmarked", log.got)
	}

	// A site that records its own event: the decorator stays silent and hands
	// it the row, so the site can name the store and the pointer.
	sctx, row := secretstore.SiteAudited(ctx)
	if v, err := st.For("alice").Get(sctx, "pat"); err != nil || string(v) != "alice-secret" {
		t.Fatalf("site-audited Get = (%q, %v)", v, err)
	}
	if len(log.got) != 2 {
		t.Fatalf("a site-audited Get was recorded by the decorator too: %+v", log.got[2:])
	}
	if row.Store != Name || row.Ref != ref || row.Owner != "alice" {
		t.Errorf("site-audited row = %+v; want store azurekv, ref %s, alice's row", row, ref)
	}

	// The value gone behind the row: a refusal, recorded against the row.
	f.mu.Lock()
	f.secrets = map[string]*kvSecret{}
	f.mu.Unlock()
	if _, err := st.For("alice").Get(secretstore.WithPurpose(ctx, secretstore.PurposeDispatch), "pat"); err == nil {
		t.Fatal("Get with the value gone succeeded")
	}
	if len(log.got) != 3 || log.got[2].Outcome != "failure" || data(2)["ref"] != ref || data(2)["row_owner"] != "alice" {
		t.Fatalf("refused read recorded %+v; want a third, failed read naming alice's row and its ref", log.got)
	}
}
