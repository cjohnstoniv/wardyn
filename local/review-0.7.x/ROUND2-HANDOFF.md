# Round 2 — additional 0.7.x patches

Date: 2026-09-18. This is a delta to the first handoff, not a replacement for
PATCH-LEDGER.md. The original `0d63ab2e` selection and its recorded acceptance
remain unchanged. Main stays clean at `dfa89f60`; the other agent's 0.7.6 worktrees
are untouched. No 0.8 features, migrations, releases or pushes are included.

## Independent changes

Every original below is based directly on `dfa89f60`, not on another patch or
the changing `feature/0.7.6` branch. Select the originals, not an integration
umbrella on top of them.

| Finding | Original commit | Change and compatibility boundary |
| --- | --- | --- |
| R024 | `f6e787a5d37a2efac00881923865df06ed5eba53` | Forward SheetOverlay's DOM ref so Radix can finish its existing backdrop exit animation. No style, timing, token, focus or copy changes. |
| R025 | `2307b07c57443776baddf1b9926c04f9c27c2587` | Use unique private sibling files for CLI recording downloads and support bundles. Existing unrelated `.part` files are preserved; simultaneous exports no longer share a temporary inode. Output permissions intentionally tighten to owner-only. Final-path replacement still uses the last successful atomic rename. |
| R026 | `f5fbb1669175074fc9e418a744b2c8cf9ed21054` | Reject additional YAML policy documents before create/update/render/run can consume only the first. Ambiguous multi-document input now errors; single JSON/YAML documents, markers/comments and legacy empty-input handling are unchanged. |
| R027 | `0bb4699b9de6f462552ddc031393ea875b437928` | Read recording upload data only when storage requests it, using the existing masking writer. An early filesystem-store failure no longer waits on an independently blocked input goroutine. Existing masking, upload cap, and tail/error ordering remain. |

R025 assumes the destination directory is trusted, as before. It does not lock
the final path or guarantee cleanup after process termination. R027 is a handler
and masking-pipeline lifetime fix: Go's HTTP server may still drain the body after
the handler returns. It is not a new transport deadline or a generic PostgreSQL
outage fix. The adapter has one 32 KiB source chunk plus bounded expanded masked
output and the existing masker tail, not a 32 KiB total-memory guarantee.

## Patch evidence

- R024: four side-specific lifetime tests fail on baseline; two focus-restoration
  tests pass. The mock was independently approved before implementation. Actual
  patched Chromium checks passed all four sides and reduced-motion behavior;
  50 focused tests and official typecheck passed. The full console gate passed:
  347 tests / 30 specs, zero failures/skips/flakes. Root independently reviewed
  the product diff. SHEET-OVERLAY-ACCEPTANCE.md retains the complete scope.
- R025: baseline regression covers existing regular/symlink `.part` files,
  rename failure and synchronized concurrent downloads. Patched complete CLI
  race suite passed in 5.033s; interruption cleanup checks all directory entries.
  Independent lifecycle peer approved. A post-commit race rerun also passed in
  4.842s, recorded with the original SHA in evidence/cli-private-temp-race.log.
- R026: extra valid, empty and malformed documents fail against the baseline;
  corrected CLI fixtures prove rejection before network access in all four paths.
  Complete CLI race suite passed in 4.844s; independent docs peer approved.
  `evidence/cli-policy-document-baseline-final.log` is the authoritative CLI
  reproduction; the earlier log used a full-body fixture that was unsuitable for
  run's bare-spec input and was corrected before the implementation. Post-commit
  race rerun passed in 4.723s; evidence/cli-policy-document-race.log records its SHA.
- R027: a real temporary FSStore fails before reading a synthetic stalled body;
  the baseline handler hangs, while the patch returns failure with zero input
  reads. Source-error, EOF, cap, bytewise raw/escaped masking, short buffers and
  panic-fail-closed checks pass. Complete API/secretmask race suites passed in
  92.161s/1.019s; root peer approved. Full default Go tree passed, including daemon
  citation guards; evidence/recording-upload-{red,package-race,full-go}.log retain
  the raw transcripts.

## Combined verification

The separate integration `cd179c617acee6dd14ba6bc273db44569310355b` adds R023
(filesystem recording read confinement) to the first frozen batch. Its
uninterrupted `make ci` passed, exit 0, with a clean worktree: Go union coverage
78.4%, all three race configurations passed, UI 155 files / 2,880 tests passed.
Log: `evidence/round2-ci-recording-root.log`, SHA-256
`94d1883e2140c2cb9dd1d2d9d09a67f1b98189d8baca67242ee94c24cebed948`.
This closes R023's combined merge-gate gap; it does not cover R024–R027.

