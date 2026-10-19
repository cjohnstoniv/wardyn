# interactive-repl

An interactive e2e task. Unlike the other tasks, there is **no batch agent run
and no model** — the sandbox is launched idle (the docker driver sets
`Cmd=["sleep","infinity"]`) and the **orchestrator drives an interactive PTY**
over the WebSocket attach channel, asserting against the output **stream** rather
than the final workspace.

## What the orchestrator does

1. Attach to the run's PTY (WS-attach), same channel a human gets from
   `wardyn run attach <run-id>`.
2. Send `echo wardyn-pty-$((6*7))\n` and wait for `wardyn-pty-42` in the stream.
   This proves the PTY is live and evaluating shell input end to end.
3. Send an **in-PTY positive control**: an allowed host fetched through the same proxy,
   then print `WOK` and wait for it in the stream.
4. Send an **in-PTY negative egress probe** and wait for `403` in the stream:
   ```sh
   curl -sS -o /dev/null -m 12 -w '%{http_code}' \
     -x http://wardyn-proxy:3128 https://evil.example.com/
   ```
   A denied host must be held by the proxy (403) even from a live interactive
   shell — the egress boundary is not relaxed for interactive sessions.
5. Kill the run to tear the sandbox down. An interactive run idles, so there is no
   `exit` or EOF step.

The sequence is driven from code in `live/interactive_test.go`. The `expects:` block in
`task.yaml` documents it and nothing reads it, so keep the two in step.

## Grading

Grading is **stream-based in the orchestrator**, which is the one acknowledged
exception to the standard "`grade.sh` inspects final `/ws` state" contract:

- `grade.sh` is a no-op placeholder that always exits 0 (there is no meaningful
  final workspace state to inspect).
- The negative case is **not** a `bad-workspace/` fixture; it is the in-PTY
  egress-probe expectation (`403`) driven in `live/interactive_test.go` and documented
  in `task.yaml` `expects:`. If the
  denied host were reachable, the stream would not contain `403` and the
  orchestrator would fail the task.
- `solution.sh` is documentation only (the drive script); it is never executed
  inside the sandbox.
