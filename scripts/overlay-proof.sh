#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# overlay-proof.sh — revert proofs, the same locally and on a hosted runner.
#
# A revert proof puts a bug back (the reverted source file) and demands that
# the pin which guards it goes red: `go test` exits 1 and prints the named
# `--- FAIL: <Test>` line. A green run means the pin guards nothing; a build
# error proves nothing. Each proof is therefore a FAIL unless it is exactly
# that. Nothing is retried.
#
#   overlay-proof.sh pack <lane-dir> <keyed_sha> [--spec FILE] [--out DIR]
#       Builds a bundle (DIR/.proofs/) from a lane's overlays/<name>/overlay.json
#       and revert/<name>.json (`go test -overlay` files). FILE (default
#       <lane-dir>/proofs.tsv) has one proof per line, tab-separated:
#       name, -run regex, want_fail line[, build tags[, package]]. The package
#       defaults to the directory of the first replaced file. Commit DIR/.proofs
#       to a throwaway proofs/<lane>-<sha12> branch, never to main.
#   overlay-proof.sh plan <bundle-dir> <keyed_sha>
#       Prints the workflow matrix as one JSON line: proofs grouped by package.
#   overlay-proof.sh run <bundle-dir> <checkout-dir> [--only <name>...]
#       Runs the selected proofs one after another, each in a clean tree.
#       Exit 0 only if every one PASSes, 1 if any FAILs, 2 if the bundle or the
#       checkout is not what the manifest names.
#
# <bundle-dir>/.proofs/manifest.json:
#   { "keyed_sha": "<40-hex>", "proofs": [ { "name", "package", "run", "tags",
#     "files": { "<repo path>": ".proofs/files/<name>/<repo path>" },
#     "want_fail": "--- FAIL: <Name>" } ] }
#
# want_fail is matched against whole output lines, after dropping leading
# whitespace (nested subtests are indented) and go's trailing " (0.01s)", so
# `--- FAIL: TestA` never matches `--- FAIL: TestAB`.
set -euo pipefail

# Timeout sizing for the workflow (docs/CI.md: at least twice the measured
# maximum, ten-minute floor). Measured locally on 2026-10-22 under gate.sh
# (GOMAXPROCS=2, nice 19): a cold `go test -c ./internal/api` took 89 s and each
# proof, which recompiles the package under its reverted file, 72 s. A hosted
# runner starts with a restored but possibly stale cache, so the cold allowance
# is 5 minutes and each proof gets 2.
COLD_MIN=${OVERLAY_PROOF_COLD_MIN:-5}
PROOF_MIN=${OVERLAY_PROOF_PROOF_MIN:-2}

die() { echo "overlay-proof.sh: $*" >&2; exit 2; }

# helper <subcommand> args...: manifest parsing, validation, bundle packing.
helper() {
python3 -I - "$@" <<'PYEOF'
import json, os, re, shutil, sys

NAME = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]*$')
PKG = re.compile(r'^\./[A-Za-z0-9_./-]+$')
TAGS = re.compile(r'^[A-Za-z0-9_,]*$')
SHA = re.compile(r'^[0-9a-f]{40}$')
FILES = '.proofs/files/'


def bad(msg):
    print(f"overlay-proof.sh: {msg}", file=sys.stderr)
    sys.exit(2)


def safe(p):
    return p and not p.startswith('/') and '..' not in p.split('/') and '\t' not in p and '\n' not in p


def load(bundle):
    try:
        with open(os.path.join(bundle, '.proofs/manifest.json')) as f:
            m = json.load(f)
    except (OSError, ValueError) as e:
        bad(f"cannot read {bundle}/.proofs/manifest.json: {e}")
    if not isinstance(m, dict) or not SHA.match(str(m.get('keyed_sha', ''))):
        bad("manifest keyed_sha is not 40 lowercase hex")
    proofs = m.get('proofs')
    if not isinstance(proofs, list) or not proofs:
        bad("manifest has no proofs")
    seen = set()
    for p in proofs:
        name = str(p.get('name', ''))
        if not NAME.match(name) or name in seen:
            bad(f"proof name {name!r} is invalid or repeated")
        seen.add(name)
        if not PKG.match(str(p.get('package', ''))) or '..' in p['package'].split('/'):
            bad(f"{name}: invalid package")
        if not TAGS.match(str(p.get('tags', ''))):
            bad(f"{name}: invalid tags")
        run, want = p.get('run'), p.get('want_fail')
        if not isinstance(run, str) or not run or run != run.strip() or '\t' in run or '\n' in run:
            bad(f"{name}: invalid run regex")
        if not isinstance(want, str) or not want.startswith('--- FAIL: ') or '\n' in want or '\t' in want or want != want.rstrip():
            bad(f"{name}: want_fail must be one '--- FAIL: <Name>' line")
        files = p.get('files')
        if not isinstance(files, dict) or not files:
            bad(f"{name}: no files")
        for repo, b in files.items():
            if not safe(repo) or not safe(b) or not b.startswith(FILES) or repo.startswith('.git/'):
                bad(f"{name}: unsafe path {repo!r} -> {b!r}")
            if not os.path.isfile(os.path.join(bundle, b)):
                bad(f"{name}: bundle file {b} is missing")
    return m


