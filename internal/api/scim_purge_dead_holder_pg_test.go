// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
)

// A previous holder that is gone does not hold the address: a revoked or expired token, a deactivated person
// row, or a principal whose identity is deactivated or purged. The purge then deletes the email-keyed rows
// the next holder would otherwise inherit, and counts none as kept.
func TestSCIMPurgeIgnoresADeadPreviousHolder(t *testing.T) {
	cases := map[string]func(e *scimEnv){
		"a revoked token": func(e *scimEnv) {
			id, _ := e.seedToken("sub-old", purgeEmail)
			e.deadExec(`UPDATE api_tokens SET revoked_at = now() WHERE id = $1`, id)
		},
		"an expired token": func(e *scimEnv) {
			id, _ := e.seedToken("sub-old", purgeEmail)
			e.deadExec(`UPDATE api_tokens SET expires_at = now() - interval '1 hour' WHERE id = $1`, id)
		},
		"a token whose principal's identity is deactivated": func(e *scimEnv) {
			e.seedSignIn("sub-old", purgeEmail)
			e.seedToken("sub-old", purgeEmail)
			e.deadExec(`UPDATE principal_identities SET deactivated_at = now() WHERE principal = 'sub-old'`)
		},
		"a token whose principal's identity is purged": func(e *scimEnv) {
			e.seedSignIn("sub-old", purgeEmail)
			e.seedToken("sub-old", purgeEmail)
			e.deadExec(`UPDATE principal_identities SET deactivated_at = now(), purged_at = now() WHERE principal = 'sub-old'`)
		},
		"a deactivated person row": func(e *scimEnv) {
			e.deadExec(`INSERT INTO people (principal, email, created_by, deactivated_at) VALUES ('sub-old', $1, 'admin', now())`, purgeEmail)
		},
		"a person row whose identity is purged": func(e *scimEnv) {
			e.seedSignIn("sub-old", purgeEmail)
			e.deadExec(`INSERT INTO people (principal, email, created_by) VALUES ('sub-old', $1, 'admin')`, purgeEmail)
			e.deadExec(`UPDATE principal_identities SET deactivated_at = now(), purged_at = now() WHERE principal = 'sub-old'`)
		},
	}
	for name, dead := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSCIMEnv(t)
			f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
			dead(e)

			if w := e.del(e.b, f.id); w.Code != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
			}
			if g, a := e.userRows(purgeEmail); g != 0 || a != 0 {
				t.Errorf("email-keyed rows left after the purge: %d grants, %d assignments; a dead holder must not keep them", g, a)
			}
			rows := e.purgeRows(e.a, e.b)
			if len(rows) != 1 {
				t.Fatalf("%d purge rows, want 1", len(rows))
			}
			if d := dataOf(t, rows[0]); d["email_rows_kept"] != float64(0) {
				t.Errorf("purge row = %v, want no email rows kept", d)
			}
		})
	}
}

// deadExec runs one statement that seeds or kills a previous holder.
func (e *scimEnv) deadExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}
