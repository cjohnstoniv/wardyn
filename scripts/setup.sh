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
  ok "Claude is set up for AWS Bedrock on this host — model access is via Bedrock, not a Claude login (auto-configured below in local mode)."
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
  # Auto-detect the host's Bedrock setup and offer to configure Wardyn end to end —
  # region, model, and creds — so the operator doesn't hand-run `wardyn secret set`.
  br_region="${WARDYN_BEDROCK_REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-}}}"
  if [ -z "$br_region" ] && command -v aws >/dev/null 2>&1; then br_region="$(aws configure get region 2>/dev/null || true)"; fi
  # Model id: Claude Code on Bedrock sets one of these (env, or ~/.claude/settings.json's
  # env block). Prefer an explicit pin, then the everyday Sonnet default, then Opus/Haiku/
  # fast. A value may be a cross-region inference-profile id (us.anthropic.claude-…) OR an
  # application-inference-profile ARN (arn:aws:bedrock:…:inference-profile/…) — both are
  # accepted by wardynd; a bare foundation-model id is not.
  BR_MODEL_VARS="ANTHROPIC_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_HAIKU_MODEL ANTHROPIC_SMALL_FAST_MODEL"
  br_model="${WARDYN_BEDROCK_MODEL:-}"
  for _mv in $BR_MODEL_VARS; do [ -n "$br_model" ] && break; br_model="${!_mv:-}"; done
  if [ -z "$br_model" ] && [ -f "$HOME/.claude/settings.json" ]; then
    for _mv in $BR_MODEL_VARS; do
      [ -n "$br_model" ] && break
      # `|| true`: a no-match grep under `set -euo pipefail` would abort the installer.
      br_model="$(grep -oE "\"$_mv\"[[:space:]]*:[[:space:]]*\"[^\"]+\"" "$HOME/.claude/settings.json" 2>/dev/null | head -1 | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/' || true)"
    done
  fi
  # Cred style: a host ~/.aws (SSO or static-file) → read-only mount (no paste, SSO
  # auto-refreshes). Else static creds in the env → import them as Wardyn secrets.
  br_mode=none
  if [ -d "$HOME/.aws" ]; then br_mode=mount
  elif [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then br_mode=static-env; fi
  say "  Detected AWS Bedrock for Claude:"
  say "    region: ${br_region:-<not found>}   model: ${br_model:-<not found>}   creds: ${br_mode}"
  auto=y
  if [ -t 0 ]; then printf "  Auto-configure Wardyn to use this Bedrock setup? [Y/n]: "; read -r a || a=""; case "$a" in n|N|no|No) auto=n;; esac
  else info "non-interactive: auto-configuring Bedrock (set WARDYN_SETUP_MODE / vars to control)"; fi
  if [ "$auto" = y ]; then
    if [ -z "$br_model" ] && [ -t 0 ]; then printf "  Bedrock model id (cross-region inference-profile, e.g. us.anthropic.claude-sonnet-4-5-...): "; read -r br_model || br_model=""; fi
    if [ -z "$br_region" ] && [ -t 0 ]; then printf "  AWS region (e.g. us-east-1): "; read -r br_region || br_region=""; fi
    if [ -n "$br_region" ] && [ -n "$br_model" ]; then
      export WARDYN_BEDROCK_REGION="$br_region" WARDYN_BEDROCK_MODEL="$br_model"
      # These are boot-time wardynd config, so a wardynd that's ALREADY running
      # must be restarted to pick them up — otherwise the config is a no-op.
      MODEL_CONFIG_APPLIED=true
      case "$br_mode" in
        mount)
          export WARDYN_BEDROCK_AWS_DIR="$HOME/.aws"
          [ -n "${AWS_PROFILE:-}" ] && export WARDYN_BEDROCK_AWS_PROFILE="$AWS_PROFILE"
          ok "Bedrock via read-only ~/.aws mount — the sandbox SDK resolves your creds (SSO auto-refreshes). Nothing stored, nothing to paste."
          [ "$(id -u)" = "1000" ] || warn "Host uid $(id -u) ≠ sandbox agent uid 1000 — the agent may not read 0600 files under ~/.aws. If a run can't auth, use static keys or run 'chmod -R a+rX ~/.aws'."
          ;;
        static-env)
          BR_IMPORT_STATIC=true  # values live in the env; imported after wardynd is healthy
          ok "Bedrock region+model set — your AWS_ACCESS_KEY_ID/SECRET will be imported as Wardyn secrets once the daemon is up."
          ;;
        none)
          warn "Region+model set, but no ~/.aws dir and no AWS creds in the env. Run 'aws sso login' (or 'aws configure'), or add creds in the UI's Connect-a-model step."
          ;;
      esac
    else
      warn "Bedrock needs both a region and a model id — skipping auto-config; set them in the UI's Connect-a-model step."
    fi
  else
    info "Skipped Bedrock auto-config — configure it in the UI's Connect-a-model step."
  fi
