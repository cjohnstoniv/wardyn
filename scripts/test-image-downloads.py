#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from image_downloads import check_file  # noqa: E402


HASH = "a" * 64
DOWNLOAD = "curl -fsSL https://example.invalid/a.tgz -o /tmp/a.tgz"
VERIFY = f'echo "{HASH}  /tmp/a.tgz" | sha256sum -c -'
USE = "tar -xzf /tmp/a.tgz"
VALIDATE = 'printf \'%s\' "$sum" | grep -zExq \'[0123456789abcdefABCDEF]{64}\''
VARIABLE_VERIFY = 'echo "$sum  /tmp/a.tgz" | sha256sum --strict -c -'


class ImageDownloads(unittest.TestCase):
    def check(self, text):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "Dockerfile"
            path.write_text(text)
            check_file(path)

    def test_supported(self):
        cases = {
            "errexit": f"RUN set -eu; {DOWNLOAD}; {VERIFY}; {USE}",
            "and": f"RUN {DOWNLOAD} && {VERIFY} && {USE}",
            "and_errexit": f"RUN set -e && {DOWNLOAD} && {VERIFY} && {USE}",
            "continuations_comments": f"RUN set -eu; \\\n  {DOWNLOAD}; \\\n# a Dockerfile comment does not terminate the RUN\n  {VERIFY}; \\\n  {USE}",
            "lowercase_tab": f"run\tset -e; {DOWNLOAD}; {VERIFY}",
            "long_output": f"RUN curl --fail --location https://example.invalid/a --output /tmp/a.tgz && {VERIFY}",
            "equals_output": f"RUN curl --output=/tmp/a.tgz https://example.invalid/a && {VERIFY}",
            "joined_output": f"RUN curl -o/tmp/a.tgz https://example.invalid/a && {VERIFY}",
            "bundled_output": f"RUN curl -fsSLo /tmp/a.tgz https://example.invalid/a && {VERIFY}",
            "remote": f'RUN curl -fSLO https://example.invalid/a.tgz && echo "{HASH}  a.tgz" | sha256sum -c - && tar -xzf a.tgz',
            "remote_long": f'RUN curl --remote-name https://example.invalid/a.tgz && echo "{HASH}  ./a.tgz" | sha256sum --check -',
            "variables": f'RUN set -eu; url="https://example.invalid/$VERSION"; sum={HASH}; curl "$url" -o /tmp/a.tgz; echo "${{sum}}  /tmp/a.tgz" | sha256sum -c -; {USE}',
            "printf": f"RUN {DOWNLOAD} && printf '%s\\n' '{HASH}  /tmp/a.tgz' | sha256sum -c -",
            "branches": f"RUN set -eu; if [ -f /staged ]; then cp /staged /tmp/a.tgz; else {DOWNLOAD}; {VERIFY}; fi; {USE}",
            "case": f"RUN set -eu; case $arch in amd64) sum={HASH};; *) exit 1;; esac; {DOWNLOAD}; {VERIFY}; {USE}",
            "refusal_group": f"RUN set -eu; test -n \"$VERSION\" || {{ echo missing >&2; exit 1; }}; {DOWNLOAD}; {VERIFY}",
            "multiple": f"RUN set -eu; {DOWNLOAD}; curl -fsSL https://example.invalid/b -o /tmp/b; {VERIFY}; echo '{HASH}  /tmp/b' | sha256sum -c -; {USE}",
            "redownload": f"RUN set -eu; {DOWNLOAD}; {VERIFY}; {USE}; {DOWNLOAD}; {VERIFY}",
            "package": "RUN apt-get update && apt-get install -y curl ca-certificates",
            "package_shell_options": "RUN set -euxo pipefail; apt-get install -y curl ca-certificates",
            "comment_shell_options": "RUN echo 'curl downloads need checks'; set +e",
            "comment": "# RUN curl https://example.invalid | sh\nRUN echo ok # curl is a package",
            "echo": "RUN echo 'curl downloads need checks; | &&'",
            "stderr_echo": "RUN echo 'curl downloads need checks' >&2",
            "quoted_semicolon": f"RUN set -e; echo ';'; {DOWNLOAD}; {VERIFY}",
            "url_end_options": f'RUN curl -o /tmp/a.tgz -- "$URL" && {VERIFY}',
            "url_prefix_inherited": f'RUN set -e; base="https://example.invalid"; url="$base/$VERSION"; curl "$url" -o /tmp/a.tgz; {VERIFY}',
            "url_prefix_retained_in_case": f'RUN set -e; url="https://example.invalid/a"; case $arch in amd64) sum={HASH};; *) exit 1;; esac; curl "$url" -o /tmp/a.tgz; {VERIFY}',
            "validated_hash": f"RUN set -e; {VALIDATE}; {DOWNLOAD}; {VARIABLE_VERIFY}",
            "validated_hash_and": f"RUN {VALIDATE} && {DOWNLOAD} && {VARIABLE_VERIFY}",
            "validated_hash_branch": f"RUN set -e; if true; then {VALIDATE}; {DOWNLOAD}; {VARIABLE_VERIFY}; fi",
            "literal_uppercase_hash": f'RUN set -e; sum={HASH.upper()}; {DOWNLOAD}; {VARIABLE_VERIFY}',
            "strict_literal_hash": f"RUN {DOWNLOAD} && " + VERIFY.replace("sha256sum -c", "sha256sum --strict -c"),
            "strict_long_check": f"RUN {DOWNLOAD} && " + VERIFY.replace("sha256sum -c", "sha256sum --strict --check"),
        }
        for name, text in cases.items():
            with self.subTest(name=name):
                self.check(text)

    def test_rejected(self):
        cases = {
            "unchecked": (f"RUN set -e; {DOWNLOAD}", "same-file checksum"),
            "no_errexit": (f"RUN {DOWNLOAD}; {VERIFY}; {USE}", "fail closed"),
            "later_run": (f"RUN {DOWNLOAD}\nRUN {VERIFY}", "fail closed"),
            "after_use": (f"RUN set -e; {DOWNLOAD}; {USE}; {VERIFY}", "before another command"),
            "wrong_file": (f"RUN set -e; {DOWNLOAD}; " + VERIFY.replace("/tmp/a.tgz", "/tmp/b.tgz"), "does not match"),
            "before_download": (f"RUN set -e; {VERIFY}; {DOWNLOAD}", "does not match"),
            "second_unchecked": (f"RUN set -e; {DOWNLOAD}; curl https://example.invalid/b -o /tmp/b; {VERIFY}", "same-file checksum"),
            "second_use_early": (f"RUN set -e; {DOWNLOAD}; curl https://example.invalid/b -o /tmp/b; {VERIFY}; cat /tmp/b", "before another command"),
            "overwrite": (f"RUN set -e; {DOWNLOAD}; {DOWNLOAD}; {VERIFY}", "overwritten"),
            "ignored_check": (f"RUN set -e; {DOWNLOAD}; {VERIFY} || true; {USE}", "fail closed"),
            "ignored_download": (f"RUN set -e; {DOWNLOAD} || true; {VERIFY}; {USE}", "fail closed"),
            "nonterminal_and": (f"RUN set -e; {DOWNLOAD} && {VERIFY}; {USE}", "fail closed"),
            "broken_and": (f"RUN {DOWNLOAD} && {VERIFY}; {USE}", "fail closed"),
            "conditional_check": (f"RUN set -e; {DOWNLOAD}; if true; then {VERIFY}; fi; {USE}", "before another command"),
            "else_check": (f"RUN set -e; if true; then {DOWNLOAD}; else {VERIFY}; fi", "same-file checksum"),
            "case_check": (f"RUN set -e; case $arch in amd64) {DOWNLOAD};; *) {VERIFY};; esac", "same-file checksum"),
            "if_condition": (f"RUN set -e; if {DOWNLOAD}; then {VERIFY}; fi", "fail closed"),
            "checksum_condition": (f"RUN set -e; {DOWNLOAD}; if {VERIFY}; then {USE}; fi", "before another command"),
            "conditional_compound": (f"RUN set -e; if true; then {DOWNLOAD}; {VERIFY}; fi || true", "fail closed"),
            "piped_compound": (f"RUN set -e; if true; then {DOWNLOAD}; {VERIFY}; fi | cat; {USE}", "fail closed"),
            "group": (f"RUN set -e; {{ {DOWNLOAD}; {VERIFY}; }} || true; {USE}", "fail closed"),
            "subshell": (f"RUN set -e; ( {DOWNLOAD}; {VERIFY}; ) || true; {USE}", "fail closed"),
            "errexit_disabled": (f"RUN set -e; set +e; {DOWNLOAD}; {VERIFY}; {USE}", "unsupported set"),
            "trap": (f"RUN set -e; trap 'exit 0' EXIT; {DOWNLOAD}; {VERIFY}", "unsupported trap"),
            "stdout": ("RUN set -e; curl https://example.invalid/install", "stdout/direct consumption"),
            "stdout_dash": ("RUN set -e; curl -o - https://example.invalid/install", "filename"),
            "discarded_output": (f"RUN set -e; curl https://example.invalid/a -o /dev/null; echo '{HASH}  /dev/null' | sha256sum -c -", "regular artifact file"),
            "pipe": ("RUN curl https://example.invalid/install | sh", "pipeline"),
            "download_pipe": (f"RUN set -e; {DOWNLOAD} | cat; {VERIFY}", "pipeline"),
            "checksum_pipe": (f"RUN set -e; {DOWNLOAD}; {VERIFY} | cat; {USE}", "pipeline"),
            "substitution": ('RUN set -e; ver="$(curl https://example.invalid/stable)"', "command substitution"),
            "backtick": ('RUN set -e; ver="`curl https://example.invalid/stable`"', "backtick"),
            "redirect_stdout": ("RUN set -e; curl https://example.invalid/a > /tmp/a", "one URL"),
            "wrapper": (f"RUN set -e; command {DOWNLOAD}; {VERIFY}", "wrapper"),
            "absolute": (f"RUN set -e; /usr/bin/{DOWNLOAD}; {VERIFY}", "wrapper"),
            "nested_shell": ("RUN sh -c 'curl https://example.invalid/a -o /tmp/a'", "fail closed"),
            "unknown_wrapper": (f"RUN set -e; retry {DOWNLOAD}; {VERIFY}", "wrapper"),
            "generated_shell": ("RUN echo 'curl https://example.invalid/a -o /tmp/a' | sh", "pipeline"),
            "indirect_command": ("RUN set -e; dl='curl https://example.invalid/a -o /tmp/a'; $dl", "wrapper"),
            "run_flag": (f"RUN --mount=type=cache,target=/tmp {DOWNLOAD}", "unsupported RUN option"),
            "exec_form": ('RUN ["curl", "https://example.invalid/a", "-o", "/tmp/a"]', "exec-form"),
            "heredoc": ("RUN <<EOF\ncurl https://example.invalid/a -o /tmp/a\nEOF", "heredoc"),
            "dynamic_output": ('RUN set -e; curl https://example.invalid/a -o "$file"', "literal file"),
            "dynamic_remote": ('RUN set -e; curl -O "$url"', "literal file"),
            "multiple_urls": ("RUN set -e; curl -o /tmp/a https://example.invalid/a https://example.invalid/b", "one URL"),
            "multiple_outputs": ("RUN set -e; curl -o /tmp/a -o /tmp/b https://example.invalid/a", "outputs"),
            "unknown_option": (f"RUN set -e; {DOWNLOAD} --config /tmp/curlrc; {VERIFY}", "unsupported curl option"),
            "checksum_file": (f"RUN set -e; {DOWNLOAD}; sha256sum -c /tmp/checksums", "before another command"),
            "ignore_missing": (f"RUN set -e; {DOWNLOAD}; {VERIFY} --ignore-missing", "unsupported checksum options"),
            "loop": (f"RUN set -e; for url in a b; do {DOWNLOAD}; {VERIFY}; done", "unsupported for"),
            "custom_shell": (f'SHELL ["/bin/bash", "-c"]\nRUN set -e; {DOWNLOAD}; {VERIFY}', "custom SHELL"),
            "custom_escape": (f"# escape=`\nRUN set -e; {DOWNLOAD}; {VERIFY}", "custom SHELL/escape"),
            "quoted_command": ('RUN set -e; cu"rl" https://example.invalid/a -o /tmp/a', "same-file checksum"),
            "escaped_command": ('RUN set -e; cu\\rl https://example.invalid/a -o /tmp/a', "same-file checksum"),
            "wrapped_set": (f"RUN set -e; command set +e; {DOWNLOAD}; {VERIFY}", "unsupported command"),
            "quoted_wrapper": (f'RUN set -e; com"mand" set +e; {DOWNLOAD}; {VERIFY}', "unsupported command"),
            "dynamic_wrapper": (f'RUN set -e; cmd=command; "$cmd" set +e; {DOWNLOAD}; {VERIFY}', "dynamic command name"),
            "builtin_wrapper": (f"RUN set -e; builtin set +e; {DOWNLOAD}; {VERIFY}", "unsupported builtin"),
            "initial_pipeline": (f"RUN set -e | cat; {DOWNLOAD}; {VERIFY}", "unsupported set"),
            "curl_dynamic_suffix": ('RUN set -e; empty=; curl$empty https://example.invalid/a -o /tmp/a', "dynamic command name"),
            "curl_dynamic_prefix": ('RUN set -e; empty=; ${empty}curl https://example.invalid/a -o /tmp/a', "dynamic command name"),
            "curl_dynamic_middle": ('RUN set -e; empty=; cu${empty}rl https://example.invalid/a -o /tmp/a', "dynamic command name"),
            "curl_dynamic_quoted_part": ('RUN set -e; empty=; cu"$empty"rl https://example.invalid/a -o /tmp/a', "dynamic command name"),
            "exec_dynamic_middle": ('RUN ["sh", "-c", "cu${empty}rl https://example.invalid/a -o /tmp/a"]', "exec-form"),
            "curl_unquoted_url": (f'RUN set -e; curl $URL -o /tmp/a.tgz; {VERIFY}', "unquoted URL expansion"),
            "curl_url_substitution": (f'RUN set -e; curl "https://example.invalid/$(cat /tmp/a)" -o /tmp/a.tgz; {VERIFY}', "command substitution"),
            "curl_output_substitution": (f'RUN set -e; curl https://example.invalid/a -o "/tmp/a$(cat /tmp/b)"; {VERIFY}', "command substitution"),
            "curl_parameter_operator": (f'RUN set -e; curl "${{URL:-https://example.invalid/a}}" -o /tmp/a.tgz; {VERIFY}', "unsupported expansion"),
            "curl_unquoted_glob": (f'RUN set -e; curl https://example.invalid/* -o /tmp/a.tgz; {VERIFY}', "unquoted glob"),
            "generated_file": ('RUN echo "curl https://example.invalid/a -o /tmp/a" > /tmp/fetch; sh /tmp/fetch', "redirection"),
            "generated_append": ('RUN printf "%s\\n" "curl https://example.invalid/a -o /tmp/a" >> /tmp/fetch; sh /tmp/fetch', "redirection"),
            "package_exec": ('RUN npm exec curl -- https://example.invalid/a -o /tmp/a', "wrapper"),
            "parent_mutating_expansion": (f'RUN set -e; echo "${{url:=-Kconfig}}"; {DOWNLOAD}; {VERIFY}', "mutating expansion"),
            "arithmetic_expansion": (f'RUN set -e; echo "$((x=1))"; {DOWNLOAD}; {VERIFY}', "mutating expansion"),
            "printf_variable_mutation": (f'RUN set -e; printf -v url "%s" -Kconfig; {DOWNLOAD}; {VERIFY}', "printf options"),
            "remote_variable_prefix": (f'RUN set -e; base="https://example.invalid"; curl -O "$base/a.tgz"; echo "{HASH}  a.tgz" | sha256sum -c -', "literal URL"),
            "unknown_hash_variable": (f"RUN set -e; {DOWNLOAD}; {VARIABLE_VERIFY}", "64-hex"),
            "short_hash_variable": (f"RUN set -e; sum=abc; {DOWNLOAD}; {VARIABLE_VERIFY}", "64-hex"),
            "unvalidated_hash_source": (f'RUN set -e; sum="$EXPECTED"; {DOWNLOAD}; {VARIABLE_VERIFY}', "64-hex"),
            "wrong_validated_hash": (f"RUN set -e; {VALIDATE.replace('$sum', '$other')}; {DOWNLOAD}; {VARIABLE_VERIFY}", "64-hex"),
            "line_hash_validation": (f"RUN set -e; {VALIDATE.replace('-zExq', '-Exq')}; {DOWNLOAD}; {VARIABLE_VERIFY}", "pipeline"),
            "conditional_hash_validation": (f"RUN set -e; if true; then {VALIDATE}; fi; {DOWNLOAD}; {VARIABLE_VERIFY}", "64-hex"),
            "ignored_hash_validation": (f"RUN set -e; {VALIDATE} || true; {DOWNLOAD}; {VARIABLE_VERIFY}", "fail closed"),
            "conditional_hash_assignment": (f"RUN set -e; false && sum={HASH}; {DOWNLOAD}; {VARIABLE_VERIFY}", "64-hex"),
            "hash_reassignment": (f'RUN set -e; {VALIDATE}; sum="$OTHER"; {DOWNLOAD}; {VARIABLE_VERIFY}', "64-hex"),
            "hash_branch_reassignment": (f'RUN set -e; {VALIDATE}; if true; then sum="$OTHER"; fi; {DOWNLOAD}; {VARIABLE_VERIFY}', "64-hex"),
            "unquoted_hash_validation": ("RUN set -e; " + VALIDATE.replace('"', '') + f"; {DOWNLOAD}; {VARIABLE_VERIFY}", "double-quoted variable"),
            "late_hash_validation": (f"RUN set -e; {DOWNLOAD}; {VALIDATE}; {VARIABLE_VERIFY}", "before another command"),
            "unquoted_hash_record": (f"RUN set -e; sum={HASH}; {DOWNLOAD}; echo $sum\\ \\ /tmp/a.tgz | sha256sum -c -", "double-quoted"),
        }
        for name, (text, diagnostic) in cases.items():
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, diagnostic):
                self.check(text)


if __name__ == "__main__":
    unittest.main(verbosity=2)
