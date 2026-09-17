#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# not-a-coding-agent.sh — installed as BOTH /usr/local/bin/claude and
# /usr/local/bin/codex in the AWS sign-in image.
#
# The operator who found this typed `claude` in the sign-in sandbox, got
# `command not found`, and concluded the run was misconfigured. Every inference
# was sound: nothing in the box said what the box was. `command not found` is
# the shell's answer to a typo; this is the image's answer to a reasonable
# question, and it is one line shorter than the mistake it prevents.
#
# Exits 1 on purpose: a script that shells out to `claude` here must fail, not
# look like it worked.
set -u

# DRAFT (M2 canon pending) — names what the box IS and where the coding agent
# lives, in the two sentences that would have ended this in seconds.
WARDYN_AWS_SSO_NOT_A_CODING_AGENT='This is the AWS sign-in sandbox, not a coding agent. Start a Claude Code run from New run.'

printf '%s\n' "$WARDYN_AWS_SSO_NOT_A_CODING_AGENT" >&2
exit 1