else
  warn "No host model auth — the setup screen will show 'needs setup' until you 'claude login', add an API key, or configure AWS Bedrock."
fi

# Ensure the local binaries + UI are built (host mode serves ./bin/wardynd + ui/dist).
if [ ! -x ./bin/wardynd ] || [ ! -f ./ui/dist/index.html ]; then
  hd "Building wardynd + UI (first run)"
  # go.mod pins `toolchain go1.26.4`, so the default GOTOOLCHAIN=auto tries to
  # FETCH that toolchain — which a corporate proxy that blocks the public Go
  # proxy denies, even though the locally-installed Go already satisfies the
  # `go 1.26` directive. Retry with GOTOOLCHAIN=local (use the installed Go, no
  # download) so a proxied/offline host still builds.
  if ! (go build -tags docker -o bin/wardynd ./cmd/wardynd && go build -o bin/wardyn ./cmd/wardyn); then
    warn "go build failed (a proxy may be blocking the pinned toolchain fetch) — retrying with the locally-installed Go (GOTOOLCHAIN=local)…"
    GOTOOLCHAIN=local go build -tags docker -o bin/wardynd ./cmd/wardynd
    GOTOOLCHAIN=local go build -o bin/wardyn ./cmd/wardyn
  fi
  # `pnpm install` first (a fresh clone has no node_modules; behind a corp
  # registry it also picks up ui/.npmrc) — matches the `make ui` target.
  ( cd ui && pnpm install && pnpm build )
  ok "built"
fi

# Persist a stable secret-store age key so secrets in Postgres survive restarts.
# Host mode reads WARDYN_AGE_KEY from deploy/compose/.env (run-host.sh); WITHOUT a
# persisted key, wardynd mints a throwaway one each run and then can't decrypt the
# secrets a prior run seeded ("age decrypt: no identity matched any of the
# recipients") — it fails to start. Mirror what the team path (scripts/up.sh) does.
AGE_ENV="deploy/compose/.env"
mkdir -p "$(dirname "$AGE_ENV")"
# The age key is the secret-store MASTER key (it decrypts every stored secret), so
# this file must be owner-only. Create it 0600 (umask) and hard-enforce 0600 even
# if it pre-existed with looser perms — before any secret is written to it.
(umask 077; touch "$AGE_ENV")
chmod 600 "$AGE_ENV" 2>/dev/null || true
if ! grep -qE '^WARDYN_AGE_KEY=AGE-SECRET-KEY-' "$AGE_ENV" 2>/dev/null; then
  _age="$(./bin/wardynd -gen-age-key 2>/dev/null | grep -E '^AGE-SECRET-KEY-' | head -1 || true)"
  if [ -n "$_age" ]; then
    printf 'WARDYN_AGE_KEY=%s\n' "$_age" >> "$AGE_ENV"
    ok "Minted a persistent secret-store age key → ${AGE_ENV} (secrets survive restarts)."
  else
    warn "Could not mint an age key; secrets may not persist across restarts."
  fi
fi

URL="http://localhost:${WARDYN_UP_PORT:-8080}"
RUNDIR="${HOME}/.wardyn"; mkdir -p "$RUNDIR"
PIDFILE="${RUNDIR}/host-wardynd.pid"; LOGFILE="${RUNDIR}/host-wardynd.log"

