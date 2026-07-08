# Wardyn agent ~/.bashrc — sourced by the interactive `wardyn attach` shell
# (tmux→bash / bash -i). Its ONE job: never drop the operator into a workspace
# that isn't ready yet.
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
