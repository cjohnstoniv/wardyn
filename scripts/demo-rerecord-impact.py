#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Which demo episodes does a diff put back in front of the camera?

  scripts/demo-rerecord-impact.py impact [--base REF]   # episodes to re-record since REF (default hardening-base)
  scripts/demo-rerecord-impact.py check 02              # pre-take gate: every label the spec asserts exists in ui/src

(`--check 02` and a bare `--base REF` are the older spellings and still work;
scripts/take-chain.sh calls `check <id>` before every take.)

An EPISODE is any id with a browser spec (ui/e2e/demo/*.spec.ts) or a terminal
beats lane (scripts/demo-beats/*.sh) — 13 is terminal-only and has no spec, so
seeding the set from the specs alone made it invisible to this tool.

Four legs per episode:

  (a) the episode's own words changed — anything in the spec's ui/e2e/demo/
      import closure (the spec, overlay.ts, stage.ts, funnel.ts, demos.ts,
      narrator.ts, task.ts, sweep.ts, 01's assets/primer.html), its
      scripts/demo-beats lane, or scripts/demo-typist.sh (the terminal
      narrator every beats lane sources);
  (b) a route the episode is filmed on changed — page.goto() AND the
      toHaveURL/waitForURL receipts, the spec's own and its helpers' (openEpisode
      and openDemo navigate for FIVE episodes, and that navigation is not in the
      spec file at all), literal or `${origin}/path` (11 films a SECOND stack
      and spells every URL that way) — matched against App.tsx's <Route> table
      and then the screen module's whole transitive import closure, because
      on-camera strings live in shared files (live-approvals.tsx, app-shell.tsx,
      new-run-rail.tsx);
  (c) the app SHELL changed — every episode renders inside it, so this leg fires
      for any episode whose closure includes stage.ts/overlay.ts (all of them),
      not just the ones with a literal goto;
  (d) an asserted label (getByRole name / getByText / getByLabel) lives in a
      changed file, or is gone from ui/src altogether — a hard break, which
      `check` refuses to roll on.

Backend-only, test-only, grader-only and catalog-only changes never trigger.
"""
import argparse, glob, os, pathlib, re, subprocess, sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
UI = ROOT / "ui/src"
APP = UI / "app/App.tsx"
E2E = ROOT / "ui/e2e/demo"
BEATS = ROOT / "scripts/demo-beats"
TYPIST = "scripts/demo-typist.sh"   # sourced by every beats lane
DEFAULT_BASE = "hardening-base"
STR = r'"((?:[^"\\]|\\.)*)"'
LBL = re.compile(r'(?:name:\s*|getByText\(\s*|getByLabel\(\s*)' + STR)
# Both quotings. A backtick template is cut at its first ${…} — `/runs/${id}`
# arrives as "/runs/", and route_for() reads that trailing slash as the :id
# segment. Cutting at ? and # drops query/hash the same way it always did. ONE
# leading ${origin} is stepped over first: 11 films a SECOND stack and spells
# every URL `${CI_STACK}/runs…`, so cutting at the ${ left it an empty path and
# no visited route at all.
GOTO = re.compile(r'page\.goto\(\s*["`](?:\$\{\w+\})?([^"`?#$]*)')
# The OTHER receipt that an episode is on a route: it navigates by clicking and
# then asserts where it landed. Two spellings, one capture group each — a regex
# literal, whose literal prefix url_prefix() reads, and the `${origin}/path`
# template (11's `${CI_STACK}/runs/${run.id}`), cut exactly as GOTO cuts one.
URLPAT = re.compile(
    r'(?:toHaveURL|waitForURL)\(\s*(?:'
    r'/((?:[^/\\\n]|\\.)+)/'      # /\/runs\/[0-9a-f-]{8,}/i
    r'|`\$\{\w+\}([^`?#$]*)'      # `${CI_STACK}/runs/${run.id}`
    r')'
)
IMPORT = re.compile(r'(?:from\s*|import\()\s*"((?:\.{1,2}|@)/[^"]+)"')
STATIC_IMPORT = re.compile(r'from\s*"((?:\.{1,2}|@)/[^"]+)"')
# The e2e side also drives non-code assets it never imports: 01 loads its deck
# with new URL("./assets/primer.html", import.meta.url).
E2E_IMPORT = re.compile(r'(?:from\s*|import\(|new URL\()\s*["`]((?:\.{1,2}|@)/[^"`]+)["`]')
# Asserted labels ui/src never spells verbatim, and what to grade them on
# instead. A composed label keeps the FIXED FRAGMENT of its template, so renaming
# the template still refuses the take — a bare skip meant `exit {exitCode}` could
# become anything and 11 would still roll. "" is the only free pass, for a string
# the DATA supplies (a principal, a speed radio, an audit action id) that ui/src
# has no receipt for at all.
ALLOW = {
    "2x speed": "", "4x speed": "", "authz.denied": "", "member@wardyn.local": "",
    # Composed at render time — `Allowed hosts · ${rows.length}`, `exit {exitCode}`,
    # `Promoted — ${n} still need(s) approval`, `${n} domain(s) allowed` — or
    # sentence-cased out of a lowercase verb table (audit.tsx's ACTION_VERB says
    # "injected the subscription credential at the proxy"). All five used to pass
    # this gate only because a *.test.tsx fixture spelled them out in full, which
    # grep_ui no longer reads.
    "Allowed hosts · 1": "Allowed hosts ·",
    "exit 0": "exit {exitCode}",
    "Promoted — 1 still needs approval": "} still need",
    "Injected the subscription credential at the proxy": "injected the subscription credential",
    "1 domain allowed": "} domain",
}


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


def e2e_closure(spec: pathlib.Path):
    """The spec plus every ui/e2e/demo/ helper and asset it reaches."""
    return {f for f in closure(spec, E2E_IMPORT) if f.startswith("ui/e2e/demo/")}


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
    # "/" is FirstRunLanding, defined in App.tsx itself (a <Navigate> to /runs or
    # /setup); the alternation receipts (/\/(runs|setup)/) and page.goto("/")
    # resolve here instead of being dropped.
    routes.setdefault("/", APP)
    return routes


def shell_closure():
    """What App.tsx reaches through STATIC imports only (app-shell, sign-in, shared
    primitives, lib/*): the frame every route renders inside. Lazy import() screens
    are excluded - they are matched per visited route instead."""
    return closure(APP, STATIC_IMPORT)


def url_prefix(rx: str) -> str:
    r"""The literal path prefix of a URL-assertion regex: \/runs\/[0-9a-f-]{8,} -> /runs/."""
    out: list[str] = []
    i = 0
    while i < len(rx):
        c = rx[i]
        if c == "\\":
            i += 1
            if i < len(rx):
                n = rx[i]
                if n in "?#":        # escaped query/hash — the path ended, as GOTO reads it
                    return "".join(out)
                if n in "dwsbDWSB":  # \d \w \s \b: a class or an anchor, never a literal
                    break
                out.append(n)
                i += 1
            continue
        if c in "?*+{":       # a quantifier makes the char BEFORE it optional
            return "".join(out[:-1])
        if c in "[(|^$.":
            break
        out.append(c)
        i += 1
    return "".join(out)


_NAV: dict[str, set[str]] = {}


def navs(rel: str) -> set[str]:
    """Every route path this ONE file navigates to or asserts. Cached: the shared
    helpers are in nineteen closures and their gotos are read once."""
    if rel not in _NAV:
        p = ROOT / rel
        src = p.read_text(errors="ignore") if p.is_file() else ""
        found = set(GOTO.findall(src))
        for rx, tpl in URLPAT.findall(src):
            found.add(url_prefix(rx) if rx else tpl)
        _NAV[rel] = {g for g in found if g.startswith("/")}
    return _NAV[rel]


def route_for(goto: str, routes):
    if goto in routes:
        return routes[goto]
    # A template literal cut at its ${…} ends in "/" — read the missing tail as
    # one path segment so /runs/ finds /runs/:id.
    for g in (goto, goto + "_") if goto.endswith("/") else (goto,):
        for path, mod in routes.items():           # /runs/:id style
            pat = "^" + re.sub(r":\w+", r"[^/]+", path) + "$"
            if re.match(pat, g):
                return mod
    return None


def labels_of(src: str):
    return {m for m in LBL.findall(src) if len(m) >= 3}


def needle(label: str):
    """What ui/src must still contain for this asserted label: the label itself,
    or a composed one's fixed fragment. None = the data supplies it and there is
    nothing in ui/src to look for."""
    return ALLOW.get(label, label) or None


def grep_ui(label: str) -> bool:
    n = needle(label)
    if n is None:
        return True
    # --exclude the unit tests: a label kept alive only by a *.test.tsx fixture
    # is gone from the product, and the camera would film its absence.
    return subprocess.run(["grep", "-rqF", "--exclude=*.test.*", "--", n, str(UI)]).returncode == 0


def episodes() -> dict[str, list[pathlib.Path]]:
    """id -> its browser spec(s). Seeded from the beats lanes TOO, so a
    terminal-only episode (13) is an episode here and not a silent hole."""
    eps: dict[str, list[pathlib.Path]] = {}
    for b in sorted(glob.glob(str(BEATS / "*.sh"))):
        eps.setdefault(pathlib.Path(b).name.split("-")[0], [])
    for s in sorted(glob.glob(str(E2E / "*.spec.ts"))):
        eps.setdefault(pathlib.Path(s).name[: -len(".spec.ts")].split("-")[0], []).append(pathlib.Path(s))
    return eps


def beats_of(eid: str) -> list[str]:
    return [os.path.relpath(b, ROOT) for b in sorted(glob.glob(str(BEATS / f"{eid}-*.sh")))]


def check(eid: str) -> int:
    eps = episodes()
    if eid not in eps:
        print(f"{eid}: unknown episode — no ui/e2e/demo/{eid}-*.spec.ts and no scripts/demo-beats/{eid}-*.sh", file=sys.stderr)
        return 2
    specs = eps[eid]
    if not specs:
        # 13 films a terminal, not a browser: there are no asserted labels to gate.
        print(f"{eid}\tok: no spec (terminal-only) — {', '.join(beats_of(eid)) or 'beats lane only'}")
        return 0
    if len(specs) != 1:
        print(f"{eid}: expected one spec, found {len(specs)}", file=sys.stderr)
        return 2
    missing = sorted(l for l in labels_of(specs[0].read_text()) if not grep_ui(l))
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
    canon = sorted(f for f in ui if f.startswith("ui/src/app/lib/") and f.endswith("-copy.ts"))
    out = {}
    for eid, specs in sorted(episodes().items()):
        why = set()
        beats = beats_of(eid)
        if any(b in changed for b in beats):
            why.add("terminal beats edited")
        if beats and TYPIST in changed:
            why.add(f"{TYPIST} edited — the terminal narrator every beats lane sources")
        for spec in specs:
            src = spec.read_text()
            clos = e2e_closure(spec)
            edited = sorted(clos & changed)
            if edited:
                why.add(f"spec/narration edited: {', '.join(edited[:3])}{'…' if len(edited) > 3 else ''}")
            # The shell frames every episode, filmed goto or not.
            if clos & {"ui/e2e/demo/stage.ts", "ui/e2e/demo/overlay.ts"} and (shell & ui):
                why.add(f"app shell changed: {', '.join(sorted(shell & ui)[:3])}")
            gotos = set().union(*(navs(f) for f in clos)) if clos else set()
            for r in sorted(gotos):
                mod = route_for(r, routes)
                if mod is None:
                    continue
                hit = closure(mod) & ui
                if hit:
                    why.add(f"visits {r}: changed {', '.join(sorted(hit)[:3])}{'…' if len(hit) > 3 else ''}")
            for lab in labels_of(src):
                if not grep_ui(lab):
                    why.add(f"asserted label GONE from ui/src: {lab!r}")
                    continue
                n = needle(lab)
                if n is None:
                    continue
                # Word-bounded: unanchored, the asserted label 'Name' matched
                # every className in the hunk and sent five episodes back to the
                # camera for a styling change. grep_ui stays a substring test —
                # there the question is presence, not identity.
                pat = re.compile(r'(?<!\w)' + re.escape(n) + r'(?!\w)')
                for f, t in txt.items():
                    if pat.search(t):
                        why.add(f"on-camera label {lab!r} is in the diff of {f}")
            if canon and gotos:
                why.add(f"a canon copy module changed: {', '.join(canon[:3])}")
        if why:
            out[eid] = why
    for eid in sorted(out):
        print(eid + "\t" + "; ".join(sorted(out[eid])))
    print(f"# {len(out)} episode(s) to re-record since {base}", file=sys.stderr)
    return 0


def main(argv: list[str]) -> int:
    # The old spellings, kept because take-chain.sh, the Makefile and a year of
    # notes all use them: `--check <id>` and a bare `--base <ref>`.
    if argv[:1] == ["--check"]:
        argv = ["check", *argv[1:]]
    elif argv[:1] == ["--base"]:
        argv = ["impact", *argv]
    elif not argv:
        argv = ["impact"]

    ap = argparse.ArgumentParser(
        description="Which demo episodes does a diff put back in front of the camera?",
        epilog=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("check", help="pre-take label gate for ONE episode (rc 1 = refuse the take, rc 2 = no such episode)")
    c.add_argument("id", help="episode id, e.g. 02, 03a, 13, walkthrough")
    i = sub.add_parser("impact", help="the episodes a diff puts back in front of the camera")
    i.add_argument("--base", default=DEFAULT_BASE, help=f"git ref to diff against (default {DEFAULT_BASE})")
    a = ap.parse_args(argv)
    if a.cmd == "check":
        # take-chain.sh reads rc 1 as REFUSE, and an uncaught exception exits 1
        # too — an unreadable spec would block a take that is probably fine. rc 2
        # is the honest answer: could not judge.
        try:
            return check(a.id)
        except Exception as e:
            print(f"{a.id}: could not judge — {type(e).__name__}: {e}", file=sys.stderr)
            return 2
    return impact(a.base)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
