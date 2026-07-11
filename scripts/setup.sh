#!/usr/bin/env bash
# Wardyn unified setup — ONE command that detects your host and, for every piece of
# host state it could use (your Claude login, AWS/Bedrock creds, git credentials),
# shows you WHAT it found and WHAT it would do with it, then asks before acting. It
# does what it can without sudo and launches the right mode with the UI already
# reflecting reality. Nothing that touches a credential happens without an explicit
# choice — no credential is copied, imported, or a volume destroyed unless you say so
# (interactively) or set the matching opt-in env var (non-interactively).
#
# Deployment: ONE front door, two supported single-user setups. Interactively
# it asks which (Enter = host); headless it defaults to host, and
# WARDYN_SETUP_MODE picks explicitly for scripts.
#  - HOST mode (default): sandbox agents run on YOUR machine with YOUR Claude
#    login — wardynd runs as you, sees ~/.claude directly (no re-login), and
#    proxy-injects your live token (never a stale copy). Best for personal use.
#  - CONTAINERIZED mode: the compose stack — wardynd runs in a container on
#    wardyn-internal, so sandbox→control-plane callbacks route in-network. This
#    is the fix for Docker Desktop + WSL2 NAT (workspace Verify/Record), at the
#    cost of host-login visibility (use an API key / Bedrock for model access).
#    Delegates to scripts/up.sh up.
#
# TEAM mode (that same compose control plane as a shared MULTI-USER service —
# SSO logins, per-user identity/RBAC) is a COMING-SOON feature;
# WARDYN_SETUP_MODE=team prints a notice and exits.
#
# Barriers (Fence/Wall/Vault) that need a package install (gVisor, Kata) require sudo —
# this script NEVER runs sudo silently. It detects what's present and prints the exact
# commands for anything missing, so you stay in control of privileged changes.
#
# Usage:  ./scripts/setup.sh                 (asks host vs containerized; headless = host)
#         WARDYN_SETUP_MODE=local ...        (host mode, no prompt)
#         WARDYN_SETUP_MODE=container ...    (containerized single-user stack, no prompt)
#         WARDYN_SETUP_MODE=team ...         (errors: team mode is coming soon)
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

# ask_yn PROMPT DEFAULT(y|n) -> returns 0 for yes, 1 for no. Non-interactive
# (no TTY on stdin) returns the DEFAULT without prompting — callers gate a
# credential write on an explicit env flag BEFORE calling, so a headless run
# never writes a secret just because the default here is "y". Empty input (Enter)
# and EOF take the default; an UNRECOGNIZED answer re-asks rather than silently
# taking the default — a typo at a default-yes credential prompt must not stage.
ask_yn() {
  local prompt="$1" def="$2" a
  if [ ! -t 0 ]; then [ "$def" = y ] && return 0 || return 1; fi
  while :; do
    if [ "$def" = y ]; then printf "  %s [Y/n] " "$prompt"; else printf "  %s [y/N] " "$prompt"; fi
    read -r a || a=""   # EOF -> empty -> default
    case "$a" in
      y|Y|yes|YES) return 0;;
      n|N|no|NO)   return 1;;
      "")          [ "$def" = y ] && return 0 || return 1;;
      *)           warn "Please answer y or n.";;
    esac
  done
}

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
if ! $HAVE_DOCKER; then warn "docker not reachable at $DSOCK — start Docker and re-run."; exit 1; fi
# The daemon FLAVOR decides which warnings are real: Docker Desktop's engine
# lives in its own VM (WSL2 NAT swallows sandbox→wardynd callbacks; custom
# runtimes reset on restart); a native in-distro dockerd has neither problem.
DOCKER_OS="$(docker info -f '{{.OperatingSystem}}' 2>/dev/null || true)"
IS_DESKTOP=false; case "$DOCKER_OS" in *"Docker Desktop"*) IS_DESKTOP=true;; esac
ok "docker reachable ($(basename "$DSOCK")${DOCKER_OS:+ — ${DOCKER_OS}})"
if $IS_WSL && $IS_DESKTOP; then
  # The known host-mode gap on Docker Desktop + WSL2 default (NAT) networking:
  # host.docker.internal resolves to WINDOWS, not this distro, so sandbox→wardynd
  # callbacks are lost — workspace Verify results never report and Record captures
  # land empty. Only warn when the picked daemon actually IS Docker Desktop —
  # firing this on a native in-distro dockerd was a false alarm that taught users
  # to ignore it.
  warn "Docker Desktop + WSL2 (NAT networking): workspace Verify/Record won't complete in host mode."
  warn "Fixes: WSL2 mirrored networking ([wsl2] networkingMode=mirrored in %UserProfile%\\.wslconfig, then 'wsl --shutdown'),"
  warn "or run the containerized stack instead: make compose-up."
