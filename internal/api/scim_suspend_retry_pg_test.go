// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// failDirectoryOnce fails the named directory read once, after skip calls of it succeed.
type failDirectoryOnce struct {
	store.PG
	fail string
	skip int
}

func (s *failDirectoryOnce) failed(name string) error {
	if s.fail != name {
		return nil
	}
	if s.skip > 0 {
		s.skip--
		return nil
	}
	s.fail = ""
	return errors.New("directory read failed")
}

func (s *failDirectoryOnce) IdentityAliasValues(ctx context.Context, id uuid.UUID) ([]string, error) {
	if err := s.failed("IdentityAliasValues"); err != nil {
		return nil, err
	}
	return s.PG.IdentityAliasValues(ctx, id)
}

func (s *failDirectoryOnce) HeldEmails(ctx context.Context, id uuid.UUID, bound, emails []string) ([]string, error) {
	if err := s.failed("HeldEmails"); err != nil {
		return nil, err
	}
	return s.PG.HeldEmails(ctx, id, bound, emails)
}

// A suspension whose email directory cannot be read deactivates the exact row and cuts its own binding,
// leaves step 1 pending with the reason, and a retry once the read works runs the whole suspension.
func TestSCIMSuspendWhileTheDirectoryIsUnreadableCutsTheExactRow(t *testing.T) {
	// A PATCH reads the aliases twice before it suspends.
	for read, skip := range map[string]int{"IdentityAliasValues": 2, "HeldEmails": 0} {
		t.Run(read, func(t *testing.T) {
			const sub, email = "sub-leaver", "leaver@corp.example"
			e := newSCIMEnv(t)
			e.seedEntra(sub, email, oidPat)
			own, _ := e.seedToken(sub, email)
			other, otherRaw := e.seedToken("sub-other", email)
			id := e.postUserID(e.a, oidPat, email, email)
			n := e.node(func(c *Config) { c.Store = &failDirectoryOnce{PG: e.st, fail: read, skip: skip} })

			if w := e.patch(n, id, patchOf("false")); w.Code != http.StatusInternalServerError {
				t.Fatalf("suspend = %d %s, want 5xx", w.Code, w.Body.String())
			}
			if row := e.identity(id); row.DeactivatedAt == nil || row.Principal != sub {
				t.Fatalf("identity = %+v, want %q deactivated", row, sub)
			}
			if refused, err := e.st.IdentityRefused(context.Background(), e.issuer, e.tenant, oidPat, sub); err != nil || !refused {
				t.Errorf("sign-in = refused %v (%v), want refused", refused, err)
			}
			if fails, err := e.st.ListDeprovisionFailures(context.Background(), 10); err != nil || len(fails) != 1 ||
				fails[0].Step != store.JobStepCutoff || fails[0].LastError != reasonIdentityDirectoryUnreadable {
				t.Errorf("failures = %+v (%v), want step 1 pending with %s", fails, err, reasonIdentityDirectoryUnreadable)
			}
			rows := e.rows(n, "person.deprovision")
			if len(rows) != 1 || rows[0].Outcome != "failure" || dataOf(t, rows[0])["reason"] != reasonIdentityDirectoryUnreadable {
				t.Errorf("person.deprovision rows = %+v, want one failure with %s", rows, reasonIdentityDirectoryUnreadable)
			}
			if e.tokenRevoked(other) || !e.tokenWorks(e.b, otherRaw) || len(e.rows(n, "scim.user.deactivate")) != 0 {
				t.Error("the stopped suspension reached another principal or was recorded as done")
			}

			if w := e.patch(n, id, patchOf("false")); w.Code != http.StatusOK {
				t.Fatalf("the retry = %d %s", w.Code, w.Body.String())
			}
			if !e.allJobsDone(id) || !e.tokenRevoked(own) || len(e.rows(n, "scim.user.deactivate")) != 1 {
				t.Error("the retry did not finish the suspension")
			}
			if e.tokenRevoked(other) {
				t.Error("the retry revoked another principal's token")
			}
		})
	}
}

