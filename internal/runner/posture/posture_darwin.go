// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"os"
	"regexp"
)

// mdmFile is where a script or package the device's MDM runs writes the posture it attests; the profile's
// MDMManaged and DiskEncrypted keys (fromProfile) are the other way, and the file outranks them.
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
// does not reach; without the MDM file or the profile's DiskEncrypted key it reads false.
func diskEncrypted() bool { return false }
