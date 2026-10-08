#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

import hashlib
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from image_downloads import check_file  # noqa: E402


class DownloadSafety(unittest.TestCase):
    proofs = []

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="wardyn-image-safety-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source, self.safe = self.root / "source", self.root / "safe"
        self.source.write_text('printf consumed >> "$PROOF_MARKER"\n')
        self.safe.write_text("safe checked bytes\n")
        self.a, self.b = self.root / "a", self.root / "b"
        self.marker = self.root / "consumed"
        self.fetch = self.root / "fetch"
        self.bad = "0" * 64
        self.hash_a = hashlib.sha256(self.source.read_bytes()).hexdigest()
        self.hash_b = hashlib.sha256(self.safe.read_bytes()).hexdigest()
        self.download = f"curl -fsSL file://{self.source} -o {self.a}"
        self.check_a = f'echo "{self.hash_a}  {self.a}" | sha256sum -c -'
        self.bad_check = f'echo "{self.bad}  {self.a}" | sha256sum -c -'
        self.check_b = f'echo "{self.hash_b}  {self.b}" | sha256sum -c -'
        self.use = f"sh {self.a}"
        self.config = self.root / "config"
        self.config.write_text(f"url=file://{self.source}\noutput={self.a}\nurl=file://{self.safe}\noutput={self.b}\n")

    def prove(self, name, body):
        for path in (self.a, self.b, self.marker, self.fetch):
            path.unlink(missing_ok=True)
        dockerfile = self.root / "Dockerfile"
        dockerfile.write_text("RUN " + body + "\n")
        error = None
        try:
            check_file(dockerfile)
        except ValueError as failure:
            error = str(failure)
        env = {**os.environ, "PROOF_MARKER": str(self.marker)}
        env.pop("I_MISSING_URL_PART", None)
        # The file:// payload only writes the private marker. curl, sh and
        # sha256sum are real so option/expansion/errexit semantics are exercised.
        result = subprocess.run(["/bin/sh", "-c", body], env=env, capture_output=True, text=True, timeout=10)
        proof = dict(name=name, body=body, guard_error=error, shell_exit=result.returncode,
                     consumed=self.marker.exists(), stdout=result.stdout, stderr=result.stderr)
        self.proofs.append(proof)
        return proof

    def test_unsafe_shell_forms(self):
        bad_tail = f"{self.download}; {self.bad_check}; {self.use}"
        split_url = f'url="file://{self.source} -o {self.a} file://{self.safe}"'
        config_url = f'url="-K{self.config}"'
        config_tail = f'curl "$url" -o {self.b}; {self.check_b}; {self.use}'
        cases = {
            "pipeline_errexit": f"set -e | cat; {bad_tail}",
            "wrapped_errexit": f"set -e; command set +e; {bad_tail}",
            "unquoted_url": f"set -e; {split_url}; curl $url -o {self.b}; {self.check_b}; {self.use}",
            "pending_substitution": f'set -e; {self.download}; curl "file://{self.safe}$({self.use})" -o {self.b}; {self.bad_check}; {self.check_b}',
            "dynamic_curl_command": f"set -e; empty=; curl${{empty}} -fsSL file://{self.source} -o {self.a}; {self.use}",
            "generated_shell": f'set -e; echo "{self.download}" > {self.fetch}; sh {self.fetch}; {self.use}',
            "pipeline_errexit_last": f"printf x | set -e; {bad_tail}",
            "pipeline_errexit_true": f"set -eu | true; {bad_tail}",
            "wrapped_errexit_option": f"set -e; command -p set +e; {bad_tail}",
            "wrapped_errexit_nested": f"set -e; command command set +e; {bad_tail}",
            "wrapped_eval": f"set -e; command eval 'set +e'; {bad_tail}",
            "unquoted_braced_url": f"set -e; {split_url}; curl ${{url}} -o {self.b}; {self.check_b}; {self.use}",
            "unquoted_url_suffix": f'set -e; extra=" -o {self.a} file://{self.safe}"; curl file://{self.source}$extra -o {self.b}; {self.check_b}; {self.use}',
            "default_substitution": f'set -e; {self.download}; curl "file://{self.safe}${{I_MISSING_URL_PART:-$({self.use})}}" -o {self.b}; {self.bad_check}; {self.check_b}',
            "output_substitution": f'set -e; {self.download}; curl file://{self.safe} -o "{self.b}$({self.use})"; {self.bad_check}; {self.check_b}',
            "option_substitution": f"set -e; {self.download}; curl -s$({self.use}) file://{self.safe} -o {self.b}; {self.bad_check}; {self.check_b}",
            "quoted_dynamic_command": f'set -e; empty=; "curl${{empty}}" -fsSL file://{self.source} -o {self.a}; {self.use}',
            "dynamic_middle": f'set -e; empty=; cu${{empty}}rl -fsSL file://{self.source} -o {self.a}; {self.use}',
            "dynamic_quoted_part": f'set -e; empty=; cu"$empty"rl -fsSL file://{self.source} -o {self.a}; {self.use}',
            "generated_printf": f'set -e; printf \'%s\\n\' "{self.download}" > {self.fetch}; sh {self.fetch}; {self.use}',
            "generated_variable": f'set -e; script="{self.download}"; printf \'%s\\n\' "$script" > {self.fetch}; sh {self.fetch}; {self.use}',
            "generated_dynamic_command": f'set -e; empty=; echo "cu${{empty}}rl -fsSL file://{self.source} -o {self.a}" > {self.fetch}; sh {self.fetch}; {self.use}',
            "generated_group": f'set -e; {{ echo "{self.download}"; }} > {self.fetch}; sh {self.fetch}; {self.use}',
            "quoted_url_option": f"set -e; {config_url}; {config_tail}",
            "conditional_url_assignment": f'set -e; {config_url}; false && url="file://{self.safe}"; {config_tail}',
            "conditional_url_or": f'set -e; {config_url}; true || url="file://{self.safe}"; {config_tail}',
            "conditional_url_branch": f'set -e; {config_url}; if false; then url="file://{self.safe}"; fi; {config_tail}',
            "url_invalidated_in_branch": f'set -e; url="file://{self.safe}"; if true; then {config_url}; fi; {config_tail}',
            "url_subshell_assignment": f'set -e; {config_url}; (url="file://{self.safe}"); {config_tail}',
        }
        for name, body in cases.items():
            with self.subTest(name=name):
                proof = self.prove(name, body)
                self.assertTrue(proof["consumed"], proof)
                self.assertIsNotNone(proof["guard_error"], proof)

    def test_supported_shell_forms_fail_closed(self):
        for mode in ["errexit", "and"]:
            for correct in [True, False]:
                with self.subTest(mode=mode, correct=correct):
                    check = self.check_a if correct else self.bad_check
                    commands = [self.download, check, self.use]
                    body = "set -e; " + "; ".join(commands) if mode == "errexit" else " && ".join(commands)
                    proof = self.prove(f"{mode}_checksum_{correct}", body)
                    self.assertIsNone(proof["guard_error"], proof)
                    self.assertEqual(proof["consumed"], correct, proof)
                    self.assertEqual(proof["shell_exit"] == 0, correct, proof)

    def test_quoted_url_and_download_batch(self):
        body = (f'set -eu; url="file://{self.source}"; curl "$url" -o {self.a}; '
                f'curl -o {self.b} -- "file://{self.safe}"; {self.check_a}; {self.check_b}; {self.use}')
        proof = self.prove("quoted_url_and_download_batch", body)
        self.assertIsNone(proof["guard_error"], proof)
        self.assertTrue(proof["consumed"], proof)
        self.assertEqual(proof["shell_exit"], 0, proof)

    def test_end_options_blocks_url_config(self):
        body = (f'set -e; url="-K{self.config}"; curl -o {self.b} -- "$url"; '
                f'{self.check_b}; {self.use}')
        proof = self.prove("end_options_blocks_url_config", body)
        self.assertIsNone(proof["guard_error"], proof)
        self.assertFalse(proof["consumed"], proof)
        self.assertNotEqual(proof["shell_exit"], 0, proof)


if __name__ == "__main__":
    unittest.main(verbosity=2)