fi

RT="$(runtimes_json)"
HAVE_RUNSC=false; case "$RT" in *'"runsc"'*) HAVE_RUNSC=true;; esac
HAVE_KATA=false;  case "$RT" in *'"kata'*)  HAVE_KATA=true;;  esac
HAVE_KVM=false;   [ -e /dev/kvm ] && HAVE_KVM=true
ok "Fence (runc) — always available"
$HAVE_RUNSC && ok "Wall (gVisor/runsc) — registered" || warn "Wall (gVisor/runsc) — not registered on this daemon"
if $HAVE_KATA; then ok "Vault (Kata microVM) — registered"; elif $HAVE_KVM; then warn "Vault (Kata) — not registered (but /dev/kvm present, so installable)"; else warn "Vault (Kata) — unavailable (no /dev/kvm)"; fi

HAVE_CLAUDE=false; command -v claude >/dev/null 2>&1 && HAVE_CLAUDE=true
# Login signal: the OAuth token file. On macOS, Claude Code keeps this in the login
# Keychain (service "Claude Code-credentials") and the file usually does NOT exist —
# so a file-only check false-negatives a logged-in Mac. Probe the Keychain too (item
# PRESENCE only, no -w, so the secret is never read and no ACL prompt fires).
# CLAUDE_LOGGED_IN drives the login badge; CLAUDE_CRED_FILE tracks whether the
# on-disk file (which host-mode staging needs) is actually present.
CLAUDE_LOGGED_IN=false; CLAUDE_CRED_FILE=false
if [ -f "$HOME/.claude/.credentials.json" ]; then CLAUDE_LOGGED_IN=true; CLAUDE_CRED_FILE=true
elif [ "$(uname -s 2>/dev/null)" = Darwin ] && security find-generic-password -s "Claude Code-credentials" >/dev/null 2>&1; then
  CLAUDE_LOGGED_IN=true  # logged in via Keychain, but no on-disk file for staging
fi
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
if $HAVE_CLAUDE && $CLAUDE_LOGGED_IN && $CLAUDE_CRED_FILE; then
  ok "Claude CLI logged in on this host ($(command -v claude)) — host mode can use it directly"
elif $HAVE_CLAUDE && $CLAUDE_LOGGED_IN; then
  # macOS Keychain login with no on-disk file: the login badge is green, but host-mode
  # subscription staging needs the file cred, which the Keychain does not expose.
  ok "Claude CLI logged in via the macOS Keychain ($(command -v claude))."
  warn "Host-mode subscription staging needs the on-disk ~/.claude/.credentials.json, which the Keychain doesn't expose. Log in over SSH once (writes the file) to stage the subscription, or use an API key / Bedrock. (The model-access badge is already green.)"
elif $CLAUDE_BEDROCK || $WARDYN_BEDROCK_SET; then
  ok "Claude is set up for AWS Bedrock on this host — model access is via Bedrock, not a Claude login (auto-configured below in local mode)."
elif $HAVE_CLAUDE; then
  warn "Claude CLI present but no model auth detected — run 'claude login', use an API key, or configure AWS Bedrock (export WARDYN_BEDROCK_REGION + WARDYN_BEDROCK_MODEL)."
else
  info "No Claude CLI on PATH (add an API key in the UI later to enable model access)"
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

