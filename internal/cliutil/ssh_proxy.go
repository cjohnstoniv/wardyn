// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import (
	"errors"
	"strings"
	"unicode"
)

// MaxSSHProxyCommandBytes bounds WARDYN_SSH_PROXY_COMMAND.
const MaxSSHProxyCommandBytes = 512

// CheckSSHProxyCommand is the one rule for an advertised SSH ProxyCommand,
// shared by wardynd's boot refusal and the CLI's refusal of a value a skewed
// daemon publishes anyway. The value is pasted inside single quotes into a
// shell one-liner and onto one ssh_config line, so a single quote, a newline
// or any other control character would break out of either.
func CheckSSHProxyCommand(v string) error {
	if len(v) > MaxSSHProxyCommandBytes {
		return errors.New("is over 512 bytes")
	}
	if strings.ContainsAny(v, "\n\r") {
		return errors.New("contains a newline")
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return errors.New("contains a control character")
	}
	if strings.ContainsRune(v, '\'') {
		return errors.New("contains a single quote")
	}
	return nil
}
