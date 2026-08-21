#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Narration renderer for the demo recording — a long-lived, cache-first TTS server.

Speaks the captions the recorder already puts on screen, so the video explains
itself. Driven by ui/e2e/demo/overlay.ts under WARDYN_DEMO_VOICE=1.

PROTOCOL — one JSON object per line, on stdin, one reply per line on stdout:

    {"text": "Every run gets its own identity."}
 -> {"file": "/abs/path/<sha1>.wav", "durMs": 5110, "cached": false}

An unrenderable line answers {"error": "..."} and the caller degrades to a silent
caption. Narration is cosmetic; it must never be able to fail a recording.

WHY A SERVER AND NOT A CLI: Kokoro loads a 310MB ONNX graph, which costs seconds.
Per-caption invocation would add minutes to a take. One process, one load, ~50
renders.

WHY THE KEY IS A HASH OF THE TEXT: the alternative is a module of stable keys
("act1.hero"), which would be a third hand-synced duplicate in a codebase that
already has two that drift (FUNNEL_DEMOS vs demo-catalog, TASK.md vs DEMO_TASK) —
and a caption edited without its key would silently narrate the OLD line. Hashing
the text means a copy change re-renders exactly that clip and nothing else, and no
list has to be kept in sync with anything.

    scripts/narrate-server.py [--engine auto|kokoro|piper] [--voice am_michael]
    scripts/narrate-server.py --selftest    # render three lines and exit
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

CACHE = Path(os.environ.get("WARDYN_NARRATE_CACHE", Path.home() / ".cache" / "wardyn-narrate" / "clips"))
HOME = Path(os.environ.get("WARDYN_NARRATE_HOME", Path.home() / ".cache" / "wardyn-narrate"))
KOKORO_MODEL = HOME / "kokoro-v1.0.onnx"
KOKORO_VOICES = HOME / "voices-v1.0.bin"
PIPER_MODEL = Path(
    os.environ.get("WARDYN_NARRATE_PIPER_MODEL", Path.home() / "tester" / "voices" / "en_US-lessac-medium.onnx")
)

# The captions are written to be READ, not spoken, so a few things are said badly
# verbatim. Keep this list short and mechanical — rewriting the captions for the
# ear would make the on-screen text worse, which is the wrong trade.
# ORDER IS LOAD-BEARING: these run in sequence, so every MORE SPECIFIC rule must
# precede the general one that would otherwise consume it. `.com` before
# `api.anthropic.com` means the anthropic rule can never match — it is looking
# for a string the earlier rule already rewrote.
_SUBS = [
    # Spaced em-dash first, so the surrounding spaces go with it; a bare "—"
    # replaced by ", " would leave "for good , Same" and an odd spoken pause.
    (" — ", ", "),
    (" – ", ", "),
    ("—", ", "),
    ("–", ", "),
    ("…", ", "),
    # Specific hosts BEFORE the generic .com/.org rules.
    ("api.anthropic.com", "the Anthropic ay pee eye"),
    ("http-intake.logs.us5.datadoghq.com", "the Datadog telemetry endpoint"),
    ("169.254.169.254", "1 6 9 dot 2 5 4 dot 1 6 9 dot 2 5 4"),
    ("192.168.1.1", "1 9 2 dot 1 6 8 dot 1 dot 1"),
    # Generic last: without these a bare "example.com" reads as one mangled token.
    (".com", " dot com"),
    (".org", " dot org"),
]


def speakable(text: str) -> str:
    """Normalize on-screen caption text into something a TTS reads correctly."""
    out = text
    for a, b in _SUBS:
        out = out.replace(a, b)
    # Initialisms Kokoro reads as words ("CI" came out wrong on camera; CLI/AI
    # are the same trap). Phonetic respellings, not bare letter-spacing: a
    # standalone "A" reads as the ARTICLE (uh/eh — "A I" came out "Ehh Eye"),
    # so each one is spelled the way it is said. Word-bounded and uppercase-
    # only, so "api.anthropic.com", "deciding" etc. never match.
    _SAY = {"CI": "see eye", "CLI": "see ell eye", "API": "ay pee eye", "APIs": "ay pee eyes", "AI": "ay eye"}
    out = re.sub(r"\b(CI|CLI|APIs|API|AI)\b", lambda m: _SAY[m.group(1)], out)
    # Drop anything that is decoration rather than words (the recorder's captions
    # are plain, but chapter subtitles and future copy may not be).
    out = re.sub(r"[*_`#]", "", out)
    return re.sub(r"\s+", " ", out).strip()


