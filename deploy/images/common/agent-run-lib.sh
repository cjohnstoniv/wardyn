# deploy/images/common/agent-run-lib.sh — shared shell functions sourced by
# every agent image's /usr/local/bin/agent-run (claude-code, codex-cli, ...).
# Installed at /usr/local/bin/agent-run-lib.sh in each image (see the Dockerfile
# COPY next to agent-run's own COPY). Byte-identical across images before this
# extraction; keep it that way — any per-agent divergence belongs back in the
# agent's own agent-run, not here.
#
# Requires `set -euo pipefail` and bash (matches agent-run itself).

# ── TLS-MITM CA install ───────────────────────────────────────────────────────
# When the run opts into intercept_tls, dispatch delivers the per-run CA PUBLIC
# cert in WARDYN_MITM_CA_PEM and points NODE_EXTRA_CA_CERTS at the bare CA
# written here (additive trust — Node keeps its bundled roots). For everything
# OpenSSL-shaped (curl, Python requests, Ruby, ...), dispatch points
# SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE at the COMBINED bundle
# (system roots + per-run CA) assembled here — those vars REPLACE the client's
# trust store, so the bare CA there would break non-MITM'd CONNECT-tunneled
# hosts. Files live under /tmp/wardyn (any-uid-writable) so images with
# arbitrary USER/HOME work; the idle main process (driver.go agentIdleScript,
# keep in lockstep) may have written them already as a different uid, hence the
# best-effort rewrites — within one run the content is identical, so a failed
# rewrite over a correct file is harmless. Best-effort system-trust install
# covers remaining clients. The CA PRIVATE key never enters the sandbox; only
# the proxy holds it. No-op when WARDYN_MITM_CA_PEM is unset.
install_mitm_ca() {
    [[ -n "${WARDYN_MITM_CA_PEM:-}" ]] || return 0
    local dir="/tmp/wardyn" sys="" c
    mkdir -p "$dir" 2>/dev/null || true
    chmod 1777 "$dir" 2>/dev/null || true
    { printf '%s\n' "$WARDYN_MITM_CA_PEM" > "$dir/mitm-ca.pem" \
        && chmod 0644 "$dir/mitm-ca.pem"; } 2>/dev/null || true
    for c in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem /etc/pki/tls/certs/ca-bundle.crt; do
        [[ -f "$c" ]] && sys="$c" && break
    done
    if [[ -n "$sys" ]]; then
        { cat "$sys" "$dir/mitm-ca.pem" > "$dir/ca-bundle.pem" \
            && chmod 0644 "$dir/ca-bundle.pem"; } 2>/dev/null || true
    else
        { cp "$dir/mitm-ca.pem" "$dir/ca-bundle.pem" \
            && chmod 0644 "$dir/ca-bundle.pem"; } 2>/dev/null || true
        echo "agent-run: WARNING: no system CA bundle found; ca-bundle.pem is proxy-CA-only (non-MITM TLS hosts will not verify)" >&2
    fi
    if command -v update-ca-certificates >/dev/null 2>&1; then
        cp "$dir/mitm-ca.pem" /usr/local/share/ca-certificates/wardyn-mitm.crt 2>/dev/null \
            && update-ca-certificates >/dev/null 2>&1 || true
    fi
}

# ── Claude config dir (writable) ─────────────────────────────────────────────
# claude-code needs a WRITABLE config dir for session-env/, history, etc. In
# subscription mode the operator's credentials are bind-mounted READ-ONLY at
# ~/.claude, so claude-code fails EROFS trying to mkdir ~/.claude/session-env.
# Dispatch points CLAUDE_CONFIG_DIR at a writable path; populate it here from the
# read-only mount (the creds + ~/.claude.json) so claude reads the creds AND can
# write its runtime state. No-op unless CLAUDE_CONFIG_DIR is set to a path other
# than ~/.claude (i.e. api-key/no-mount runs, which keep the writable image default).
prepare_claude_config_dir() {
    local cfg="${CLAUDE_CONFIG_DIR:-}"
    [[ -n "$cfg" && "$cfg" != "${HOME}/.claude" ]] || return 0
    mkdir -p "$cfg" || return 0
    [[ -d "${HOME}/.claude" ]] && cp -a "${HOME}/.claude/." "$cfg/" 2>/dev/null || true
    [[ -f "${HOME}/.claude.json" ]] && cp -a "${HOME}/.claude.json" "$cfg/.claude.json" 2>/dev/null || true
}