# ── DECIDE: two supported single-user modes — host (default) and containerized
# (delegates to scripts/up.sh up). ONE front door: with a TTY and no explicit
# WARDYN_SETUP_MODE we ask which (Enter = host; the WSL2 hint recommends
# containerized where host mode's Verify/Record callbacks are known-broken).
# Headless stays promptless and defaults to host — same behavior as before.
# TEAM (that compose control plane as a shared MULTI-USER service: SSO logins,
# per-user identity/RBAC) is a COMING-SOON feature; an explicit request gets a
# clear notice and exits.
if [ -z "${WARDYN_SETUP_MODE:-}" ] && [ -t 0 ]; then
  hd "Where should the control plane run?"
  say "    1) host          — wardynd runs as you; uses your Claude login directly (default)"
  say "    2) containerized — the compose stack; add an API key/Bedrock for model access"
  if $IS_WSL && $IS_DESKTOP; then
    info "WSL2 + Docker Desktop (NAT) detected: pick 2 —"
    info "host mode's workspace Verify/Record callbacks don't route there."
  fi
  while :; do
    printf "  Choice [1/2] (Enter = 1): "
    read -r _mode || _mode=""
    case "${_mode}" in
      ""|1|host)          WARDYN_SETUP_MODE=local; break;;
      2|container*)       WARDYN_SETUP_MODE=container; break;;
      *)                  warn "Please answer 1 or 2.";;
    esac
  done
fi
case "${WARDYN_SETUP_MODE:-local}" in
  local) ;;
  container)
    hd "Containerized mode — delegating to scripts/up.sh up"
    info "wardynd runs in a container on wardyn-internal: sandbox→control-plane callbacks route"
    info "in-network (the Docker Desktop + WSL2 NAT workspace-Verify/Record fix). The container"
    info "can't see your host Claude login — add an API key in the UI (or Bedrock) for model access."
    exec ./scripts/up.sh up
    ;;
  team)
    hd "Team mode is coming soon"
    warn "Team mode — this compose control plane as a shared MULTI-USER service (SSO logins,"
    warn "per-user identity/RBAC) — is a COMING-SOON feature and isn't available in this version."
    warn "What works today, both single-user: HOST mode ('make setup' — wardynd runs as you, your"
    warn "Claude login injected per-request at the proxy) and CONTAINERIZED mode"
    warn "(WARDYN_SETUP_MODE=container — the compose stack, the WSL2 workspace-Verify/Record fix)."
    exit 2
    ;;
  *)
    warn "WARDYN_SETUP_MODE='${WARDYN_SETUP_MODE:-local}' is not valid. Supported: unset/local (host"
    warn "mode) or container (compose stack, single-user). Team mode is coming soon."
    exit 2
    ;;
esac
ok "Mode: host (local) — team (multi-user) is coming soon"

# ── ACT (host mode only; team is coming soon, so there is no team branch) ──────
# local / host mode
hd "Setting up local (host) mode"

