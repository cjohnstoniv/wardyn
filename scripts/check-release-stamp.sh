#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-release-stamp.sh <go-binary> — prove a built Go binary carries the release stamp.
#
# release.yml stamps a release build with `-ldflags -X .../internal/version.releaseBuild=true`.
# `-trimpath` makes Go drop -ldflags from `go version -m`, and the macOS guest agent has no
# version command, so neither can say whether the agent was stamped. The binary itself can: the
# linker turns the -X value into an initialised `releaseBuild` variable (symbol type D; an
# unstamped build leaves it zero-initialised, type B) plus a `releaseBuild.str` symbol holding
# the text. This reads that text out of the file. A darwin/arm64 Go binary maps its read-only data
# at virtual address 0x100000000 + file offset, so the symbol address gives the file offset; the
# macOS guest agent is the only caller, and any other target is refused rather than guessed at.
# Exit 0 only for a variable stamped exactly "true"; 1 for anything else; 2 for a bad call.
set -euo pipefail

SYM="github.com/cjohnstoniv/wardyn/internal/version.releaseBuild"
BASE=$((0x100000000))

[ "$#" -eq 1 ] && [ -f "$1" ] || { echo "usage: $0 <go-binary>" >&2; exit 2; }
bin="$1"

case "$(go version -m "$bin")" in
    *$'\t'build$'\t'GOOS=darwin*GOARCH=arm64*|*$'\t'build$'\t'GOARCH=arm64*GOOS=darwin*) ;;
    *) echo "check-release-stamp: $bin is not a darwin/arm64 Go binary; the file offset is only known for that target" >&2; exit 2 ;;
esac
nm="$(go tool nm "$bin")"
var="$(awk -v s="$SYM" '$3 == s { print $2 }' <<<"$nm")"
str="$(awk -v s="${SYM}.str" '$3 == s { print $1 }' <<<"$nm")"
if [ "$var" != D ] || [ -z "$str" ]; then
    echo "check-release-stamp: $bin has no stamped ${SYM##*/} (symbol type '${var:-absent}', value symbol '${str:-absent}'): it was not built with the release -ldflags" >&2
    exit 1
fi
# The variable is a string header (data pointer, length; 8 bytes each, little-endian) in __DATA. Read
# the stored length and bytes, so only exactly "true" passes and a longer value starting with it fails.
addr="$(awk -v s="$SYM" '$3 == s { print $1 }' <<<"$nm")"
read -r ptr len < <(od -An -v -t u8 -j $((0x$addr - BASE)) -N 16 "$bin")
got="$(dd if="$bin" bs=1 skip=$((ptr - BASE)) count="$len" 2>/dev/null || true)"
if [ "$len" != 4 ] || [ "$got" != true ]; then
    echo "check-release-stamp: $bin carries ${SYM##*/}='${got}', want 'true'" >&2
    exit 1
fi
echo "check-release-stamp: $bin carries ${SYM##*/}=true"
