// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// update_needed has two causes, and each has its own remedy: a base of another format is prepared again
// (failure "format"), and a base whose tools are behind waits for its tools update (no failure).
func TestMacOSVMUpdateNeededCarriesItsCause(t *testing.T) {
	if runnerwire.MacVMFailureFormat != "format" {
		t.Fatalf("MacVMFailureFormat = %q", runnerwire.MacVMFailureFormat)
	}
	m := newMacLedgerRig(t).open(t)
	base := m.layout.BaseDir(0)
	write := func(format int, state string) {
		t.Helper()
		man := `{"format":` + strconv.Itoa(format) + `,"state":"` + state + `","guest_os":"macOS 26.1","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
		if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(man), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(macvm.BaseFormat-1, macvm.BaseReady)
	if _, b := m.base(); b.State != runnerwire.MacVMBaseUpdateNeeded || b.Failure != runnerwire.MacVMFailureFormat {
		t.Fatalf("a base of another format: %+v, want update_needed with failure %q", b, runnerwire.MacVMFailureFormat)
	}
	write(macvm.BaseFormat, macvm.BaseUpdateNeeded)
	if _, b := m.base(); b.State != runnerwire.MacVMBaseUpdateNeeded || b.Failure != "" {
		t.Fatalf("a base waiting on its tools update: %+v, want update_needed with no failure", b)
	}
}

// The base's pairing key never leaves the base's directory: no caps or status the runner sends or prints
// holds it.
func TestMacOSVMCapsAndStatusHoldNoPairKey(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	base := m.layout.BaseDir(0)
	key := []byte("wardyn-test-pair-key-0123456789!")
	if err := os.WriteFile(filepath.Join(base, macvm.PairKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	caps := m.build(nil)
	cj, _ := json.Marshal(caps)
	st, err := m.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sj, _ := json.Marshal(st)
	text := string(cj) + string(sj)
	for _, f := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)} {
		if strings.Contains(text, f) {
			t.Fatalf("the pairing key is in the caps or status:\n%s", text)
		}
	}
}
