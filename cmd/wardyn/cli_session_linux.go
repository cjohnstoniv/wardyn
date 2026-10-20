// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/godbus/dbus/v5"
)

// keychainReachable finds the session bus the way internal/runner/keystore does, without godbus's last
// resort of running dbus-launch from PATH: with no DBUS_SESSION_BUS_ADDRESS and no user bus socket there
// is no keychain, and the sign-in goes to the file. It pins the address it found so godbus looks no further.
func keychainReachable() error {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" && a != "autolaunch:" {
		return nil
	}
	bus := filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "bus")
	if _, err := os.Stat(bus); err != nil {
		return errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset and " + bus + " is absent)")
	}
	return os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+dbus.EscapeBusAddressValue(bus))
}
