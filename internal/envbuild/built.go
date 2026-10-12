// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/client"
)

// Built is what a successful build produced. ID is the image ID the daemon's build stream
// reported ("" when the stream carried none); BaseDigest is the digest a BYOI wrap built FROM
// ("" when the base had none, and for the generated and repo lanes). PinFile is the claude-code pin
// file the generated lane reads from its image (nil for every other lane, and when it is missing or
// oversized); see readPinFile.
type Built struct {
	Ref        string
	ID         string
	BaseDigest string
	PinFile    []byte
}

// LocalImageID is the ID of the image ref names on the daemon now, with found=false when the
// daemon has no such image.
func (b *Builder) LocalImageID(ctx context.Context, ref string) (id string, found bool, err error) {
	res, err := b.cli.ImageInspect(ctx, ref)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("envbuild: inspect %q: %w", ref, err)
	}
	return res.ID, true, nil
}

// RemoteDigest is the digest the registry answers for ref (a tag or a digest form), asked through
// the daemon. For a multi-architecture image it is the index digest. An unknown manifest keeps the
// daemon's not-found error in the chain (errdefs.IsNotFound).
func (b *Builder) RemoteDigest(ctx context.Context, ref string) (string, error) {
	res, err := b.cli.DistributionInspect(ctx, ref, client.DistributionInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("envbuild: resolve %q: %w", ref, err)
	}
	return res.Descriptor.Digest.String(), nil
}

// repoDigestFor returns the first entry of repoDigests ("repo@sha256:…") whose repository is
// baseRef's, or "" when none is: a local-only image has no digest, and a ref that does not parse
// matches nothing.
func repoDigestFor(baseRef string, repoDigests []string) string {
	want, err := reference.ParseNormalizedNamed(baseRef)
	if err != nil {
		return ""
	}
	for _, entry := range repoDigests {
		got, err := reference.ParseNormalizedNamed(entry)
		if err == nil && got.Name() == want.Name() && strings.Contains(entry, "@") {
			return entry
		}
	}
	return ""
}