# ── Managed subscription (proxy-injected, compose mode) ───────────────────────
# In managed mode there is NO host ~/.claude to mount (the compose control plane
# is distroless). Dispatch instead delivers an inert SENTINEL .credentials.json
# in WARDYN_CLAUDE_MANAGED_B64 (base64 of the JSON) — the same shape
# stage-claude-creds.sh writes for the resident path: a placeholder access token,
# blank refresh token, far-future expiry. It only lets `claude` consider itself
# logged in and start cleanly; the LIVE token is injected proxy-side per request
# (Authorization: Bearer, replacing whatever the sandbox sends), so no usable
# credential is ever resident. Written into CLAUDE_CONFIG_DIR (dispatch points it
# at a writable dir). No-op when WARDYN_CLAUDE_MANAGED_B64 is unset. Must run
# AFTER prepare_claude_config_dir (which created the dir).
materialize_managed_claude_config() {
    [[ -n "${WARDYN_CLAUDE_MANAGED_B64:-}" ]] || return 0
    local cfg="${CLAUDE_CONFIG_DIR:-${HOME}/.claude}"
    mkdir -p "$cfg" 2>/dev/null || true
    if printf '%s' "$WARDYN_CLAUDE_MANAGED_B64" | base64 -d > "$cfg/.credentials.json" 2>/dev/null; then
        chmod 0600 "$cfg/.credentials.json" 2>/dev/null || true
    fi
    # Onboarding-complete marker so an interactive managed session doesn't prompt.
    [[ -f "${HOME}/.claude.json" ]] || printf '%s\n' '{"hasCompletedOnboarding":true}' > "${HOME}/.claude.json" 2>/dev/null || true
}

