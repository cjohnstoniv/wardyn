// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// `wardynd -recovery-rehearse` (docs/design/0.9/recovery-set.md §7): prove, against a restored copy of
// the database, that the set pairs, the audit chain holds and the keys in this process open every
// stored row. It reads one read-only backend on the target and writes two audit rows to live.

// Exit codes beyond the migrate-only pair (exitMigrateFailed 1, exitMigrateRefused 3).
const (
	exitRehearseFail       = 4
	exitRehearseIncomplete = 5
)

const (
	rehearseActor      = "wardyn/recovery-rehearse"
	actionRehearseBeg  = "recovery.rehearse.begin"
	actionRehearseDone = "recovery.rehearse"

	resultPass       = "pass"
	resultFail       = "fail"
	resultRefused    = "refused"
	resultIncomplete = "incomplete"
)

// rehearseSet is what the rehearsal needs of the set's manifest.
type rehearseSet struct {
	ID        string
	Dir       string
	HeadSeq   int64
	HeadHash  string
	Migration string
	Source    dsnIdentity
	// AgeRecipient is the manifest's fingerprint of WARDYN_AGE_KEY, "" when the deployment has none.
	AgeRecipient string
	// KEKMode is the manifest's KEK mode, "" to skip the comparison.
	KEKMode string
	// KeyClasses is the row count per key class at manifest time.
	KeyClasses map[string]int
	// Spool is the manifest's audit spool entry, which the staged files in Dir must equal.
	Spool recoverySpool
	// Recordings and Drives are the manifest's staged-file entries, which the set's directories must equal.
	Recordings recoveryRecordings
	Drives     []recoveryDrive
}

