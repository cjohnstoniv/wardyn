// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/godbus/dbus/v5"
)

// runtimeDir is the user's runtime directory, where a session bus socket lives.
var runtimeDir = func() string { return "/run/user/" + strconv.Itoa(os.Getuid()) }

// ready finds the session bus the way godbus does, without its last resort:
// with no DBUS_SESSION_BUS_ADDRESS and no user bus socket, godbus runs
// dbus-launch from PATH. The runner runs no tool, so it refuses instead, and
// pins the address it found so godbus never looks further.
func ready() error {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" && a != "autolaunch:" {
		return nil
	}
	bus := filepath.Join(runtimeDir(), "bus")
	if _, err := os.Stat(bus); err != nil {
		return fmt.Errorf("%w: no session bus (DBUS_SESSION_BUS_ADDRESS is unset and %s is absent)", ErrUnavailable, bus)
	}
	return os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+dbus.EscapeBusAddressValue(bus))
}
