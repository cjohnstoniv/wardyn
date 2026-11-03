// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package main

import (
	"os/exec"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
)

// The Docker driver is the macOS VM handle's kept machines driver, and the updater the driver calls before a
// kept machine resumes.
var (
	_ keptDriver              = (*docker.Driver)(nil)
	_ docker.KeptToolsUpdater = (*keptToolsUpdater)(nil)
)

// macHostOS is this Mac's build in the form a VM machine records, e.g. "macOS 27.0.1 (26A434)".
func macHostOS() string {
	v, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	b, _ := exec.Command("/usr/bin/sw_vers", "-buildVersion").Output()
	out := "macOS " + strings.TrimSpace(string(v))
	if s := strings.TrimSpace(string(b)); s != "" {
		out += " (" + s + ")"
	}
	return out
}
