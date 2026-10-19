#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-script-hazards.sh — two shell patterns that turn a pass into a flake.
#
#   1. `producer | grep -q pat` in a script that sets pipefail. grep -q exits on
#      the first match, the producer is then killed by SIGPIPE (exit 141), and
#      pipefail reports the whole pipeline as failed: a match becomes a
#      failure, only when the producer still has output to write. Capture the
#      output, then match it (`out="$(cmd)"; grep -q pat <<<"$out"`), or read
#      everything (`| grep pat >/dev/null`).
#   2. A bare `sleep <n>` in a conformance script. A fixed wait either wastes
#      the budget or is too short on a slow runner; wait on the condition in a
#      bounded loop (for/while/until ... sleep) instead.
#
# Heredoc bodies and comments are not scanned. `--selftest` plants a violation
# of each rule and proves the check refuses it.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v python3 >/dev/null 2>&1 || { echo "check-script-hazards.sh: python3 not found" >&2; exit 1; }

python3 - "$@" <<'PYEOF'
import re
import subprocess
import sys

PIPEFAIL = re.compile(r'^\s*set\s+[^#\n]*pipefail')
GREP_Q = re.compile(
    r'(?<!\|)\|\s*(?:command\s+|/usr/bin/)?[ef]?grep\s+(?:-[A-Za-z0-9-]+\s+)*?'
    r'(?:-[A-Za-z]*q[A-Za-z]*|--quiet|--silent)(?=\s|$)')
HEREDOC = re.compile(r'(?<!<)<<-?\s*([\'"]?)([A-Za-z_][A-Za-z0-9_]*)\1')
LOOP_OPEN = re.compile(r'(?:^|[;&|({"\'`]|\b(?:then|do|else)\b)\s*(?:for|while|until)\s')
LOOP_CLOSE = re.compile(r'(?:^|[;&|(){}"\'`\s])done(?=\s|$|[;&|)"\'`])')
SLEEP = re.compile(r'(?<![\w-])sleep\s+[0-9]')
CONFORMANCE = re.compile(r'(^|/)[^/]*conformance[^/]*\.sh$')


def code_lines(text):
    """(lineno, line) for each line outside heredoc bodies, comments cut, plus
    the (marker, lineno) of each heredoc still open at the end of the text.
    A `<<WORD` counts only outside quotes and $(( )) arithmetic."""
    lines, pending = [], []
    for no, raw in enumerate(text.splitlines(), 1):
        if pending:
            if raw.strip() == pending[0][0]:
                pending.pop(0)
            continue
        # Open contexts, innermost last: ' or " (quotes), ( for $( ) or a
        # subshell, A for $(( )) arithmetic. Quotes reopen inside $( ).
        line, stack, esc = [], [], False
        for i, ch in enumerate(raw):
            top = stack[-1] if stack else ''
            if esc:
                esc = False
            elif top == "'":
                if ch == "'":
                    stack.pop()
            elif ch == '\\':
                esc = True
            elif ch == '(' and raw[i - 1:i] == '$' and raw[i - 2:i - 1] != '\\':
                stack.append('A' if raw[i + 1:i + 2] == '(' else '(')
            elif top == '"':
                if ch == '"':
                    stack.pop()
            elif ch in '"\'(':
                stack.append(ch)
            elif ch == ')':
                if top in ('(', 'A'):
                    stack.pop()
            elif ch == '#' and (i == 0 or raw[i - 1] in ' \t;'):
                break
            elif ch == '<' and 'A' not in stack and (m := HEREDOC.match(raw, i)):
                pending.append((m.group(2), no))
            line.append(ch)
        lines.append((no, ''.join(line)))
    return lines, pending


def check_text(path, text):
    lines, unclosed = code_lines(text)
    found = [f"{path}:{no}: heredoc `<<{word}` never closes, so the lines after it go unchecked"
             for word, no in unclosed]
    if any(PIPEFAIL.match(line) for _, line in lines):
        for no, line in lines:
            if GREP_Q.search(line):
                found.append(f"{path}:{no}: `| grep -q` under pipefail (SIGPIPE turns a match into a failure): {line.strip()}")
    if CONFORMANCE.search(path):
        depth = 0
        for no, line in lines:
            events = [(m.start(), 'open') for m in LOOP_OPEN.finditer(line)]
            events += [(m.start(), 'close') for m in LOOP_CLOSE.finditer(line)]
            events += [(m.start(), 'sleep') for m in SLEEP.finditer(re.sub(r"'[^']*'", lambda m: ' ' * len(m.group(0)), line))]
            for _, kind in sorted(events):
                if kind == 'open':
                    depth += 1
                elif kind == 'close':
                    depth = max(0, depth - 1)
                elif depth == 0:
                    found.append(f"{path}:{no}: bare `sleep` in a conformance script (wait on the condition in a bounded loop): {line.strip()}")
    return found


