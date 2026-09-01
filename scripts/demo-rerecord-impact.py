#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Which demo episodes does a diff put back in front of the camera?

  scripts/demo-rerecord-impact.py [--base REF]      # episodes to re-record since REF (default hardening-base)
  scripts/demo-rerecord-impact.py --check 02        # pre-take gate: every label the spec asserts exists in ui/src

Three legs per spec under ui/e2e/demo/: (a) the spec or its scripts/demo-beats
lane changed; (b) a page.goto() route's screen module — or ANYTHING in that
module's transitive import closure — changed (on-camera strings live in shared
files: live-approvals.tsx, app-shell.tsx, new-run-rail.tsx); (c) an asserted
label (getByRole name / getByText / getByLabel) lives in a changed file, or is
gone from ui/src altogether (a hard break; --check refuses to roll on it).
Backend-only, test-only, grader-only and catalog-only changes never trigger.
"""
import glob, os, pathlib, re, subprocess, sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
UI = ROOT / "ui/src"
APP = UI / "app/App.tsx"
STR = r'"((?:[^"\\]|\\.)*)"'
LBL = re.compile(r'(?:name:\s*|getByText\(\s*|getByLabel\(\s*)' + STR)
GOTO = re.compile(r'page\.goto\(\s*"([^"?#]*)')
IMPORT = re.compile(r'(?:from\s*|import\()\s*"((?:\.{1,2}|@)/[^"]+)"')
STATIC_IMPORT = re.compile(r'from\s*"((?:\.{1,2}|@)/[^"]+)"')
# Data/dynamic labels that never exist verbatim in ui/src.
ALLOW = {"1 domain allowed", "2x speed", "4x speed", "authz.denied", "member@wardyn.local"}


def resolve(spec_from: pathlib.Path, target: str):
    base = (UI / target[2:]) if target.startswith("@/") else (spec_from.parent / target)
    for cand in (base, *(base.with_suffix(e) for e in (".ts", ".tsx")), base / "index.ts", base / "index.tsx"):
        if cand.is_file():
            return cand.resolve()
    return None


def closure(entry: pathlib.Path, pattern=IMPORT):
    seen, todo = set(), [entry]
    while todo:
        f = todo.pop()
        if f in seen or not f.is_file():
            continue
        seen.add(f)
        for t in pattern.findall(f.read_text(errors="ignore")):
            r = resolve(f, t)
            if r and r not in seen:
                todo.append(r)
    return {str(p.relative_to(ROOT)) for p in seen}


def route_modules():
    """route path -> screen module file, via App.tsx's lazy/static imports + <Route> table."""
    app = APP.read_text()
    mods = {}
    for name, mod in re.findall(r'const (\w+) = React\.lazy\(\(\) =>\s*import\("(\./[^"]+)"\)', app, re.S):
        mods[name] = resolve(APP, mod)
    for names, mod in re.findall(r'import\s*\{([^}]+)\}\s*from\s*"(\./[^"]+)"', app):
        for n in names.split(","):
            mods.setdefault(n.strip().split(" as ")[-1], resolve(APP, mod))
    routes = {}
    for path, body in re.findall(r'<Route\s+path="([^"]+)"([\s\S]*?)(?=<Route\s|</Routes>)', app):
        for comp in re.findall(r'<(?!React\.)([A-Z]\w+)', body):
            if comp in mods and mods[comp]:
                routes[path] = mods[comp]
                break
    return routes


def shell_closure():
    """What App.tsx reaches through STATIC imports only (app-shell, sign-in, shared
    primitives, lib/*): the frame every route renders inside. Lazy import() screens
    are excluded - they are matched per visited route instead."""
    return closure(APP, STATIC_IMPORT)


def route_for(goto: str, routes):
    if goto in routes:
        return routes[goto]
    for path, mod in routes.items():           # /runs/:id style
        pat = "^" + re.sub(r":\w+", r"[^/]+", path) + "$"
        if re.match(pat, goto):
            return mod
    return None


def labels_of(src: str):
    return {m for m in LBL.findall(src) if len(m) >= 3} - ALLOW


def grep_ui(label: str) -> bool:
    return subprocess.run(["grep", "-rqF", "--", label, str(UI)]).returncode == 0


def check(eid: str) -> int:
    specs = glob.glob(str(ROOT / f"ui/e2e/demo/{eid}-*.spec.ts")) or glob.glob(str(ROOT / f"ui/e2e/demo/{eid}.spec.ts"))
    if len(specs) != 1:
        print(f"{eid}: expected one spec, found {len(specs)}", file=sys.stderr)
        return 2
    missing = sorted(l for l in labels_of(pathlib.Path(specs[0]).read_text()) if not grep_ui(l))
    for l in missing:
        print(f"{eid}\tLABEL GONE from ui/src: {l!r}")
    print(f"{eid}\t{'REFUSE' if missing else 'ok'}: {len(missing)} asserted label(s) missing")
    return 1 if missing else 0


def impact(base: str) -> int:
    changed = subprocess.run(["git", "-C", str(ROOT), "diff", "--name-only", base, "HEAD"], capture_output=True, text=True, check=True).stdout.split()
    changed += subprocess.run(["git", "-C", str(ROOT), "diff", "--name-only", "HEAD"], capture_output=True, text=True).stdout.split()  # uncommitted too
    changed = set(changed)
    ui = {f for f in changed if f.startswith("ui/src/") and ".test." not in f}
    def hunks(f):
        d = subprocess.run(["git", "-C", str(ROOT), "diff", "-U0", base, "--", f], capture_output=True, text=True).stdout
        return "\n".join(l[1:] for l in d.splitlines() if l[:1] in "+-" and not l.startswith(("+++", "---")))
    txt = {f: hunks(f) for f in ui}
    routes = route_modules()
    shell = shell_closure()
    out = {}
    for spec in sorted(glob.glob(str(ROOT / "ui/e2e/demo/*.spec.ts"))):
        rel = os.path.relpath(spec, ROOT)
        eid = pathlib.Path(spec).name[: -len(".spec.ts")].split("-")[0]
        src = pathlib.Path(spec).read_text()
        why = set()
        if rel in changed:
            why.add("spec/narration edited")
        if any(os.path.relpath(b, ROOT) in changed for b in glob.glob(str(ROOT / f"scripts/demo-beats/{eid}-*.sh"))):
            why.add("terminal beats edited")
        gotos = set(GOTO.findall(src))
        if gotos and (shell & ui):
            why.add(f"app shell changed: {', '.join(sorted(shell & ui)[:3])}")
        for r in gotos:
            mod = route_for(r, routes)
            if mod is None:
                continue
            hit = closure(mod) & ui
            if hit:
                why.add(f"visits {r}: changed {', '.join(sorted(hit)[:3])}{'…' if len(hit) > 3 else ''}")
        for lab in labels_of(src):
            if not grep_ui(lab):
                why.add(f"asserted label GONE from ui/src: {lab!r}")
            else:
                for f, t in txt.items():
                    if lab in t:
                        why.add(f"on-camera label {lab!r} is in the diff of {f}")
        if any(f.startswith("ui/src/app/lib/") and "copy" in f for f in ui) and GOTO.search(src):
            why.add("a canon copy module changed")
        if why:
            out[eid] = why
    for eid in sorted(out):
        print(eid + "\t" + "; ".join(sorted(out[eid])))
    print(f"# {len(out)} episode(s) to re-record since {base}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    a = sys.argv[1:]
    if a[:1] == ["--check"]:
        sys.exit(check(a[1]))
    sys.exit(impact(a[1] if a[:1] == ["--base"] else "hardening-base"))
