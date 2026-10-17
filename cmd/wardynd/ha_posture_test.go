// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestValidateHAPosture pins the boot half of high availability: the removed
// flag is refused with a pointer, and WARDYN_HA boots only on the Kubernetes
// runner with a recording store every replica reads.
func TestValidateHAPosture(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ha, oldFlag   bool
		runner, store string
		wantErr       string // substring; empty = must be accepted
	}{
		{name: "no HA on docker boots as before", runner: "docker", store: "fs"},
		{name: "HA on k8s with the pg store", ha: true, runner: "k8s", store: "pg"},
		{name: "HA on k8s with recording off", ha: true, runner: "k8s", store: "off"},
		{name: "HA on the docker runner is refused", ha: true, runner: "docker", store: "pg", wantErr: "Kubernetes runner only"},
		{name: "HA with no runner is refused", ha: true, runner: "none", store: "pg", wantErr: "Kubernetes runner only"},
		{name: "HA with the fs store is refused", ha: true, runner: "k8s", store: "fs", wantErr: "WARDYN_RECORDING_STORE"},
		{name: "the removed flag is refused with a pointer", oldFlag: true, runner: "k8s", store: "pg", wantErr: "WARDYN_HA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &bootFlags{ha: &tc.ha, allowMultiInstance: &tc.oldFlag, runnerSel: &tc.runner, recordingSel: &tc.store}
			err := validateHAPosture(f)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}
}