# ── Artifact-registry redirect config ────────────────────────────────────────
# When the operator has configured artifact-registry redirects (site-config),
# dispatch delivers the per-tool config files (URL-only, NO token — a token is
# injected proxy-side) in WARDYN_ARTIFACT_CONFIG_B64 as newline-delimited
# "<home-relative-path>\t<base64(content)>" records. Materialize each under $HOME
# before any clone/install so npm/pip/cargo/maven/nuget pull from the corp mirror
# (go rides GOPROXY/GOSUMDB env, set directly on the sandbox). Paths are a fixed,
# control-plane-generated set; reject any "../" defensively (never sandbox input).
# No-op when the var is unset.
install_artifact_config() {
    [[ -n "${WARDYN_ARTIFACT_CONFIG_B64:-}" ]] || return 0
    local relpath b64 dst
    while IFS=$'\t' read -r relpath b64; do
        [[ -n "$relpath" && -n "$b64" ]] || continue
        case "$relpath" in
            /*|*..*) echo "agent-run: skipping unsafe artifact-config path: $relpath" >&2; continue ;;
        esac
        dst="${HOME}/${relpath}"
        mkdir -p "$(dirname "$dst")"
        if printf '%s' "$b64" | base64 -d > "$dst" 2>/dev/null; then
            chmod 0644 "$dst"
        else
            echo "agent-run: WARNING failed to write artifact config ${relpath}" >&2
        fi
    done <<< "$WARDYN_ARTIFACT_CONFIG_B64"
}

# ── git credential helper caller-auth secret ─────────────────────────────────
# Provision the per-run secret that gates wardyn-git-helper credential emission.
# Only meaningful when the run can mint a git credential — i.e. it has a
# github_token grant (WARDYN_GITHUB_GRANT_ID) OR a git_pat grant
# (WARDYN_GIT_PAT_GRANTS).  A run with neither never reaches the helper's mint
# path, so we skip it.
#
# The secret is written to a 0400 file OWNED BY the agent uid (this script runs as
# agent) so it is not group/other-readable, and exported as WARDYN_GIT_HELPER_SECRET
# for THIS process tree only — the subsequent `git clone` and the exec'd agent
# inherit it, but a separate `wardyn attach` exec (a fresh docker exec, not a
# descendant of this script) does NOT.  The credential helper compares the
# presented env value against the file before emitting a credential.
#
# MUST be called directly (not in a subshell) so the `export` survives for the
# clone and the exec below.  No-op (returns 0) when there is no git grant.
# The secret is never echoed to stdout/stderr.
provision_git_helper_secret() {
    [[ -n "${WARDYN_GITHUB_GRANT_ID:-}" || -n "${WARDYN_GIT_PAT_GRANTS:-}" ]] || return 0
    local dir="${HOME}/.wardyn"
    local secret_file="${dir}/git-helper.secret"
    mkdir -p "$dir"
    # Generate a fresh 256-bit random secret as hex (no shell-special chars).
    # /dev/urandom + od are always present in the bookworm-slim base image.
    local secret
    secret="$(od -An -tx1 -N 32 /dev/urandom | tr -d ' \n')"
    if [[ -z "$secret" ]]; then
        echo "agent-run: WARNING could not generate git-helper secret; private git auth disabled this run" >&2
        return 0
    fi
    # Write 0400, agent-owned. umask guards the create window; chmod is explicit.
    ( umask 077; printf '%s' "$secret" > "$secret_file" )
    chmod 0400 "$secret_file"
    export WARDYN_GIT_HELPER_SECRET="$secret"
}

# ── git-broker insteadOf (Option C: repo-scoped git egress) ──────────────────
# When the run has a github_token grant, route every GRANTED GitHub repo's git
# traffic through the Wardyn git-broker (WARDYN_PROXY_URL/wardyn/gh/<org>/<repo>)
# instead of dialing github.com directly. git rewrites the transport via
# url.<broker>.insteadOf; the proxy enforces the per-repo allowlist and mints the
# scoped installation token SERVER-SIDE (it never enters this sandbox). github.com
# is not in this run's egress allowlist, so an un-brokered github URL is denied —
# the repo is the unit of trust. No-op when there is no github grant.
#
# Only bare "<org>/<repo>" slugs (the github_token clone form) are rewritten. The
# ssh-form is additionally rewritten ONLY when the run has no ssh_key grants, so an
# ssh_key-backed repo keeps its own ssh-over-443 lane (the broker is HTTPS-only).
# MUST be called before dispatch_repo_clones so the clone uses the broker URL.
configure_git_broker_insteadof() {
    [[ -n "${WARDYN_GITHUB_GRANT_ID:-}" ]] || return 0
    local base="${WARDYN_PROXY_URL:-http://wardyn-proxy:3128}"
    base="${base%/}"
    local records="${WARDYN_REPOS:-}"
    if [[ -z "$records" && -n "${WARDYN_REPO_SLUG:-}" ]]; then
        records=$'\t\t'"${WARDYN_REPO_SLUG}"
    fi
    [[ -n "$records" ]] || return 0
    local have_ssh=0
    [[ -n "${WARDYN_SSH_GRANTS:-}" && "${WARDYN_SSH_GRANTS}" != "{}" ]] && have_ssh=1
    local _u _d slug broker
    while IFS=$'\t' read -r _u _d slug; do
        slug="${slug%.git}"
        # bare <org>/<repo> only — skip full URLs, ssh slugs, and deeper paths.
        [[ "$slug" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || continue
        broker="${base}/wardyn/gh/${slug}"
        git config --global url."${broker}".insteadOf "https://github.com/${slug}" || true
        if [[ "$have_ssh" -eq 0 ]]; then
            git config --global --add url."${broker}".insteadOf "git@github.com:${slug}" || true
            git config --global --add url."${broker}".insteadOf "ssh://git@github.com/${slug}" || true
        fi
    done <<< "$records"
}

# Clone the run's repo(s) into the workspace, if requested and not already present.
# WARDYN_REPOS (multi) supersedes the legacy single WARDYN_REPO_URL: a
# newline-delimited list of TAB-separated <url>\t<dest>\t<slug> records built by the
# control plane. Every field is control-plane-sanitised (no whitespace/control
# chars, repoFieldSafe) and every dest is a validated in-container path — ALWAYS
# quote, never interpolate into a shell word. A clone is attempted only when the
# dest has no existing .git, so a pre-populated / bind-mounted repo is never
# clobbered. Failure is a governance signal, not fatal: log and continue.
clone_one() {  # $1=url  $2=dest  $3=slug
    local url="$1" dest="$2" slug="$3"
    [[ -n "$url" && -n "$dest" ]] || return 0
    [[ -e "${dest}/.git" ]] && return 0   # never clobber a populated/bind-mounted repo
    mkdir -p "$dest"
    echo "agent-run: cloning ${slug:-$url} into ${dest} (shallow, via wardyn-proxy)" >&2
    if git clone --depth 1 -- "$url" "$dest"; then
        echo "agent-run: clone OK (${dest})" >&2
    else
        echo "agent-run: clone FAILED for ${dest} — the egress policy or credential broker may have blocked it (a governance signal); continuing" >&2
    fi
}

# dispatch_repo_clones — runs clone_one over WARDYN_REPOS (preferred) or falls
# back to the legacy single WARDYN_REPO_URL. Reads/writes the caller's
# `workdir` global (must be set before calling); MUST be called directly (not
# in a subshell/function-with-local-workdir) if the caller wants `workdir`
# itself to reflect any legacy single-repo naming — it does not mutate
# `workdir` itself, only uses it to compute clone destinations.
dispatch_repo_clones() {
    if [[ -n "${WARDYN_REPOS:-}" ]]; then
        while IFS=$'\t' read -r r_url r_dest r_slug; do
            [[ -n "$r_url" ]] || continue
            clone_one "$r_url" "$r_dest" "$r_slug"
        done <<< "$WARDYN_REPOS"
    elif [[ -n "${WARDYN_REPO_URL:-}" ]]; then
        # Legacy single-repo fallback (one release; superseded by WARDYN_REPOS).
        local existing reponame
        existing="$(find "$workdir" -maxdepth 2 -mindepth 1 -type d -name ".git" \
                     2>/dev/null | head -1 || true)"
        if [[ -z "$existing" ]]; then
            reponame="${WARDYN_REPO_URL##*/}"
            reponame="${reponame%.git}"
            [[ -z "$reponame" ]] && reponame="repo"
            clone_one "$WARDYN_REPO_URL" "${workdir}/${reponame}" "${WARDYN_REPO_SLUG:-}"
        fi
    fi
}

