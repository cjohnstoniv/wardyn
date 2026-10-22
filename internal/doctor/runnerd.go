// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import "path/filepath"

// RunnerdPath is where `wardyn-runnerd setup` installs the runner binary for this person, "" on another OS.
// `wardyn doctor` looks there to tell whether a runner is installed on this computer.
func RunnerdPath(goos, home, xdgData string) string {
	switch goos {
	case "linux":
		if xdgData == "" {
			xdgData = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(xdgData, "wardyn", "bin", "wardyn-runnerd")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "wardyn", "bin", "wardyn-runnerd")
	}
	return ""
}
