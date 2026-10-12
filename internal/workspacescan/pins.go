// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"regexp"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Pins is what a generated build is pinned to: a digest for the base, a digest per
// feature ref, and a version and checksum per installer tool. The zero Pins pins
// nothing. Pins come from a registry answer or from a stored record, and reach a FROM
// line, a devcontainer feature key and a shell, so every use goes through the
// accessors below, which ignore (leave unpinned) any value of the wrong shape.
type Pins struct {
	BaseDigest string
	Features   map[string]string // feature ref -> digest
	Installers map[string]InstallerPin
}

// InstallerPin is the version, platform and checksum an installer fetched.
type InstallerPin struct {
	Version, Platform, SHA256 string
}

var (
	pinDigestRe  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	pinVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	pinSumRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validDigest(s string) bool { return pinDigestRe.MatchString(s) }

func (p InstallerPin) valid() bool {
	return pinVersionRe.MatchString(p.Version) && (p.Platform == "linux-x64" || p.Platform == "linux-arm64") && pinSumRe.MatchString(p.SHA256)
}

func (p Pins) base() string {
	if validDigest(p.BaseDigest) {
		return p.BaseDigest
	}
	return ""
}

func (p Pins) feature(ref string) string {
	if d := p.Features[ref]; validDigest(d) {
		return d
	}
	return ""
}

func (p Pins) installer(tool string) (InstallerPin, bool) {
	if i, ok := p.Installers[tool]; ok && i.valid() {
		return i, true
	}
	return InstallerPin{}, false
}

// ParseInstallerPin parses the pin file a generated build writes, "<ver> <plat> <sum>\n",
// by the same rules as a pin. ok=false for anything else.
func ParseInstallerPin(b []byte) (InstallerPin, bool) {
	f := strings.Split(strings.TrimSuffix(string(b), "\n"), " ")
	if len(f) != 3 {
		return InstallerPin{}, false
	}
	pin := InstallerPin{Version: f[0], Platform: f[1], SHA256: f[2]}
	return pin, pin.valid()
}

// PinsFromInputs reads the pins out of a stored record's inputs, keeping only the
// values that are digests, versions and checksums.
func PinsFromInputs(in types.ImageBuildInputs) Pins {
	p := Pins{Features: map[string]string{}, Installers: map[string]InstallerPin{}}
	if validDigest(in.Base.Digest) {
		p.BaseDigest = in.Base.Digest
	}
	for _, f := range in.Features {
		if validDigest(f.Digest) {
			p.Features[f.Ref] = f.Digest
		}
	}
	for _, i := range in.Installers {
		if pin := (InstallerPin{i.Version, i.Platform, i.SHA256}); pin.valid() {
			p.Installers[i.Tool] = pin
		}
	}
	return p
}