# resolve_workdir — prefer a cloned/discovered repo workspace if one exists
# below the caller's `workdir` global, else leave it as-is. Matches both a repo
# cloned into workdir/<reponame> (its .git is one level down) and a
# pre-populated workdir that is itself a repo (.git directly under it).
# MUST be called directly (not in a subshell) so the `workdir` reassignment
# survives for the caller.
resolve_workdir() {
    if [[ -d "$workdir" ]]; then
        local repo
        repo="$(find "$workdir" -maxdepth 2 -mindepth 1 -type d -name ".git" \
                 -exec dirname {} \; 2>/dev/null | head -1 || true)"
        if [[ -n "$repo" ]]; then
            workdir="$repo"
        fi
    fi
}

# maybe_exec_task_mode "<task>" — exec task mode (BYOA/CI lane): when the
# control plane set WARDYN_TASK_MODE=exec, run the task as a plain shell
# command INSTEAD of the agent harness and never return. No-op otherwise.
# Everything before this call (MITM CA, clone, brokered creds, recording) is
# identical to a harness run — this only chooses WHAT gets exec'd, and it
# deliberately skips the LLM auth wiring (a plain command needs no model).
# The command's exit code propagates to the caller (0 -> COMPLETED, else FAILED).
maybe_exec_task_mode() {
    if [[ "${WARDYN_TASK_MODE:-}" == "exec" ]]; then
        echo "agent-run: exec task mode — running task as a shell command (no agent harness)" >&2
        exec /bin/sh -lc "$1"
    fi
}

