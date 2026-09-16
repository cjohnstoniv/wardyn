# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# THE ONE CHAINED LOGIN COMMAND for the aws-sso image, and the attach shell's
# hint that prints it.
#
# It had three copies: the console pane types it into the attach PTY
# (LOGIN_FLOWS.aws.cmd, ui/src/app/components/screens/settings/harness-login-pane.tsx),
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

# INTERACTIVE SHELLS ONLY. agent-run sources this file for the variable and must
# print nothing while doing it; the attach shell is the one that needs the hint.
case $- in
  *i*) printf '\033[36mℹ AWS sign-in sandbox — the AWS CLI and nothing else. Run: %s\033[0m\n' "$WARDYN_AWS_SSO_LOGIN_COMMAND" ;;
esac
