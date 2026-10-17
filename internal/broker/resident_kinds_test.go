// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two resident kinds are delivered at dispatch and never minted: a caller
// POSTing their grant id at the mint route gets a refusal naming the kind,
// never a token, and the stored secret is never read on the way.
func TestMintForGrant_ResidentKindsAreNeverMinted(t *testing.T) {
	for _, tc := range []struct {
		kind  types.GrantKind
		scope string
		says  string
	}{
		{types.GrantEnvSecret, `{"name":"CORP_TOKEN","secret_name":"corp-token"}`, "env var"},
		{types.GrantFileSecret, `{"file":"corp-token","secret_name":"corp-token"}`, "sandbox file"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			b, db, _, _ := newTestBroker(t)
			runID := uuid.New()
			gid := seedGrant(db, runID, types.GrantSpec{Kind: tc.kind, Scope: json.RawMessage(tc.scope)})
			m, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
			if !errors.Is(err, ErrUnknownGrantKind) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("mint %s = %+v, %v; want ErrUnknownGrantKind naming the %s it is delivered as", tc.kind, m, err, tc.says)
			}
			if m.Token != "" || m.KnownHosts != "" {
				t.Errorf("a refused mint returned material: %+v", m)
			}
		})
	}
}
