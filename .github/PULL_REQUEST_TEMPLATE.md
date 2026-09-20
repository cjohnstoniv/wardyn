## Summary

<!-- What this PR does and why, in a few lines. -->

Closes #<!-- issue --> <!-- or: Refs #N when a later PR finishes the issue; Depends on #N for a stacked PR -->

## Checklist

- [ ] The issue is on the milestone and carries the `approved` label
- [ ] One issue per PR (or ≤3 tightly coupled issues, each named above)
- [ ] The check I ran is quoted below: the command and the test names it executed, not a summary line
- [ ] Docs land here: CHANGELOG `[Unreleased]`, `docs/AUDIT-ACTIONS.md` rows for new audit actions, `docs/ENV.md` rows for new variables
- [ ] Console change: mock/canon round approved or waived on the issue; a Playwright pin covers the new path
- [ ] Conformance impact considered — new behaviour covered by or explicitly exempt from `test/conformance` on both substrates
- [ ] Security invariants preserved ([ARCHITECTURE.md](../ARCHITECTURE.md) §Security invariants); `security-review` label if in doubt
- [ ] Every commit is DCO-signed (`git commit -s`)

## Check

```
<!-- e.g. go test ./internal/api/ -run 'TestX|TestY' -count=1 -v  →  === RUN TestX … PASS -->
```

## DCO sign-off (required)

Every commit in this PR must carry a `Signed-off-by` line. CI enforces this.
Use `git commit -s` to add it automatically. If any commit is missing the
sign-off, rebase and re-sign before requesting review.

```
Signed-off-by: Your Name <your.email@example.com>
```
