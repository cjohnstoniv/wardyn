#!/usr/bin/env bash
# Wardyn unified setup — ONE command that detects your host, asks only the choices
# a human must make, does what it can without sudo, and launches the right mode with
# the UI already reflecting reality.
#
# Two deployment shapes:
#   local (host mode)  — sandbox agents on YOUR machine with YOUR Claude login. wardynd
#                        runs as you, sees ~/.claude directly (no re-login), proxy-injects
#                        your live token (never a stale copy). Best for personal use.
#   team  (compose)    — wardynd runs sealed in a container as a shared service; each user
#                        brings brokered keys. Best for a multi-user server.
#
# Barriers (Fence/Wall/Vault) that need a package install (gVisor, Kata) require sudo —
# this script NEVER runs sudo silently. It detects what's present and prints the exact
# commands for anything missing, so you stay in control of privileged changes.
#
# Usage:  ./scripts/setup.sh            (interactive)
#         WARDYN_SETUP_MODE=local ./scripts/setup.sh   (non-interactive: local|team)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ── tiny UI helpers ──────────────────────────────────────────────────────────
if [ -t 1 ]; then B="\033[1m"; G="\033[32m"; Y="\033[33m"; C="\033[36m"; R="\033[0m"; else B=""; G=""; Y=""; C=""; R=""; fi
say()  { printf "%b\n" "$*"; }
ok()   { printf "  ${G}✓${R} %s\n" "$*"; }
warn() { printf "  ${Y}!${R} %s\n" "$*"; }
info() { printf "  ${C}·${R} %s\n" "$*"; }
hd()   { printf "\n${B}%s${R}\n" "$*"; }

# ── daemon selection: prefer a tier-capable native dockerd if one is present ──
# A dedicated native dockerd (own data-root) can register runsc/kata; Docker Desktop's
# managed engine resets custom runtimes on restart, so it's Fence-only. Honor an
# explicit DOCKER_HOST; else pick the wardyn socket if it exists, else the default.
pick_daemon() {
  if [ -n "${DOCKER_HOST:-}" ]; then echo "${DOCKER_HOST#unix://}"; return; fi
  for s in /run/wardyn-docker.sock /var/run/wardyn-docker.sock; do
    [ -S "$s" ] && { echo "$s"; return; }
  done
  echo /var/run/docker.sock
}
DSOCK="$(pick_daemon)"
export DOCKER_HOST="unix://${DSOCK}"

runtimes_json() { docker info -f '{{json .Runtimes}}' 2>/dev/null || echo '{}'; }

# ── DETECT ───────────────────────────────────────────────────────────────────
hd "Detecting your environment"
IS_WSL=false; grep -qiE 'microsoft|wsl' /proc/version 2>/dev/null && IS_WSL=true
$IS_WSL && info "WSL2 detected (host↔sandbox networking is split — the UI opens in your Windows browser)."

HAVE_DOCKER=false; docker version >/dev/null 2>&1 && HAVE_DOCKER=true
if $HAVE_DOCKER; then ok "docker reachable ($(basename "$DSOCK"))"; else warn "docker not reachable at $DSOCK — start Docker and re-run."; exit 1; fi

RT="$(runtimes_json)"
HAVE_RUNSC=false; case "$RT" in *'"runsc"'*) HAVE_RUNSC=true;; esac
HAVE_KATA=false;  case "$RT" in *'"kata'*)  HAVE_KATA=true;;  esac
HAVE_KVM=false;   [ -e /dev/kvm ] && HAVE_KVM=true
ok "Fence (runc) — always available"
$HAVE_RUNSC && ok "Wall (gVisor/runsc) — registered" || warn "Wall (gVisor/runsc) — not registered on this daemon"
if $HAVE_KATA; then ok "Vault (Kata microVM) — registered"; elif $HAVE_KVM; then warn "Vault (Kata) — not registered (but /dev/kvm present, so installable)"; else warn "Vault (Kata) — unavailable (no /dev/kvm)"; fi

HAVE_CLAUDE=false; command -v claude >/dev/null 2>&1 && HAVE_CLAUDE=true
CLAUDE_LOGGED_IN=false; [ -f "$HOME/.claude/.credentials.json" ] && CLAUDE_LOGGED_IN=true
# A Bedrock-configured Claude authenticates via AWS, NOT an OAuth login file — so
# an absent .credentials.json is not "not logged in" when Bedrock is in use. Detect
# it (CLAUDE_CODE_USE_BEDROCK in env or ~/.claude/settings.json, or an operator who
# already set the wardynd Bedrock config) so we don't wrongly demand `claude login`.
CLAUDE_BEDROCK=false
case "${CLAUDE_CODE_USE_BEDROCK:-}" in 1|true|TRUE|True|yes) CLAUDE_BEDROCK=true;; esac
if ! $CLAUDE_BEDROCK && [ -f "$HOME/.claude/settings.json" ] \
   && grep -Eq '"CLAUDE_CODE_USE_BEDROCK"[[:space:]]*:[[:space:]]*"?(1|true|yes)"?' "$HOME/.claude/settings.json" 2>/dev/null; then
  CLAUDE_BEDROCK=true
