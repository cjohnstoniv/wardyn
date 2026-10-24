// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macosguest_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"

	macosguest "github.com/cjohnstoniv/wardyn/deploy/macos-guest"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vmnet/prepproxy"
)

func TestEmbeddedBytesEqualTheFiles(t *testing.T) {
	for name, got := range map[string][]byte{"tooling.json": macosguest.ToolingJSON, "prep-allowlist.json": macosguest.PrepAllowlistJSON} {
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || !bytes.Equal(got, want) {
			t.Errorf("%s: the embedded bytes differ from the file", name)
		}
	}
}

func TestShippedToolingParses(t *testing.T) {
	tl, err := macvm.ParseTooling(macosguest.ToolingJSON)
	if err != nil || len(tl.Harnesses) == 0 {
		t.Fatalf("ParseTooling(embedded) = %+v, %v", tl, err)
	}
	if s, err := macvm.ShippedTooling(); err != nil || len(s.Harnesses) != len(tl.Harnesses) {
		t.Fatalf("ShippedTooling = %+v, %v", s, err)
	}
}

func TestPrepProxyConfigAcceptsShippedAllowlist(t *testing.T) {
	if _, err := prepproxy.Config(macosguest.PrepAllowlistJSON, 3128, []netip.Prefix{netip.MustParsePrefix("172.20.0.0/24")}); err != nil {
		t.Fatal(err)
	}
}

// Stage 2 starts by checking the shipped pins: with no Tooling of its own it runs them,
// and it boots nothing when they are bad (the bad case is the revert proof's overlay).
func TestStage2StartsOnShippedTooling(t *testing.T) {
	boots := 0
	st := &macvm.ToolsStage{Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		boots++
		return nil, nil, errors.New("stop here")
	}}
	_, err := st.Run(context.Background(), macvm.Stage2Input{})
	if boots != 1 || err == nil {
		t.Fatalf("boots = %d, err = %v: stage 2 did not reach its boot on the shipped tooling", boots, err)
	}
}