cmd, args = sys.argv[1], sys.argv[2:]
if cmd == 'keyed':
    print(load(args[0])['keyed_sha'])
elif cmd == 'emit':  # P/F lines for bash; "-" stands for empty tags
    bundle, only = args[0], args[1:]
    m = load(bundle)
    names = {p['name'] for p in m['proofs']}
    for o in only:
        if o not in names:
            bad(f"--only {o}: no such proof")
    for p in m['proofs']:
        if only and p['name'] not in only:
            continue
        print('\t'.join(['P', p['name'], p['package'], p['run'], p.get('tags') or '-', p['want_fail']]))
        for repo, b in p['files'].items():
            print('\t'.join(['F', repo, b]))
elif cmd == 'plan':
    bundle, sha, cold, per = args[0], args[1], int(args[2]), int(args[3])
    m = load(bundle)
    if m['keyed_sha'] != sha:
        bad(f"manifest keyed_sha {m['keyed_sha']} is not {sha}")
    groups = {}
    for p in m['proofs']:
        groups.setdefault(p['package'], []).append(p['name'])
    inc = [{'package': k, 'only': ' '.join(v), 'n': len(v), 'timeout': max(10, 2 * (cold + per * len(v)))}
           for k, v in sorted(groups.items())]
    print(json.dumps({'include': inc}, separators=(',', ':')))
elif cmd == 'pack':
    lane, sha, spec, out = args
    if not SHA.match(sha):
        bad("keyed_sha must be 40 lowercase hex")
    if os.path.exists(os.path.join(out, '.proofs')):
        bad(f"{out}/.proofs exists; pack into a new directory")
    try:
        lines = [l.rstrip('\n').split('\t') for l in open(spec) if l.strip() and not l.startswith('#')]
    except OSError as e:
        bad(f"cannot read the spec: {e}")
    proofs = []
    for cols in lines:
        if len(cols) < 3:
            bad(f"spec line needs name, run, want_fail: {cols}")
        name, run, want = cols[:3]
        tags = cols[3] if len(cols) > 3 else ''
        src = next((c for c in (os.path.join(lane, 'overlays', name, 'overlay.json'),
                                os.path.join(lane, 'revert', name + '.json')) if os.path.isfile(c)), None)
        if not src:
            bad(f"{name}: no overlays/{name}/overlay.json or revert/{name}.json under {lane}")
        files = {}
        for target, repl in sorted(json.load(open(src))['Replace'].items()):
            repo = re.sub(r'^.*?/wt-[^/]+/', '', target, count=1)
            if repo == target:
                bad(f"{name}: {target} is not under a wt-* worktree")
            dest = FILES + name + '/' + repo
            os.makedirs(os.path.dirname(os.path.join(out, dest)), exist_ok=True)
            shutil.copyfile(repl, os.path.join(out, dest))
            files[repo] = dest
        pkg = cols[4] if len(cols) > 4 else './' + os.path.dirname(next(iter(files)))
        proofs.append({'name': name, 'package': pkg, 'run': run, 'tags': tags, 'files': files, 'want_fail': want})
    with open(os.path.join(out, '.proofs/manifest.json'), 'w') as f:
        json.dump({'keyed_sha': sha, 'proofs': proofs}, f, indent=1)
        f.write('\n')
    load(out)
    print(f"packed {len(proofs)} proofs into {out}/.proofs")
else:
    bad(f"unknown helper command {cmd}")
PYEOF
}

