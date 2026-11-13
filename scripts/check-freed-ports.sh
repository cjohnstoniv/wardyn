#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-freed-ports.sh — refuses Go code that picks a free port by listening on port 0, closes that
# listener, and then uses the port. Between the close and a later bind (or a dial that expects a
# refusal) another process can take the port, and the test then talks to that process or fails on a
# bind error that has nothing to do with the code under test.
#
# Instead, listen on port 0 and keep the listener (hand it to the server, or read the bound address
# from it or from a hook such as proxy.Server.BoundAddr); for an address nothing must answer on, use
# "127.0.0.1:1", where nothing listens.
#
# A hit is a `x := net.Listen*(... ":0" ...)` (or Port: 0, JoinHostPort(h, "0"), AddrPortFrom(a, 0)) whose address is read before a plain
# `x.Close()` and is still used after it, or a function that defers `x.Close()` and returns the address.
# ALLOWED lists the few sites that must name a port and pick again when the bind fails, each with why.
# `--selftest` plants each shape and proves the check refuses it.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v python3 >/dev/null 2>&1 || { echo "check-freed-ports.sh: python3 not found" >&2; exit 1; }

python3 - "$@" <<'PYEOF'
import re
import subprocess
import sys

# (path, enclosing top-level function): why the site may pick, free and reuse a port.
ALLOWED = {
    ("cmd/wardyn-runnerd/production_docker_test.go", "TestDockerSinkNeverListensOnAKeptAddressTheRelayMayNotUse"):
        "the port is only text in a kept-address file; the test never binds or dials it",
    ("cmd/wardyn-runnerd/relay_test.go", "TestRelayEnsureNeedsAPair"):
        "Ensure listens on the URL's own port, never 0; another port is picked when the bind fails",
    ("cmd/wardyn/proxyrelay_test.go", "TestProxyRelay_WarningGoesToStderrNotStdout"):
        "the command takes a listen port, never 0; another port is picked when the bind fails",
    ("cmd/wardynd/boot_metrics_test.go", "TestStartMetricsListener"):
        "startMetricsListener takes an address, not a listener; another port is picked when the bind fails",
    ("internal/egress/proxy/dns_peers_test.go", "listenDNSOnFreePort"):
        "ListenDNS needs one port for UDP and TCP and reports no bound address; another port is picked when the bind fails",
}

LISTEN = re.compile(r'(\w+)\s*(?:,\s*\w+)?\s*:?=\s*net\.Listen(?:TCP|UDP|Packet)?\((.*)$')
PORT0 = re.compile(r':0"|Port:\s*0\b|JoinHostPort\([^()]*,\s*"0"\)|AddrPortFrom\([^()]*,\s*0\)')
FUNC = re.compile(r'^func\s+(?:\([^)]*\)\s*)?(\w+)')
ASSIGN = re.compile(r'^\s*([\w.]+(?:\s*,\s*[\w.]+)*)\s*:?=(?!=)(.*)$')
STRING = re.compile(r'"(?:\\.|[^"\\])*"|`[^`]*`')
# Calls that only reshape an address or a port; a value built with any other call is not the address.
RESHAPE = {'', 'String', 'int', 'uint16', 'string', 'strconv.Itoa', 'fmt.Sprint', 'fmt.Sprintf', 'net.SplitHostPort', 'net.JoinHostPort'}


def code(line):
    """line with its string literals emptied and its trailing comment cut, so a name in text is no use."""
    return STRING.sub('""', line).split('//')[0]


def carried(body, addr):
    """names that hold the listener's address or port, through assignments that only reshape it."""
    names = set()
    for b in body:
        a = ASSIGN.match(b)
        if not a:
            continue
        rhs = code(a.group(2))
        calls = {c.strip('.') for c in re.findall(r'([\w.]*)\(', addr.sub('', rhs))}
        if (addr.search(rhs) or any(re.search(r'(?<![\w.])%s\b' % re.escape(n), rhs) for n in names)) and calls <= RESHAPE:
            names.update(n.split('.')[0] for n in a.group(1).replace(' ', '').split(',') if n != '_')
    return names


