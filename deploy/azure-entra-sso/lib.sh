# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# lib.sh — shared by 01-tenant-prep.sh, 02-app.sh, 03-people.sh. Sourced, not
# executed (no shebang, no `set -e` of its own — inherits the caller's).

set_var() { # set_var NAME VALUE — idempotent upsert into .env.local, 0600
  local name="$1" value="$2"
  touch "${ENV_FILE}"; chmod 600 "${ENV_FILE}"
  local quoted
  quoted="$(printf '%q' "${value}")"
  if grep -q "^${name}=" "${ENV_FILE}" 2>/dev/null; then
    sed -i "s|^${name}=.*|${name}=${quoted}|" "${ENV_FILE}"
  else
    printf '%s=%s\n' "${name}" "${quoted}" >> "${ENV_FILE}"
  fi
}
