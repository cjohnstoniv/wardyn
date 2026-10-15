// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// parseBootImages reads every image setting main hands the API: WARDYN_AGENT_IMAGES (plain refs, then the
// `@arch` variants) and the per-architecture proxy images a runner receives at registration. A proxy image
// outside types.SafeImageRef is refused at boot, naming its variable, because a runner writes it into a
// unit file or env file.
func parseBootImages(f *bootFlags) (agent map[string]string, variants map[string]map[string]string, proxyByArch map[string]string, err error) {
	if agent, variants, err = parseAgentImages(*f.agentImagesJSON); err != nil {
		return nil, nil, nil, err
	}
	for _, p := range []struct{ env, arch, ref string }{
		{"WARDYN_PROXY_IMAGE_AMD64", "amd64", *f.proxyImageAMD64},
		{"WARDYN_PROXY_IMAGE_ARM64", "arm64", *f.proxyImageARM64},
	} {
		if p.ref == "" {
			continue
		}
		if !types.SafeImageRef(p.ref) {
			return nil, nil, nil, fmt.Errorf("%s %q is not an image reference: use only letters, digits and . _ : / @ %% + = - (at most 512 bytes), with no spaces or quotes", p.env, p.ref)
		}
		if proxyByArch == nil {
			proxyByArch = map[string]string{}
		}
		proxyByArch[p.arch] = p.ref
	}
	return agent, variants, proxyByArch, nil
}

// parseAgentImages decodes WARDYN_AGENT_IMAGES (a JSON object mapping agent
// name to OCI image ref) and logs what it took. Empty is not an error: it
// means "use the ghcr convention for every agent". A malformed value fails
// closed at boot rather than silently falling back to the convention.
//
// A key `K@A` (A is amd64 or arm64) names the image a runner of architecture A
// uses where K's ref would otherwise be used. Those entries are split out into
// variants (arch → plain key → ref) and never reach the plain map, so every
// reader of the plain map still sees one ref per key. A variant must have a
// non-empty ref and a plain K beside it: the arch axis only swaps the ref of a
// key that resolution already chose.
func parseAgentImages(raw string) (plain map[string]string, variants map[string]map[string]string, err error) {
	if raw == "" {
		return nil, nil, nil
	}
	var all map[string]string
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, nil, fmt.Errorf("parse WARDYN_AGENT_IMAGES: %w", err)
	}
	if all == nil {
		return nil, nil, nil
	}
	plain = map[string]string{}
	for key, ref := range all {
		k, arch, found := strings.Cut(key, "@")
		if !found {
			plain[key] = ref
			continue
		}
		if strings.Contains(arch, "@") {
			return nil, nil, fmt.Errorf("WARDYN_AGENT_IMAGES key %q has more than one @: write <key>@<arch>, with arch amd64 or arm64", key)
		}
		if arch != "amd64" && arch != "arm64" {
			return nil, nil, fmt.Errorf("WARDYN_AGENT_IMAGES key %q: the architecture after @ must be amd64 or arm64", key)
		}
		if k == "" {
			return nil, nil, fmt.Errorf("WARDYN_AGENT_IMAGES key %q names no image key before @%s", key, arch)
		}
		if ref == "" {
			return nil, nil, fmt.Errorf("WARDYN_AGENT_IMAGES key %q has an empty image ref", key)
		}
		if variants == nil {
			variants = map[string]map[string]string{}
		}
		if variants[arch] == nil {
			variants[arch] = map[string]string{}
		}
		variants[arch][k] = ref
	}
	for arch, byKey := range variants {
		for k := range byKey {
			if _, ok := plain[k]; !ok {
				return nil, nil, fmt.Errorf("WARDYN_AGENT_IMAGES key %q has no plain %q entry: the plain key serves every runner without an @%s image", k+"@"+arch, k, arch)
			}
		}
	}
	if err := api.ValidateHarnessSetImages(plain); err != nil {
		return nil, nil, err
	}
	slog.Info("wardynd: agent image overrides", slog.Any("images", plain), slog.Any("arch_variants", variants))
	return plain, variants, nil
}