The new `/tmp/wardyn-review-0.7x-round2-final` selection is frozen at
`6a3dc8659ea7105ae81d127b5f3f1a198333f67d`, including all four new patches. All
combined gates passed with clean source worktrees. Browser worktree
`/tmp/wardyn-review-0.7x-round2-ui` uses the same SHA and its own build artifacts,
database wardyn_review_round2 and ports 18896/18897 (released after completion).
Real PostgreSQL acceptance used a third same-tip worktree. Composition review passed; see
evidence/round2-composition-review.md for the exact original/integrated mappings.

| Gate at the frozen SHA | Result | Evidence |
| --- | --- | --- |
| One uninterrupted `make ci` | Exit 0; Go reports/races for all three builds, union coverage 78.5%, all remaining merge gates passed | evidence/ROUND2-FINAL-VERIFICATION.md; evidence/round2-ci-final.log |
| UI unit/component suite inside CI | 156 files / 2,886 tests passed; 94.91% statements, 90.25% branches, 82.35% functions | Same CI log |
| Both official and default/editor TypeScript checks | Exit 0 | ROUND2-UI-ACCEPTANCE.md; evidence/round2-ui-typecheck.log |
| Complete browser suite | Exit 0; 347 tests / 30 specs, zero failures/skips/flakes | ROUND2-UI-ACCEPTANCE.md; evidence/round2-ui-e2e.log |
| Real PostgreSQL report and broker/store race gate | Exit 0; all nine required probes passed, 69.6% report coverage; race 2.764s / 16.772s | ROUND2-PG-ACCEPTANCE.md; evidence/round2-pg-final.log |

The PG report has 2,245 top-level tests passed and five documented skips;
including subtests, 5,525 pass / five skip / zero fail. These are not independent
end-to-end scenarios. The same-tip worktrees stayed clean, and all earlier frozen
trees remain unchanged. No gate was weakened, skipped wholesale or rerun to hide
a failure. Existing environment warnings and live-acceptance gaps remain disclosed
in the detailed reports.

Final raw-log SHA-256 values:

- CI: `ed717e2b34afcffaaa5e4b37b7688ea7c43a3959392aecf2c87ae7c75209db31`.
- Browser: `d6b617f48eb4c1e9ab30be2134e67811be21050e34af6a259ecabb268c2698b6`.
- Typechecks: `8c3ffd074da4a1e0fd6fb051e6cb7fe7d5b6b6e8b3a3fd8a3b31255351916bc9`.
- PostgreSQL: `aea81e648723653464a909d301759cdb8ad1063e87b4453520c7976d4144fcf2`.

## Release integration

R025's supportbundle.go hunk combines cleanly with R017's structured redaction.
R024 is separate from R018's Dialog/AlertDialog wrappers. R027 re-points the guarded
recording.upload citation in docs/AUDIT-ACTIONS.md; that file is also changing in
the other 0.7.6 campaign. Re-point against the actual selected source, preserving
the other campaign's rows and guards. The old R022 citation patch remains specific
to its documentation selection, not a universal release prerequisite.

R027's adjacent citation conflict in this review integration was resolved by
keeping recording.upload's new source line 119 and the earlier R022 SSH doc
references 380/432/437. The actual patch delta is unchanged (zero-context patch
IDs match); the focused citation guards passed after resolution. Do not accept
the entire baseline-side hunk and accidentally revert the existing SSH citations.

The other owner's selection plans to fold R024 into R018 and update the console
rules' overlay references. Do not apply the Sheet hunk twice if that has already
happened. R002/R003/R004 are marked deferred to 0.7.7 in that selection; the new
R003-K8S-CACHE-ASSESSMENT.md records the profile-override and prewarmed-cache
compatibility constraints, without introducing a runtime cache relocation.

One further low-risk cleanup candidate, R028, was code-reviewed while acceptance
ran: FSStore does not unlink its owned temporary file when the final rename
fails. It is recorded for the next isolated patch/reproduction, not silently
added to this frozen batch or claimed tested. Default PostgreSQL is unaffected.

Release-owner gates are still required after mixing these patches with 0.7.6.
The existing live SSO/Kubernetes/full-application-restore gaps remain; daemon-free
CI and hermetic console acceptance do not certify production readiness.
