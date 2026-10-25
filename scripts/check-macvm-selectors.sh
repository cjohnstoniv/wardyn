#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-macvm-selectors.sh — the gate for the macOS 27 names that
# internal/macvm/provision_darwin.m looks up at run time.
#
# The provisioning file builds against the macOS 26 SDK, so the compiler never
# sees the macOS 27 class and selectors it uses: a misspelt name only shows up
# as "this Mac's Virtualization framework has no ..." on a real macOS 27 host.
# This gate pins every such name in two halves:
#
#   list half    every NSClassFromString(@"...") and @selector(...) in the
#                file is in LISTED below, and every LISTED name is still in
#                the file. Other run-time lookup forms (NSSelectorFromString,
#                objc_getClass, sel_registerName, ...) are refused, because
#                this gate cannot read them. Runs anywhere.
#   header half  every LISTED name is declared in the Virtualization headers of
#                a macOS 27 or later SDK. Runs on macOS with that SDK; skipped
#                elsewhere unless --sdk makes it required.
#
# Adding a lookup to the file means adding its name here in the same change.
#
# Usage:
#   check-macvm-selectors.sh              list half; header half when the SDK is there
#   check-macvm-selectors.sh --sdk        both halves; fail without a macOS 27 SDK
#   check-macvm-selectors.sh --selftest   plant violations in temp copies; each must fail
#   (--source FILE and --headers DIR point the halves at other inputs.)
set -euo pipefail
cd "$(dirname "$0")/.."

LISTED=(
	"class VZMacGuestProvisioningOptions"
	"selector setUsername:"
	"selector setPassword:"
	"selector setFullName:"
	"selector setLogsInAutomatically:"
	"selector setEnablesRemoteLogin:"
	"selector setGuestProvisioningOptions:error:"
)
MIN_SDK_MAJOR=27

SOURCE=internal/macvm/provision_darwin.m
HEADERS=""
MODE=default
while [ $# -gt 0 ]; do
	case "$1" in
	--sdk) MODE=sdk; shift ;;
	--selftest) MODE=selftest; shift ;;
	--source) SOURCE="$2"; shift 2 ;;
	--headers) HEADERS="$2"; shift 2 ;;
	*) echo "check-macvm-selectors.sh: unknown argument: $1" >&2; exit 2 ;;
	esac
done

# The names the source looks up, one "class X" / "selector x:" per line.
found_names() {
	{
		grep -oE 'NSClassFromString\(@"[A-Za-z0-9_]+"\)' "$1" | sed -E 's/.*@"([^"]+)".*/class \1/' || true
		grep -oE '@selector\([[:space:]]*[A-Za-z0-9_:]+[[:space:]]*\)' "$1" | sed -E 's/@selector\([[:space:]]*([^[:space:])]+)[[:space:]]*\)/selector \1/' || true
	} | sort -u
}

list_half() {
	local src="$1" bad=0 name
	[ -f "$src" ] || { echo "check-macvm-selectors.sh: no source file $src" >&2; return 1; }
	if grep -nE 'NSSelectorFromString|NSProtocolFromString|objc_getClass|objc_lookUpClass|objc_getRequiredClass|sel_registerName|sel_getUid|performSelector' "$src"; then
		echo "check-macvm-selectors.sh: $src uses a run-time lookup form this gate cannot read; use NSClassFromString(@\"...\") or @selector(...)" >&2
		bad=1
	fi
	local listed
	listed="$(printf '%s\n' "${LISTED[@]}" | sort -u)"
	while IFS= read -r name; do
		[ -n "$name" ] || continue
		if ! grep -qxF "$name" <<<"$listed"; then
			echo "check-macvm-selectors.sh: $src looks up \"$name\", which is not in LISTED" >&2
			bad=1
		fi
	done < <(found_names "$src")
	local found
	found="$(found_names "$src")"
	while IFS= read -r name; do
		if ! grep -qxF "$name" <<<"$found"; then
			echo "check-macvm-selectors.sh: LISTED has \"$name\", which $src no longer looks up" >&2
			bad=1
		fi
	done <<<"$listed"
	return "$bad"
}

