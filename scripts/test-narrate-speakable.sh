#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-narrate-speakable.sh — speakable()'s kokoro-only respellings stay behind
# the `initialisms` guard, and NOTHING leaks into an engine that has its own
# text frontend.
#
# Why this exists: the respellings in narrate-server.py are tuned to kokoro's
# G2P. A model with its own frontend (myvoice) reads the bare forms correctly
# and is HURT by them — it says "eh pee eye" as a word. So `initialisms=False`
# has to skip four separate blocks: _SUBS's tagged entries, _SAY, the
# digit-string rules and the identifier loop.
#
# The DANGEROUS failure is silent. Miss one block and the take still renders,
# still passes every existing gate, and only sounds wrong — which is discovered
# after ~14 episodes are shot. Two shapes in particular:
#
#   * _SUBS must stay ONE ordered list. Its own header says ORDER IS
#     LOAD-BEARING: ".com" before "api.anthropic.com" means the anthropic rule
#     can never match. Splitting it into guarded/unguarded halves and running
#     the unguarded half first degrades the host to "api.anthropic dot com" —
#     no error, just a wrong reading.
#   * "_"→space is NOT kokoro-specific. ssh_key must reach myvoice as "ssh key"
#     (words), never as "ssh_key" (fused) and never as the kokoro respelling
#     "ess ess aitch key".
#
# Daemon-free, network-free, no TTS engine: it calls speakable() directly.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$ROOT/scripts/narrate-server.py" <<'PY'
import importlib.util, sys

spec = importlib.util.spec_from_file_location("ns", sys.argv[1])
ns = importlib.util.module_from_spec(spec)
sys.modules["ns"] = ns
spec.loader.exec_module(ns)

# One caption touching all four guarded blocks plus the unguarded "_"→space.
CAPTION = ("Set ssh_key to 0400 and open 443, then reach api.anthropic.com "
           "on a live run and read it back.")

WANT_KOKORO = ("Set ess ess aitch key to oh four oh oh and open four four three, "
               "then reach the Anthropic eh pee eye on a lyve run and reed it back.")
WANT_BARE = ("Set ssh key to 0400 and open 443, then reach api.anthropic.com "
             "on a live run and read it back.")

rc = 0

def check(label, got, want):
    global rc
    if got != want:
        print(f"FAIL: {label}\n  want: {want}\n  got : {got}", file=sys.stderr)
        rc = 1

check("kokoro keeps every respelling", ns.speakable(CAPTION, initialisms=True), WANT_KOKORO)
check("initialisms=False keeps every bare form", ns.speakable(CAPTION, initialisms=False), WANT_BARE)

# The leak check is the one that catches a newly-added unguarded block.
bare = ns.speakable(CAPTION, initialisms=False)
leaks = [r for r in ("ess ess aitch", "oh four oh oh", "four four three", "eh pee eye",
                     "lyve", "reed it back", "pie pee eye", " dot com") if r in bare]
if leaks:
    print(f"FAIL: kokoro respellings leaked past the guard: {leaks}", file=sys.stderr)
    rc = 1

# _SUBS must stay one ordered list of 3-tuples; a 2-tuple means someone dropped
# the tag, and a split list means the ordering hazard is back.
if not all(len(t) == 3 for t in ns._SUBS):
    print("FAIL: _SUBS entries are not (pattern, replacement, kokoro_only) 3-tuples", file=sys.stderr)
    rc = 1

# Guard is real: with every entry forced kokoro-only, the bare render is untouched
# except for the engine-neutral "_"→space.
if ns.speakable("api.anthropic.com", initialisms=False) != "api.anthropic.com":
    print("FAIL: a bare host was rewritten with initialisms=False "
          "(the .com/anthropic ordering hazard is back)", file=sys.stderr)
    rc = 1

print("test-narrate-speakable: PASS" if rc == 0 else "test-narrate-speakable: FAIL")
sys.exit(rc)
PY
