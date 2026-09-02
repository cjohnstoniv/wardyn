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
import itertools
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
# Per-clip filename counter for the myvoice engine's shared render directory.
# Paired with the pid because ~/myvoice/renders/narrate is mounted into the
# container and a rehearse can run beside a take: pid alone collides on the
# second clip, the counter alone collides across processes.
_counter = itertools.count()
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
    (" — ", ", ", False),
    (" – ", ", ", False),
    ("—", ", ", False),
    ("–", ", ", False),
    ("…", ", ", False),
    # HETERONYMS, phrase-scoped on purpose: "live" the adjective is /laɪv/
    # ("a live run") while "lives" the verb is /lɪv/ ("where a yes lives") —
    # a bare word sub would break the verb, so only known adjective phrases
    # are respelled. Add phrases here as scripts grow them; the verifier's
    # pronunciation watch flags unmapped occurrences for review.
    ("watch it live", "watch it lyve", True),
    ("live run", "lyve run", True),
    ("live decision", "lyve decision", True),
    ("live strip", "lyve strip", True),
    ("held live", "held lyve", True),
    ("caught it live", "caught it lyve", True),
    ("blocked live", "blocked lyve", True),
    # dialog round 2026-09-01: new adjective phrases the rewrite introduced.
    ("live credential", "lyve credential", True),
    ("live session", "lyve session", True),
    ("watched this one live", "watched this one lyve", True),
    ("audit trail, live", "audit trail, lyve", True),
    # ...and the VERB sense (/lɪv/) where the scripts use it:
    ("attacks live exactly here", "attacks liv exactly here", True),
    ("no keys live in the room", "no keys liv in the room", True),
    ("keys live inside", "keys liv inside", True),
    ("keys don't live in the room", "keys don't liv in the room", True),
    ("run can live", "run can liv", True),
    ("cloud credentials live", "cloud credentials liv", True),
    # "record" the VERB (/rɪˈkɔːɹd/) where espeak would stress it as the noun:
    ("Only record work you trust", "Only ruh-cord work you trust", True),
    ("You can't record what a policy", "You can't ruh-cord what a policy", True),
    ("going to record", "going to ruh-cord", True),
    ("Record it.", "Ruh-cord it.", True),
    # "live" the VERB in the 03 split's owner line, and the ADJECTIVE in 03d's
    # "the live GitHub API" — both senses in one round, so both are pinned.
    ("credentials can live", "credentials can liv", True),
    ("the live GitHub", "the lyve GitHub", True),
    # "read-back"/"read it back" are present tense everywhere in 03a (/riːd/) —
    # Kokoro reads a bare "read" as past tense after "the"; pin the phrases.
    ("read-back", "reed-back", True),
    ("read it back", "reed it back", True),
    # 03c's audit-panel line reads the panel upward — imperative /riːd/ again.
    ("so read it upward", "so reed it upward", True),
    ("never read it", "never reed it", True),
    ("reads back", "reeds back", True),
    ("Read which gate refused", "Reed which gate refused", True),
    # "use" the VERB (/juːz/) where espeak guesses the noun — validated against
    # the venv phonemizer 2026-08-24 ("yooz" → /juːz/). NOTE: the r2 adjudication's
    # H1 pin ("first-use approval" → "first yoos approval") was REJECTED by that
    # same validation: the phrase already reads /juːs/ correctly, and "yoos"
    # phonemizes to /juːz/ — the exact inversion it meant to prevent.
    ("use it, not have it", "yooz it, not have it", True),
    ("demos ahead use this key", "demos ahead yooz this key", True),
    ("can still use it", "can still yooz it", True),
    ("allowed to use", "allowed to yooz", True),
    # The colon in the header name is inaudible; a comma lands the pause (r2 H3).
    ("Authorization: Bearer", "Authorization, Bearer", False),
    # The quote marks around 'forbidden' are inaudible; a comma lands the beat.
    ("Not 'forbidden'.", "Not, forbidden.", False),
    # 09's honesty line quotes two phrases; the curly quotes are inaudible, so
    # commas land the same beat (same treatment as "Not 'forbidden'." above).
    ("that \u201cnone observed\u201d means \u201cnone happened.\u201d",
     "that, none observed, means, none happened.", False),
    # "PyPI" reads as "pie-pie" bare:
    ("PyPI", "pie pee eye", True),
    # Product/tool names and one dotted identifier the captions speak aloud.
    ("gVisor", "gee visor", True),
    ("kubectl", "cube control", True),
    ("ci-run.sh", "see eye run dot ess aitch", True),
    ("TRY-IT dot md", "try it dot em dee", True),
    ("Apache-2.0", "Apache two point oh", True),
    ("authz.denied", "auth-zee denied", True),
    # The owner's script uses three-dot trailing ellipses ("useful...") — read
    # as a breath, not dots. Must precede nothing (plain literal).
    ("...", ", ", False),
    # Specific hosts BEFORE the generic .com/.org rules.
    ("api.anthropic.com", "the Anthropic eh pee eye", True),
    ("http-intake.logs.us5.datadoghq.com", "the Datadog telemetry endpoint", True),
    ("169.254.169.254", "1 6 9 dot 2 5 4 dot 1 6 9 dot 2 5 4", True),
    ("192.168.1.1", "1 9 2 dot 1 6 8 dot 1 dot 1", True),
    # ".org" alone leaves "files.pythonhosted" as one mangled token.
    ("files.pythonhosted.org", "the python-hosted download host", True),
    # Generic last: without these a bare "example.com" reads as one mangled token.
    (".com", " dot com", True),
    (".org", " dot org", True),
]


