#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# signin-pane.sh — the body of the `wardyn` tmux pane in the AWS sign-in sandbox.
#
# WHY THE SANDBOX RUNS THE PAIR ITSELF. `aws sso login` alone leaves the token in
# ~/.aws/sso/cache, where it dies with the container; wardyn-aws-sso is the half
# that uploads it. Until now the only thing that ran the two as one command was
# the console's login pane typing them into the attach PTY — so a human who
# opened the SAME run from the Runs list got a bare prompt, typed the obvious
# half, saw "Successfully logged into Start URL", and captured nothing. That is
# the worst available failure shape, so the IMAGE runs the pair now and every
# attach path (pane, Runs list, `wardyn attach`, ssh) joins the one session that
# is already running it (both drivers attach with `tmux new-session -A -s wardyn
# bash` — attachShell in internal/runner/docker/session.go and its k8s sibling).
#
# A SCRIPT FILE, not a `sh -c '…'` string in agent-run: the pane body quotes
# operator-facing prose, and the first apostrophe a future canon edit adds would
# end the quoting and split the command.
#
# ENV, NOT SOURCING: agent-run exports WARDYN_AWS_SSO_LOGIN_COMMAND and the three
# SELFRUN_* lines (deploy/images/aws-sso/login-hint.sh, one definition) before it
# creates the session, and the tmux server inherits that environment — the same
# channel claude-code's boot pane uses for its seed. The fallback below covers a
# human running this script by hand from an attached shell.
set -u

[[ -n "${WARDYN_AWS_SSO_LOGIN_COMMAND:-}" ]] || . /usr/local/lib/wardyn-attach-hint.sh

# THE BANNER IS THE FIRST ACT, BEFORE THE WAIT. Not cosmetic ordering: the
# console pane gives an old image a grace window and then types the pair itself
# (SELFRUN_GRACE_MS, harness-login-pane.tsx), and the ONLY thing that stops it is
# seeing this marker. Session prep is a measured ~18s (see attachShell's comment
# in internal/runner/docker/session.go), so a banner printed after the prep wait
# would arrive after the grace window: the pane would type the pair, tmux would
# BUFFER those bytes at a pane whose script is still waiting, and the trailing
# shell below would run them — a SECOND device code, minted while the human is
# still entering the first.
printf '%s\n' "${WARDYN_AWS_SSO_SELFRUN_BANNER:-}"

# The login needs the MITM CA and the materialised ~/.aws/config, both written by
# agent-run's shared prep, which runs AFTER this session is created (that
# ordering is deliberate — see the bootstrap in deploy/images/aws-sso/agent-run).
# Bounded like the attach shell's own wait: a failed prep still writes prep-done,
# so this cannot hang, and a prep that never finishes falls through to a login
# that reports its own error rather than a pane that says nothing forever.
_i=0
while [[ ! -f "${HOME}/.wardyn/prep-done" ]] && [[ $_i -lt 300 ]]; do sleep 1; _i=$((_i + 1)); done
unset _i

# ONCE. No re-arm loop: a retry mints a SECOND live device code while the human
# may still be entering the first, and `A && B` would re-run the login after a
# deterministic wardyn-aws-sso refusal (a pin contradiction) that a second login
# cannot fix. A device code that expires before anyone attaches (~600s) lands on
# the FAILED line, which names the command to run — honest, and rare: the console
# attaches within seconds of RUNNING.
if bash -c "$WARDYN_AWS_SSO_LOGIN_COMMAND"; then
    printf '%s\n' "${WARDYN_AWS_SSO_SELFRUN_DONE:-}"
else
    # shellcheck disable=SC2059 # the format IS the canon string; its one %s is the command to re-run
    printf "${WARDYN_AWS_SSO_SELFRUN_FAILED:-%s}\n" "$WARDYN_AWS_SSO_LOGIN_COMMAND"
fi

# DRAIN. Anything typed at this pane while the script ran — a console on an old
# version that typed the pair anyway, a human's keystrokes during the device-code
# wait — is sitting in the tty buffer, and `exec bash` below would execute it.
while IFS= read -r -t 0.2 -n 4096 _discard; do :; done
unset _discard

# The attach hint's "the sign-in did not start on its own" line is printed by
# EVERY interactive shell in this image, including the one below — without this
# marker a Runs-list user reads that false sentence right after a successful
# capture, re-runs the pair, and meets already_captured + the fail marker. The
# export covers this pane's own shell; set-environment covers a tmux WINDOW the
# human opens later, which inherits the session's environment and not this
# process's.
export WARDYN_AWS_SSO_SELFRAN=1
tmux set-environment -t wardyn WARDYN_AWS_SSO_SELFRAN 1 2>/dev/null || true

# Hand the pane over as a plain shell — NEVER `sleep`: holding the container open
# is `agent-run --idle`'s job, this pane's job is to stay usable and keep its
# scrollback (the device code, the portal's reply, the DONE/FAILED line) on
# screen for an attach that arrives late.
exec bash
