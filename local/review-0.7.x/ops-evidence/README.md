# Ops rehearsal evidence

`OPS-ACCEPTANCE.md` records scope, results, and limitations; `ops-results.json`
and `ops-transcript.log` are the captured outputs. The `main.go` helper uses
Wardyn's existing database, audit, secret, and recording APIs. The preserved
helper adds the repository's SPDX header; its executable code is unchanged.

Only synthetic data and nonsecret evidence are retained here. The SQL dump
remains temporary at
`/tmp/wardyn-review-0.7x-doc-oidc-email/local/review-ops/ops-backup.sql`;
no age private key was persisted. No production credential is required.

To reproduce in another isolated Wardyn worktree, put `main.go` in the ignored
`local/review-ops/` directory. The two database names are deliberately fixed and
creation refuses if either exists; choose unused `wardyn_review_ops_*` names
in the helper for a second run. The helper targets only
`wardyn-review-07x-pg` on `unix:///var/run/docker.sock`, published at
`localhost:58432`, with the review container's synthetic database credentials.
Build with `GOFLAGS=-buildvcs=false go build -o local/review-ops/ops-check ./local/review-ops`,
then run `./local/review-ops/ops-check` from that worktree's root with Docker and
loopback PostgreSQL access. The flag works around temporary-worktree Go VCS
discovery; it changes no Wardyn behavior. Use shell pipefail when capturing a
transcript with tee.

The helper leaves its two databases for inspection. It does not clean up any
database or container and never overwrites an existing dump.