# ── Model access, round 1: Claude subscription staging ───────────────────────
# CONSENT: copying your Claude login is a credential-touching action, so we show
# exactly what happens and ask first (default yes interactively; headless requires
# WARDYN_STAGE_CLAUDE=1 — a non-interactive run NEVER copies your login silently).
if $HAVE_CLAUDE && $CLAUDE_LOGGED_IN && $CLAUDE_CRED_FILE; then
  hd "Model access — stage your Claude login for sandbox runs?"
  say "  What this does:"
  say "    · COPIES ~/.claude + ~/.claude.json to ~/.wardyn/claude-creds (outside the repo)"
  # The sanitization only happens in the default inject-on mode. The
  # WARDYN_SUBSCRIPTION_INJECT=off escape hatch stages a REAL, refreshable credential
  # instead — so promise honestly based on the mode actually in effect.
  if [ "$(printf '%s' "${WARDYN_SUBSCRIPTION_INJECT:-}" | tr '[:upper:]' '[:lower:]')" = off ]; then
    say "    · WARDYN_SUBSCRIPTION_INJECT=off — the copy keeps a REAL, refreshable credential"
    say "      (resident and can go stale; MCP servers + tokens are still stripped). Re-run to refresh."
  else
    say "    · SANITIZES the copy: refresh token blanked, access token replaced with an inert"
    say "      placeholder, MCP servers + tokens stripped (nothing usable is left resident;"
    say "      the live token is injected at the proxy per-request)"
  fi
  say "    · GENERATES ~/.wardyn/composer-dev-subscription.json (the subscription ceiling)"
  say "  A run only receives these mounts when you tick \"Use my Claude subscription\" per run."
  stage_do=false
  if [ "${WARDYN_STAGE_CLAUDE:-}" = 1 ]; then stage_do=true
  elif [ -t 0 ]; then ask_yn "Stage your Claude login now?" y && stage_do=true
  else info "non-interactive: skipping Claude staging (run 'make stage-claude' later, or set WARDYN_STAGE_CLAUDE=1)."; fi
  if $stage_do; then
    # Output is NOT suppressed: the stage script's sanitization lines are the honest
    # record of what it did to your credential — the user should see them.
    if ./scripts/stage-claude-creds.sh; then
      ok "Claude subscription staged. (The model-access badge is green from your host login regardless; staging is what enables the per-run subscription MOUNT.)"
      # Staging just generated/refreshed the subscription ceiling that run-host.sh
      # picks as WARDYN_DEFAULT_POLICY — an already-running wardynd won't load it
      # until restarted, same as the Bedrock boot-time config.
      MODEL_CONFIG_APPLIED=true
    else
      warn "Could not stage creds (continuing; you can add an API key in the UI)."
    fi
  else
    info "Skipped Claude staging. The model-access badge stays green from your host login, but the"
    info "per-run \"Use my Claude subscription\" mount won't work until you stage — run 'make stage-claude'"
    info "later (it stages and restarts wardynd; there is no UI action for this)."
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
  say "  This step wires Wardyn's region+model (and a READ-ONLY ~/.aws mount if present)."
  say "  It stores NO credential — importing static AWS keys is a separate, explicit choice below."
  auto=y
  if [ -t 0 ]; then ask_yn "Point Wardyn at this Bedrock region+model?" y || auto=n
  else info "non-interactive: wiring Bedrock region+model (stores no credential)."; fi
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
          # SECURITY: never advise a recursive world-read chmod on a credential dir —
          # that would expose ~/.aws/credentials to every local user and sandbox. If the
          # agent uid can't read the 0600 files, grant JUST that uid via an ACL.
          [ "$(id -u)" = "1000" ] || warn "Host uid $(id -u) ≠ sandbox agent uid 1000 — the agent may not read 0600 files under ~/.aws. If a run can't auth: run the agent as your host uid, grant the sandbox uid with an ACL ('setfacl -R -m u:1000:rX ~/.aws'), or choose static-key import instead."
          ;;
        static-env)
          # CONSENT: importing static AWS keys WRITES long-lived cloud credentials into
          # Wardyn's encrypted secret store — its own explicit, default-No decision,
          # decoupled from the region/model wiring above. Headless requires
          # WARDYN_IMPORT_AWS=1 (a non-interactive run never stores keys silently).
          say "  Static AWS keys detected in the environment. Importing them WRITES these secrets:"
          say "      · aws-access-key-id       (from \$AWS_ACCESS_KEY_ID)"
          say "      · aws-secret-access-key   (from \$AWS_SECRET_ACCESS_KEY)"
          [ -n "${AWS_SESSION_TOKEN:-}" ] && say "      · aws-session-token       (from \$AWS_SESSION_TOKEN)"
          say "    into Wardyn's encrypted secret store on this host."
          # Residency honesty (threatmodel: Bedrock SigV4 can't be proxy-injected):
          # unlike the subscription/API-key paths, these keys are handed to the
          # sandbox at run time. Say so, and name the safer rung before asking.
          warn "At RUN time these long-lived keys become RESIDENT in sandboxes that use Bedrock"
          warn "(SigV4 signing can't be proxy-injected, so the SDK must hold real credentials)."
          info "Safer: AWS SSO with a read-only ~/.aws mount — credentials are short-lived and"
          info "auto-rotate, and Wardyn stores nothing. Set up with 'aws configure sso' +"
          info "'aws sso login', then re-run 'make setup' (mount mode is picked automatically)."
          if [ "${WARDYN_IMPORT_AWS:-}" = 1 ]; then
            BR_IMPORT_STATIC=true; ok "WARDYN_IMPORT_AWS=1 — will import the AWS keys once the daemon is up."
          elif [ -t 0 ] && ask_yn "Import your static AWS keys into Wardyn's secret store?" n; then
            BR_IMPORT_STATIC=true; ok "Will import the AWS keys once the daemon is up."
          else
            info "Skipped AWS key import — add them in the UI's Connect-a-model step, or set WARDYN_IMPORT_AWS=1 to import headlessly. (Region+model are still wired.)"
          fi
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
elif $HAVE_CLAUDE && $CLAUDE_LOGGED_IN; then
  # macOS Keychain login (no on-disk cred file, no Bedrock): the badge is GREEN, but
  # host-mode subscription staging needs the file — already explained up top.
  info "Logged in via the macOS Keychain — the model-access badge is green. Host-mode subscription staging needs the on-disk credential (log in over SSH once), or use an API key / Bedrock."
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

