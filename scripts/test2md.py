#!/usr/bin/env python3
"""Convert a `go test -json` event stream into a detailed Markdown report.

Reads test2json events on stdin and writes a human-readable report with one row
per test case: name, package, result, duration, and (on failure) the captured
output as the "reason". This is the detailed reporting the Wardyn test plan
requires (descriptions, results, reasons, timing) and needs no external tools.

Usage:
    go test -json ./... | python3 scripts/test2md.py --title "Go unit tests" \
        --out test/reports/go/unit/report.md

Exit code mirrors the test run: non-zero if any test failed.
"""
import argparse
import collections
import json
import sys


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--title", default="Go tests")
    ap.add_argument("--out", required=True, help="markdown report path")
    args = ap.parse_args()

    # key = (package, test-or-None); value = dict(status, elapsed, output[])
    tests = collections.OrderedDict()
    pkg_status = {}
    output_buf = collections.defaultdict(list)

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue  # non-JSON noise (e.g. build output) — skip
        action = ev.get("Action")
        pkg = ev.get("Package", "")
        test = ev.get("Test")  # None for package-level events
        key = (pkg, test)

        if action == "output":
            output_buf[key].append(ev.get("Output", ""))
        elif action in ("pass", "fail", "skip"):
            if test is None:
                pkg_status[pkg] = action
            else:
                tests[key] = {
                    "package": pkg,
                    "test": test,
                    "status": action,
                    "elapsed": ev.get("Elapsed", 0.0),
                    "output": "".join(output_buf.get(key, [])),
                }

    # Tally.
    counts = collections.Counter(t["status"] for t in tests.values())
    total = sum(counts.values())
    passed, failed, skipped = counts.get("pass", 0), counts.get("fail", 0), counts.get("skip", 0)

    # --- Markdown ---
    lines = []
    lines.append(f"# {args.title}")
    lines.append("")
    lines.append(f"**{passed} passed · {failed} failed · {skipped} skipped** "
                 f"({total} test cases across {len(pkg_status)} packages)")
    lines.append("")

    if failed:
        lines.append("## ❌ Failures")
        lines.append("")
        for t in tests.values():
            if t["status"] != "fail":
                continue
            lines.append(f"### `{t['package']}` — `{t['test']}` ({t['elapsed']:.3f}s)")
            lines.append("")
            reason = t["output"].strip() or "(no output captured)"
            lines.append("```")
            lines.append(reason)
            lines.append("```")
            lines.append("")

    lines.append("## All test cases")
    lines.append("")
    lines.append("| Package | Test | Result | Duration |")
    lines.append("|---|---|---|---|")
    badge = {"pass": "✅ pass", "fail": "❌ fail", "skip": "⊘ skip"}
    for t in tests.values():
        short_pkg = t["package"].replace("github.com/cjohnstoniv/wardyn/", "")
        lines.append(f"| {short_pkg} | {t['test']} | {badge.get(t['status'], t['status'])} "
                     f"| {t['elapsed']:.3f}s |")
    lines.append("")

    # Packages with no test cases (build-only / no tests).
    empty = [p for p, s in pkg_status.items() if not any(k[0] == p for k in tests)]
    if empty:
        lines.append("## Packages with no test cases")
        lines.append("")
        for p in sorted(empty):
            short = p.replace("github.com/cjohnstoniv/wardyn/", "")
            lines.append(f"- {short} ({pkg_status[p]})")
        lines.append("")

    with open(args.out, "w") as f:
        f.write("\n".join(lines))

    print(f"[test2md] {args.title}: {passed} passed, {failed} failed, {skipped} skipped "
          f"-> {args.out}", file=sys.stderr)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
