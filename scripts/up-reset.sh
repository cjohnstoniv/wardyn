# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/up-reset.sh — the destructive teardown machinery for scripts/up.sh:
# `reset` (wipe compose volumes, then re-up) and `reset-all` (full undo of
# local setup across both host and compose modes, no re-up). Split out of
# up.sh purely to keep that file under the repo's file-size gate
# (scripts/check-file-size.sh) — sourced by up.sh, never run standalone.
#
# Depends on names defined in up.sh itself (sourced before this file's
# functions are ever CALLED, which is all that matters for a POSIX-sourced
# file): REPO_ROOT, ENV_FILE, COMPOSE_FILE, compose(), _confirm(),
# _confirm_host_stop(), cmd_up(), plus log/warn/die from scripts/lib/common.sh.

# reset — deliberate clean slate. `down` keeps the named volumes (postgres_data,
# recordings, audit) so runs + the append-only audit log survive a restart, which
# is what you want normally. reset REMOVES them, so the following `up` starts with
# an EMPTY Runs list — the honest "fresh like a new clone" state on a machine that
# has run Wardyn before. Irreversible, so it CONFIRMS (default No; headless needs
# WARDYN_FORCE_RESET=1). A live HOST-mode (`make setup`) daemon is offered a stop
# first: the containerized wardynd this brings up binds the same 127.0.0.1:8080,
# so leaving the host one running guarantees a port collision, not a second UI.
# That stop is gated SEPARATELY, on WARDYN_FORCE_STOP_HOST (_confirm_host_stop) —
# WARDYN_FORCE_RESET alone confirms only the (unrelated) volume wipe, so a
# headless `WARDYN_FORCE_RESET=1 make reset` never silently kills a host-mode
# daemon it didn't ask to touch.
# For the FULL undo (host daemon + rundir + compose, no re-up) use reset-all.
cmd_reset() {
  warn "reset REMOVES the compose volumes: Postgres (ALL runs + the append-only audit log) and recordings."
  warn "This is irreversible. Plain \`make compose-down\` keeps them; use that if you only want to stop the stack."
  _host_pid="$(cat "${HOME}/.wardyn/host-wardynd.pid" 2>/dev/null || true)"
  if [ -n "${_host_pid}" ] && kill -0 "${_host_pid}" 2>/dev/null; then
    warn "Host-mode wardynd is running (PID ${_host_pid}) — the containerized wardynd this brings up needs its :8080."
    if _confirm_host_stop "Stop the host daemon first (make stop-host)?"; then
      make -C "${REPO_ROOT}" stop-host
    else
      warn "Left the host daemon up — the fresh containerized wardynd will fail to bind :8080."
    fi
  fi
  if ! _confirm "Wipe the compose volumes and re-up?"; then
    [ -t 0 ] && { log "Aborted — nothing was removed."; exit 0; }
    exit 2
  fi
  compose down -v --remove-orphans
  log "Volumes removed — bringing up a fresh, empty Wardyn"
  cmd_up
}

# reset-all — the TRUE fresh-install undo: everything `make setup` / `make
# compose-up` created, across BOTH modes (host daemon + compose stack), so an
# iteration loop can start each round from a genuinely clean box. Unlike
# `reset` it does NOT re-up — it leaves the machine clean and stops.
#
# ~/.wardyn is shared real estate (other tools keep source trees and scratch
# there), so removal is a NAMED ALLOWLIST of the files setup.sh /
# stage-claude-creds.sh create — never `rm -rf ~/.wardyn`. Everything else
# found there is reported as PRESERVED.
#
# Kept by default (flags to purge):
#   deploy/compose/.env  (--purge-env)     the persisted age key. Safe to keep:
#                                          `down -v` destroyed every secret
#                                          sealed under it, and the same key
#                                          just seals the next ones. Purge only
#                                          for a pristine first-contact baseline.
#   built :local images  (--purge-images)  the minutes-long rebuild set.
# Built binaries (bin/, ui/dist) are `make clean`'s job — not duplicated here.
_ra_mark() {  # _ra_mark 0|1 LABEL — one manifest line
  if [ "$1" = 1 ]; then printf '  [present] %s\n' "$2"; else printf '  [absent]  %s\n' "$2"; fi
}

