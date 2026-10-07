#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from image_downloads import instructions  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
STUB = r'''#!/usr/bin/python3
import json, os, pathlib, shutil, subprocess, sys
root = pathlib.Path(os.environ['RECIPE_FIXTURE'])
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with (root / 'events').open('a') as log:
    log.write(json.dumps([name, *args]) + '\n')
if name == 'curl':
    url, output = args[1], pathlib.Path(args[3])
    source = 'manifest' if url.endswith('/manifest.json') else 'binary'
    if 'awscli.amazonaws.com/' in url:
        source = 'sig' if url.endswith('.sig') else 'zip'
    shutil.copyfile(root / source, output)
    if os.environ.get('CORRUPT') == source:
        with output.open('ab') as out: out.write(b'corrupt')
elif name == 'unzip':
    dest = pathlib.Path(args[-1]) / 'aws'
    dest.mkdir(parents=True)
    installer = dest / 'install'
    installer.write_text('#!/bin/sh\nprintf installed > "$RECIPE_FIXTURE/installed"\n')
    installer.chmod(0o755)
    (dest / 'THIRD_PARTY_LICENSES').write_text('fixture attribution')
elif name == 'gpg':
    sys.exit(subprocess.run([os.environ['REAL_GPG'], *args]).returncode)
'''


def recipe(name):
    text = (ROOT / f"deploy/images/{name}/Dockerfile").read_text()
    return next(body for _, kind, body in instructions(text) if kind == "RUN" and "curl -fsSL" in body)


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


