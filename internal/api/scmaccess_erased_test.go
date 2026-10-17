// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A sign-in Wardyn deleted — erased by an admin, or refused for good by the
// provider — leaves the person not connected, and the answer must not explain
// that with row_is_newer ("you signed in before your admin turned Azure DevOps
// on"): the person had signed in, and the row was not newer.
func TestSCMAccess_DeletedSignInDoesNotClaimRowIsNewer(t *testing.T) {
	for name, remove := range map[string]func(*Server) error{
		"erased": func(s *Server) error {
			_, err := secretstore.EraseOwner(context.Background(), s.cfg.Secrets, "alice")
			return err
		},
		"refused for good": func(s *Server) error {
			s.deleteDeadCredential(context.Background(), s.cfg.Secrets.For("alice"), "alice", adoEntraSecretName(scmTestRowID), "ado", false)
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			sc := adoTestSiteConfig(false)
			s := newSCMTestServer(t, sc, true)
			ctx := context.Background()
			if err := s.storeADOEntraBlob(ctx, "alice", scmTestRowID, adoEntraBlob{
				RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
				Subject: "alice", Source: adoEntraSourceLogin,
			}); err != nil {
				t.Fatalf("store blob: %v", err)
			}
			if rows := scmAccessRows(t, s, ctx, sc, "alice"); len(rows) != 1 || rows[0].State != modelAccessLive {
				t.Fatalf("before the delete: %+v, want live", rows)
			}
			if err := remove(s); err != nil {
				t.Fatal(err)
			}
			rows := scmAccessRows(t, s, ctx, sc, "alice")
			if len(rows) != 1 || rows[0].State != modelAccessNotConfigured || rows[0].Cause != "" {
				t.Fatalf("after the delete: %+v, want not_configured with no cause", rows)
			}
		})
	}
}
