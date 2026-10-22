// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// pinnedRunnerdSHA256 is the SHA-256 (lower-case hex) of the wardyn-runnerd
// built for this CLI's own platform. The release job builds the runner first
// and stamps it with
//
//	-X main.pinnedRunnerdSHA256=<sha256 of wardyn-runnerd-<os>-<arch>>
//
// so `wardyn runner setup` can check the runner binary it downloads against a
// value that travelled with the CLI, not with the download. Empty on a
// development build and on a platform with no published runner (Windows): an
// unstamped CLI pins nothing, and `wardyn version --json` reports it empty.
var pinnedRunnerdSHA256 string