cmd_reset_all() {
  _ra_dry=0; _ra_purge_images=0; _ra_purge_env=0
  for _ra_a in "$@"; do
    case "${_ra_a}" in
      --dry-run)      _ra_dry=1 ;;
      --purge-images) _ra_purge_images=1 ;;
      --purge-env)    _ra_purge_env=1 ;;
      *) die "reset-all: unknown flag '${_ra_a}' (known: --dry-run --purge-images --purge-env)" ;;
    esac
  done
  _ra_rundir="${HOME}/.wardyn"
  # :local is the current locally-built tag; the :demo variants are the
  # pre-rename generation still present on boxes that set up before it.
  _ra_images="wardyn/wardynd:local wardyn/wardyn-proxy:local wardyn/agent-claude-code:local wardyn/agent-codex-cli:local wardyn/agent-oracle:local wardyn/wardyn-tetragon-ingest:local wardyn/wardynd:demo wardyn/wardyn-proxy:demo wardyn/agent-claude-code:demo wardyn/agent-codex-cli:demo wardyn/agent-oracle:demo wardyn/wardyn-tetragon-ingest:demo"

  # ── gather facts (read-only) ─────────────────────────────────────────
  _ra_host_pid=$(cat "${_ra_rundir}/host-wardynd.pid" 2>/dev/null || true)
  _ra_host_live=0
  [ -n "${_ra_host_pid}" ] && kill -0 "${_ra_host_pid}" 2>/dev/null && _ra_host_live=1

  _ra_proj=$(compose config 2>/dev/null | awk '/^name:/{print $2; exit}')
  [ -n "${_ra_proj}" ] || _ra_proj=compose
  _ra_containers=$(compose ps -aq 2>/dev/null | grep -c . || true)

  # Enable every profile the file declares (sso, groundtruth, build-only, …) so
  # profile-gated volumes like tetragon_export are seen AND torn down too.
  _ra_profiles=""
  for _ra_p in $(compose config --profiles 2>/dev/null); do
    _ra_profiles="${_ra_profiles} --profile ${_ra_p}"
  done

  # Resolve the EXACT docker volume names this compose file owns (explicit
  # `name:` when set, else <project>_<logical>) — the same set `down -v`
  # removes. A project-label filter is NOT safe here: the label is just the
  # directory name ("compose"), which this repo's pre-rename eras (warden-*/
  # writ-*) share, and reset-all must never claim volumes that aren't its own.
  # The set is STATIC — docker-compose.yaml's top-level `volumes:` declares
  # exactly these six, unconditionally, regardless of which profiles are
  # active — so it is hardcoded here instead of derived via
  # `compose config --format json | jq`. That derivation used to make jq a
  # HARD dependency for an accurate manifest: absent jq (or `config` failing),
  # _ra_volnames silently resolved empty, every volume line below rendered
  # [absent], and the site-config/secrets [destroy] warning was suppressed
  # entirely — while `down -v` a few lines down still wiped them for real (a
  # false-negative consent prompt, not just a cosmetic gap). Only `recordings`
  # carries an explicit `name:` in docker-compose.yaml (namespaced by
  # WARDYN_NS, not the project); the other five follow compose's default
  # <project>_<logical> naming.
  _ra_volnames="${_ra_proj}_postgres_data ${_ra_proj}_audit ${_ra_proj}_registry_data ${_ra_proj}_groundtruth_token ${_ra_proj}_tetragon_export ${WARDYN_NS:-wardyn}-recordings"
  _ra_volumes=""
  for _ra_v in ${_ra_volnames}; do
    docker volume inspect "${_ra_v}" >/dev/null 2>&1 && _ra_volumes="${_ra_volumes}${_ra_v} "
  done

  _ra_net=0; _ra_net_attached=0
  if docker network inspect wardyn-internal >/dev/null 2>&1; then
    _ra_net=1
    _ra_net_attached=$(docker network inspect -f '{{len .Containers}}' wardyn-internal 2>/dev/null || echo 0)
  fi

  # Per-run sandbox containers + their per-run internal networks: wardynd
  # launches these dynamically via the docker runner (internal/runner/docker/
  # naming.go), NOT as part of this compose project — `compose down`
  # (below) only tears down services declared in docker-compose.yaml, so a run
  # mid-flight when reset-all fires leaves these live and unlisted. Once -v
  # drops the Postgres volume a few lines down there is no run row left to
  # reconcile against either, so they become unreapable garbage. Selector-free
  # by construction: labelManaged tags every wardynd-owned object, and every
  # per-run internal network is named wardyn-int-<runID>.
  _ra_sandbox_ids=$(docker ps -aq --filter "label=wardyn.managed=true" 2>/dev/null || true)
  _ra_sandbox_containers=$(printf '%s\n' "${_ra_sandbox_ids}" | grep -c . || true)
  _ra_sandbox_net_ids=$(docker network ls -q --filter "name=^wardyn-int-" 2>/dev/null || true)
  _ra_sandbox_nets=$(printf '%s\n' "${_ra_sandbox_net_ids}" | grep -c . || true)

  _ra_testpg=0
  docker inspect wardyn-test-pg >/dev/null 2>&1 && _ra_testpg=1

  # ~/.wardyn: split into install files (ours to delete) vs preserved (not ours)
  _ra_install=""; _ra_preserved=""
  if [ -d "${_ra_rundir}" ]; then
    for _ra_e in "${_ra_rundir}"/* "${_ra_rundir}"/.[!.]*; do
      [ -e "${_ra_e}" ] || continue
      case "$(basename "${_ra_e}")" in
        host-wardynd.pid|host-wardynd.log|claude-creds|composer-dev-subscription.json)
          _ra_install="${_ra_install}$(basename "${_ra_e}") " ;;
        *)
          _ra_preserved="${_ra_preserved}$(basename "${_ra_e}") " ;;
      esac
    done
  fi

  _ra_env_present=0
  [ -f "${ENV_FILE}" ] && _ra_env_present=1

  _ra_images_present=""
  for _ra_img in ${_ra_images}; do
    docker image inspect "${_ra_img}" >/dev/null 2>&1 && _ra_images_present="${_ra_images_present}${_ra_img} "
  done

  # ── manifest ─────────────────────────────────────────────────────────
  log "reset-all — full undo of local Wardyn setup (daemon: ${DOCKER_HOST:-default socket}). Manifest:"
  if [ "${_ra_host_live}" = 1 ]; then
    _ra_mark 1 "host-mode wardynd (PID ${_ra_host_pid}, ~/.wardyn/host-wardynd.pid) — will be stopped"
  else
    _ra_mark 0 "host-mode wardynd (no live PID)"
  fi
  _ra_mark "$([ "${_ra_containers:-0}" -gt 0 ] && echo 1 || echo 0)" "compose containers: ${_ra_containers:-0} (project '${_ra_proj}')"
  _ra_mark "$([ -n "${_ra_volumes}" ] && echo 1 || echo 0)" "compose volumes: ${_ra_volumes:-none }(runs + audit + recordings — IRREVERSIBLE)"
  # The corporate baseline lives in Postgres, so the volume takes it too. Say so
  # HERE, while the operator can still capture it: otherwise the stack comes back
  # up looking healthy and sandbox egress is silently unconfigured.
  if [ -n "${_ra_volumes}" ]; then
    printf '  [destroy] site-config + secrets (upstream proxy, artifact mirrors, SCM hosts) — they live in the Postgres volume.\n'
    # Name a command that EXISTS and RESOLVES on the flagship path. Two traps:
    # containerized setup ships no host-side wardyn binary (it lives in the
    # wardynd image, and nothing in the Makefile builds bin/), and this box may
    # run a second dockerd — wardyn_pick_docker_host already resolved which one,
    # so echo it into the hint rather than let a bare `docker compose` hit the
    # default socket and report the stack as not running.
    printf '            Capture first if this host needs them back:\n'
    printf '              %sdocker compose -f %s exec -T wardynd wardyn site-config get > corp-baseline.json\n' \
      "${DOCKER_HOST:+DOCKER_HOST=${DOCKER_HOST} }" "${COMPOSE_FILE#"${REPO_ROOT}/"}"
    printf '            (host mode / built CLI:  wardyn site-config get > corp-baseline.json)\n'
  fi
  if [ "${_ra_net}" = 1 ]; then
    _ra_mark 1 "docker network wardyn-internal (${_ra_net_attached} attached — removed only if 0 remain after teardown)"
  else
    _ra_mark 0 "docker network wardyn-internal"
  fi
  _ra_mark "$([ "${_ra_sandbox_containers:-0}" -gt 0 ] && echo 1 || echo 0)" \
    "live per-run sandbox containers: ${_ra_sandbox_containers:-0} (label wardyn.managed=true — compose down never touches these)"
  _ra_mark "$([ "${_ra_sandbox_nets:-0}" -gt 0 ] && echo 1 || echo 0)" \
    "per-run internal networks: ${_ra_sandbox_nets:-0} (wardyn-int-*)"
  _ra_mark "${_ra_testpg}" "dev/e2e postgres container wardyn-test-pg (:55432)"
  _ra_mark "$([ -n "${_ra_install}" ] && echo 1 || echo 0)" "~/.wardyn install files: ${_ra_install:-none }(includes STAGED CLAUDE CREDS — re-stage after next setup)"
  [ -n "${_ra_preserved}" ] && printf '  [keep]    ~/.wardyn PRESERVED (not Wardyn setup'\''s): %s\n' "${_ra_preserved}"
  if [ "${_ra_purge_env}" = 1 ]; then
    _ra_mark "${_ra_env_present}" "deploy/compose/.env (age key) — --purge-env"
  else
    printf '  [keep]    deploy/compose/.env (age key; sealed secrets die with the volume, so keeping it is safe — --purge-env for a pristine baseline)\n'
  fi
  if [ "${_ra_purge_images}" = 1 ]; then
    _ra_mark "$([ -n "${_ra_images_present}" ] && echo 1 || echo 0)" "built images: ${_ra_images_present:-none}— --purge-images (minutes to rebuild)"
  else
    printf '  [keep]    built images (%s) — --purge-images to remove; rebuilds take minutes\n' "${_ra_images_present:-none present}"
  fi
  printf '  [note]    built binaries (bin/, ui/dist): use `make clean`\n'

  if [ "${_ra_dry}" = 1 ]; then
    log "Dry run — nothing was touched. After a real reset-all every line above reads [absent]."
    exit 0
  fi

  # ── consent, then act on the facts above ─────────────────────────────
  warn "This removes everything marked [present]: all runs, the audit log, recordings, and staged Claude creds."
  if ! _confirm "Proceed with reset-all?"; then
    [ -t 0 ] && { log "Aborted — nothing was removed."; exit 0; }
    exit 2
  fi

  [ "${_ra_host_live}" = 1 ] && make -C "${REPO_ROOT}" stop-host

  # Reap per-run sandbox containers + their internal networks BEFORE compose
  # down/-v drops the Postgres volume — compose down never touches these (see
  # the manifest-gathering comment above), and once the volume is gone there is
  # no run row left to reconcile them against. Containers first: a network
  # with an attached container refuses `network rm`.
  if [ -n "${_ra_sandbox_ids}" ]; then
    # shellcheck disable=SC2086 — word-split container ids by construction
    docker rm -f ${_ra_sandbox_ids} >/dev/null 2>&1 \
      || warn "could not remove some per-run sandbox containers (label wardyn.managed=true)"
  fi
  if [ -n "${_ra_sandbox_net_ids}" ]; then
    # shellcheck disable=SC2086 — word-split network ids by construction
    docker network rm ${_ra_sandbox_net_ids} >/dev/null 2>&1 \
      || warn "some per-run internal networks (wardyn-int-*) still have attached containers — left in place"
  fi

  # shellcheck disable=SC2086 — _ra_profiles is a flat flag list by construction
  compose ${_ra_profiles} down -v --remove-orphans \
    || warn "compose down failed (docker unreachable?) — continuing with filesystem cleanup"

  # run-host.sh creates wardyn-internal OUTSIDE compose ownership (the source of
  # setup.sh's "incorrect label" recovery dance) — remove it when nothing is
  # attached so the next setup recreates it cleanly; a busy network is left alone.
  if docker network inspect wardyn-internal >/dev/null 2>&1; then
    docker network rm wardyn-internal >/dev/null 2>&1 \
      || warn "wardyn-internal still has attached containers — left in place"
  fi

  # -v: also drop the anonymous pgdata volume docker auto-created for it
  [ "${_ra_testpg}" = 1 ] && docker rm -f -v wardyn-test-pg >/dev/null 2>&1

  # Allowlist only — never `rm -rf ~/.wardyn` (see comment above).
  rm -f  "${_ra_rundir}/host-wardynd.pid" "${_ra_rundir}/host-wardynd.log"
  rm -rf "${_ra_rundir}/claude-creds"
  rm -f  "${_ra_rundir}/composer-dev-subscription.json"

  [ "${_ra_purge_env}" = 1 ] && rm -f "${ENV_FILE}"

  if [ "${_ra_purge_images}" = 1 ]; then
    for _ra_img in ${_ra_images_present}; do
      docker rmi "${_ra_img}" >/dev/null 2>&1 || warn "could not remove ${_ra_img} (in use?)"
    done
  fi

  log "Clean. Verify: scripts/up.sh reset-all --dry-run   (every line should read [absent])"
  log "Next: make setup (asks; Enter = containerized)  or  make compose-up (containerized, no prompt)"
}
