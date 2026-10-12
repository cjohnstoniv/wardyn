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

// GeneratedInputs describes the unpinned inputs of the generated build for p:
// the base, the features, the baked installers, and the hash of every file
// GenerateDevcontainer returns. It calls GenerateDevcontainer, so the record
// and the files cannot drift. Digests stay empty until a resolver fills them.
func GeneratedInputs(p WorkspaceProfile) (types.ImageBuildInputs, error) {
	files, err := GenerateDevcontainer(p)
	if err != nil {
		return types.ImageBuildInputs{}, err
	}
	in := types.ImageBuildInputs{
		Base:      types.ImageBuildRef{Ref: genBaseImage},
		Generator: &types.ImageBuildGenerator{Salt: cacheKeySalt, FilesSHA256: make(map[string]string, len(files))},
	}
	for ref, opts := range featuresFor(p.Languages) {
		in.Features = append(in.Features, types.ImageBuildFeature{Ref: ref, Options: opts})
	}
	slices.SortFunc(in.Features, func(a, b types.ImageBuildFeature) int { return strings.Compare(a.Ref, b.Ref) })
	for _, t := range genStandardTools {
		if _, baked := genAgentToolInstalls[t]; baked {
			in.Installers = append(in.Installers, types.ImageBuildInstaller{Tool: t, Channel: "stable"})
		}
	}
	for path, content := range files {
		sum := sha256.Sum256([]byte(content))
		in.Generator.FilesSHA256[path] = hex.EncodeToString(sum[:])
	}
	return in, nil
}
