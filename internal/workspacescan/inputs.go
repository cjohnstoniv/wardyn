// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GeneratedInputs describes the inputs of the generated build for p under pins: the
// base, the features, the baked installers, and the hash of every file
// GenerateDevcontainer(p, pins) returns. It calls GenerateDevcontainer, so the record
// and the files cannot drift. A digest, version or checksum is filled only from a
// valid pin; an unpinned input has it empty.
func GeneratedInputs(p WorkspaceProfile, pins Pins) (types.ImageBuildInputs, error) {
	files, err := GenerateDevcontainer(p, pins)
	if err != nil {
		return types.ImageBuildInputs{}, err
	}
	in := types.ImageBuildInputs{
		Base:      types.ImageBuildRef{Ref: genBaseImage, Digest: pins.base()},
		Generator: &types.ImageBuildGenerator{Salt: cacheKeySalt, FilesSHA256: make(map[string]string, len(files))},
	}
	for ref, opts := range featuresFor(p.Languages) {
		in.Features = append(in.Features, types.ImageBuildFeature{Ref: ref, Digest: pins.feature(ref), Options: opts})
	}
	slices.SortFunc(in.Features, func(a, b types.ImageBuildFeature) int { return strings.Compare(a.Ref, b.Ref) })
	for _, t := range genStandardTools {
		if _, baked := genAgentToolInstalls[t]; baked {
			pin, _ := pins.installer(t)
			in.Installers = append(in.Installers, types.ImageBuildInstaller{Tool: t, Channel: "stable", Version: pin.Version, Platform: pin.Platform, SHA256: pin.SHA256})
		}
	}
	for path, content := range files {
		sum := sha256.Sum256([]byte(content))
		in.Generator.FilesSHA256[path] = hex.EncodeToString(sum[:])
	}
	return in, nil
}