def hits(path, text):
    """(line, function, why) for each freed-port use in text."""
    lines = text.split('\n')
    found = []
    for i, line in enumerate(lines):
        m = LISTEN.search(line)
        if not m or not PORT0.search(m.group(2)) or line.lstrip().startswith('//'):
            continue
        v = re.escape(m.group(1))
        addr = re.compile(r'\b%s\.(?:Addr|LocalAddr)\(\)' % v)
        fn = next((f.group(1) for f in (FUNC.match(lines[j]) for j in range(i, -1, -1)) if f), '?')
        end = next((j for j in range(i + 1, len(lines)) if lines[j] == '}'), len(lines))
        body = lines[i + 1:end]
        names, escaped, seen, held, cleanup = set(), False, False, None, False
        for k, b in enumerate(body):
            # A close inside a t.Cleanup(func() { ... }) or defer func() { ... }() block runs at the end, as a defer does.
            if held is None and re.match(r'\s*(?:t\.Cleanup\(|defer\s+)func\(\)\s*\{\s*$', b):
                held, cleanup = len(b) - len(b.lstrip()), 't.Cleanup(' in b
            elif held is not None and b.strip() in ('})', '}()') and len(b) - len(b.lstrip()) == held:
                held = None
            close = re.match(r'\s*(?:_\s*=\s*)?%s\.Close\(\)' % v, b)
            if re.match(r'\s*defer\s+(?:%s\.Close\(\)|func\(\)\s*\{\s*_\s*=\s*%s\.Close\(\)\s*\}\(\))' % (v, v), b) or (close and held is not None):
                # A close in a t.Cleanup block keeps the listener for the test; a deferred one runs at the return.
                # A name that carries the address counts in a return of the listener's own block, not in a closure's.
                gone, own = carried(body, addr), len(line) - len(line.lstrip())
                if not (close and held is not None and cleanup) and any(
                        re.match(r'\s*return\b', r) and (addr.search(r) or (len(r) - len(r.lstrip()) == own and any(
                            re.search(r'(?<![\w.])%s\b' % re.escape(n), code(r)) for n in gone)))
                        for r in body[k:]):
                    found.append((i + 1, fn, 'returns the address of a listener it closes'))
                break
            if seen and close:
                rest = [c for c in (code(r) for r in body[k + 1:]) if c.strip()]
                used = any(addr.search(r) or any(re.search(r'(?<![\w.])%s\b' % re.escape(n), r) for n in names) for r in rest)
                if used or (escaped and any(r.strip() not in ('}', '})', 'return') for r in rest)):
                    found.append((i + 1, fn, 'closes the listener, then uses its port'))
                break
            a = ASSIGN.match(b)
            direct = addr.search(b)
            if not direct and not (a and any(re.search(r'(?<![\w.])%s\b' % re.escape(n), code(a.group(2))) for n in names)):
                continue
            seen = True
            if a:
                # x := ... and x.f = ...: the address now travels in x.
                names.update(n.split('.')[0] for n in a.group(1).replace(' ', '').split(',') if n != '_')
            elif direct:
                escaped = True
    return found


def check(files, allowed):
    """Findings for files ({path: text}), and allowed entries that matched nothing."""
    found, used = [], set()
    for path, text in files.items():
        for line, fn, why in hits(path, text):
            if (path, fn) in allowed:
                used.add((path, fn))
            else:
                found.append(f"{path}:{line}: {fn}: {why}")
    stale = [f"{p}: {f}: allowed, but nothing there picks and frees a port any more (drop the entry)"
             for (p, f) in allowed if (p, f) not in used and p in files]
    return found, stale


