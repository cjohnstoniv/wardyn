// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sweepRetiredADOSharedCredentials deletes the stored shared Azure DevOps
// credentials (api.RetiredADOSharedSecretNames: git-pat-<host>, ssh-key-<host>
// and known-hosts-<host> for every Azure DevOps host) from every namespace —
// the operator's and each person's, since a person could store their own copy
// and the git broker reads the owner's row first (#1429). It runs on every
// boot, audits once per namespace that held any, and is idempotent: nothing
// stores those names for Azure DevOps any more, so after the first boot there
// is nothing left to find and no audit row.
//
// The deletion is irreversible: a typed secret is write-only and cannot be
// exported first. A boot sweep rather than a migration because SQL cannot
// delete a store-mode secret's value. A store or site config that cannot answer
// refuses boot rather than leave a retired credential in place.
func sweepRetiredADOSharedCredentials(ctx context.Context, st secretstore.Store, sc types.SiteConfig, rec audit.Recorder) error {
	names := api.RetiredADOSharedSecretNames(sc)
	holders, err := st.Holders(ctx, names)
	if err != nil {
		return fmt.Errorf("refusing to start: list retired Azure DevOps shared credentials: %w", err)
	}
	if len(holders) == 0 {
		return nil
	}
	byNamespace := map[string][]string{}
	for name, owners := range holders {
		for _, owner := range owners {
			byNamespace[owner] = append(byNamespace[owner], name)
		}
	}
	if _, err := st.DeleteEverywhere(ctx, names); err != nil {
		return fmt.Errorf("refusing to start: delete retired Azure DevOps shared credentials: %w", err)
	}
	for _, owner := range slices.Sorted(maps.Keys(byNamespace)) {
		namespace := owner
		if owner == "" {
			namespace = "operator"
		}
		slices.Sort(byNamespace[owner])
		data, _ := json.Marshal(map[string]any{
			"namespace": namespace,
			"count":     len(byNamespace[owner]),
			"names":     byNamespace[owner],
		})
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
			Action: "ado_shared_credential.retire", Target: st.Name(), Outcome: "success", Data: data,
		}
		if err := rec.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return nil
}