# Build the per-run agent images if missing (first run only; slow). Without them
# the very first run fails at pull time ("registry: denied") on the locally-built
# :local tags, because those tags exist in no registry. Docker is already
# confirmed reachable above (setup exits early otherwise), so no extra guard is
# needed here. Progress streams so a slow first-run build is visibly happening,
# not a silent hang. The oracle image (a deterministic e2e stand-in, no LLM) is
# deliberately NOT built here — the e2e scripts that use it build it themselves;
# a first-time user never runs it.
. scripts/lib/images.sh
hd "Agent images (per-run sandboxes)"
for _img in claude-code:wardyn/agent-claude-code:local \
            codex-cli:wardyn/agent-codex-cli:local; do
  _img_dir="${_img%%:*}"; _img_tag="${_img#*:}"
  if image_missing "$_img_tag"; then
    info "Building ${_img_tag} (first run; this can take several minutes)…"
    if docker build -f "deploy/images/${_img_dir}/Dockerfile" -t "$_img_tag" .; then
      ok "built ${_img_tag}"
    else
      warn "build failed for ${_img_tag} — runs naming this agent fail until 'make agent-images'"
    fi
  else
    ok "present: ${_img_tag}"
  fi
done
unset _img _img_dir _img_tag

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
# F10: create the wardynd log 0600 before anything writes to it. wardynd logs a
# WARNING containing an ephemeral age identity (public fingerprint only after the
# main.go fix, but keep the file owner-only regardless) — a world-readable log is a
# needless disclosure surface. Truncate on (re)create to match nohup's '>' redirect.
(umask 077; : > "$LOGFILE") 2>/dev/null || true
chmod 600 "$LOGFILE" 2>/dev/null || true

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
# run seeded with a now-lost throwaway key ("age decrypt: no identity matched any of
# the recipients"), the undecryptable secrets must be cleared so wardynd re-seeds its
# boot keys under the persistent key we just ensured. F6: scope the fix to the
# `secrets` table ONLY — the age-encrypted rows are the sole thing the lost key made
# unrecoverable; the audit log, run history, and recordings in the SAME Postgres
# volume are NOT encrypted under it and stay readable. The old `docker compose down -v`
# also destroyed all of that. TRUNCATE removes only the already-worthless secrets (any
# imported PAT/AWS/api key was encrypted under the lost key too, so it's gone
# regardless), so this is a safe, announced auto-recovery — no volume wipe.
if ! $healthy && grep -q 'no identity matched any of the recipients' "$LOGFILE" 2>/dev/null; then
  warn "Stale secret store from an earlier run (encrypted with a lost throwaway key)."
  kill "$WPID" 2>/dev/null || true; rm -f "$PIDFILE"
  # Wait for the killed wardynd to release :8080 before relaunching — TRUNCATE takes
  # only ms (unlike the old down -v), so an immediate relaunch could race the dying
  # process and fail to bind ("address already in use").
  for _ in $(seq 1 10); do kill -0 "$WPID" 2>/dev/null || break; sleep 1; done
  if docker compose -f deploy/compose/docker-compose.yaml exec -T postgres \
       psql -U wardyn -d wardyn -c 'TRUNCATE secrets;' >/dev/null 2>&1; then
    ok "Cleared the undecryptable secrets table (audit log, runs, and recordings preserved) — retrying once…"
    launch_wardynd
    $healthy && ok "Recovered — wardynd re-seeded its boot keys under the persistent age key. Re-import any PAT/AWS/API-key secrets (they were encrypted under the lost key)."
  else
    # Fallback: couldn't truncate (psql unavailable). The only other repair is a full
    # volume wipe, which ALSO destroys audit/runs/recordings — gate it behind explicit
    # consent (prompt naming the blast radius; headless requires WARDYN_FORCE_RESET=1).
    warn "Could not clear the secrets table via psql. The only remaining repair is a full volume wipe."
    warn "That DESTROYS the append-only audit log, run history, AND recordings — not just secrets."
    reset_do=false
    if [ "${WARDYN_FORCE_RESET:-}" = 1 ]; then reset_do=true; warn "WARDYN_FORCE_RESET=1 — wiping volumes."
    elif [ -t 0 ] && ask_yn "Wipe ALL local volumes (audit log + runs + recordings + secrets)?" n; then reset_do=true
    else warn "Skipped volume wipe. Fix manually: make stop-host && docker compose -f deploy/compose/docker-compose.yaml down -v && make setup (or set WARDYN_FORCE_RESET=1)."; fi
    if $reset_do; then
      docker compose -f deploy/compose/docker-compose.yaml down -v >/dev/null 2>&1 || true
      ensure_postgres
      launch_wardynd
      $healthy && ok "Recovered — wardynd re-seeded the secret store with the persistent age key (local volumes were wiped)."
    fi
  fi