def selftest():
    bad = []

    def expect(path, text, want, why):
        got = check_text(path, text)
        if bool(got) != want:
            bad.append(f"{why}: wanted {'a finding' if want else 'none'}, got {got}")

    pf = "set -euo pipefail\n"
    for line in ["ps | grep -q foo", "ps | grep -qE 'a|b'", "ps | grep -iq foo", "ps | grep -x -q foo",
                 "ps |grep --quiet foo", "ps | egrep -q foo", "if ps | grep -qx foo; then :; fi"]:
        expect("scripts/x.sh", pf + line + "\n", True, f"planted `{line}`")
    for line in ["ps | grep foo >/dev/null", "ps || grep -q foo f", "grep -q foo <<<\"$out\"",
                 "# ps | grep -q foo", "ps | grep -c foo", "ps | grep -e foo"]:
        expect("scripts/x.sh", pf + line + "\n", False, f"clean `{line}`")
    expect("scripts/x.sh", pf + "cat <<'EOF'\nps | grep -q foo\nEOF\n", False, "heredoc body")
    expect("scripts/x.sh", pf + "v=\"$(awk '/x\" <<EOF/{print}' f)\"\nps | grep -q foo\n", True, "`<<EOF` inside quotes")
    expect("scripts/x.sh", pf + "x=$((1<<FOO))\nps | grep -q foo\n", True, "`<<` in arithmetic")
    expect("scripts/x.sh", "cat <<EOF\nno end marker\n", True, "heredoc open at end of file")
    expect("scripts/x.sh", "set -eu\nps | grep -q foo\n", False, "no pipefail")
    expect("scripts/x.sh", "set -o pipefail\nps | grep -q foo\n", True, "set -o pipefail")

    conf = "scripts/k-conformance.sh"
    expect(conf, "sleep 5\n", True, "bare sleep")
    expect(conf, "kubectl apply -f x; sleep 2\n", True, "bare sleep after a command")
    for text in ["for i in 1 2; do\n  check && break\n  sleep 1\ndone\n",
                 "until check; do sleep 3; done\n",
                 "while true; do\n  sleep 5\ndone\n",
                 "timeout 9 bash -c \"until check; do sleep 3; done\"\n",
                 "( while true; do sleep 5; done ) &\n"]:
        expect(conf, text, False, f"polled sleep {text!r}")
    expect(conf, "for i in 1; do :; done\nsleep 2\n", True, "sleep after the loop closed")
    expect("scripts/other.sh", "sleep 5\n", False, "not a conformance script")
    expect(conf, "printf '[ x ] && exec sleep 99' > agent\n", False, "sleep inside a single-quoted literal")
    expect(conf, "echo waiting for the node\nsleep 5\n", True, "`for` inside text is not a loop")

    if bad:
        print("check-script-hazards.sh --selftest: FAILED", file=sys.stderr)
        for b in bad:
            print(f"  {b}", file=sys.stderr)
        sys.exit(1)
    print("check-script-hazards.sh --selftest: every planted violation is refused, every clean line passes")


def main():
    if "--selftest" in sys.argv[1:]:
        selftest()
        return
    files = subprocess.run(["git", "ls-files", "*.sh"], capture_output=True, text=True, check=True).stdout.split()
    found = []
    for path in files:
        with open(path, encoding="utf-8", errors="replace") as f:
            found += check_text(path, f.read())
    if found:
        print("check-script-hazards.sh: FAILED", file=sys.stderr)
        for line in found:
            print(f"  {line}", file=sys.stderr)
        sys.exit(1)
    print(f"check-script-hazards.sh: {len(files)} shell scripts, no `| grep -q` under pipefail, no bare sleep in a conformance script")


main()
PYEOF
