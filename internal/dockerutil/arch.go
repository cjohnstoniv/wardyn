// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package dockerutil

import "strings"

// NormalizeArch maps the architecture a Docker engine reports (system.Info.Architecture is the kernel's
// uname -m: "x86_64", "aarch64") onto the two Wardyn knows, "amd64" and "arm64"; the container
// platform spellings pass through. Anything else, including "", is "": unknown, never guessed.
func NormalizeArch(reported string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}
