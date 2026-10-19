#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# One record per UI package from `pnpm licenses list --json` on stdin:
# name, versions, licence, author, homepage, path. Fields are separated by the
# ASCII unit separator (0x1f), never a tab: tab is IFS whitespace, so `read`
# folds the empty author or homepage most packages have and shifts every later
# field left (#2465). Read with IFS=$'\x1f'.
set -euo pipefail
export LC_ALL=C
jq -r '.[][] | [.name, ((.versions // [.version]) | map(tostring) | join(", ")), (.license // ""), (.author // ""), (.homepage // ""), (.paths[0] // "")]
       | map(tostring | gsub("[\n\r\u001f]"; " ")) | join("\u001f")' | sort