def wav_duration_ms(path: Path) -> int:
    with wave.open(str(path)) as w:
        return int(round(w.getnframes() / float(w.getframerate()) * 1000))


class Engine:
    name = "none"

    def render(self, text: str, dest: Path) -> None:  # pragma: no cover - interface
        raise NotImplementedError


class Kokoro(Engine):
    name = "kokoro"

    def __init__(self, voice: str) -> None:
        from kokoro_onnx import Kokoro as _K  # imported late: optional dependency

        self.voice = voice
        self._k = _K(str(KOKORO_MODEL), str(KOKORO_VOICES))

    def render(self, text: str, dest: Path) -> None:
        import soundfile as sf

        samples, rate = self._k.create(text, voice=self.voice, speed=1.0, lang="en-us")
        sf.write(str(dest), samples, rate)


class Piper(Engine):
    name = "piper"

    def __init__(self, model: Path) -> None:
        self.bin = shutil.which("piper") or str(Path.home() / ".local" / "bin" / "piper")
        self.model = model

    def render(self, text: str, dest: Path) -> None:
        subprocess.run(
            [self.bin, "-m", str(self.model), "-f", str(dest)],
            input=text.encode(),
            check=True,
            capture_output=True,
        )


def pick_engine(which: str, voice: str) -> Engine:
    """kokoro when its model is present, else piper. Never raise: see module docstring."""
    if which in ("auto", "kokoro") and KOKORO_MODEL.exists() and KOKORO_VOICES.exists():
        try:
            return Kokoro(voice)
        except Exception as e:  # noqa: BLE001 - a broken optional dep must not be fatal
            print(f"narrate: kokoro unavailable ({e}); falling back to piper", file=sys.stderr)
    if which == "kokoro":
        raise SystemExit(f"narrate: kokoro requested but model missing at {KOKORO_MODEL}")
    if not PIPER_MODEL.exists():
        raise SystemExit(f"narrate: no engine available (no kokoro model, no piper model at {PIPER_MODEL})")
    return Piper(PIPER_MODEL)


def clip_for(engine: Engine, voice: str, text: str) -> tuple[Path, bool]:
    """Render `text` if it is not already cached. Returns (path, was_cached)."""
    spoken = speakable(text)
    key = hashlib.sha1(f"{engine.name}|{voice}|{spoken}".encode()).hexdigest()[:16]
    dest = CACHE / f"{key}.wav"
    if dest.exists() and dest.stat().st_size > 0:
        return dest, True
    CACHE.mkdir(parents=True, exist_ok=True)
    # Render to a temp file in the same directory, then rename: a take that dies
    # mid-render must not leave a truncated clip that every later run treats as a
    # cache hit.
    with tempfile.NamedTemporaryFile(dir=CACHE, suffix=".wav", delete=False) as tmp:
        tmp_path = Path(tmp.name)
    try:
        engine.render(spoken, tmp_path)
        tmp_path.replace(dest)
    except Exception:
        tmp_path.unlink(missing_ok=True)
        raise
    return dest, False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--engine", default=os.environ.get("WARDYN_NARRATE_ENGINE", "auto"))
    ap.add_argument("--voice", default=os.environ.get("WARDYN_NARRATE_VOICE", "am_michael"))
    ap.add_argument("--selftest", action="store_true", help="render three sample lines and exit")
    args = ap.parse_args()

    engine = pick_engine(args.engine, args.voice)
    print(f"narrate: engine={engine.name} voice={args.voice} cache={CACHE}", file=sys.stderr)

    if args.selftest:
        for line in (
            "Every run gets its own identity, its own barrier, and no resident credentials.",
            "The agent just reached for example.com. It is not on the list, so it was refused — and raised for me to decide.",
            "Egress was wide open, and these two never even got a connection.",
        ):
            path, cached = clip_for(engine, args.voice, line)
            print(f"{wav_duration_ms(path):6d}ms {'cached' if cached else 'rendered'}  {path}")
        return 0

    # Line-oriented so the caller can be any language. Flush every reply: the
    # recorder blocks on it.
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            text = json.loads(raw).get("text", "")
            if not text.strip():
                reply = {"error": "empty text"}
            else:
                path, cached = clip_for(engine, args.voice, text)
                reply = {"file": str(path), "durMs": wav_duration_ms(path), "cached": cached}
        except Exception as e:  # noqa: BLE001 - one bad line must not end the run
            reply = {"error": str(e)}
        print(json.dumps(reply), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
