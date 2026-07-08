# OSS Contributor-Path Campaign — per-language specs (live-verified by agents)

All commits verified to exist; each feature's held-out test verified fail-before / pass-after;
each setup verified to build+test offline-after-install where noted.

## JS — uuidjs/uuid @ a67db57f7ae169e97c2573fd9d852b8364f89bf8  (15,306★)
- clone: https://github.com/uuidjs/uuid.git
- setup: `npm ci --no-audit --no-fund` (11s, 904 pkgs) ; `npm test` (2–4s, 82 tests; runs pretest→tsc→node --test)
- egress: clone github.com/codeload.github.com ; `npm ci` = registry.npmjs.org ONLY ; tests offline
- feature: new `src/compare.ts` default-export `compare(a,b:string):number` — validate() both (TypeError on invalid),
  case-insensitive canonical compare, returns -1/0/1 (Array.sort contract). One-line export in src/index.ts.
- heldout: `src/test/compare.test.ts` (equal, case-insensitive-equal, NIL<mid<MAX, v7 sort, throws). Run:
  `npm run build && node --test dist-node/test/compare.test.js`
- grader: node:22-bookworm-slim, --network none, overlay pristine compare.test.ts, `npm test`; pass = `# fail 0`, ≥82 pass
- RISK (live-found): `npm test`→npm pack→prepare→lefthook install HARD-FAILS without `.git`. Keep .git or `git init`.
  node_modules ~376MB. Reuse node_modules in grader (don't re-`npm ci`).

## Go — go-chi/chi @ 8b258c7bb28f97a5f2a856ff7ef962578fec9215  (22,498★)
- clone: https://github.com/go-chi/chi.git
- setup: `go mod download` (no-op, zero deps) ; `go build ./...` (~3s) ; `go test ./...` (~26s; middleware pkg dominates)
- egress: NONE (zero deps, no go.sum). Run GOPROXY=off. (else would only touch proxy.golang.org/sum.golang.org)
- feature: `context.go` add `func URLParamInt(r *http.Request, key string) (int, error)` = strconv.Atoi(URLParam(...)).
- heldout: `context_urlparamint_test.go` pkg chi (valid+/-/0/non-numeric/missing). Run `go test -run TestURLParamInt .`
- grader: golang:1.23-bookworm, GOPROXY=off CGO_ENABLED=0, `go build ./... && go test ./... && go test -run TestURLParamInt -v .`
- RISK: middleware tests ~26s (timers) — give headroom. Pin GOPROXY=off.

## Python — jd/tenacity @ b2cd0274c67610d615019ab4745f521504a0576d  (8,706★)
- clone: https://github.com/jd/tenacity.git  (FULL clone — hatch-vcs needs .git; or SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0)
- setup: `pip install -e ".[test]"` (~4s) ; `pytest -q` (~2.3s, 165 passed 1 skipped[trio])
- egress: pypi.org, files.pythonhosted.org
- feature: `tenacity/wait.py` new class `wait_random_incrementing(wait_incrementing)` (jittered incrementing backoff);
  export in tenacity/__init__.py (import block + __all__). jitter=0 deterministic; clamp [0,max]; never negative.
- heldout: `tests/test_wait_random_incrementing.py` (5 tests). Run `pytest tests/test_wait_random_incrementing.py -q`
- grader: python:3.12-slim, SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0, network yes, cp workspace, pip install -e .[test], pytest -q
- RISK (live): missing .git → LookupError on install unless SETUPTOOLS_SCM_PRETEND_VERSION set. 1 skipped=trio (not a fail).

## Rust — BurntSushi/walkdir @ 6fd031c82ba5a4204b4ce6eae73dacb00dc072ec  (1,513★)
- clone: https://github.com/BurntSushi/walkdir.git
- setup: `cargo build` (1.3s) ; `cargo test` (~1.7s, 48 unit + 20 doctest). edition 2018, MSRV 1.60.
- egress: github.com (clone) ; index.crates.io + static.crates.io (single dep same-file ~15KB). No apex crates.io.
- feature: `src/dent.rs` add to impl DirEntry: `pub fn extension(&self) -> Option<&OsStr>` = self.path.extension().
- heldout: `tests/extension.rs` (notes.txt→txt, README→None, .hidden→None; rolls its own tmpdir, no dev-deps). `cargo test --test extension`
- grader: rust:1.82-bookworm, copy ~/.cargo/registry + target/ for --offline (else needs 2 registry hosts), `cargo test --locked`
- RISK: compile time (minimal here). Copy target/ + registry to grader to avoid recompile+network.

## Java — jhy/jsoup @ d8c49e5ec72a08ca1ac4e08740e70dc0f47ad911  (11,380★)  Maven, single module, JDK 21
- clone: https://github.com/jhy/jsoup.git
- setup: `mvn -q -B -DskipTests package` ; `mvn -q -B test` (Surefire only; *IT.java integration tests skipped)
- egress: repo.maven.apache.org, repo1.maven.org (Maven Central)
- feature: `src/main/java/org/jsoup/nodes/Element.java` add `public boolean isFirstOfType()` (CSS :first-of-type; no parent→true).
- heldout: `src/test/java/org/jsoup/nodes/ElementIsFirstOfTypeTest.java` (4 tests). `mvn -q test -Dtest=ElementIsFirstOfTypeTest`
- grader: maven:3.9-eclipse-temurin-21, pre-warm ~/.m2 (else Central), `mvn -q -B test`; pass = BUILD SUCCESS + Failures:0
- RISK: ~/.m2 cold download time dominates — pre-warm + reuse. animal-sniffer enforces Java-8 API (ref impl is safe).
  Loopback integration tests (org/jsoup/integration/*Test.java) DO run under mvn test — need loopback sockets, no external egress.
