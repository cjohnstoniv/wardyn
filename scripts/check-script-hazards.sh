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
#      A sourced library (scripts/lib/*.sh, *-lib.sh) is held to it too: it runs under its caller's pipefail.
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

LEADER = r'(?:^|[;&|({]|\b(?:then|do|else)\b)\s*'
PIPEFAIL = re.compile(LEADER + r'set\s+[^#;&|\n]*pipefail')
# A sourced library (scripts/lib/*.sh, *-lib.sh) runs under its caller's pipefail.
SOURCED_LIB = re.compile(r'(^|/)lib/[^/]+\.sh$|(^|/)[^/]*-?lib\.sh$')
GREP_Q = re.compile(
    r'(?<!\|)\|&?\s*(?:command\s+|/usr/bin/|sudo\s+(?:-\S+\s+)*)?[ef]?grep\s+[^|;&)\]}\n]*?'
    r'(?<![\w-])(?:-[A-Za-z]*q[A-Za-z]*|--quiet|--silent)(?=\s|$)')
HEREDOC = re.compile(r'(?<!<)<<-?\s*([\'"]?)([A-Za-z_][A-Za-z0-9_]*)\1')
LOOP_OPEN = re.compile(r'(?:^|[;&|({"\'`]|\b(?:then|do|else)\b)\s*(?:for|while|until)\s')
LOOP_CLOSE = re.compile(r'(?:^|[;&|(){}"\'`\s])done(?=\s|$|[;&|)"\'`])')
SLEEP = re.compile(r'(?<![\w-])sleep\s+["\']?[0-9$]')
LET = re.compile(LEADER + r'let\s[^;&|]*$')
ARITH_OPENER = re.compile(r'(?:^|[;&|({]|\b(?:then|do|else|if|elif|while|until|for)\b)\s*$')
CONFORMANCE = re.compile(r'(^|/)[^/]*conformance[^/]*\.sh$')


def code_lines(text):
    """(lineno, line) for each line outside heredoc bodies, comments cut, plus
    the (marker, lineno) of each heredoc still open at the end of the text.
    A `<<WORD` counts only outside quotes, ANSI-C $'..' strings, `let` arguments
    and $(( )) / (( )) arithmetic."""
    lines, pending = [], []
    for no, raw in enumerate(text.splitlines(), 1):
        if pending:
            if raw.strip() == pending[0][0]:
                pending.pop(0)
            continue
        # Open contexts, innermost last: ' or " (quotes), C for an ANSI-C $'..'
        # string, ( for $( ) or a subshell, A for $(( )) or (( )) arithmetic.
        # Quotes reopen inside $( ).
        line, stack, esc, skip = [], [], False, 0
        for i, ch in enumerate(raw):
            if skip:
                skip -= 1
                line.append(ch)
                continue
            top = stack[-1] if stack else ''
            if esc:
                esc = False
            elif top == "'":
                if ch == "'":
                    stack.pop()
            elif top == 'C':
                if ch == '\\':
                    esc = True
                elif ch == "'":
                    stack.pop()
            elif ch == '\\':
                esc = True
            elif ch == '(' and raw[i - 1:i] == '$' and raw[i - 2:i - 1] != '\\':
                stack.append('A' if raw[i + 1:i + 2] == '(' else '(')
                skip = 1 if stack[-1] == 'A' else 0
            elif top == '"':
                if ch == '"':
                    stack.pop()
            elif ch == "'" and raw[i - 1:i] == '$':
                stack.append('C')
            elif ch == '(' and raw[i + 1:i + 2] == '(' and ARITH_OPENER.search(''.join(line)):
                stack.append('A')
                skip = 1
            elif ch in '"\'(':
                stack.append(ch)
            elif ch == ')':
                if top == 'A':
                    stack.pop()
                    skip = 1 if raw[i + 1:i + 2] == ')' else 0
                elif top == '(':
                    stack.pop()
            elif ch == '#' and (i == 0 or raw[i - 1] in ' \t;'):
                break
            elif ch == '<' and 'A' not in stack and not LET.search(''.join(line)) and (m := HEREDOC.match(raw, i)):
                pending.append((m.group(2), no))
            line.append(ch)
        lines.append((no, ''.join(line)))
    return lines, pending


def pipelines(lines):
    """(lineno, line) with a line ending in `|`, `|&` or a backslash joined to the
    next code line, so a pipe split across lines is one pipeline."""
    out, i = [], 0
    while i < len(lines):
        no, line = lines[i]
        i += 1
        while re.search(r'(?<!\|)\|&?\s*$|\\\s*$', line) and i < len(lines):
            nxt = lines[i][1]
            i += 1
            if nxt.strip():
                line = re.sub(r'\\\s*$', '', line) + ' ' + nxt.strip()
        out.append((no, line))
    return out


