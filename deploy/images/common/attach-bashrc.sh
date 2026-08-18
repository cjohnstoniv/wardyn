# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Wardyn agent ~/.bashrc — sourced by the interactive `wardyn attach` shell
# (tmux→bash / bash -i), and by the fallback shell a seeded run's boot pane execs
# once its seed finishes (agent-run --boot-seed). Its ONE job: never drop the
# operator into a workspace that isn't ready yet.
#
# An interactive/record run's main process (`agent-run --idle`) clones the repo
# THEN idles, but the attach shell becomes available the instant the container is
# up — so a shell that lands mid-clone would show an empty ~/work (a confusing
# "lost clone"). agent-run --idle writes ~/.wardyn/workdir (the resolved workspace
# dir) and, LAST, touches ~/.wardyn/prep-done. Here we wait briefly for that marker
# and cd into the prepared workspace, so the operator always lands in a ready repo.
#
# Defensive throughout: no `set -e`, every step guarded, and a bounded wait — a
# failed clone still writes prep-done (agent-run continues past clone failure), so
# this never hangs. No-op for runs with no repo to prepare (no marker, no WARDYN_REPOS).

# Only for interactive shells (attach); skip scripts/non-interactive execs.
case $- in *i*) ;; *) return 2>/dev/null || true ;; esac

if [ -n "${WARDYN_REPOS:-}${WARDYN_REPO_URL:-}" ] || [ -f "$HOME/.wardyn/workdir" ]; then
  if [ ! -f "$HOME/.wardyn/prep-done" ]; then
    printf '\033[36m⏳ Preparing workspace (cloning the repo)… one moment.\033[0m\n'
    _i=0
    while [ ! -f "$HOME/.wardyn/prep-done" ] && [ "$_i" -lt 120 ]; do sleep 1; _i=$((_i+1)); done
    unset _i
  fi
  _wd="$(cat "$HOME/.wardyn/workdir" 2>/dev/null)"
  if [ -n "$_wd" ] && [ -d "$_wd" ]; then
    [ "$PWD" = "$_wd" ] || cd "$_wd" 2>/dev/null
    printf '\033[32m✓ workspace ready:\033[0m %s\n' "$_wd"
  fi
  unset _wd
fi

# WARDYN_INTERACTIVE_START=agent (CreateRunRequest.interactive_start): the
# operator asked to land IN the agent CLI rather than at a bare prompt. Placed
# after the cd above so the agent starts in the prepared workspace, not $HOME.
#
# RUN it, never exec — quitting the agent drops back to this shell in the same
# workspace instead of ending the attach session.
#
# The marker makes this a FIRST-SHELL affordance rather than a per-shell one: a
# new tmux window/pane and a second (SSH) attach each get a plain shell. Note
# the common case needs no marker at all — the attach chain is `tmux
# new-session -A -s wardyn`, so a second attach REJOINS the first session and
# never sources this file again; the marker covers the shells tmux spawns
# itself.
#
# The command is still resolved from a fixed list against PATH, never from the
# request: each image ships exactly one of these, so no client-supplied text is
# parsed by THIS file.
#
# What this comment used to promise, and no longer can: that no client text
# reached a shell in this container AT ALL. A run may now carry a SEED (its task
# text, per interactive_start) — an initial prompt for the agent, or a startup
# command. That path deliberately does not run here: `agent-run --idle` starts it
# at boot inside the `wardyn` tmux session and pre-writes the agent-started
# marker, so the guard below skips and the human's attach JOINS that live session
# rather than launching a second agent over it.
#
# The seed stays bounded on the far side of that door: it reaches only the run
# creator's own sandbox, as the agent user, inside the same confinement and
# default-deny egress envelope; it rides the env (WARDYN_INTERACTIVE_SEED), never
# a command string; the agent form is a single quoted argv with no shell parse;
# the shell form is a deliberate parse of the operator's own startup command,
# granting nothing they don't already have the moment they attach and type; and
# server-launched reserved tasks are excluded control-plane-side. The full
# invariant lives at the --boot-seed branch in each image's agent-run.
if [ "${WARDYN_INTERACTIVE_START:-}" = "agent" ] && [ ! -e "$HOME/.wardyn/agent-started" ]; then
  mkdir -p "$HOME/.wardyn" 2>/dev/null
  : > "$HOME/.wardyn/agent-started" 2>/dev/null
  for _c in claude codex; do
    if command -v "$_c" >/dev/null 2>&1; then
      printf '\033[36m▸ starting %s — exit it for a shell.\033[0m\n' "$_c"
      "$_c"
      break
    fi
  done
  unset _c
fi