// The sweeper's resume of a suspension still stopped for the same reason writes no new audit row and
// leaves the identity's epoch and cutoff alone; it only touches the ledger, and finishes once the
// emails resolve.
func TestSCIMSweeperResumeOfAStoppedSuspensionWritesNothingNew(t *testing.T) {
	const twin = "twin@corp.example"
	e := newSCIMEnv(t)
	tok1, _ := e.seedToken("sub-t1", twin)
	tok2, _ := e.seedToken("sub-t2", twin)
	form := entraPrincipalPrefix + e.tenant + ":" + oidGhost
	id := e.postUserID(e.a, oidGhost, twin, twin)
	if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend = %d %s, want 5xx", w.Code, w.Body.String())
	}
	type state struct {
		audits, attempts int
		epoch            int64
		cut              time.Time
	}
	read := func() state {
		var s state
		if err := e.pool.QueryRow(context.Background(), `
			SELECT (SELECT revoked_at FROM oidc_session_revocations WHERE sub = $2),
			       (SELECT attempts FROM deprovision_jobs WHERE identity_id = $1 AND kind = 'suspend' AND step = $3)`,
			id, form, store.JobStepCutoff).Scan(&s.cut, &s.attempts); err != nil {
			t.Fatal(err)
		}
		s.audits, s.epoch = len(e.rows(e.a, "person.deprovision"))+len(e.rows(e.b, "person.deprovision")), e.identity(id).AuthorityEpoch
		return s
	}
	tick := func() {
		if _, err := e.pool.Exec(context.Background(), `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		_ = e.b.srv.SweepSCIMPurge(context.Background())
	}

	first := read()
	if first.audits != 1 {
		t.Fatalf("person.deprovision rows after the request = %d, want 1", first.audits)
	}
	for i := 1; i <= 2; i++ {
		tick()
		got := read()
		if got.audits != first.audits || got.epoch != first.epoch || !got.cut.Equal(first.cut) {
			t.Errorf("tick %d: audits %d epoch %d cut %v, want %d %d %v", i, got.audits, got.epoch, got.cut, first.audits, first.epoch, first.cut)
		}
		if got.attempts != first.attempts+i {
			t.Errorf("tick %d: attempts %d, want %d (the sweeper resumed it)", i, got.attempts, first.attempts+i)
		}
	}

	if _, err := e.pool.Exec(context.Background(), `DELETE FROM api_tokens WHERE id = $1`, tok2); err != nil {
		t.Fatal(err)
	}
	tick()
	if !e.allJobsDone(id) || !e.tokenRevoked(tok1) {
		t.Error("the resume once the emails resolve did not finish the suspension")
	}
}

// A suspension that stops while neither the store nor the spool takes its person.deprovision row keeps the row
// owed: every resume writes it again, with no new cut and the epoch unchanged, and once the audit write works
// exactly one row lands and later resumes write none.
func TestSCIMStoppedSuspensionKeepsItsAuditRowOwedUntilWritten(t *testing.T) {
	const twin = "twin@corp.example"
	e := newSCIMEnv(t)
	e.seedToken("sub-t1", twin)
	e.seedToken("sub-t2", twin)
	form := entraPrincipalPrefix + e.tenant + ":" + oidGhost
	setFailing := func(v bool) {
		e.a.audit.fail("person.deprovision", v)
		e.b.audit.fail("person.deprovision", v)
	}
	setFailing(true)
	id := e.postUserID(e.a, oidGhost, twin, twin)
	if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend = %d %s, want 5xx", w.Code, w.Body.String())
	}
	type state struct {
		rows, tried int
		epoch       int64
		cut         time.Time
	}
	read := func() state {
		var s state
		if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at FROM oidc_session_revocations WHERE sub = $1`, form).Scan(&s.cut); err != nil {
			t.Fatal(err)
		}
		s.rows = len(e.rows(e.a, "person.deprovision")) + len(e.rows(e.b, "person.deprovision"))
		s.tried = e.a.audit.attempts("person.deprovision") + e.b.audit.attempts("person.deprovision")
		s.epoch = e.identity(id).AuthorityEpoch
		return s
	}
	tick := func() {
		if _, err := e.pool.Exec(context.Background(), `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		_ = e.b.srv.SweepSCIMPurge(context.Background())
	}
	check := func(when string, want state) {
		t.Helper()
		if got := read(); got.rows != want.rows || got.tried != want.tried || got.epoch != want.epoch || !got.cut.Equal(want.cut) {
			t.Errorf("%s: rows %d tried %d epoch %d cut %v, want %d %d %d %v", when, got.rows, got.tried, got.epoch, got.cut, want.rows, want.tried, want.epoch, want.cut)
		}
	}

	first := read()
	if first.rows != 0 || first.tried != 1 {
		t.Fatalf("after the request: rows %d tried %d, want 0 1", first.rows, first.tried)
	}
	want := first
	for i := 1; i <= 2; i++ {
		tick()
		want.tried++
		check(fmt.Sprintf("resume %d while the audit write fails", i), want)
	}
	setFailing(false)
	tick()
	want.rows, want.tried = 1, want.tried+1
	check("the resume once the audit write works", want)
	tick()
	check("a later resume", want)
}