def speakable(text: str, initialisms: bool = True) -> str:
    """Normalize on-screen caption text into something a TTS reads correctly.

    `initialisms=False` skips every kokoro-specific respelling. Those are tuned to
    kokoro's G2P; a model with its own text frontend reads the bare forms correctly and
    is actively HURT by them -- chatterbox says "eh pee eye" as a word, stressed like
    "a-PEE-eye", instead of three letters.

    The four kokoro-only blocks are `_SUBS`'s tagged entries, `_SAY`, the digit-string
    rules and the identifier loop. `_SUBS` stays ONE ordered list and filters in place:
    ORDER IS LOAD-BEARING (see its header), so splitting it into guarded/unguarded halves
    would let ".com" fire before "api.anthropic.com" and silently mangle the host.
    """
    out = text
    for a, b, kokoro_only in _SUBS:
        if kokoro_only and not initialisms:
            continue
        out = out.replace(a, b)
    # Initialisms Kokoro reads as words ("CI" came out wrong on camera; CLI/AI
    # are the same trap). Phonetic respellings, not bare letter-spacing: a
    # standalone "A" reads as the ARTICLE (uh/eh — "A I" came out "Ehh Eye"),
    # so each one is spelled the way it is said. Word-bounded and uppercase-
    # only, so "api.anthropic.com", "deciding" etc. never match.
    # Respellings VALIDATED against the venv's own phonemizer (the exact G2P
    # kokoro-onnx uses): "eh"→/eɪ/ is the letter A ("ay" is /aɪ/ — it shipped
    # as "eye eye" once), "see"→/siː/, "ell"→/ɛl/, "pee"→/piː/. To re-check a
    # candidate: phonemizer_fork + espeakng_loader, language en-us.
    # 2026-08-24 (persona round on the 03 split): the demo captions also speak
    # CC1 (the barrier's policy token), PAT (Kokoro reads the NAME "Pat"), STS,
    # SSH, TTL and TLS — same treatment. "ess"→/ɛs/, "aitch"→/eɪtʃ/, "one"/"oh"
    # validated the same way (2026-08-24).
    _SAY = {"CI": "see eye", "CLI": "see ell eye", "API": "eh pee eye", "APIs": "eh pee eyes", "AI": "eh eye",
            "CC1": "see see one", "PAT": "pee eh tee", "PATs": "pee eh tees", "STS": "ess tee ess",
            "SSH": "ess ess aitch", "TTL": "tee tee ell", "TLS": "tee ell ess", "npm": "en pee em",
            "SSO": "ess ess oh", "OIDC": "oh eye dee see",
            "MCP": "em see pee", "MCPs": "em see pees"}
    if initialisms:
        out = re.sub(r"\b(CI|CLI|APIs|API|AI|CC1|PATs|PAT|MCPs|MCP|STS|SSH|SSO|OIDC|TTL|TLS|npm)\b", lambda m: _SAY[m.group(1)], out)
    # Numbers the captions spell as digits but mean as digit STRINGS: a file mode,
    # a port, the cloud-metadata address. Read as quantities they come out as
    # "four hundred forty-three" / "one hundred sixty-nine…" (checked against the
    # venv's phonemizer 2026-08-24); the captions already write "Four-oh-four".
    if initialisms:
        out = re.sub(r"\b0400\b", "oh four oh oh", out)
        out = re.sub(r"\b443\b", "four four three", out)
        out = re.sub(r"\b8280\b", "eight two eight oh", out)
        out = re.sub(r"\b2322\b", "two three two two", out)
    # Lowercase identifier PARTS (ssh_key, cloud_sts, ttl_seconds, api_key) reach
    # the engine as bare words after the underscore split below; spell them too,
    # but never inside a hostname (ssh.github.com, api.anthropic.com).
    # Policy identifiers the walks speak by name. The initialism parts are
    # lowercase here, so the uppercase map above never sees them; spell the
    # known ones explicitly (a generic lowercase rule ate "the pat on the back").
    if initialisms:
        for ident, said in (("ssh_key", "ess ess aitch key"), ("cloud_sts", "cloud ess tee ess"),
                            ("ttl_seconds", "tee tee ell seconds"), ("api_key", "eh pee eye key"),
                            ("git_pat", "git pee eh tee"), ("the ssh client", "the ess ess aitch client")):
            out = out.replace(ident, said)
    # Every other policy key is spoken as words: eligible_grants → "eligible
    # grants" (the decoration strip below would otherwise fuse them).
    out = out.replace("_", " ")
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


