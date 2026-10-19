#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/lib/image-build.sh — `docker build` that keeps its output.
#
# The scripts built images with `-q ... >/dev/null`, so a failed build said only
# "build failed" and the tool's own error (a `go mod download` network fault in a
# hosted run) was thrown away. wardyn_docker_build logs to a temp file and prints
# its tail on failure; a clean build prints nothing, as before.

# wardyn_docker_build ARGS... — `docker build ARGS...`, returning docker's status.
wardyn_docker_build() {
  local blog rc
  blog="$(mktemp /tmp/wardyn-image-build.XXXXXX.log)" || return 1
  docker build "$@" >"${blog}" 2>&1
  rc=$?
  if [[ ${rc} -ne 0 ]]; then
    printf '[image build failed (rc=%s); last 40 lines of its log]\n' "${rc}" >&2
    tail -n 40 "${blog}" >&2
  fi
  rm -f "${blog}"
  return "${rc}"
}

# wardyn_go_image_build ARGS... — wardyn_docker_build for a Dockerfile with the
# `gomodcache` stage (deploy/compose/Dockerfile.proxy, deploy/images/oracle).
# When WARDYN_BUILD_GOMODCACHE names a directory, it becomes that stage, so the
# build's `go mod download` finds the job's module cache and fetches nothing it
# already has. go.sum still verifies every module. Unset, the stage is empty.
wardyn_go_image_build() {
  local seed=()
  if [[ -n "${WARDYN_BUILD_GOMODCACHE:-}" && -d "${WARDYN_BUILD_GOMODCACHE}" ]]; then
    seed=(--build-context "gomodcache=${WARDYN_BUILD_GOMODCACHE}")
  fi
  wardyn_docker_build "${seed[@]}" "$@"
}
