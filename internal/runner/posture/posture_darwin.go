// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"os"
	"regexp"
)

// mdmFile is where a device-management profile writes the posture it attests.
const mdmFile = "/Library/Application Support/Wardyn/posture.json"

var productVersion = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([0-9.]+)</string>`)

func osVersion() string {
	b, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return ""
	}
	if m := productVersion.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// ponytail: FileVault state needs fdesetup or IOKit, which a static binary
// does not reach; without the MDM file it reads false.
func diskEncrypted() bool { return false }