def selftest():
    bad = []
    pre = 'package x\n\n'

    def expect(text, want, why, allowed=None):
        found, _ = check({"x_test.go": pre + text}, allowed or {})
        if bool(found) != want:
            bad.append(f"{why}: wanted {'a finding' if want else 'none'}, got {found}")

    expect('func freePort(t *testing.T) int {\n\tln, err := net.Listen("tcp", "127.0.0.1:0")\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n'
           '\tdefer func() { _ = ln.Close() }()\n\treturn ln.Addr().(*net.TCPAddr).Port\n}\n', True, "a helper returning a freed port")
    expect('func freeAddr(t *testing.T) string {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer ln.Close()\n\treturn ln.Addr().String()\n}\n',
           True, "a helper returning a freed address")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\taddr := ln.Addr().String()\n\t_ = ln.Close()\n'
           '\tc, _ := net.Dial("tcp", addr)\n\t_ = c\n}\n', True, "a freed address dialled later")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tport := ln.Addr().(*net.TCPAddr).Port\n'
           '\tps := strconv.Itoa(port)\n\tln.Close()\n\trun(ps)\n}\n', True, "a name derived from the freed port")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tt.Setenv("DOCKER_HOST", "tcp://"+ln.Addr().String())\n'
           '\t_ = ln.Close()\n\trun()\n}\n', True, "a freed address handed on before the close")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tcfg.Addr = ln.Addr().String()\n\t_ = ln.Close()\n\tstart(cfg)\n}\n',
           True, "a freed address stored in a field")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})\n'
           '\taddr := ln.Addr().String()\n\tln.Close()\n\tserve(addr)\n}\n', True, "ListenTCP on Port: 0")
    expect('func TestX(t *testing.T) {\n\tl, _ := net.Listen("tcp", "0.0.0.0:0")\n\t_, p, _ := net.SplitHostPort(l.Addr().String())\n\t_ = l.Close()\n\tuse(p)\n}\n',
           True, "a port split out of a freed address")
    expect('func freePort() int {\n\tl, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer l.Close()\n\tp := l.Addr().(*net.TCPAddr).Port\n\treturn p\n}\n',
           True, "a helper returning a freed port through a name")
    expect('func freeAddr() string {\n\tl, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer l.Close()\n\tport := l.Addr().(*net.TCPAddr).Port\n'
           '\taddr := "127.0.0.1:" + strconv.Itoa(port)\n\treturn addr\n}\n', True, "a helper returning a freed address built from the port")
    expect('func echo(t *testing.T) string {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tgo serve(ln)\n\tt.Cleanup(func() {\n\t\t_ = ln.Close()\n\t})\n'
           '\treturn ln.Addr().String()\n}\n', False, "a helper returning the address of a listener closed at cleanup")
    expect('func TestX(t *testing.T) {\n\tl, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer l.Close()\n\taddr := l.Addr().String()\n'
           '\tdial := func() (net.Conn, error) {\n\t\treturn net.Dial("tcp", addr)\n\t}\n\tuse(dial)\n}\n', False, "a closure returning a dial of a held listener")
    expect('func pair(t *testing.T) net.Conn {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer ln.Close()\n\tc, err := net.Dial("tcp", ln.Addr().String())\n'
           '\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\treturn c\n}\n', False, "a helper returning a connection, not the address")
    expect('func TestX(t *testing.T) {\n\tl, _ := net.Listen("tcp", net.JoinHostPort(bind, "0"))\n\tp := l.Addr().(*net.TCPAddr).Port\n\t_ = l.Close()\n\tuse(p)\n}\n',
           True, "port 0 through net.JoinHostPort")
    expect('func TestX(t *testing.T) {\n\tl, _ := net.Listen("tcp", netip.AddrPortFrom(bind, 0).String())\n\tp := l.Addr().(*net.TCPAddr).Port\n\t_ = l.Close()\n\tuse(p)\n}\n',
           True, "port 0 through netip.AddrPortFrom")
    expect('func TestY(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\taddr := ln.Addr().String()\n\t_ = ln.Close()\n\tuse(addr)\n}\n',
           True, "an allowed path but another function", {("x_test.go", "TestX"): "why"})
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\taddr := ln.Addr().String()\n\t_ = ln.Close()\n\tuse(addr)\n}\n',
           False, "an allowed site", {("x_test.go", "TestX"): "why"})
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tdefer ln.Close()\n\tgo serve(ln)\n\tget(ln.Addr().String())\n}\n',
           False, "a listener held for the test")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tt.Cleanup(func() { _ = ln.Close() })\n\tget(ln.Addr().String())\n}\n',
           False, "a listener closed at cleanup")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\taddr := ln.Addr().String()\n\tgo serve(ln)\n\tget(addr)\n\t_ = ln.Close()\n}\n',
           False, "a listener closed after its last use")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tgo serve(ln)\n\tget(ln.Addr().String())\n\t_ = ln.Close()\n}\n',
           False, "an address handed on and closed at the end")
    expect('func TestX(t *testing.T) {\n\tcfg.Listen = "127.0.0.1:0"\n\tsrv := start(cfg)\n\tget(srv.BoundAddr().String())\n}\n',
           False, "port 0 handed to the server")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tport := ln.Addr().(*net.TCPAddr).Port\n'
           '\tget(port)\n\tln.Close()\n\tdead := "127.0.0.1:1"\n\tget(dead)\n}\n', False, "the port is not used after the close")
    expect('func TestX(t *testing.T) {\n\t// ln, _ := net.Listen("tcp", "127.0.0.1:0")\n}\n', False, "a comment")
    expect('func rig(t *testing.T) *R {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tcli, _ := net.Dial("tcp", ln.Addr().String())\n'
           '\tr := &R{c: cli}\n\tt.Cleanup(func() {\n\t\t_ = cli.Close()\n\t\t_ = ln.Close()\n\t\tsrv.Close()\n\t})\n\treturn r\n}\n',
           False, "a close inside a t.Cleanup block")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\taddr := ln.Addr().String()\n\tcfg.Addr = addr\n'
           '\t_ = ln.Close()\n\tstart(cfg)\n}\n', True, "a freed address carried in a struct")
    expect('func TestX(t *testing.T) {\n\tln, _ := net.Listen("tcp", "127.0.0.1:0")\n\tport := ln.Addr().(*net.TCPAddr).Port\n'
           '\tget(port)\n\tln.Close()\n\tt.Errorf("a closed port") // port\n}\n', False, "the name only in text after the close")
    _, stale = check({"x_test.go": pre + 'func TestX(t *testing.T) {}\n'}, {("x_test.go", "TestX"): "why"})
    if not stale:
        bad.append("an allowed entry that matches nothing: wanted it reported, got none")

    if bad:
        print("check-freed-ports.sh --selftest: FAILED", file=sys.stderr)
        for b in bad:
            print(f"  {b}", file=sys.stderr)
        sys.exit(1)
    print("check-freed-ports.sh --selftest: every planted freed-port use is refused, every held listener passes")


def main():
    if "--selftest" in sys.argv[1:]:
        selftest()
        return
    paths = subprocess.run(["git", "ls-files", "*.go"], capture_output=True, text=True, check=True).stdout.split()
    files = {}
    for path in paths:
        with open(path, encoding="utf-8", errors="replace") as f:
            files[path] = f.read()
    found, stale = check(files, ALLOWED)
    if found or stale:
        print("check-freed-ports.sh: FAILED (listen on port 0 and keep the listener, or use 127.0.0.1:1 for a refusal)", file=sys.stderr)
        for line in found + stale:
            print(f"  {line}", file=sys.stderr)
        sys.exit(1)
    print(f"check-freed-ports.sh: {len(files)} Go files, no port picked, freed and used again outside the {len(ALLOWED)} allowed sites")


main()
PYEOF
