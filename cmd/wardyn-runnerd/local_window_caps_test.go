// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The caps say local_window only when the runner has a local socket (--local-attach on) and offers Linux VM
// sandboxes: a window needs both the socket and a VM display to splice it to.
func TestRunnerRuntimeAdvertisesLocalWindow(t *testing.T) {
	for _, c := range []struct {
		local bool
		vm    *runnerwire.VMCaps
		want  bool
	}{
		{true, &runnerwire.VMCaps{Desktop: true}, true},
		{true, nil, false},
		{false, &runnerwire.VMCaps{Desktop: true}, false},
		{false, nil, false},
	} {
		t.Run(fmt.Sprintf("local=%v,linux_vm=%v", c.local, c.vm != nil), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			caps := composedCaps(t, runtimeExtras{noPosture: true, arch: "amd64", localAttach: c.local, orgURL: "https://org.example", linuxVM: c.vm})
			if caps.LocalAttach != c.local || (caps.LinuxVM != nil) != (c.vm != nil) {
				t.Fatalf("caps local_attach %v, linux_vm %v: the row's runtime was not built", caps.LocalAttach, caps.LinuxVM)
			}
			if caps.LocalWindow != c.want {
				t.Fatalf("caps local_window = %v, want %v", caps.LocalWindow, c.want)
			}
		})
	}
}
