// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "regexp"

// safeImageRef is the character set an image reference may use where it reaches a runner's unit file,
// plist or env file, so a value from configuration or a register answer cannot inject a setting.
var safeImageRef = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=-]+$`)

// SafeImageRef reports whether s is a non-empty image reference of at most 512 bytes in that set.
func SafeImageRef(s string) bool { return len(s) <= 512 && safeImageRef.MatchString(s) }
