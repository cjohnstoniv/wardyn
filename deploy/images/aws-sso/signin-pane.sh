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

# `set -u` and $HOME in the same script: every expansion below has to be one this
# file guarantees. agent-run exports HOME on the create path, but on the RESPAWN
# path this pane's environment is the ATTACH exec's — and with HOME unset the
# script died at the prep wait with "HOME: unbound variable", AFTER the banner.
# The console had already seen SELFRUN_MARKER and will never type, so the pane
# just closed: precisely the silent shape this file exists to remove. Same
# default agent-run itself uses.
: "${HOME:=/home/agent}"; export HOME

# UNCONDITIONALLY, so all five strings are image-baked. agent-run exports them and
# the tmux server inherits that environment — but on the respawn path the server
# was created by the ATTACH exec, whose environment is the container's, not
# agent-run's. Nothing puts a caller's text in there today (the login launch is
# server-built and the roster's values travel as a base64 config FILE, never as a
# command string), and re-reading the file is what keeps it that way: the command
# this script hands to `bash -c` is always the one baked into the image.
[ -r /usr/local/lib/wardyn-attach-hint.sh ] && . /usr/local/lib/wardyn-attach-hint.sh

# THE BANNER IS THE FIRST ACT, BEFORE THE WAIT. Not cosmetic ordering: the
# console pane gives an old image a grace window and then types the pair itself
# (SELFRUN_GRACE_MS, harness-login-pane.tsx), and the ONLY thing that stops it is
# seeing this marker. Session prep is a measured ~18s (see attachShell's comment
# in internal/runner/docker/session.go), so a banner printed after the prep wait
# would arrive after the grace window: the pane would type the pair, tmux would
# BUFFER those bytes at a pane whose script is still waiting, and the trailing
# shell below would run them — a SECOND device code, minted while the human is
# still entering the first.
#
# CLEARED FIRST, on both paths. On the RESPAWN path (`respawn-pane -k`, when an
# attach won the session name) this pane has already run a bare interactive bash,
# which printed the attach hint's "the sign-in did not start on its own; run: …"
# line — untrue the instant this script starts, and sitting directly above the
# banner that says otherwise. On the create path the pane is empty and this is a
# no-op. The escape clears screen + scrollback for the terminal, `clear-history`
# for tmux's own buffer; both best-effort, neither load-bearing.
printf '\033[H\033[2J\033[3J'
tmux clear-history 2>/dev/null || true
printf '%s\n' "${WARDYN_AWS_SSO_SELFRUN_BANNER:-}"

# AND THE SESSION IS MARKED AS SELF-RUN HERE, NOT AT THE END. The attach hint's
# "the sign-in did not start on its own" line fires in every interactive shell in
# this image, and tmux's prefix is intact — so a human who opens a second WINDOW
# or a split WHILE the login is in flight would read that line, believe it, and
# run the pair a second time. "Started" is already true at this point, which is
# the only thing the line is about. A new window inherits the SESSION environment,
# not this process's, which is why it takes `set-environment` and not the `export`
# below.
tmux set-environment -t wardyn WARDYN_AWS_SSO_SELFRAN 1 2>/dev/null || true

# The login needs the MITM CA and the materialised ~/.aws/config, both written by
# agent-run's shared prep, which runs AFTER this session is created (that
# ordering is deliberate — see the bootstrap in deploy/images/aws-sso/agent-run).
# Bounded like the attach shell's own wait: a failed prep still writes prep-done,
# so this cannot hang. A prep that never finishes is a DIFFERENT outcome, not a
# late one — see the PREP_STUCK arm below, which skips the pair rather than
# running it against a workspace that has no CA and no ~/.aws/config.
_i=0
while [[ ! -f "${HOME}/.wardyn/prep-done" ]] && [[ $_i -lt 300 ]]; do sleep 1; _i=$((_i + 1)); done
unset _i