fi

if $healthy; then
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

  # Opt-in SCM credential import. SECURITY/PRIVACY: never copies a private key or
  # PAT without explicit consent (an env flag or an interactive yes; default No).
  # Detects a host GitHub/Azure DevOps PAT (env) and a private ~/.ssh key, and —
  # only when the operator opts in — stores them in Wardyn's ENCRYPTED secret store
  # under the documented convention (git-pat-<host>, ssh-key-<host>, host dots→
  # hyphens). The ssh-key-<host> secret is exactly what onboarding an SSH workspace
  # consumes, so no manual grant is needed for that path.
  scm_pat=""; scm_pat_host=""
  if   [ -n "${GITHUB_PAT:-}" ];       then scm_pat="$GITHUB_PAT";       scm_pat_host="github.com"
  elif [ -n "${GH_TOKEN:-}" ];         then scm_pat="$GH_TOKEN";         scm_pat_host="github.com"
  elif [ -n "${AZURE_DEVOPS_PAT:-}" ]; then scm_pat="$AZURE_DEVOPS_PAT"; scm_pat_host="dev.azure.com"
  elif [ -n "${ADO_PAT:-}" ];          then scm_pat="$ADO_PAT";          scm_pat_host="dev.azure.com"
  fi
  scm_key=""
  for k in "$HOME/.ssh/id_ed25519" "$HOME/.ssh/id_rsa"; do
    [ -f "$k" ] && { scm_key="$k"; break; }
  done
  # Canonical provider hosts to register the SSH key under (the common case is
  # github.com). Override e.g. WARDYN_SCM_SSH_HOSTS="github.com dev.azure.com".
  scm_ssh_hosts="${WARDYN_SCM_SSH_HOSTS:-github.com}"

  if [ -n "$scm_pat" ] || [ -n "$scm_key" ]; then
    scm_plan=""
    [ -n "$scm_pat" ] && scm_plan="${scm_plan}    · ${scm_pat_host} PAT (env) → secret git-pat-${scm_pat_host//./-}\n"
    if [ -n "$scm_key" ]; then
      for h in $scm_ssh_hosts; do
        scm_plan="${scm_plan}    · ${scm_key} → secret ssh-key-${h//./-}\n"
      done
    fi

    scm_do=false
    if [ "${WARDYN_IMPORT_SCM:-}" = 1 ]; then
      scm_do=true
    elif [ -t 0 ] && [ -t 1 ]; then
      hd "Import host git credentials into Wardyn? (lets agents clone private repos)"
      printf '%b' "$scm_plan"
      warn "These are copied into Wardyn's ENCRYPTED store on this host."
      # F7: disclose the standing auto-grant BEFORE consent — an imported SSH key is
      # not just stored, it is auto-used by every future SSH workspace clone with no
      # per-run prompt (unlike the subscription's per-run checkbox).
      if [ -n "$scm_key" ]; then
        warn "An imported ssh-key-<host> becomes a STANDING credential: every future SSH workspace"
        warn "clone to that host uses it automatically, with no per-run prompt. Prefer a scoped deploy"
        warn "key over your personal id_ed25519/id_rsa (which likely has full account access)."
      fi
      ask_yn "Import now?" n && scm_do=true
    else
      info "Detected host git credentials — set WARDYN_IMPORT_SCM=1 to import them into Wardyn's secret store (skipped)."
    fi

    if $scm_do; then
      export WARDYN_URL="$URL"
      hd "Importing SCM credentials into Wardyn's secret store"
      if [ -n "$scm_pat" ]; then
        scm_n="git-pat-${scm_pat_host//./-}"
        printf '%s' "$scm_pat" | ./bin/wardyn secret set "$scm_n" >/dev/null 2>&1 \
          && ok "stored ${scm_n}" || warn "could not store ${scm_n}"
      fi
      if [ -n "$scm_key" ]; then
        # Refuse a passphrase-protected key: agents run ssh non-interactively, so an
        # encrypted key would hang the clone. `ssh-keygen -y -P ""` fails on one
        # (covers classic-PEM and OpenSSH-v1 formats; a text grep would not).
        if ssh-keygen -y -P "" -f "$scm_key" >/dev/null 2>&1; then
          for h in $scm_ssh_hosts; do
            scm_n="ssh-key-${h//./-}"
            ./bin/wardyn secret set "$scm_n" < "$scm_key" >/dev/null 2>&1 \
              && ok "stored ${scm_n} (from ${scm_key})" || warn "could not store ${scm_n}"
          done
        else
          warn "${scm_key} looks passphrase-protected — skipped (agents clone non-interactively; an encrypted key would hang). Use an unencrypted deploy key."
        fi
      fi
      info "Reference a git-pat-<host> secret from an SCM grant (New Run wizard or a stored policy). Onboarding an SSH workspace consumes ssh-key-<host> automatically."
      [ -n "$scm_key" ] && warn "An SSH identity now lives in Wardyn's encrypted store on this host (the store's age key persists across restarts)."
    fi
  fi
  unset scm_pat scm_pat_host scm_key scm_ssh_hosts scm_plan scm_do scm_n h k

  hd "Wardyn is up (host mode) — your terminal is free"
  ok "UI     ${URL}   (opening in your browser)"
  ok "PID    ${WPID}   (${PIDFILE})"
  ok "Logs   ${LOGFILE}   (tail -f to watch)"
  ok "Stop   make stop-host   (or: kill ${WPID})"
  # Open the UI LAST — every interactive prompt above is done, so the browser
  # never steals focus while the terminal is still waiting on an answer.
  { command -v wslview  >/dev/null 2>&1 && wslview  "$URL"; } >/dev/null 2>&1 \
    || { command -v explorer.exe >/dev/null 2>&1 && explorer.exe "$URL"; } >/dev/null 2>&1 \
    || { command -v xdg-open >/dev/null 2>&1 && xdg-open "$URL"; } >/dev/null 2>&1 || true
else
  rm -f "$PIDFILE"
  warn "wardynd did not become healthy — last log lines:"
  tail -n 15 "$LOGFILE" 2>/dev/null | sed 's/^/    /'
  warn "Full log: ${LOGFILE}"
  if grep -q 'no identity matched any of the recipients' "$LOGFILE" 2>/dev/null; then
    warn ""
    warn "^ Still can't decrypt the secret store even after an automatic reset — something is"
    warn "  holding stale data, or the persistent key in ${AGE_ENV:-deploy/compose/.env} is wrong."
    warn "  Preferred (keeps audit/runs/recordings): make stop-host && docker compose -f deploy/compose/docker-compose.yaml exec -T postgres psql -U wardyn -d wardyn -c 'TRUNCATE secrets;' && make setup"
    warn "  Full reset (also wipes audit log + runs + recordings): make stop-host && docker compose -f deploy/compose/docker-compose.yaml down -v && make setup"
  fi
  exit 1
fi
