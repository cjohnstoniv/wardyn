// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

// The reclaim side of a reusable build (reuse.go): the server lists the tags reusable builds left on the daemon and
// removes the ones no run has used for a while.

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// reuseTagPattern is a reusable build's tag: byoiReuseRepo and its 64-hex key.
var reuseTagPattern = regexp.MustCompile("^" + regexp.QuoteMeta(byoiReuseRepo) + ":([0-9a-f]{64})$")

// ReuseImage is one reusable build's tag on the daemon and when its image was built.
type ReuseImage struct {
	Ref     string
	Created time.Time
}

// ListReuseImages lists the tags reusable builds left on the daemon: each tag of the form byoiReuseRepo:<key> whose
// image carries reuseKeyLabel equal to that key. Any other tag, on the same image or not, is left out.
func (b *Builder) ListReuseImages(ctx context.Context) ([]ReuseImage, error) {
	res, err := b.cli.ImageList(ctx, client.ImageListOptions{
		Filters: client.Filters{}.Add("reference", byoiReuseRepo).Add("label", reuseKeyLabel),
	})
	if err != nil {
		return nil, fmt.Errorf("envbuild: list reusable images: %w", err)
	}
	var out []ReuseImage
	for _, im := range res.Items {
		for _, tag := range im.RepoTags {
			if m := reuseTagPattern.FindStringSubmatch(tag); m != nil && im.Labels[reuseKeyLabel] == m[1] {
				out = append(out, ReuseImage{Ref: tag, Created: time.Unix(im.Created, 0)})
			}
		}
	}
	return out, nil
}

// RemoveReuseImage removes one reusable build's tag, never forced, so the daemon keeps an image a container still
// uses; its untagged parents go with it unless another image or a container uses them. A tag already gone is not an
// error; a ref of any other form is refused before the daemon is asked.
func (b *Builder) RemoveReuseImage(ctx context.Context, ref string) error {
	if !reuseTagPattern.MatchString(ref) {
		return fmt.Errorf("envbuild: %q is not a reusable build's tag", ref)
	}
	if _, err := b.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("envbuild: remove reusable image %q: %w", ref, err)
	}
	return nil
}
