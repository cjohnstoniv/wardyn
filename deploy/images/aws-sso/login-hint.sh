# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# THE ONE CHAINED LOGIN COMMAND for the aws-sso image, and the attach shell's
# hint that prints it.
#
# It had three copies: the console pane types it into the attach PTY
# (LOGIN_FLOWS.aws.cmd, ui/src/app/components/screens/settings/login-flows.tsx),
# agent-run --idle echoed it to the container's stdout (where no human attaching
# later ever sees it), and a human who opened the run from /runs got a bare
# prompt with no hint at all — so the reasonable next move was `aws sso login`
# ALONE, which leaves the token in ~/.aws/sso/cache to die with the container.
# wardyn-aws-sso is what uploads it; the two are one command or the sign-in
# captures nothing.
#
# Now one definition, sourced by both agent-run and the interactive attach shell
# (deploy/images/common/attach-bashrc.sh), and pinned against the console's copy
# by TestLoginCommand_UIParity (cmd/wardyn-aws-sso) — the same source-parse
# idiom TestSuccessMarker_UIParity uses for the helper's success marker, because
# Go, TypeScript and shell cannot share a constant.
WARDYN_AWS_SSO_LOGIN_COMMAND='aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso'

# DRAFT (M2 canon pending) — the sandbox RUNS the pair itself now
# (deploy/images/aws-sso/signin-pane.sh, started by agent-run --idle before its
# own prep), so these four lines are the sign-in's whole narration in the pane
# every attach path joins. BANNER is printed BEFORE the prep wait, and its first
# words are the console's SELFRUN_MARKER: seeing them is the one thing that stops
# a console on an older version typing the pair a second time.
#
# FAILED's `%s` is substituted by PARAMETER EXPANSION in the pane, never by
# handing the string to printf as a FORMAT: a canon table is prose under review,
# and the first edit that adds a second `%` (a literal "100%", a second
# placeholder) would print garbage or swallow the command outright.
#
# PREP_STUCK is NOT a variant of FAILED, which is why it is its own line: FAILED
# names a command to re-run, and while prep is hung that command fails
# identically. This one names the only move that works — a new run — and
# deliberately carries no `%s`.
WARDYN_AWS_SSO_SELFRUN_BANNER='wardyn: sign-in running — AWS sign-in sandbox. Finish the device-code step in your browser. Nothing else runs here.'
WARDYN_AWS_SSO_SELFRUN_DONE='wardyn: sign-in command finished — this pane is now a plain shell.'
WARDYN_AWS_SSO_SELFRUN_FAILED='wardyn: sign-in did not complete — start a new sign-in from Getting Started, or run: %s'
WARDYN_AWS_SSO_SELFRUN_PREP_STUCK='wardyn: workspace preparation did not finish — stop this run and start a new sign-in.'

# EXPORTED, because the sign-in pane is a separate process: `agent-run --idle`
# creates the tmux session and the tmux server inherits this environment, which
# is how signin-pane.sh gets the command and the lines above without a second
# copy of any of them.
export WARDYN_AWS_SSO_LOGIN_COMMAND \
       WARDYN_AWS_SSO_SELFRUN_BANNER \
       WARDYN_AWS_SSO_SELFRUN_DONE \
       WARDYN_AWS_SSO_SELFRUN_FAILED \
       WARDYN_AWS_SSO_SELFRUN_PREP_STUCK

# INTERACTIVE SHELLS ONLY. agent-run sources this file for the variables and must
# print nothing while doing it; the attach shell is the one that needs the hint.
#
# …AND ONLY WHEN THE SIGN-IN DID NOT RUN ITSELF. This file is sourced by EVERY
# interactive shell in the image — including the plain shell signin-pane.sh execs
# when the sign-in is over. Unguarded, a Runs-list user reads "run this command"
# immediately after a SUCCESSFUL capture, runs it, and meets an already_captured
# refusal plus the console's fail marker on a sign-in that worked.
case $- in
  *i*)
    if [ -z "${WARDYN_AWS_SSO_SELFRAN:-}" ]; then
      printf '\033[36mℹ AWS sign-in sandbox — nothing else runs here. The sign-in did not start on its own; run: %s\033[0m\n' "$WARDYN_AWS_SSO_LOGIN_COMMAND"
    fi
    ;;
esac
