# Test reports

Regenerable test artifacts for the Wardyn platform. Every suite emits the raw
`go test -json` event stream **and** coverage profiles, so a run carries
per-test results, durations, and failure reasons — not just a pass/fail exit
code.

## Layout

```
test/reports/
  README.md                 # this file (the only committed file here)
  go/<suite>/
    test-output.json        # raw `go test -json` stream (gitignored)
    cover.out               # coverage profile (gitignored; *.out)
    coverage.html           # HTML coverage (gitignored)
    coverage-func.txt       # per-func coverage + total (gitignored)
  ui/
    junit.xml               # vitest JUnit (gitignored)
    coverage/               # vitest v8 coverage html+lcov (gitignored)
  e2e/
    playwright-report/      # Playwright HTML report + traces (gitignored)
    junit.xml results.json  # (gitignored)
```

Only this README is committed; everything else under `test/reports/` is
regenerable output and gitignored (see `.gitignore`).

## How to regenerate

```bash
make test-report                 # Go unit suite  -> test/reports/go/unit/
WARDYN_TEST_PG=postgres://...  make test-report-pg
WARDYN_TEST_DOCKER=1           make test-report-docker
make cover-check                 # enforce the coverage floor (COVER_MIN, default 65) over the
                                 # UNION of both shipped builds (tagless + -tags docker)
make ui-test                     # vitest + coverage -> test/reports/ui/
cd ui && pnpm e2e                # Playwright (seeded backend) -> test/reports/e2e/
```

The Go report driver is `scripts/test-report.sh` — `go test -json` plus
`go tool cover`, no extra runtimes.