type rehearseCheck struct {
	Name      string         `json:"name"`
	Result    string         `json:"result"`
	Detail    string         `json:"detail,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	Failing   []string       `json:"failing,omitempty"`
	BrokenSeq int64          `json:"broken_seq,omitempty"`
}

// rehearseReport is the JSON on stdout. It holds counts and row references, never a value.
type rehearseReport struct {
	SetID           string          `json:"set_id"`
	Verdict         string          `json:"verdict"`
	Checks          []rehearseCheck `json:"checks"`
	VerdictRecorded bool            `json:"verdict_recorded"`
}

func (r *rehearseReport) add(c rehearseCheck) { r.Checks = append(r.Checks, c) }

// verdictOf is the report's verdict: any fail is fail, else any incomplete is incomplete.
func (r *rehearseReport) verdictOf() (string, int) {
	verdict := resultPass
	for _, c := range r.Checks {
		switch c.Result {
		case resultFail:
			return resultFail, exitRehearseFail
		case resultIncomplete:
			verdict = resultIncomplete
		}
	}
	if verdict == resultIncomplete {
		return verdict, exitRehearseIncomplete
	}
	return verdict, 0
}

// scrubber removes the values a rehearsal holds from any text it prints: both DSNs and their
// passwords, the age key and the key-service token. Nothing here puts one in a message on purpose;
// this is for the library error that might.
type scrubber []string

func newScrubber(f *bootFlags, targetDSN string) scrubber {
	var s scrubber
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			s = append(s, v)
		}
	}
	for _, dsn := range []string{*f.dsn, targetDSN} {
		add(dsn)
		if cfg, err := pgconn.ParseConfig(dsn); err == nil {
			add(cfg.Password)
		}
	}
	add(*f.ageKey)
	if p := strings.TrimSpace(*f.vault.tokenFile); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			add(string(b))
		}
	}
	slices.SortFunc(s, func(a, b string) int { return len(b) - len(a) })
	return s
}

func (s scrubber) text(t string) string {
	for _, v := range s {
		t = strings.ReplaceAll(t, v, "[redacted]")
	}
	return t
}

func (s scrubber) err(code int, err error) error {
	return &exitCodeError{code: code, err: fmt.Errorf("%s", s.text(err.Error()))}
}

// rehearse runs the rehearsal and returns an error carrying the exit code; nil is exit 0. The report
// goes to out, a failed verdict row to errOut.
func rehearse(ctx context.Context, f *bootFlags, set rehearseSet, targetDSN string, out, errOut io.Writer) error {
	scrub := newScrubber(f, targetDSN)
	rep := &rehearseReport{SetID: set.ID}
	err := rehearseRun(ctx, f, set, targetDSN, rep, errOut, scrub)
	if err == nil {
		return writeReport(out, rep, scrub)
	}
	if exitCodeOf(err) == exitMigrateRefused {
		rep.Verdict = resultRefused
		rep.add(rehearseCheck{Name: "refusal", Result: resultRefused, Detail: scrub.text(err.Error())})
		_ = writeReport(out, rep, scrub)
	}
	return scrub.err(exitCodeOf(err), err)
}

func writeReport(out io.Writer, rep *rehearseReport, scrub scrubber) error {
	if rep.Verdict == "" {
		rep.Verdict, _ = rep.verdictOf()
	}
	// Only the free text is scrubbed: rewriting the whole JSON would turn a secret that happens to
	// spell an enum (a password "pass") into a verdict of "[redacted]".
	shown := *rep
	shown.Checks = make([]rehearseCheck, len(rep.Checks))
	for i, c := range rep.Checks {
		c.Detail = scrub.text(c.Detail)
		c.Failing = slices.Clone(c.Failing)
		for j := range c.Failing {
			c.Failing[j] = scrub.text(c.Failing[j])
		}
		shown.Checks[i] = c
	}
	raw, err := json.MarshalIndent(shown, "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, string(raw)); err != nil {
		return err
	}
	if rep.Verdict == resultPass {
		return nil
	}
	_, code := rep.verdictOf()
	return &exitCodeError{code: code, err: fmt.Errorf("rehearsal verdict: %s", rep.Verdict)}
}

func rehearseRun(ctx context.Context, f *bootFlags, set rehearseSet, targetDSN string, rep *rehearseReport, errOut io.Writer, scrub scrubber) error {
	sess, err := (targetChecks{
		liveDSN: *f.dsn, targetDSN: targetDSN, source: set.Source, migration: set.Migration,
		setDir: set.Dir, protected: protectedDirs(f),
	}).run(ctx)
	if err != nil {
		return err
	}
	defer sess.close()
	root, err := os.OpenRoot(set.Dir)
	if err != nil {
		return fmt.Errorf("open the set: %w", err)
	}
	defer root.Close()
	if err := sess.pair(ctx, set, root); err != nil {
		return err
	}
	store, id, err := rehearseKeyStore(ctx, f)
	if err != nil {
		return err
	}
	rec, fan, _, _, err := buildAuditChain(ctx, *f.auditSinks, "", *f.auditSource, sess.live, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil {
		return err
	}
	if fan != nil {
		defer func() { _ = fan.Close() }()
	}
	begin := map[string]any{"set_id": set.ID, "manifest_head_seq": set.HeadSeq, "target": sess.id.String()}
	if err := recordMaintenanceAudit(ctx, rec, rehearseActor, actionRehearseBeg, set.ID, "success", begin); err != nil {
		return refusedTo(rehearseVerb, "the begin row could not be written to live (%v); nothing was decrypted", err)
	}
	rep.add(rehearseCheck{Name: "pairing", Result: resultPass, Detail: "audit head matches the manifest"})

	if c := checkFingerprint(set, id, kekModeOf(f)); c != nil {
		rep.Checks = append([]rehearseCheck{*c}, rep.Checks...)
	}
	chain, err := sess.chainSweep(ctx)
	if err != nil {
		return err
	}
	rep.add(chain)
	use, err := sess.keyUse(ctx, store, id)
	if err != nil {
		return err
	}
	rep.add(keyUseCheck(set, use))
	files, err := sess.fileChecks(ctx, set, root)
	if err != nil {
		return err
	}
	rep.Checks = append(rep.Checks, files...)

	verdict, _ := rep.verdictOf()
	rep.Verdict = verdict
	data := map[string]any{"set_id": set.ID, "manifest_head_seq": set.HeadSeq, "target": sess.id.String(), "verdict": verdict, "checks": auditChecks(rep.Checks)}
	outcome := "success"
	if verdict != resultPass {
		outcome = "failure"
	}
	if err := recordMaintenanceAudit(ctx, rec, rehearseActor, actionRehearseDone, set.ID, outcome, data); err != nil {
		fmt.Fprintf(errOut, "wardynd: the verdict row could not be written to live; the verdict stands: %s\n", scrub.text(err.Error()))
	} else {
		rep.VerdictRecorded = true
	}
	return nil
}

// auditChecks is the checks as the verdict row records them: results and counts. The failing
// references name rows and their owners; they stay in the report, which only the operator reads.
func auditChecks(checks []rehearseCheck) []rehearseCheck {
	out := make([]rehearseCheck, len(checks))
	for i, c := range checks {
		out[i] = rehearseCheck{Name: c.Name, Result: c.Result, Counts: c.Counts, BrokenSeq: c.BrokenSeq}
		if len(c.Failing) > 0 {
			out[i].Counts = maps.Clone(c.Counts)
			if out[i].Counts == nil {
				out[i].Counts = map[string]int{}
			}
			out[i].Counts["failing"] = len(c.Failing)
		}
	}
	return out
}

// pair is pairing: the target's newest audit row must be the one the manifest recorded, and each
// staged spool file must be its manifest entry. A mismatch refuses, naming "audit head" or the file.
func (s *rehearseSession) pair(ctx context.Context, set rehearseSet, root *os.Root) error {
	var seq int64
	var hash string
	err := s.conn().QueryRow(ctx, `SELECT COALESCE(max(seq), 0), COALESCE((SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1), '') FROM audit_events`).Scan(&seq, &hash)
	if err != nil {
		return fmt.Errorf("read the target's audit head: %w", err)
	}
	if seq != set.HeadSeq || hash != set.HeadHash {
		return refusedTo(rehearseVerb, "audit head: the database is at seq %d and the manifest records seq %d (or a different row): they are not one snapshot", seq, set.HeadSeq)
	}
	got, err := stageSpoolFiles(root, set.Spool.Basename)
	if err != nil {
		return refusedTo(rehearseVerb, "%v", err)
	}
	return pairSpool(rehearseVerb, set.Spool, got)
}

// chainSweep is VerifyAuditChain over the one target backend.
func (s *rehearseSession) chainSweep(ctx context.Context) (rehearseCheck, error) {
	tx, err := s.readTx(ctx, pgx.RepeatableRead)
	if err != nil {
		return rehearseCheck{}, fmt.Errorf("begin the audit chain sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st, err := store.VerifyAuditChainTx(ctx, tx)
	if err != nil {
		return rehearseCheck{}, err
	}
	c := rehearseCheck{Name: "audit chain", Result: resultPass, Counts: map[string]int{"checked": int(st.Checked)}}
	if !st.OK {
		c.Result, c.BrokenSeq, c.Detail = resultFail, st.BrokenSeq, st.Reason
	}
	return c, nil
}

// checkFingerprint compares the age recipient the manifest recorded with the one in this process:
// a different key is reported before any row is tried.
func checkFingerprint(set rehearseSet, id *age.X25519Identity, kekMode string) *rehearseCheck {
	switch {
	case set.KEKMode != "" && set.KEKMode != kekMode:
		return &rehearseCheck{Name: "key fingerprint", Result: resultFail,
			Detail: fmt.Sprintf("WARDYN_KEK is %q here and %q in the manifest: this is not the key configuration the set was taken under", kekMode, set.KEKMode)}
	case set.AgeRecipient == "" || id == nil || id.Recipient().String() == set.AgeRecipient:
		return nil
	}
	return &rehearseCheck{Name: "key fingerprint", Result: resultFail,
		Detail: "WARDYN_AGE_KEY is not the key the set was taken under: its recipient differs from the manifest's"}
}

func keyUseCheck(set rehearseSet, use keyUseResult) rehearseCheck {
	c := rehearseCheck{Name: "key use", Result: resultPass, Counts: use.Counts}
	c.Failing = append(c.Failing, use.Failing...)
	for _, class := range slices.Sorted(maps.Keys(set.KeyClasses)) {
		if use.Classes[class] != set.KeyClasses[class] {
			c.Failing = append(c.Failing, fmt.Sprintf("key class %s: %d rows in the target, %d in the manifest", class, use.Classes[class], set.KeyClasses[class]))
		}
	}
	for _, class := range slices.Sorted(maps.Keys(use.Classes)) {
		if _, ok := set.KeyClasses[class]; !ok {
			c.Failing = append(c.Failing, fmt.Sprintf("key class %s: %d rows in the target, none in the manifest", class, use.Classes[class]))
		}
	}
	if len(c.Failing) > 0 {
		c.Result = resultFail
	}
	return c
}