fi
WARDYN_BEDROCK_SET=false
[ -n "${WARDYN_BEDROCK_REGION:-}" ] && [ -n "${WARDYN_BEDROCK_MODEL:-}" ] && WARDYN_BEDROCK_SET=true
if $HAVE_CLAUDE && $CLAUDE_LOGGED_IN; then
  ok "Claude CLI logged in on this host ($(command -v claude)) — host mode can use it directly"
elif $CLAUDE_BEDROCK || $WARDYN_BEDROCK_SET; then
  ok "Claude is set up for AWS Bedrock on this host — model access is via Bedrock, not a Claude login"
  $WARDYN_BEDROCK_SET \
    || warn "Bedrock needs wardynd config: export WARDYN_BEDROCK_REGION + WARDYN_BEDROCK_MODEL (a cross-region inference-profile id), then add aws-access-key-id/aws-secret-access-key (or a bedrock-api-key) in the UI's Connect-a-model step."
elif $HAVE_CLAUDE; then
  warn "Claude CLI present but no model auth detected — run 'claude login', use an API key, or configure AWS Bedrock (export WARDYN_BEDROCK_REGION + WARDYN_BEDROCK_MODEL)."
else
  info "No Claude CLI on PATH (fine for team/compose mode with brokered keys)"
fi

# ── barrier install guidance (sudo — never run silently) ─────────────────────
missing_barriers=()
$HAVE_RUNSC || missing_barriers+=("Wall/gVisor")
{ $HAVE_KATA || ! $HAVE_KVM; } || missing_barriers+=("Vault/Kata")
if [ "${#missing_barriers[@]}" -gt 0 ]; then
  hd "Optional: install stronger barriers (needs sudo — run these yourself)"
  say "  Wardyn works now with the barriers above. To add the missing ones on a"
  say "  ${B}native dockerd${R} (not Docker Desktop), register the runtime in"
  say "  /etc/docker/daemon.json and restart docker. Examples:"
  $HAVE_RUNSC || say "    ${C}Wall (gVisor):${R} install runsc, then add  \"runsc\": { \"path\": \"/usr/bin/runsc\" }"
  { $HAVE_KATA || ! $HAVE_KVM; } || say "    ${C}Vault (Kata):${R}  install kata-containers, then add  \"kata\": { \"runtimeType\": \"io.containerd.kata.v2\" }"
  say "  (Skip for now — you can add them later and re-run this command.)"
fi

# ── DECIDE: the one genuine human choice — local vs team ─────────────────────
hd "How are you running Wardyn?"
MODE="${WARDYN_SETUP_MODE:-}"
if [ -z "$MODE" ]; then
  say "  ${B}1) Local${R}  — sandbox agents on THIS machine with your Claude login (recommended for personal use)"
  say "  ${B}2) Team${R}   — run Wardyn as a shared service; each user brings brokered keys"
  if [ -t 0 ]; then
    printf "  Choose [1]: "; read -r ans || ans=""
    case "$ans" in 2|team|Team) MODE=team;; *) MODE=local;; esac
  else
    MODE=local; info "non-interactive: defaulting to local (set WARDYN_SETUP_MODE=team to override)"
  fi
fi
ok "Mode: ${MODE}"

# ── ACT ──────────────────────────────────────────────────────────────────────
if [ "$MODE" = "team" ]; then
  hd "Bringing up the compose (team) stack"
  info "wardynd runs sealed in a container; the UI's 'Connect a model' step brokers per-user keys."
  exec ./scripts/up.sh up
fi

# local / host mode
hd "Setting up local (host) mode"
if $HAVE_CLAUDE && $CLAUDE_LOGGED_IN; then
  info "Staging your Claude login for the sandbox + generating the subscription ceiling…"
  # Default (sentinel) staging: the sandbox copy is inert; host mode proxy-injects a
  # live, host-refreshed token on the wire — so nothing sensitive stays resident and
  # it never goes stale. (Matches run-host.sh's inject-on default.)
  if ./scripts/stage-claude-creds.sh >/dev/null 2>&1; then
    ok "Claude subscription staged (proxy-inject) — the setup screen shows model access GREEN, no re-login."
  else
    warn "Could not stage creds (continuing; you can add an API key in the UI)."
  fi
