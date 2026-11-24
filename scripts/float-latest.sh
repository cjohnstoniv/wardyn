#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# float-latest.sh REF_NAME IMAGE... (with DRY_RUN=true|false): point ghcr.io/cjohnstoniv/<IMAGE>:latest at the
# highest final release that is published, for each IMAGE. release.yml's float-latest job runs it.
#
# The rule: `:latest` names the highest final X.Y.Z across every release line, so a patch to an older
# line never moves it back. A final is a candidate when its vX.Y.Z tag is on origin (read now, with
# git ls-remote) and it is at or above REF_NAME (every final, for a ref that is not a final tag). It is published
# for IMAGE when ghcr.io/cjohnstoniv/IMAGE:X.Y.Z exists (an anonymous registry read) and that digest carries the
# signature of release.yml's run for vX.Y.Z. The highest published candidate is the target: the run that writes
# last leaves the same answer whatever order the runs finish in, and a run that finds a higher final already
# published moves :latest up to it, never down.
#
# A prerelease tag ref (vX.Y.Z-...) is skipped: it prints a notice and exits 0 before any read, so an RC never
# floats `:latest` and never stops a publish. A non-tag ref (the dry-run proof on a branch) still prints the plan.
#
# Fails closed, before any write: a registry answer other than 200 or 404, a published candidate whose
# signature does not verify, or no published final at or above REF_NAME. Writes only for a final REF_NAME with
# DRY_RUN=false; otherwise it prints the plan and writes nothing.
set -euo pipefail
die() { echo "float-latest: $*" >&2; exit 1; }
[ "$#" -ge 2 ] || { echo "usage: DRY_RUN=true|false $0 REF_NAME IMAGE..." >&2; exit 2; }
case "${DRY_RUN:-}" in true|false) ;; *) echo "float-latest: DRY_RUN must be true or false" >&2; exit 2 ;; esac
REF="$1"; shift
if [[ "$REF" =~ ^v[0-9]+\.[0-9]+\.[0-9]+- ]]; then echo "float-latest: ${REF} is a prerelease tag: skipped, no write"; exit 0; fi
OWNER=cjohnstoniv
ISS=https://token.actions.githubusercontent.com
FINAL_RE='^v[0-9]+\.[0-9]+\.[0-9]+$'
ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

# manifest IMAGE REF: prints "<http code> <digest>" for an anonymous HEAD of ghcr.io/cjohnstoniv/IMAGE:REF.
manifest() {
  local token out
  token="$(curl -fsS "https://ghcr.io/token?scope=repository:${OWNER}/$1:pull&service=ghcr.io" | jq -r '.token // empty')" || return 1
  [ -n "$token" ] || return 1
  out="$(curl -sS -I -w '\n%{http_code}' -H "Authorization: Bearer ${token}" -H "Accept: ${ACCEPT}" \
    "https://ghcr.io/v2/${OWNER}/$1/manifests/$2")" || return 1
  printf '%s %s\n' "$(tail -n 1 <<<"$out")" \
    "$(tr -d '\r' <<<"$out" | awk 'tolower($1) == "docker-content-digest:" { print $2 }' | head -n 1)"
}

mapfile -t finals < <(git ls-remote --tags --refs origin 'refs/tags/v*' | awk '{ sub("^refs/tags/", "", $2); print $2 }' \
  | grep -E "$FINAL_RE" | sort -V -r)
[ "${#finals[@]}" -gt 0 ] || die "no final release tag on origin"
is_final=0
if [[ "$REF" =~ $FINAL_RE ]]; then
  is_final=1
  grep -qxF "$REF" <<<"$(printf '%s\n' "${finals[@]}")" || die "$REF is not a tag on origin"
fi
cands=()
for t in "${finals[@]}"; do
  if [ "$is_final" = 1 ] && [ "$(printf '%s\n' "$t" "$REF" | sort -V | head -n 1)" != "$REF" ]; then break; fi
  cands+=("$t")
done

plan=()
for img in "$@"; do
  target="" tdigest=""
  for t in "${cands[@]}"; do
    out="$(manifest "$img" "${t#v}")" || die "${img}:${t#v}: the registry read failed"
    read -r code digest <<<"$out"
    case "$code" in
      404) echo "skip ${img}:${t#v}: not published"; continue ;;
      200) ;;
      *) die "${img}:${t#v}: the registry answered ${code}" ;;
    esac
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "${img}:${t#v}: no digest in the registry's answer"
    cosign verify "ghcr.io/${OWNER}/${img}@${digest}" \
      --certificate-identity "https://github.com/${OWNER}/wardyn/.github/workflows/release.yml@refs/tags/${t}" \
      --certificate-oidc-issuer "$ISS" >/dev/null 2>&1 \
      || die "${img}:${t#v} (${digest}) does not carry the signature of release.yml's run for ${t}"
    target="$t" tdigest="$digest"
    break
  done
  [ -n "$target" ] || die "${img}: no published final at or above ${REF}"
  out="$(manifest "$img" latest)" || die "${img}:latest: the registry read failed"
  read -r code cur <<<"$out"
  case "$code" in 200) ;; 404) cur=none ;; *) die "${img}:latest: the registry answered ${code}" ;; esac
  action=keep
  [ "$cur" = "$tdigest" ] || action=move
  echo "PLAN ${img} latest=${cur} target=${target} ${tdigest} action=${action}"
  plan+=("${img} ${tdigest} ${action}")
done

[ "$is_final" = 1 ] || { echo "float-latest: ${REF} is not a final release tag: no write"; exit 0; }
[ "$DRY_RUN" = false ] || { echo "float-latest: DRY RUN: no write"; exit 0; }
for p in "${plan[@]}"; do
  read -r img digest action <<<"$p"
  if [ "$action" = keep ]; then echo "kept ghcr.io/${OWNER}/${img}:latest -> ${digest}"; continue; fi
  cosign copy --force "ghcr.io/${OWNER}/${img}@${digest}" "ghcr.io/${OWNER}/${img}:latest"
  out="$(manifest "$img" latest)" || die "${img}:latest: the registry read failed after the copy"
  read -r code got <<<"$out"
  { [ "$code" = 200 ] && [ "$got" = "$digest" ]; } || die "${img}:latest is ${got:-unreadable} after the copy, expected ${digest}"
  echo "moved ghcr.io/${OWNER}/${img}:latest -> ${digest}"
done