# maybe_exec_compose_mode — governed COMPOSE run (AI Run Composer sandbox
# backend): when WARDYN_COMPOSE_ONLY=1, run the REAL claude in PLAN mode with all
# built-in tools disabled and structured JSON output — the EXACT least-privilege
# arg set the host `cli` composer wire uses (runClaude) — capture its stdout, PUT
# it to the brokered compose-results route (the proxy injects the run token, the
# SAME mechanism wardyn-scan uses to deliver scan-results), and exit. No agent
# harness and no model action against the host: plan mode + the full
# --disallowedTools denylist + --strict-mcp-config. claude is used purely as a
# schema-forced JSON generator; the control plane Grade+Clamps the proposal.
#
# The system prompt, user message, and JSON schema arrive base64-encoded in env
# (so multi-line / shell-special content survives untouched); the tool denylist,
# turn cap, and optional model are plain fixed strings. The managed subscription
# token is NEVER here — it is injected proxy-side (invariant 1); the sandbox holds
# only the inert sentinel materialize_managed_claude_config wrote. No-op unless
# WARDYN_COMPOSE_ONLY=1; NEVER returns when it is set.
maybe_exec_compose_mode() {
    [[ "${WARDYN_COMPOSE_ONLY:-}" == "1" ]] || return 0
    local sys user schema out url args
    sys="$(printf '%s' "${WARDYN_COMPOSE_SYSTEM_B64:-}" | base64 -d)"
    user="$(printf '%s' "${WARDYN_COMPOSE_PROMPT_B64:-}" | base64 -d)"
    schema="$(printf '%s' "${WARDYN_COMPOSE_SCHEMA_B64:-}" | base64 -d)"
    args=(
        -p "$user"
        --output-format json
        --json-schema "$schema"
        --append-system-prompt "$sys"
        --permission-mode plan
        --strict-mcp-config
        --disallowedTools "${WARDYN_COMPOSE_DISALLOWED_TOOLS:-}"
        --max-turns "${WARDYN_COMPOSE_MAX_TURNS:-6}"
    )
    [[ -n "${WARDYN_COMPOSE_MODEL:-}" ]] && args+=( --model "$WARDYN_COMPOSE_MODEL" )
    echo "agent-run: compose-only mode — running claude in plan mode for a run proposal" >&2
    # Capture stdout (the JSON wrapper). A nonzero claude exit / is_error wrapper is
    # STILL delivered: the waiting composer fails closed on it (ExtractProposalJSON),
    # which is a clearer signal than an empty upload, so never abort before the PUT.
    out="$(claude "${args[@]}" 2>/dev/null || true)"
    url="${WARDYN_PROXY_URL%/}/wardyn/v1/compose-results/${WARDYN_RUN_ID}"
    # PUT directly to the proxy's brokered local route (--noproxy '*' so curl dials
    # the proxy with an origin-form path it serves locally + injects the run token,
    # exactly as mint_ssh_key does). No Authorization header — the proxy strips any.
    if printf '%s' "$out" | curl -fsS --noproxy '*' -X PUT \
        -H 'Content-Type: application/json' --data-binary @- "$url" >/dev/null; then
        echo "agent-run: compose proposal uploaded" >&2
    else
        echo "agent-run: WARNING compose proposal upload failed (the composer will report no proposal)" >&2
    fi
    exit 0
}
