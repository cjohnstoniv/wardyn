# slugify-demo

A four-file Node project with no dependencies. It exists so a governed run has
real work to do on camera: add one function, add one test, watch it go green.

```sh
node --test
```

Node 22's built-in test runner is used deliberately — the `claude-code` agent
image ships Node but no `pytest`, so a dependency-free `node --test` is the one
test command that is certain to work inside the sandbox.

Used by `scripts/record-demo.sh`, which copies this directory to a scratch
location and `git init`s it before the recording starts. Nothing edits this
directory in place — the copy is what the agent mounts and modifies, so a demo
run never dirties the Wardyn repo.

See [`TASK.md`](TASK.md) for the exact task text and PASS criteria.
