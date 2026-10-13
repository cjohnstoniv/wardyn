// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"archive/tar"
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	// pinFilePath is where the generated claude-code RUN records what it installed
	// (workspacescan.claudePinFile).
	pinFilePath = "/usr/local/share/wardyn/inputs/claude-code"
	// maxPinFileBytes bounds the read: the file is "<version> <platform> <sha256>\n".
	maxPinFileBytes = 4096
	pinReadTimeout  = 30 * time.Second
)

// readPinFile copies pinFilePath out of imageRef through a container that is never started, with
// networking off. A missing file, one over maxPinFileBytes, anything that is not a regular file and
// any error all return nil: the build succeeded and the installer simply goes unrecorded.
// The value is what the build reported, not an attestation: a later feature script could rewrite it.
func (b *Builder) readPinFile(ctx context.Context, imageRef string) []byte {
	ctx, cancel := context.WithTimeout(ctx, pinReadTimeout)
	defer cancel()
	created, err := b.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:           imageRef,
			Cmd:             []string{"/wardyn-pin-read"}, // never started; the image's own command must not matter
			NetworkDisabled: true,
			Labels:          map[string]string{envbuildContainerLabel: imageRef}, // reaper.go's orphan scan
		},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		slog.Debug("envbuild: pin read: create failed", slog.String("error", err.Error()))
		return nil
	}
	defer b.liveBuilds.track(created.ID)()
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), pinReadTimeout)
		defer rmCancel()
		_, _ = b.cli.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	res, err := b.cli.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: pinFilePath})
	if err != nil {
		return nil // an image without the file answers not-found
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	hdr, err := tr.Next()
	if err != nil || hdr.Typeflag != tar.TypeReg {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(tr, maxPinFileBytes+1))
	if err != nil || len(data) > maxPinFileBytes {
		return nil
	}
	return data
}
