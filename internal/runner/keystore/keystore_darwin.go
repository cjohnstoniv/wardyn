// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"encoding/base64"
	"fmt"
)

// ready: the login Keychain needs no discovery; `security` is the one tool the
// runner runs (owner ruling), and only through go-keyring.
func ready() error { return nil }

// macCommandLimit is the longest command go-keyring v0.2.8 writes to
// `security -i`. It checks that only after starting the child process, so the
// runner checks first.
const macCommandLimit = 4096

// macSetCommandLen is the length of go-keyring's add-generic-password command
// for one item: the same rule as go-keyring's command, pinned by
// TestSet_OversizeRefusedBeforeStart. The service (wardyn-runnerd:<id>), the
// account (<run>/<grant>) and the prefixed base64 value carry only characters
// its shell quoting leaves bare, so the length is the plain sum.
func macSetCommandLen(service, account, value string) int {
	return len("add-generic-password -U -s  -a  -w \n") + len(service) + len(account) +
		len("go-keyring-base64:") + base64.StdEncoding.EncodedLen(len(value))
}

// precheck refuses a value whose keychain command would pass macCommandLimit
// (a value over about 3 KB).
func precheck(service, account, value string) error {
	if n := macSetCommandLen(service, account, value); n > macCommandLimit {
		return fmt.Errorf("%w: the value is too long for the macOS keychain (a %d-byte command, the limit is %d)", ErrUnavailable, n, macCommandLimit)
	}
	return nil
}