class ImageRecipes(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.signing = tempfile.TemporaryDirectory(prefix="wardyn-image-signing-")
        cls.sign = Path(cls.signing.name)
        cls.gpg = shutil.which("gpg")
        if not cls.gpg:
            raise RuntimeError("gpg is required to test the recipe's signature path")
        subprocess.run([cls.gpg, "--homedir", str(cls.sign), "--batch", "--pinentry-mode", "loopback",
                        "--passphrase", "", "--quick-generate-key", "Wardyn fixture <fixture@example.invalid>",
                        "rsa1024", "sign", "0"], check=True, capture_output=True)
        (cls.sign / "zip").write_bytes(b"fixture AWS installer")
        subprocess.run([cls.gpg, "--homedir", str(cls.sign), "--batch", "--detach-sign", "--output",
                        str(cls.sign / "sig"), str(cls.sign / "zip")], check=True, capture_output=True)
        cls.key = subprocess.check_output([cls.gpg, "--homedir", str(cls.sign), "--armor", "--export"], stderr=subprocess.DEVNULL)

    @classmethod
    def tearDownClass(cls):
        cls.signing.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="wardyn-image-recipe-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for directory in ["bin", "claude-stage", "awscli-stage", "gpg", "lists"]:
            (self.root / directory).mkdir(mode=0o700)
        for command in ["curl", "unzip", "gpg", "npm", "apt-get"]:
            script = self.root / "bin" / command
            script.write_text(STUB)
            script.chmod(0o755)
        (self.root / "events").touch()
        (self.root / "binary").write_bytes(b"fixture native claude")
        (self.root / "manifest").write_text(json.dumps({"platforms": {
            plat: {"checksum": sha(self.root / "binary")} for plat in ["linux-x64", "linux-arm64"]
        }}, indent=2))
        for name in ["zip", "sig"]:
            shutil.copyfile(self.sign / name, self.root / name)
        (self.root / "aws-cli-pubkey.asc").write_bytes(self.key)
        self.env = {**os.environ, "PATH": f"{self.root / 'bin'}:{os.environ['PATH']}",
                    "RECIPE_FIXTURE": str(self.root), "REAL_GPG": self.gpg, "GNUPGHOME": str(self.root / "gpg"),
                    "TARGETARCH": "amd64", "CLAUDE_INSTALL": "native", "CLAUDE_CODE_VERSION": "9.9.9",
                    "CLAUDE_MANIFEST_SHA256": sha(self.root / "manifest"), "AWS_CLI_INSTALL": "download",
                    "AWS_CLI_VERSION": "9.9.9", "AWS_CLI_SHA256": sha(self.root / "zip"),
                    "AWS_CLI_SIG_SHA256": sha(self.root / "sig"), "NPM_REGISTRY": "", "HTTP_PROXY": "", "HTTPS_PROXY": ""}

    def run_recipe(self, name, **overrides):
        script = recipe(name)
        replacements = {
            "/tmp/claude-manifest.json": "/claude-manifest.json", "/usr/local/bin/claude": "/claude",
            "/tmp/awscliv2.zip": "/awscliv2.zip", "/tmp/awscliv2.sig": "/awscliv2.sig",
            "/tmp/awscli-install": "/awscli-install", "/usr/share/doc/aws-cli": "/aws-cli-doc",
            "/var/lib/apt/lists": "/lists", "/claude-stage": "/claude-stage", "/awscli-stage": "/awscli-stage",
            "/aws-cli-pubkey.asc": "/aws-cli-pubkey.asc",
        }
        for old, new in replacements.items():
            script = script.replace(old, str(self.root) + new)
        result = subprocess.run(["/bin/sh", "-c", script], env={**self.env, **overrides}, capture_output=True, text=True)
        self.events = [json.loads(line) for line in (self.root / "events").read_text().splitlines()]
        return result

    def test_claude_native_override(self):
        result = self.run_recipe("claude-code")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([event[0] for event in self.events], ["curl", "curl"])
        self.assertEqual((self.root / "claude").stat().st_mode & 0o777, 0o755)

    def test_claude_manifest_mismatch(self):
        result = self.run_recipe("claude-code", CORRUPT="manifest")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.events), 1)
        self.assertFalse((self.root / "claude").exists())

    def test_claude_binary_mismatch(self):
        result = self.run_recipe("claude-code", CORRUPT="binary")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "claude").stat().st_mode & 0o111, 0)

    def test_claude_missing_override(self):
        result = self.run_recipe("claude-code", CLAUDE_MANIFEST_SHA256="")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires CLAUDE_MANIFEST_SHA256", result.stderr)
        self.assertEqual(self.events, [])

    def test_claude_channel_refusal(self):
        for channel in ["stable", "latest"]:
            with self.subTest(channel=channel):
                result = self.run_recipe("claude-code", CLAUDE_CODE_VERSION=channel)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("requires an exact", result.stderr)
                self.assertEqual(self.events, [])

    def test_claude_staged_channel(self):
        (self.root / "claude-stage/claude-bin").write_bytes(b"staged native")
        result = self.run_recipe("claude-code", CLAUDE_CODE_VERSION="stable", CLAUDE_MANIFEST_SHA256="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "claude").read_bytes(), b"staged native")
        self.assertEqual(self.events, [])

    def test_claude_npm_channel(self):
        result = self.run_recipe("claude-code", CLAUDE_INSTALL="npm", CLAUDE_CODE_VERSION="stable", CLAUDE_MANIFEST_SHA256="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.events, [["npm", "install", "-g", "@anthropic-ai/claude-code@stable"]])

    def test_aws_override_keeps_gpg(self):
        result = self.run_recipe("aws-sso")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([event[0] for event in self.events], ["curl", "curl", "gpg", "gpg", "unzip", "apt-get"])
        self.assertTrue((self.root / "installed").exists())

    def test_aws_hash_refusals(self):
        for source in ["zip", "sig"]:
            with self.subTest(source=source):
                (self.root / "events").write_text("")
                result = self.run_recipe("aws-sso", CORRUPT=source)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([event[0] for event in self.events], ["curl", "curl"])
                self.assertFalse((self.root / "installed").exists())

    def test_aws_signature_refusal_after_valid_hashes(self):
        (self.root / "zip").write_bytes(b"tampered installer with a matching SHA override")
        result = self.run_recipe("aws-sso", AWS_CLI_SHA256=sha(self.root / "zip"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("BAD signature", result.stderr)
        self.assertEqual([event[0] for event in self.events], ["curl", "curl", "gpg", "gpg"])
        self.assertFalse((self.root / "installed").exists())

    def test_aws_missing_override(self):
        for missing in ["AWS_CLI_SHA256", "AWS_CLI_SIG_SHA256"]:
            with self.subTest(missing=missing):
                result = self.run_recipe("aws-sso", **{missing: ""})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("requires AWS_CLI_SHA256 and AWS_CLI_SIG_SHA256", result.stderr)
                self.assertEqual(self.events, [])

    def test_aws_staged(self):
        (self.root / "awscli-stage/awscliv2-bin").write_bytes(b"trusted staged installer")
        result = self.run_recipe("aws-sso", AWS_CLI_INSTALL="staged", AWS_CLI_SHA256="", AWS_CLI_SIG_SHA256="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([event[0] for event in self.events], ["unzip", "apt-get"])
        self.assertIn("trust the staging host", result.stdout)

    def test_aws_missing_stage(self):
        result = self.run_recipe("aws-sso", AWS_CLI_INSTALL="staged")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is not staged", result.stderr)
        self.assertEqual(self.events, [])

    def test_hash_override_shape(self):
        invalid = ["abc", "a" * 63, "a" * 65, "g" * 64, "a" * 32 + "\n" + "a" * 32,
                   "a" * 64 + "\n", " " + "a" * 64, "a" * 64 + " "]
        overrides = [("claude-code", "CLAUDE_MANIFEST_SHA256"),
                     ("aws-sso", "AWS_CLI_SHA256"), ("aws-sso", "AWS_CLI_SIG_SHA256")]
        for name, variable in overrides:
            for value in invalid:
                with self.subTest(recipe=name, variable=variable, value=repr(value)):
                    (self.root / "events").write_text("")
                    result = self.run_recipe(name, **{variable: value})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(self.events, [])

    def test_uppercase_hash_overrides(self):
        for name, variables in [("claude-code", ["CLAUDE_MANIFEST_SHA256"]),
                                ("aws-sso", ["AWS_CLI_SHA256", "AWS_CLI_SIG_SHA256"])]:
            with self.subTest(recipe=name):
                result = self.run_recipe(name, **{key: self.env[key].upper() for key in variables})
                self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