# ensure_postgres: bring up the compose Postgres (loopback :5432) and wait until
# it is HEALTHY. A FRESH volume runs initdb first (notably right after the stale-
# store `down -v` recovery below); wardynd exits if it connects mid-init.
# `up -d --wait` blocks on the service's compose healthcheck, which natively covers
# that slow first-run initdb — a fixed-length poll races it and a silent `up`
# failure would leave wardynd dialing a dead DB ("connection refused").
ensure_postgres() {
  info "Starting Postgres (compose, loopback :5432) and waiting for it to be healthy…"
  local out
  if out="$(docker compose -f deploy/compose/docker-compose.yaml up -d --wait --wait-timeout 150 postgres 2>&1)"; then
    ok "Postgres ready"; return 0
  fi
  # Network-ownership conflict: host-mode wardynd creates `wardyn-internal` itself
  # (not via compose), so a compose `up` that attaches postgres to it fails
  # instantly ("network ... was not created by compose"). If nothing is attached,
  # drop the stray network so compose can recreate it labeled, then retry once —
  # wardynd adopts compose's network on its next launch.
  case "$out" in
    *"not created by compose"*|*"incorrect label"*)
      if [ "$(docker network inspect wardyn-internal -f '{{len .Containers}}' 2>/dev/null || echo 1)" = "0" ]; then
        warn "Stray 'wardyn-internal' network (made by host-mode wardynd, not compose) — removing so compose can own it…"
        docker network rm wardyn-internal >/dev/null 2>&1 || true
        if out="$(docker compose -f deploy/compose/docker-compose.yaml up -d --wait --wait-timeout 150 postgres 2>&1)"; then
          ok "Postgres ready"; return 0
        fi
      fi
      ;;
  esac
  case "$out" in
    *unknown*flag*|*--wait*)
      # Pre-2.17 compose lacks --wait/--wait-timeout: fall back to a manual poll
      # (longer than before so a fresh-volume initdb on a slow disk fits).
      docker compose -f deploy/compose/docker-compose.yaml up -d postgres >/dev/null 2>&1 || true
      local ok_pg=false
      for _ in $(seq 1 120); do
        if docker compose -f deploy/compose/docker-compose.yaml exec -T postgres pg_isready -U wardyn -d wardyn >/dev/null 2>&1; then
          ok_pg=true; break
        fi
        sleep 1
      done
      $ok_pg && ok "Postgres ready" || warn "Postgres not ready after 120s — check 'docker compose -f deploy/compose/docker-compose.yaml ps postgres'."
      ;;
    *)
      warn "Postgres did not become healthy: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
      warn "Inspect: docker compose -f deploy/compose/docker-compose.yaml ps postgres && docker compose -f deploy/compose/docker-compose.yaml logs postgres"
      ;;
  esac
}

# launch_wardynd: start wardynd detached and wait for /healthz. Sets WPID + healthy.
# nohup + </dev/null detaches; setsid (Linux only) adds a fresh session (macOS lacks it).
launch_wardynd() {
  if command -v setsid >/dev/null 2>&1; then
    setsid nohup ./scripts/run-host.sh >"$LOGFILE" 2>&1 < /dev/null &
  else
    nohup ./scripts/run-host.sh >"$LOGFILE" 2>&1 < /dev/null &
  fi
  WPID=$!
  echo "$WPID" > "$PIDFILE"
  healthy=false
  for _ in $(seq 1 45); do
    if [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)" = "200" ]; then healthy=true; break; fi
    kill -0 "$WPID" 2>/dev/null || break
    sleep 1
  done
}

ensure_postgres

# Already running? A running wardynd won't pick up boot-time config we just set
# (e.g. the Bedrock region/model/mount env), so if we applied model config this
# run, RESTART it; otherwise leave it as-is and don't double-launch on the port.
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; then
  if [ "${MODEL_CONFIG_APPLIED:-false}" = true ]; then
    info "Applying the new model config — restarting the running host wardynd (PID $(cat "$PIDFILE"))…"
    kill "$(cat "$PIDFILE")" 2>/dev/null || true
    rm -f "$PIDFILE"
    for _ in $(seq 1 10); do
      [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)" = "200" ] || break
      sleep 1
    done
  else
    ok "Wardyn is already running (host mode) — PID $(cat "$PIDFILE"), UI ${URL}."
    ok "Stop it with:  make stop-host"
    exit 0
  fi