elif $CLAUDE_BEDROCK || $WARDYN_BEDROCK_SET; then
  if $WARDYN_BEDROCK_SET; then
    ok "AWS Bedrock configured (region + model set). Finish by adding aws-access-key-id/aws-secret-access-key (or a bedrock-api-key) in the UI's Connect-a-model step."
  else
    warn "Claude uses AWS Bedrock here, but wardynd isn't pointed at it yet. Export WARDYN_BEDROCK_REGION + WARDYN_BEDROCK_MODEL (inference-profile id) and re-run, then add the AWS creds in the UI. No 'claude login' needed."
  fi
else
  warn "No host model auth — the setup screen will show 'needs setup' until you 'claude login', add an API key, or configure AWS Bedrock."
fi

# Ensure the local binaries + UI are built (host mode serves ./bin/wardynd + ui/dist).
if [ ! -x ./bin/wardynd ] || [ ! -f ./ui/dist/index.html ]; then
  hd "Building wardynd + UI (first run)"
  go build -tags docker -o bin/wardynd ./cmd/wardynd && go build -o bin/wardyn ./cmd/wardyn
  ( cd ui && pnpm build )
  ok "built"
fi

# Need a Postgres. Reuse the compose one (publishes 127.0.0.1:5432) if not already up.
if ! (exec 3<>/dev/tcp/127.0.0.1/5432) 2>/dev/null; then
  info "Starting Postgres (compose, loopback :5432)…"
  docker compose -f deploy/compose/docker-compose.yaml up -d postgres >/dev/null 2>&1 || true
fi
# Wait until it actually accepts connections — a FRESH volume runs initdb first, and
# wardynd exits if it connects mid-init ("unexpected EOF"). Poll pg_isready.
info "Waiting for Postgres to accept connections…"
pg_ok=false
for _ in $(seq 1 60); do
  if docker compose -f deploy/compose/docker-compose.yaml exec -T postgres pg_isready -U wardyn -d wardyn >/dev/null 2>&1; then
    pg_ok=true; break
  fi
  sleep 1
done
$pg_ok && ok "Postgres ready" || warn "Postgres not ready after 60s — wardynd may fail to connect; check 'docker compose ps postgres'."

URL="http://localhost:${WARDYN_UP_PORT:-8080}"
RUNDIR="${HOME}/.wardyn"; mkdir -p "$RUNDIR"
PIDFILE="${RUNDIR}/host-wardynd.pid"; LOGFILE="${RUNDIR}/host-wardynd.log"

# Already running? Don't double-launch a second wardynd on the same port.
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; then
  ok "Wardyn is already running (host mode) — PID $(cat "$PIDFILE"), UI ${URL}."
  ok "Stop it with:  make stop-host"
  exit 0
fi

# Guard: if Wardyn already answers on :8080 (started elsewhere, no pidfile), don't
# launch a doomed second wardynd that just logs "address already in use".
if [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)" = "200" ]; then
  ok "Wardyn is already responding at ${URL} — leaving it as-is. (Stop: make stop-host)"
  exit 0
fi

hd "Launching wardynd (host mode) in the background on ${URL}"
info "Runs as you, sees your Claude login, launches sandboxes on $(basename "$DSOCK")."
# Detached: nohup + setsid so it survives this shell; run-host.sh exec's wardynd, so
# the recorded PID IS wardynd. Logs stream to the logfile.
setsid nohup ./scripts/run-host.sh >"$LOGFILE" 2>&1 < /dev/null &
WPID=$!
echo "$WPID" > "$PIDFILE"

# Wait for healthy — but stop early if the process dies (so we surface the log).
healthy=false
for _ in $(seq 1 45); do
  if [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)" = "200" ]; then healthy=true; break; fi
  kill -0 "$WPID" 2>/dev/null || break
  sleep 1
done

if $healthy; then
  { command -v wslview  >/dev/null 2>&1 && wslview  "$URL"; } >/dev/null 2>&1 \
    || { command -v explorer.exe >/dev/null 2>&1 && explorer.exe "$URL"; } >/dev/null 2>&1 \
    || { command -v xdg-open >/dev/null 2>&1 && xdg-open "$URL"; } >/dev/null 2>&1 || true
  hd "Wardyn is up (host mode) — your terminal is free"
  ok "UI     ${URL}   (opening in your browser)"
  ok "PID    ${WPID}   (${PIDFILE})"
  ok "Logs   ${LOGFILE}   (tail -f to watch)"
  ok "Stop   make stop-host   (or: kill ${WPID})"
else
  rm -f "$PIDFILE"
  warn "wardynd did not become healthy — last log lines:"
  tail -n 15 "$LOGFILE" 2>/dev/null | sed 's/^/    /'
  warn "Full log: ${LOGFILE}"
  exit 1
fi
