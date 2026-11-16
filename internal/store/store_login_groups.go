// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// DeleteLoginGroups deletes the IdP group snapshots of principals: key-domain membership's
// (key_domain_login_groups) and the record of their last sign-in (principal_groups). A person erased or
// purged leaves no group memberships behind; their key-domain assignments are audited governance facts
// and stay. It reports how many key-domain snapshots went.
func (s PG) DeleteLoginGroups(ctx context.Context, principals []string) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `WITH signed_in AS (DELETE FROM principal_groups WHERE principal = ANY($1::text[]))
		DELETE FROM key_domain_login_groups WHERE principal = ANY($1::text[])`, principals)
	if err != nil {
		return 0, fmt.Errorf("store: delete login group snapshots: %w", err)
	}
	return tag.RowsAffected(), nil
}