# prove_one <src> <bundle> <name> <package> <run> <tags> <want> <repo>... -- <bundlefile>...
# Prints the PROOF line; returns 0 on PASS, 1 on FAIL.
prove_one() {
  local src=$1 bundle=$2 name=$3 pkg=$4 run=$5 tags=$6 want=$7
  shift 7
  local repos=() bfiles=() i
  while [ "$1" != -- ]; do repos+=("$1"); shift; done
  shift
  bfiles=("$@")
  local out rc='-' reason=ok t0=$SECONDS
  out=$(mktemp)

  for i in "${!repos[@]}"; do
    mkdir -p "$src/$(dirname "${repos[$i]}")"
    cp "$bundle/${bfiles[$i]}" "$src/${repos[$i]}"
  done
  local got exp
  got=$(git -C "$src" diff --name-only | LC_ALL=C sort)
  exp=$(printf '%s\n' "${repos[@]}" | LC_ALL=C sort)
  if [ -z "$got" ] || [ "$got" != "$exp" ]; then
    reason="changed files differ from the manifest: git diff has [$(tr '\n' ' ' <<<"$got")]"
  else
    rc=0
    (cd "$src" && go test -count=1 ${tags:+-tags "$tags"} -run "$run" "$pkg") >"$out" 2>&1 || rc=$?
    local norm wl pl
    norm=$(sed -E -e 's/^[[:space:]]+//' -e 's/ \([0-9.]+s\)$//' "$out")
    wl=$(grep -Fxn -- "$want" <<<"$norm" | head -n 1 | cut -d: -f1 || true)
    pl=$(grep -n '^panic:' <<<"$norm" | head -n 1 | cut -d: -f1 || true)
    if grep -Fq '[build failed]' "$out"; then reason="build failed"
    elif grep -Fq '[setup failed]' "$out"; then reason="setup failed"
    elif [ "$rc" = 0 ]; then reason="overlay green"
    elif [ "$rc" != 1 ]; then reason="rc $rc, want 1"
    elif [ -n "$pl" ] && { [ -z "$wl" ] || [ "$pl" -lt "$wl" ]; }; then reason="panic before the FAIL"
    elif [ -z "$wl" ]; then reason="no output line equals the want_fail"
    fi
    grep -E '^[[:space:]]*(--- FAIL|FAIL|ok|panic:)|\[(build|setup) failed\]' "$out" | head -n 20 | sed 's/^/  | /'
    [ "$reason" = ok ] || { echo "  | tail of the go test output:"; tail -n 25 "$out" | sed 's/^/  | /'; }
  fi
  rm -f "$out"

  git -C "$src" checkout -q -- .
  git -C "$src" clean -fdq
  if [ -n "$(git -C "$src" status --porcelain --untracked-files=all)" ]; then
    reason="$reason; the tree is not clean after the proof"
    echo "PROOF $name rc=$rc want=$want result=FAIL reason=$reason"
    return 3
  fi
  local result=PASS
  [ "$reason" = ok ] || result=FAIL
  echo "PROOF $name rc=$rc want=$want result=$result reason=$reason"
  echo "TIME $name $((SECONDS - t0))s"
  [ "$result" = PASS ]
}

cmd_run() {
  [ $# -ge 2 ] || die "usage: run <bundle-dir> <checkout-dir> [--only <name>...]"
  local bundle=$1 src=$2 only=()
  shift 2
  if [ $# -gt 0 ]; then
    if [ "$1" != --only ] || [ $# -lt 2 ]; then die "usage: run <bundle-dir> <checkout-dir> [--only <name>...]"; fi
    shift
    only=("$@")
  fi
  local rows keyed head
  rows=$(helper emit "$bundle" "${only[@]}")
  keyed=$(helper keyed "$bundle")
  head=$(git -C "$src" rev-parse HEAD 2>/dev/null) || die "$src is not a git checkout"
  [ "$head" = "$keyed" ] || die "checkout is at $head, the manifest is keyed to $keyed"
  [ -z "$(git -C "$src" status --porcelain --untracked-files=all)" ] || die "$src is not clean"

  local failed=0 n=0 kind a b c d e
  local name='' pkg='' run='' tags='' want='' repos=() bfiles=()
  flush() {
    [ -n "$name" ] || return 0
    local rcp=0
    prove_one "$src" "$bundle" "$name" "$pkg" "$run" "$tags" "$want" "${repos[@]}" -- "${bfiles[@]}" || rcp=$?
    n=$((n + 1))
    [ "$rcp" = 0 ] || failed=1
    [ "$rcp" != 3 ] || { echo "overlay-proof.sh: stopping, the tree could not be restored" >&2; exit 1; }
  }
  while IFS=$'\t' read -r kind a b c d e; do
    if [ "$kind" = P ]; then
      flush
      name=$a pkg=$b run=$c tags=$d want=$e repos=() bfiles=()
      [ "$tags" != - ] || tags=
    else
      repos+=("$a")
      bfiles+=("$b")
    fi
  done <<<"$rows"
  flush
  echo "overlay-proof.sh: $n proofs, $([ "$failed" = 0 ] && echo all PASS || echo at least one FAIL)"
  exit "$failed"
}

main() {
  local sub=${1:-}
  [ $# -eq 0 ] || shift
  case "$sub" in
    pack)
      [ $# -ge 2 ] || die "usage: pack <lane-dir> <keyed_sha> [--spec FILE] [--out DIR]"
      local lane=$1 sha=$2 spec out=./overlay-bundle
      shift 2
      spec=$lane/proofs.tsv
      while [ $# -gt 0 ]; do
        case "$1" in
          --spec) spec=${2:?--spec needs a file}; shift 2 ;;
          --out) out=${2:?--out needs a directory}; shift 2 ;;
          *) die "pack: unknown argument $1" ;;
        esac
      done
      helper pack "$lane" "$sha" "$spec" "$out"
      ;;
    plan)
      [ $# -eq 2 ] || die "usage: plan <bundle-dir> <keyed_sha>"
      helper plan "$1" "$2" "$COLD_MIN" "$PROOF_MIN"
      ;;
    run) cmd_run "$@" ;;
    *) die "usage: overlay-proof.sh pack|plan|run ... (see the header)" ;;
  esac
}
main "$@"
