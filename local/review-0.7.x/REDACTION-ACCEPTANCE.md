# Support-bundle redaction acceptance

Patch `c4c5fa16`, branch `review/0.7x-cli-bundle-redaction`, independently based
on `dfa89f608fa223b469b0a2435226538f0035d533`.

## Reproduction and correction

The new regression file was copied into a separate detached baseline worktree;
no baseline production source was changed. Its targeted test run exited 1 and
exposed direct multiline, folded JSON, escaped JSON, value/key aliases, nested
secret mappings and commented multiline leakage, plus invalid YAML serialization.
The malformed/multi-document cases additionally pin the new fail-closed omission.
Full output: `evidence/redaction-baseline.log`.

The same cases and all existing `TestRedactSecrets*` tests passed after the patch
(exit 0, `evidence/redaction-fixed.log`). Complete `go test -race ./cmd/wardyn
-count=1` passed in 4.602 seconds after the final alias-key correction. This
includes archive creation against a loopback test server with multiline secrets
and folded embedded JSON in the actual emitted Compose entry. Initial sandbox
socket denial was rerun with local test-server permission; it is not product
failure evidence. File-size and diff checks passed before commit.

Independent security review identified the alias-key case during review; its
regression failed before the correction and passed afterward. Final review
approved structured redaction, comment omission, valid anchor preservation,
cycle handling and fail-closed parse behavior.

## Behavioral scope

- Uses the existing YAML dependency and existing secret-marker vocabulary.
- Redacts sensitive values structurally and whole opaque scalars containing
  credential-shaped pairs or credential-bearing URLs.
- Omits comments (including commented-out credentials), malformed YAML and
  multiple-document YAML instead of exporting potentially unredacted fragments.
- Does not change deployments, daemon state, API behavior, environment variables,
  or the underlying Compose file. Output formatting can normalize during redaction.
- Secret detection remains heuristic. Unrecognized secret names or arbitrary
  sensitive prose are not universally detected; help explicitly requires review
  before sharing a bundle. Unrelated bundle entries retain their existing policy.

Provenance for replacing the old serialized-line regex is in the commit body:
`git log -S secretLineRe` led to `20733ec3`; its nested-value/key-marker intent is
preserved. No local working note invoked that removed internal symbol.

Final combined full-tree validation is recorded separately in PATCH-LEDGER.md.

## Standalone CLI acceptance

Built the actual CLI from `c4c5fa16` and ran `support-bundle` twice against one
synthetic valid Compose fixture in `/tmp/wardyn-review-bundle-acceptance.wjeNCN`.
One run used installed Docker Compose to resolve the config; the other used
`PATH=/nonexistent` to force raw-file fallback. Both commands exited 0 and wrote
tar.gz archives. Extracted Compose entries contained none of the six synthetic
credential fragments (multiline, folded JSON, aliased key and commented secret),
while retaining the image and listen address. Both exported YAML documents also
passed `docker compose config --quiet`; no containers were launched or images pulled.

The API URL intentionally pointed at unused loopback port 1, so health/setup/audit
entries exercised existing best-effort error collection, not a live daemon. The
unit archive test separately exercises a loopback fake control plane.

- CLI SHA-256: `5bfc779a1376e1bb7ce528e54bae098b07db7bc4de192d48b9f00b33ee735197`.
- Resolved archive: `8484ef43669d7dd8774f7c1e00a1062063a193f57963ce73c3dea97c390be6c1`.
- Fallback archive: `7e9a815df9ab7c66aad9c763fb5fcaa494e9fd60e2dc6ec6d9cd7ae6c4f6e7a1`.

Artifacts are temporary synthetic test data; the product commit and regression
tests are the durable reproduction. No production credentials were used.
