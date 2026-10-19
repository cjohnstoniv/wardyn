# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/up-seed.sh — seed_host_proxy, split out of up.sh to keep that file
# under the repo's file-size gate (scripts/check-file-size.sh). Sourced by up.sh,
# never run standalone. Uses names defined there: REPO_ROOT, ENV_FILE,
# IMAGES_ORIGIN, host_goarch(), plus log/warn/env_set from the common helpers.

# seed_host_proxy — run the host-proxy detector ON THE HOST and stash the result
# for the containerized wardynd. Inside the container every tier is blind (the OS
# tier dispatches on the process's GOOS; git needs a binary distroless lacks; HOME
# is unset so no shell profile or tool config is reachable), so the wizard would
# otherwise assert a false negative on exactly the corp hosts that have a proxy.
#
# Wholly best-effort: every step tolerates failure and leaves the seed unset, in
# which case the wizard says "detection ran in a container and couldn't look at
# your host" instead of "nothing is configured". Rewritten on every `up`, so it
# can never go stale across a network change. base64 because compose interpolates
# `$` inside .env values; the payload is credential-masked before it is emitted.
#
# Also leaves a working host-native CLI at bin/wardyn (same convention as
# scripts/setup.sh's host-mode `go build -o bin/wardyn`) for cmd_up's hand-off
# to point at — the containerized path installs no `wardyn` on PATH otherwise.
seed_host_proxy() {
  _shp_bin="${REPO_ROOT}/bin/wardyn"
  _shp_mark="${REPO_ROOT}/bin/.wardyn-from-image"
  mkdir -p "${REPO_ROOT}/bin" 2>/dev/null || return 0
  # A CLI already on this host that runs is the operator's (a `go build`, a
  # package): keep it. Only a binary this function extracted earlier (marked), or
  # one that cannot run here (a foreign-arch seed from before 0.10), is replaced.
  if [ -x "${_shp_bin}" ] && { [ ! -e "${_shp_mark}" ] && "${_shp_bin}" --help >/dev/null 2>&1; }; then
    :
  else
    _shp_os="${WARDYN_HOST_GOOS:-linux}"
    _shp_arch="${WARDYN_HOST_GOARCH:-$(host_goarch)}"
    _shp_new="${_shp_bin}.new"
    rm -f "${_shp_new}"
    # distroless has no shell, so `docker run … cat` can't work — create, cp, rm.
    _shp_cid=$(docker create wardyn/wardynd:local 2>/dev/null) || { unset _shp_os _shp_arch _shp_new _shp_cid; return 0; }
    # The image carries one CLI per host platform. /host/wardyn alone is the
    # image's own build target: the host's when built from this tree, linux/amd64
    # in a pulled image, so it is only right where those agree.
    if ! docker cp "${_shp_cid}:/host/wardyn-${_shp_os}-${_shp_arch}" "${_shp_new}" >/dev/null 2>&1; then
      if [ "${IMAGES_ORIGIN}" = built ] || { [ "${_shp_os}" = linux ] && [ "${_shp_arch}" = amd64 ]; }; then
        docker cp "${_shp_cid}:/host/wardyn" "${_shp_new}" >/dev/null 2>&1 || true
      fi
    fi
    docker rm -f "${_shp_cid}" >/dev/null 2>&1 || true
    if [ -f "${_shp_new}" ] && chmod +x "${_shp_new}" && "${_shp_new}" --help >/dev/null 2>&1; then
      mv -f "${_shp_new}" "${_shp_bin}"
      : > "${_shp_mark}"
    else
      rm -f "${_shp_new}"
      # A seed that cannot run here is worse than none: the hand-off would print
      # it as the command to run.
      [ ! -x "${_shp_bin}" ] || "${_shp_bin}" --help >/dev/null 2>&1 || rm -f "${_shp_bin}"
      warn "No wardyn CLI in the image runs on this host (${_shp_os}/${_shp_arch}); skipping host proxy detection. Build one: go install github.com/cjohnstoniv/wardyn/cmd/wardyn@latest"
    fi
    unset _shp_os _shp_arch _shp_new _shp_cid
  fi
  [ -x "${_shp_bin}" ] || { unset _shp_bin _shp_mark; return 0; }
  # This is HOST-NATIVE execution of a binary taken out of a container image —
  # not a container entrypoint, so no confinement applies to it at all. Fine
  # when the image was built from this tree, and fine when cosign verified it;
  # say so when it was neither (threatmodel/THREAT-MODEL.md s5 residual 32).
  [ "${IMAGES_ORIGIN}" = pulled ] && \
    warn "About to run bin/wardyn on this HOST, extracted from an image pulled by tag and verified by nothing (no cosign on PATH). Skip it with WARDYN_BUILD_LOCAL=1, or verify first — docs/VERIFY.md s1."
  _shp_json=$("${_shp_bin}" setup detect-proxy 2>/dev/null) || _shp_json=""
  if [ -n "${_shp_json}" ]; then
    _shp_b64=$(printf '%s' "${_shp_json}" | base64 2>/dev/null | tr -d '\n') || _shp_b64=""
    if [ -n "${_shp_b64}" ]; then
      env_set "${ENV_FILE}" WARDYN_HOST_PROXY_B64 "${_shp_b64}"
      log "Host proxy detection seeded from this host (the container can't read your shell/OS/PAC settings)."
    fi
  fi
  unset _shp_bin _shp_mark _shp_json _shp_b64
}