# Declarations from every header, one per line (newlines folded, split on ';').
declarations() {
	cat "$1"/*.h | tr '\n\t' '  ' | tr ';' '\n'
}

header_half() {
	local dir="$1" decls bad=0 entry kind name re prop props piece
	compgen -G "$dir/*.h" >/dev/null || { echo "check-macvm-selectors.sh: no headers in $dir" >&2; return 1; }
	decls="$(declarations "$dir")"
	for entry in "${LISTED[@]}"; do
		kind="${entry%% *}"
		name="${entry#* }"
		if [ "$kind" = class ]; then
			re="@interface[[:space:]]+${name}[[:space:]]*:"
		else
			# A method declaration with these selector pieces, in order.
			re="[-+][[:space:]]*\([^)]*\)[[:space:]]*"
			local first=1
			IFS=: read -ra pieces <<<"${name%:}"
			for piece in "${pieces[@]}"; do
				if [ "$first" = 1 ]; then
					re+="${piece}[[:space:]]*:"
					first=0
				else
					re+=".*[^A-Za-z0-9_]${piece}[[:space:]]*:"
				fi
			done
		fi
		if grep -qE "$re" <<<"$decls"; then
			continue
		fi
		# A one-piece setX: is also declared by a writable @property x.
		if [ "$kind" = selector ] && [[ "$name" =~ ^set([A-Z][A-Za-z0-9_]*):$ ]]; then
			prop="${BASH_REMATCH[1]}"
			prop="$(tr '[:upper:]' '[:lower:]' <<<"${prop:0:1}")${prop:1}"
			props="$(grep -E "@property.*[^A-Za-z0-9_]${prop}([[:space:]]+[A-Z_][A-Za-z0-9_]*(\\(.*\\))?)*[[:space:]]*$" <<<"$decls" || true)"
			if [ -n "$props" ] && grep -v readonly <<<"$props" >/dev/null; then
				continue
			fi
		fi
		echo "check-macvm-selectors.sh: $entry is not declared in $dir" >&2
		bad=1
	done
	return "$bad"
}

# The SDK's Virtualization header dir, or empty when there is no macOS 27 SDK.
sdk_headers() {
	[ "$(uname -s)" = Darwin ] && command -v xcrun >/dev/null || return 0
	local ver
	ver="$(xcrun --sdk macosx --show-sdk-version)"
	echo "check-macvm-selectors.sh: macOS SDK $ver" >&2
	[ "${ver%%.*}" -ge "$MIN_SDK_MAJOR" ] || return 0
	echo "$(xcrun --sdk macosx --show-sdk-path)/System/Library/Frameworks/Virtualization.framework/Headers"
}

selftest() {
	local self="scripts/check-macvm-selectors.sh" fails=0
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT

	expect() { # expect pass|fail <description> <args...>
		local want="$1" what="$2"
		shift 2
		if bash "$self" "$@" >/dev/null 2>&1; then got=pass; else got=fail; fi
		if [ "$got" = "$want" ]; then
			echo "  ok   $what ($got)"
		else
			echo "  FAIL $what: wanted $want, got $got" >&2
			fails=1
		fi
	}

	echo "check-macvm-selectors.sh --selftest:"
	expect pass "the checked-in source" --source "$SOURCE"

	cp "$SOURCE" "$tmp/unlisted-selector.m"
	echo 'static SEL extra(void) { return @selector(setGuestAgentPort:); }' >>"$tmp/unlisted-selector.m"
	expect fail "an unlisted selector" --source "$tmp/unlisted-selector.m"

	cp "$SOURCE" "$tmp/unlisted-class.m"
	echo 'static Class extra(void) { return NSClassFromString(@"VZMacGuestAgentOptions"); }' >>"$tmp/unlisted-class.m"
	expect fail "an unlisted class" --source "$tmp/unlisted-class.m"

	grep -vF '@selector(setFullName:)' "$SOURCE" >"$tmp/stale-list.m"
	expect fail "a listed selector the source dropped" --source "$tmp/stale-list.m"

	cp "$SOURCE" "$tmp/opaque.m"
	echo 'static SEL extra(void) { return NSSelectorFromString(@"setUsername:"); }' >>"$tmp/opaque.m"
	expect fail "an opaque lookup form" --source "$tmp/opaque.m"

	mkdir "$tmp/h-good" "$tmp/h-readonly" "$tmp/h-renamed"
	cat >"$tmp/h-good/VZMacGuestProvisioningOptions.h" <<'H'
VZ_EXPORT API_AVAILABLE(macos(27.0))
@interface VZMacGuestProvisioningOptions : VZGuestProvisioningOptions <NSCopying>
@property (copy) NSString *username;
@property (copy) NSString *password;
@property (copy) NSString *fullName;
@property BOOL logsInAutomatically;
@property BOOL enablesRemoteLogin API_AVAILABLE(macos(27.0));
@end
H
	cat >"$tmp/h-good/VZMacOSVirtualMachineStartOptions.h" <<'H'
@interface VZMacOSVirtualMachineStartOptions : VZVirtualMachineStartOptions
@property (readonly, nullable) VZMacGuestProvisioningOptions *guestProvisioningOptions;
- (BOOL)setGuestProvisioningOptions:(nullable VZMacGuestProvisioningOptions *)options
                              error:(NSError **)error API_AVAILABLE(macos(27.0));
@end
H
	expect pass "headers that declare every name" --source "$SOURCE" --headers "$tmp/h-good"

	cp "$tmp/h-good/VZMacOSVirtualMachineStartOptions.h" "$tmp/h-readonly/"
	sed 's/@property (copy) NSString \*password/@property (readonly, copy) NSString *password/' \
		"$tmp/h-good/VZMacGuestProvisioningOptions.h" >"$tmp/h-readonly/VZMacGuestProvisioningOptions.h"
	expect fail "headers with a readonly property" --source "$SOURCE" --headers "$tmp/h-readonly"

	cp "$tmp/h-good/VZMacGuestProvisioningOptions.h" "$tmp/h-renamed/"
	sed 's/setGuestProvisioningOptions:/setGuestProvisioning:/' \
		"$tmp/h-good/VZMacOSVirtualMachineStartOptions.h" >"$tmp/h-renamed/VZMacOSVirtualMachineStartOptions.h"
	expect fail "headers with a renamed method" --source "$SOURCE" --headers "$tmp/h-renamed"

	if [ "$fails" = 0 ]; then
		echo "check-macvm-selectors.sh --selftest: every planted violation is refused, every clean input passes"
	fi
	return "$fails"
}

case "$MODE" in
selftest)
	selftest
	exit
	;;
esac

list_half "$SOURCE"
echo "check-macvm-selectors.sh: list half ok (${#LISTED[@]} names in $SOURCE)"

if [ -z "$HEADERS" ]; then
	HEADERS="$(sdk_headers)"
fi
if [ -z "$HEADERS" ]; then
	if [ "$MODE" = sdk ]; then
		echo "check-macvm-selectors.sh: --sdk needs macOS with a macOS $MIN_SDK_MAJOR or later SDK" >&2
		exit 1
	fi
	echo "check-macvm-selectors.sh: header half skipped (no macOS $MIN_SDK_MAJOR SDK here)"
	exit 0
fi
header_half "$HEADERS"
echo "check-macvm-selectors.sh: header half ok against $HEADERS"