def check_text(path, text):
    lines, unclosed = code_lines(text)
    found = [f"{path}:{no}: heredoc `<<{word}` never closes, so the lines after it go unchecked"
             for word, no in unclosed]
    if SOURCED_LIB.search(path) or any(PIPEFAIL.search(line) for _, line in lines):
        for no, line in pipelines(lines):
            if GREP_Q.search(re.sub(r"'[^']*'", lambda m: ' ' * len(m.group(0)), line)):
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
                 "ps |grep --quiet foo", "ps | egrep -q foo", "if ps | grep -qx foo; then :; fi",
                 "ps |\n  grep -q foo", "ps | \\\n  grep -q foo", "ps |\n\n  # why\n  grep -q foo",
                 "ps |& grep -q foo", "ps | sudo grep -q foo", "ps | sudo -n grep -q foo",
                 "ps | grep -e foo -q", "ps | grep -e foo -E -q", "ps | grep foo -q"]:
        expect("scripts/x.sh", pf + line + "\n", True, f"planted `{line}`")
    for line in ["ps | grep foo >/dev/null", "ps || grep -q foo f", "grep -q foo <<<\"$out\"",
                 "# ps | grep -q foo", "ps | grep -c foo", "ps | grep -e foo",
                 "ps |\n  grep foo >/dev/null", "ps | grep -e foo -c", "ps | grep 'x -q y'", "test \"$(ps | grep -c foo)\" -eq 0",
                 "ps ||\n  grep -q foo f", "ps | grep -e foo\nout=1; grep -q a f"]:
        expect("scripts/x.sh", pf + line + "\n", False, f"clean `{line}`")
    expect("scripts/x.sh", pf + "cat <<'EOF'\nps | grep -q foo\nEOF\n", False, "heredoc body")
    expect("scripts/x.sh", pf + "v=\"$(awk '/x\" <<EOF/{print}' f)\"\nps | grep -q foo\n", True, "`<<EOF` inside quotes")
    expect("scripts/x.sh", pf + "x=$((1<<FOO))\nps | grep -q foo\n", True, "`<<` in arithmetic")
    for stmt in ["(( x=1<<FOO ))", "let x=1<<FOO", "let x=1<<FOO y=2", "for ((i=1<<FOO; i<9; i++)); do :; done",
                 "if (( x=1<<FOO )); then :; fi", "y=$'a\\'<<EOF'", "y=$'a\\\\'; z=$((1<<FOO))",
                 "y=$(echo $((1<<FOO)))"]:
        expect("scripts/x.sh", pf + stmt + "\nps | grep -q foo\nFOO\nEOF\n", True, f"`{stmt}` opens no heredoc")
    expect("scripts/x.sh", pf + "cat <<EOF\nEOF\nps | grep -q foo\n", True, "a real heredoc still closes")
    expect("scripts/x.sh", pf + "let x=1; cat <<EOF\nps | grep -q foo\nEOF\n", False, "heredoc after `let ...;`")
    expect("scripts/x.sh", pf + "y=\"$'\"; cat <<EOF\nps | grep -q foo\nEOF\n", False, "`$'` inside double quotes is no ANSI-C string")
    expect("scripts/x.sh", "cat <<EOF\nno end marker\n", True, "heredoc open at end of file")
    expect("scripts/x.sh", "set -eu\nps | grep -q foo\n", False, "no pipefail")
    expect("scripts/x.sh", "set -o pipefail\nps | grep -q foo\n", True, "set -o pipefail")
    for line in ["[ -n \"$X\" ] && set -o pipefail", "{ set -euo pipefail; }", "if true; then set -o pipefail; fi",
                 "(set -o pipefail; :)"]:
        expect("scripts/x.sh", line + "\nps | grep -q foo\n", True, f"non-leading `{line}`")
    expect("scripts/x.sh", "echo set -o pipefail\nps | grep -q foo\n", False, "`set -o pipefail` as echo text")
    expect("scripts/lib/x.sh", "ps | grep -q foo\n", True, "sourced lib without its own pipefail")
    expect("deploy/images/common/x-lib.sh", "ps | grep -q foo\n", True, "-lib.sh without its own pipefail")

    conf = "scripts/k-conformance.sh"
    expect(conf, "sleep 5\n", True, "bare sleep")
    for line in ['sleep "$N"', "sleep $N", 'sleep "${N:-5}"']:
        expect(conf, line + "\n", True, f"bare `{line}`")
    expect(conf, 'until check; do sleep "$N"; done\n', False, "polled sleep with a variable")
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
