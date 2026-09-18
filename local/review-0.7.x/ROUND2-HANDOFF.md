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
| R024 | Pending final browser gate | Forward SheetOverlay's DOM ref so Radix can finish its existing backdrop exit animation. No style, timing, token, focus or copy changes. |
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
  50 focused tests and official typecheck passed. Full console gate is running.
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

A new `/tmp/wardyn-review-0.7x-round2-final` integration is being prepared for all
four new patches. No combined pass is claimed until that selection is frozen and
its gates finish. Browser acceptance will use a separate worktree at the same
exact SHA, avoiding concurrent CI/browser build artifacts.

## Release integration

R025's supportbundle.go hunk combines cleanly with R017's structured redaction.
R024 is separate from R018's Dialog/AlertDialog wrappers. R027 re-points the guarded
recording.upload citation in docs/AUDIT-ACTIONS.md; that file is also changing in
the other 0.7.6 campaign. Re-point against the actual selected source, preserving
the other campaign's rows and guards. The old R022 citation patch remains specific
to its documentation selection, not a universal release prerequisite.

Release-owner gates are still required after mixing these patches with 0.7.6.
The existing live SSO/Kubernetes/full-application-restore gaps remain; daemon-free
CI and hermetic console acceptance do not certify production readiness.
