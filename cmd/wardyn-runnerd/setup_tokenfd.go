// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strconv"
	"strings"
)

// tokenFD is N when path names an inherited descriptor, /dev/fd/N: how `wardyn runner setup` hands
// setup a registration token over a pipe, so it is never on disk.
func tokenFD(path string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(path, "/dev/fd/"))
	return n, err == nil && strings.HasPrefix(path, "/dev/fd/") && n > 2
}

// readTokenFD reads the token from an inherited descriptor and closes it, before preflight runs a
// single child (systemctl, launchctl, docker), so no child is ever handed it. ok is false for any
// other path, which register reads as before.
func readTokenFD(path string) (token string, ok bool, err error) {
	n, ok := tokenFD(path)
	if !ok {
		return "", false, nil
	}
	token, err = readRegistrationToken(path)
	closeFD(n)
	return token, true, err
}