# print_failed — the FAILED line with its ONE %s replaced by the command, by
# PARAMETER EXPANSION rather than by handing the canon string to printf as a
# FORMAT. Three reasons it is not `printf "$FAILED\n" "$CMD"`: a canon edit that
# adds a literal `%` prints garbage ("printf: CMD: invalid number"), one that
# drops the `%s` makes the command vanish with no error at all, and printf is the
# wrong tool for substituting into prose. And not bash's `${FAILED/\%s/$CMD}`
# either: since bash 5.2 an unescaped `&` in the REPLACEMENT expands to the
# matched text, and the command is `aws sso login … && wardyn-aws-sso` — that
# form prints `… --use-device-code %s%s wardyn-aws-sso` (executed). Prefix +
# command + suffix is the one shape that is correct for every string.
print_failed() {
    local _f="${WARDYN_AWS_SSO_SELFRUN_FAILED:-%s}" _c="${WARDYN_AWS_SSO_LOGIN_COMMAND:-}"
    case "$_f" in
        *%s*) printf '%s%s%s\n' "${_f%%\%s*}" "$_c" "${_f#*\%s}" ;;
        *)    printf '%s\n' "$_f" ;;
    esac
}

# ONCE. No re-arm loop: a retry mints a SECOND live device code while the human
# may still be entering the first, and `A && B` would re-run the login after a
# deterministic wardyn-aws-sso refusal (a pin contradiction) that a second login
# cannot fix. A device code that expires before anyone attaches (~600s) lands on
# the FAILED line, which names the command to run — honest, and rare: the console
# attaches within seconds of RUNNING.
#
# NOT ALWAYS UNATTENDED: with no sso_account_id/sso_role_name pin on the roster
# row and more than one account or role reachable, wardyn-aws-sso asks WHICH ONE
# in this pane (chooseAccountRole, cmd/wardyn-aws-sso) and gives three tries. Only
# a WRITABLE attach can answer it — a read-only Runs-list viewer watches it time
# out. Bytes buffered at this pane before that prompt appears (a 0.7.4 console's
# auto-typed line, a human's stray keystroke) are read by the chooser as one of
# those tries; wrong answers are refused, not acted on.
#
# EVERY ARM ENDS AT THE `exec bash` BELOW — that is the invariant, not a
# convenience. The console has already seen SELFRUN_MARKER and stopped typing, so
# a pane that exits instead is a session destroyed (create path) or an attached
# client dropped (respawn path), with nothing said. Under `set -u` an unguarded
# expansion is exactly that: silent death after the banner.
if [[ ! -f "${HOME}/.wardyn/prep-done" ]]; then
    # Prep never finished. SKIP the pair rather than run it: it needs the MITM CA
    # and the materialised ~/.aws/config that prep writes, so it would fail —
    # safely (no CA means TLS verify fails closed, no [sso-session wardyn] means
    # the CLI errors, nothing is captured) but on an error about a missing
    # profile, ending on a FAILED line that names a command which fails
    # identically for as long as prep is hung. Its own line, naming the only move
    # that works.
    printf '%s\n' "${WARDYN_AWS_SSO_SELFRUN_PREP_STUCK:-}"
elif [[ -z "${WARDYN_AWS_SSO_LOGIN_COMMAND:-}" ]]; then
    # No command to run: the image's login-hint.sh was unreadable or defines
    # nothing. `bash -c ""` would "succeed" and print the DONE line over a
    # sign-in that never happened, and under `set -u` the bare expansion killed
    # the pane outright. Report it as the failure it is and hand over the shell.
    print_failed
elif bash -c "$WARDYN_AWS_SSO_LOGIN_COMMAND"; then
    printf '%s\n' "${WARDYN_AWS_SSO_SELFRUN_DONE:-}"
else
    print_failed
fi

# The session was marked self-run above; this covers THIS process's own shell,
# which is a child of it rather than a new client of the session.
export WARDYN_AWS_SSO_SELFRAN=1

# DRAIN, LAST. Anything typed at this pane while the script ran — a console on an
# old version that typed the pair anyway, a human's keystrokes during the
# device-code wait — is sitting in the tty buffer, and `exec bash` below would
# execute it. It sits immediately before the exec, with no fork (no `tmux`, no
# subshell) between: every command that follows a drain is another window in which
# bytes can arrive and survive into the shell.
while IFS= read -r -t 0.2 -n 4096 _discard; do :; done
unset _discard

# Hand the pane over as a plain shell — NEVER `sleep`: holding the container open
# is `agent-run --idle`'s job, this pane's job is to stay usable and keep its
# scrollback (the device code, the portal's reply, the DONE/FAILED line) on
# screen for an attach that arrives late.
exec bash
