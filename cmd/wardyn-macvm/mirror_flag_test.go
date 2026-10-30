// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// --ipsw-url needs its hash, is one image with --ipsw not two, and must be an https URL: usage errors that
// stop before anything is written.
func TestPrepareIPSWURLUsageErrors(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	const mirror = "https://mirror.example/UniversalMac_27.0_26A1_Restore.ipsw"
	for _, tc := range []struct {
		extra []string
		want  string
	}{
		{[]string{"--ipsw-url", mirror}, macvm.ErrIPSWURLNeedsSHA256.Error()},
		{[]string{"--ipsw-url", mirror, "--ipsw", "/x/a.ipsw", "--sha256", hash}, macvm.ErrIPSWAndURL.Error()},
		{[]string{"--ipsw-url", "http://mirror.example/a.ipsw", "--sha256", hash}, "is not an https URL"},
	} {
		root := t.TempDir()
		d, _, errb := testDeps(&macvmtest.Engine{})
		args := append([]string{"prepare", "--root", root, "--cpus", "4", "--memory-mib", "8192"}, tc.extra...)
		if code := run(context.Background(), args, d); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%v: exit %d: %s", tc.extra, code, errb.String())
		}
		if des, _ := os.ReadDir(root); len(des) != 0 {
			t.Errorf("%v wrote %d entries under root", tc.extra, len(des))
		}
	}
}