class Myvoice(Engine):
    """The operator's own cloned voice, rendered in a network-less container.

    The engine lives in ~/myvoice/engines/<e>, is built once with network access and run
    with none. It speaks JSON-lines on stdin/stdout, so ONE process is started here and
    reused for every clip: a fresh container per line would pay ~25 s of model load each
    time, which for a full narration pass is hours of pure overhead.

    `voice` is the reference clip id under ~/myvoice/corpus/ref/, so switching which
    recording is cloned goes through the same --voice seam every other engine uses -- and
    because the cache key is sha1(engine|voice|text), a new reference re-renders
    automatically instead of silently serving clips of the old one.
    """

    name = "myvoice"

    def __init__(self, voice: str, arm: str = "") -> None:
        arm = arm or os.environ.get("MYVOICE_ARM", "cosyvoice")
        self.root = Path(os.environ.get("MYVOICE_ROOT", Path.home() / "myvoice"))
        run_sh = self.root / "engines" / "run.sh"
        ref = self.root / "corpus" / "ref" / f"{voice}.wav"
        if not run_sh.exists():
            raise RuntimeError(f"no myvoice checkout at {self.root}")
        if not ref.exists():
            raise RuntimeError(f"no reference clip {ref}")
        self.out_dir = self.root / "renders" / "narrate"
        self.out_dir.mkdir(parents=True, exist_ok=True)
        self.run_sh, self.arm, self.voice = run_sh, arm, voice
        self.proc = None      # started on first real render, not here: see _start()

    def _start(self) -> None:
        """Boot the container the first time a clip actually has to be rendered.

        A re-run with a warm cache renders nothing, and paying ~25 s of model load to
        answer entirely from cache is pure waste on a pipeline that re-runs constantly.
        """
        env = dict(os.environ, MYVOICE_REF=self.voice)
        # CosyVoice's rate depends on how much text it gets in one call, and captions here
        # average ~38 chars: at speed 1.0 short lines land near 115 wpm, far under the
        # operator's own 157. 1.18 was measured against real captions, not single sentences.
        env.setdefault("MYVOICE_SPEED", "1.18")
        env.setdefault("MYVOICE_CFG", "0.3")   # chatterbox only; ignored by cosyvoice
        self.proc = subprocess.Popen(
            [str(self.run_sh), self.arm, "serve"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, env=env,
        )
        if not self._reply().get("ready"):            # blocks through model load, once
            raise RuntimeError("engine did not come up")

    def _reply(self) -> dict:
        """Next JSON line from the engine, skipping anything else on stdout.

        stdout is contractually JSON-only, but chatterbox's watermarker prints
        "loaded PerthNet (Implicit)..." straight to it during the first generate(). Skip
        non-JSON rather than trust the contract -- one stray print should not end a take.
        """
        while True:
            line = self.proc.stdout.readline()
            if not line:
                raise RuntimeError("engine closed its output")
            line = line.strip()
            if line.startswith("{"):
                try:
                    return json.loads(line)
                except json.JSONDecodeError:
                    pass

    def render(self, text: str, dest: Path) -> None:
        # One restart on a dead engine, then fail. Without this a single engine
        # death (a transient GPU squeeze during model load, say) poisons EVERY
        # later line in the session: proc is no longer None, so each render
        # writes into a dead pipe and the whole prewarm reports N-1 failures
        # for one real fault (observed 2026-08-31: 1 death -> 1227 "failed").
        for attempt in (1, 2):
            if self.proc is None or self.proc.poll() is not None:
                self.proc = None
                self._start()
            name = f"{os.getpid()}_{next(_counter)}.wav"
            try:
                self.proc.stdin.write(json.dumps({"text": text, "out": f"/renders/narrate/{name}"}) + "\n")
                self.proc.stdin.flush()
                reply = self._reply()
            except (BrokenPipeError, RuntimeError):
                if attempt == 1:
                    try:
                        self.proc.kill()
                    except Exception:  # noqa: BLE001
                        pass
                    self.proc = None
                    continue
                raise
            if not reply.get("ok"):
                raise RuntimeError(reply.get("error", "engine returned no clip"))
            shutil.move(str(self.out_dir / name), str(dest))
            return


def pick_engine(which: str, voice: str) -> Engine:
    """kokoro when its model is present, else piper. Never raise: see module docstring.

    myvoice is opt-in only -- it is never what `auto` picks, because it needs docker and a
    recorded reference clip that a plain checkout does not have.
    """
    if which == "myvoice":
        try:
            return Myvoice(voice)
        except Exception as e:  # noqa: BLE001
            # An explicitly requested engine that is missing must fail loudly: silently
            # narrating a take in the wrong voice is worse than not narrating it.
            raise SystemExit(f"narrate: myvoice requested but unavailable ({e})")
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
    # Engines with their own text frontend read the bare forms better than kokoro's
    # respellings, and the key covers `spoken`, so the two never share a cache entry.
    spoken = speakable(text, initialisms=engine.name != "myvoice")
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
