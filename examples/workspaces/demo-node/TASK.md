# Scenario 7 — Demo recording: real work plus a host held at the door

The task the demo videos' agent runs perform. One run, two egress outcomes: a
host allowed outright (the model), and a host **held** until a human approves
it. (The line-that-cannot-be-crossed — the cloud-metadata probe — is V01's
lesson on the keyless funnel now; paying a minute of a quota-bound agent run to
re-teach it was the trade the 2026-08-18 reorder refused.)

## Wardyn run command

    wardyn run \
      --agent claude-code \
      --task "$(sed -n '/^1\./,/^3\./p' TASK.md)"

The canonical copy of this text lives in `ui/e2e/demo/task.ts` (`DEMO_TASK`) so
the recording driver and this file cannot drift; the driver types it into the
New Run form verbatim.

1. Run `curl -sS --max-time 90 -o /dev/null -w '%{http_code}' https://example.com/` — if it is refused (status `000`, or a `403` from the proxy), wait 20 seconds and run the exact same command once more, because an operator may be approving the host while you wait. Repeat that retry up to 3 times, then write the final status code into `NOTES.md`.
2. Add a `slugify(s)` function to `src/slug.js` that lower-cases the input, replaces every run of non-alphanumeric characters with a single hyphen, and trims leading and trailing hyphens. Add a matching test to `test/slug.test.js`, then run `node --test` and make sure every test passes.
3. Finish `NOTES.md` with one short paragraph: the final status code you got for `example.com`, and what you built in step 2.

## Why the task is shaped this way

- **Step 1 is the held host, front-loaded.** The decision beat surfaces about a
  minute after launch instead of after the whole coding stretch, so the human
  moment happens early and the long unwatchable part lands where the recorder's
  fast-forward can compress it (`ui/e2e/demo/overlay.ts` ffwdStart/ffwdEnd).
- **Step 2 is the meaningful work** — a real edit, a real test run, a real diff on
  the run's Files surface.
- **The held host is refused, then the RETRY succeeds.** `example.com` is off the run's
  allowlist. The New Run wizard picks **Hold it for approval** (`wait_for_review`),
  but the operator policy ceiling clamps the effective mode down to **Deny, but
  ask** (`deny_with_review`) — see `docs/DEMO-SCRIPT.md`'s "Act 5 specifics" for
  the mechanics — so the first attempt is refused outright and raises an approval;
  it is the agent's own retry (step 1's instructions above), not the same
  in-flight request, that succeeds once an operator approves. `--max-time 90`
  still gives the operator a comfortable window before the next retry lands.
- **The model call is the allowed case.** Every Claude Code request to
  `api.anthropic.com` is allow-listed and brokered, and shows up in the audit
  trail as `brokered:llm`.

## PASS criteria

1. `node --test` passes inside the sandbox and `GET /api/v1/runs/{id}/files`
   reports a non-empty diff.
2. Audit has `approval.decide outcome=approved` → `egress.allow` for
   `example.com`, and `NOTES.md` records `200`. **Not `egress.pending`**: the
   ceiling clamp is member-only (`inline_policy.go` gates it on
   `!isOperator`), so a demo run keeps `wait_for_review` and the request is
   genuinely HELD — an approved hold logs only the allow.
3. Audit has `egress.allow` for `api.anthropic.com`.
4. The run is not bricked by the hold — it reaches COMPLETED.
