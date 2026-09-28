#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/kind-sso-walk.sh's rate-limited pull path for the cold aws-sso image
# (#891, #1307), split out to keep the walk under the 1000-line file-size gate.
# Sourced, not exec'd: it reads REGISTRY_NAME and KIND_NETWORK from the walk
# and sets COLD_PULL_THROTTLE_NAME, which the walk's hosts.toml and its EXIT
# trap use.
#
# WHY A RATE LIMIT, NOT A BIGGER IMAGE. The sign-in pane learns about the pull
# from the pod's Events, read at most once a second (the k8s runner's
# pullEventsEvery), and polls the run every 2 s itself (harness-login-pane.tsx's
# RUN_POLL_MS). A local registry on the same host delivers the whole pad in
# well under a second whatever its size (nightly: 123 MiB in 709 ms), so the
# download step went pending -> done between two polls and the walk's
# "the download step lit" assertion failed on a pull that really happened.
# Serving the node through nginx's per-connection limit_rate makes the pull
# take seconds by construction: at a 4 MB/s cap, 64-128 MiB is 16-32 s nominal
# (nginx overshoots its cap somewhat; 32 MiB at 8m measured 3.1 s, not 4.2 s).
#
# Only the NODE's pulls go through the limit. The host's `docker push` still
# talks to the registry directly on its published loopback port.
COLD_PULL_THROTTLE_NAME="${REGISTRY_NAME}-throttle"
COLD_PULL_RATE="4m"

serve_cold_pull_throttle() {
  docker rm -f "${COLD_PULL_THROTTLE_NAME}" >/dev/null 2>&1 || true
  # $http_host is nginx's variable, not the shell's: the single-quoted heredoc
  # keeps it literal, and the two walk values go in through sed.
  local conf
  conf="$(sed -e "s|@REGISTRY@|${REGISTRY_NAME}|" -e "s|@RATE@|${COLD_PULL_RATE}|" <<'NGINX'
server {
  listen 5000;
  client_max_body_size 0;
  location / {
    proxy_pass http://@REGISTRY@:5000;
    proxy_set_header Host $http_host;
    proxy_max_temp_file_size 0;
    limit_rate @RATE@;
  }
}
NGINX
)"
  docker run -d --name "${COLD_PULL_THROTTLE_NAME}" --network "${KIND_NETWORK}" \
    -e "WALK_NGINX_CONF=${conf}" nginx:1.27-alpine \
    sh -c 'printf "%s\n" "${WALK_NGINX_CONF}" > /etc/nginx/conf.d/default.conf && exec nginx -g "daemon off;"' \
    >/dev/null || die "could not start the rate-limited pull path ${COLD_PULL_THROTTLE_NAME}"
  for _ in $(seq 1 30); do
    docker exec "${COLD_PULL_THROTTLE_NAME}" wget -q -O /dev/null "http://127.0.0.1:5000/v2/" 2>/dev/null && return 0
    sleep 1
  done
  docker logs "${COLD_PULL_THROTTLE_NAME}" 2>&1 | tail -20 >&2
  die "${COLD_PULL_THROTTLE_NAME} never answered /v2/ through to ${REGISTRY_NAME}"
}
