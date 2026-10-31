// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macosguest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	macosguest "github.com/cjohnstoniv/wardyn/deploy/macos-guest"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vmnet/prepproxy"
)

func TestEmbeddedBytesEqualTheFiles(t *testing.T) {
	for name, got := range map[string][]byte{"tooling.json": macosguest.ToolingJSON, "prep-allowlist.json": macosguest.PrepAllowlistJSON, "boot-hosts.json": macosguest.BootHostsJSON} {
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
// and it boots nothing when they are bad (TestStage2BootsNothingOnBadShippedTooling).
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

func TestStage2BootsNothingOnBadShippedTooling(t *testing.T) {
	saved := macosguest.ToolingJSON
	t.Cleanup(func() { macosguest.ToolingJSON = saved })
	macosguest.ToolingJSON = []byte("{")
	boots := 0
	st := &macvm.ToolsStage{Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		boots++
		return nil, nil, errors.New("stop here")
	}}
	if _, err := st.Run(context.Background(), macvm.Stage2Input{}); boots != 0 || err == nil {
		t.Fatalf("boots = %d, err = %v: stage 2 booted on bad shipped tooling", boots, err)
	}
}

// The boot-host list is a short list of exact background hosts. Developer-facing Apple hosts (sign-in, the
// App Store, developer downloads) and certificate checks are never on it: they keep first-use approval.
func TestBootHostsAreOnlyBackgroundHosts(t *testing.T) {
	var doc struct {
		Version int      `json:"version"`
		Hosts   []string `json:"hosts"`
	}
	if err := json.Unmarshal(macosguest.BootHostsJSON, &doc); err != nil || doc.Version != 1 {
		t.Fatalf("boot-hosts.json = %+v, %v", doc, err)
	}
	want := []string{"gdmf.apple.com", "mesu.apple.com", "gateway.icloud.com", "init-kt.apple.com"}
	if !slices.Equal(doc.Hosts, want) {
		t.Fatalf("hosts = %q, want the starting list %q", doc.Hosts, want)
	}
	for _, h := range doc.Hosts {
		if strings.ContainsAny(h, "*:/ ") || h != strings.ToLower(h) {
			t.Errorf("%q is not a lower-case exact host", h)
		}
		for _, never := range []string{"gsa.", "idmsa.", "buy.itunes.", "bag.itunes.", "appleid.", "developer.apple.com", "ocsp"} {
			if strings.Contains(h, never) {
				t.Errorf("%q is a developer-facing or certificate-check host, which keeps first-use approval", h)
			}
		}
	}
}
