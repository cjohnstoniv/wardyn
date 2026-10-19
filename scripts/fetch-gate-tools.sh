#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/fetch-gate-tools.sh [--list | NAME...] — download the modules of the
# `go run tool@version` gate tools into the Go module cache, and nothing else.
#
# A gate's `go run` otherwise fetches the tool's own dependencies cold, from the
# network, inside the make step: one stalled fetch ate the whole job timeout, and
# another failed reading sum.golang.org. CI runs this as its own time-bounded step
# before the gate, and the go-modules job runs it so the shared module cache already
# holds the modules (and the checksum-database tiles that vouch for them).
#
# NAME is one of: govulncheck staticcheck licenses (alias: notices) gitleaks
# license-headers golangci-lint actionlint. No NAME fetches all of them. --list prints "NAME package@version"
# per tool, which CI hashes into the module cache key so a version bump re-saves it.
# Versions come from the pins the gates themselves use (Makefile, license-headers.sh).
# golangci-lint and actionlint are `make lint`'s, run in ci.yml's go (lint) leg.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkver() { grep -oP "^$1\s*\?=\s*\K\S+" Makefile || { echo "fetch-gate-tools: no $1 in Makefile" >&2; exit 1; }; }
TOOLS="govulncheck golang.org/x/vuln/cmd/govulncheck@$(mkver GOVULNCHECK_VERSION)
staticcheck honnef.co/go/tools/cmd/staticcheck@$(mkver STATICCHECK_VERSION)
licenses github.com/google/go-licenses@$(mkver GO_LICENSES_VERSION)
gitleaks github.com/zricethezav/gitleaks/v8@$(mkver GITLEAKS_VERSION)
license-headers $(grep -oP '^ADDL=\K\S+' scripts/license-headers.sh)
golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(mkver GOLANGCI_LINT_VERSION)
actionlint github.com/rhysd/actionlint/cmd/actionlint@$(mkver ACTIONLINT_VERSION)"

if [[ "${1:-}" == "--list" ]]; then
  printf '%s\n' "${TOOLS}"
  exit 0
fi

want=("$@")
[[ ${#want[@]} -gt 0 ]] || want=(govulncheck staticcheck licenses gitleaks license-headers golangci-lint actionlint)
for name in "${want[@]}"; do
  [[ "${name}" == notices ]] && name=licenses
  spec="$(awk -v n="${name}" '$1 == n {print $2}' <<<"${TOOLS}")"
  [[ -n "${spec}" ]] || { echo "fetch-gate-tools: unknown tool ${name}" >&2; exit 2; }
  # One throwaway module per tool: tools resolved together could select different
  # dependency versions than the tool's own `go run tool@version` does.
  work="$(mktemp -d)"
  (
    cd "${work}"
    go mod init gatetools >/dev/null 2>&1
    go get "${spec}"
    go list -deps "${spec%@*}" >/dev/null
  )
  rm -rf "${work}"
  echo "fetch-gate-tools: ${spec} modules are in the module cache"
done