fi

# Guard: if Wardyn already answers on :8080 (started elsewhere, no pidfile), don't
# launch a doomed second wardynd that just logs "address already in use".
if [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)" = "200" ]; then
  if [ "${MODEL_CONFIG_APPLIED:-false}" = true ]; then
    warn "Wardyn is already running but was NOT started by this installer (no pidfile), so I can't safely restart it to apply the new model config. Stop it and re-run: make stop-host && make setup"
  else
    ok "Wardyn is already responding at ${URL} — leaving it as-is. (Stop: make stop-host)"
  fi
  exit 0
fi

hd "Launching wardynd (host mode) in the background on ${URL}"
info "Runs as you, sees your Claude login, launches sandboxes on $(basename "$DSOCK")."
launch_wardynd

# Auto-recover from a stale secret store: if wardynd can't decrypt secrets a PRIOR
# run seeded with a now-lost throwaway key ("age decrypt: no identity matched any
# of the recipients"), the only fix is to wipe local data and re-seed with the
# persistent key we just ensured. That store is unrecoverable and worthless, so do
# it automatically — once (announced, not silent).
if ! $healthy && grep -q 'no identity matched any of the recipients' "$LOGFILE" 2>/dev/null; then
  warn "Stale secret store from an earlier run (encrypted with a lost throwaway key) — resetting local data and retrying once…"
  kill "$WPID" 2>/dev/null || true; rm -f "$PIDFILE"
  docker compose -f deploy/compose/docker-compose.yaml down -v >/dev/null 2>&1 || true
  ensure_postgres
  launch_wardynd
  $healthy && ok "Recovered — wardynd re-seeded the secret store with the persistent age key."
fi

if $healthy; then
  { command -v wslview  >/dev/null 2>&1 && wslview  "$URL"; } >/dev/null 2>&1 \
    || { command -v explorer.exe >/dev/null 2>&1 && explorer.exe "$URL"; } >/dev/null 2>&1 \
    || { command -v xdg-open >/dev/null 2>&1 && xdg-open "$URL"; } >/dev/null 2>&1 || true
  # Import static AWS creds detected earlier (mount mode needs none — the SDK reads
  # the ~/.aws mount). Local host mode has no admin token, so the CLI just works.
  if [ "${BR_IMPORT_STATIC:-false}" = true ]; then
    info "Importing your AWS credentials into Wardyn's secret store…"
    export WARDYN_URL="$URL"
    printf '%s' "$AWS_ACCESS_KEY_ID"     | ./bin/wardyn secret set aws-access-key-id     >/dev/null 2>&1 && \
    printf '%s' "$AWS_SECRET_ACCESS_KEY" | ./bin/wardyn secret set aws-secret-access-key >/dev/null 2>&1 \
      && ok "AWS credentials imported — Bedrock model access should show GREEN." \
      || warn "Could not import AWS creds automatically — add them in the UI's Connect-a-model step."
    if [ -n "${AWS_SESSION_TOKEN:-}" ]; then
      printf '%s' "$AWS_SESSION_TOKEN" | ./bin/wardyn secret set aws-session-token >/dev/null 2>&1 \
        && info "AWS session token imported (STS/AssumeRole)." \
        || warn "Could not import the AWS session token — add it in the UI if your creds are STS."
    fi
  fi
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
  if grep -q 'no identity matched any of the recipients' "$LOGFILE" 2>/dev/null; then
    warn ""
    warn "^ Still can't decrypt the secret store even after an automatic reset — something is"
    warn "  holding stale data, or the persistent key in ${AGE_ENV:-deploy/compose/.env} is wrong."
    warn "  Try a full manual reset: make stop-host && docker compose -f deploy/compose/docker-compose.yaml down -v && make setup"
  fi
  exit 1
fi
