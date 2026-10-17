// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package dockerutil holds tiny helpers shared by the Docker-backed packages
// (internal/runner/docker, internal/envbuild) that each define their own
// narrow slice of the Docker client interface.
package dockerutil

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/moby/moby/client"
)

// ImagePuller is the narrow Docker client surface PullImage needs, declaring
// the exact client.ImagePullResponse return type *client.Client needs to
// satisfy it; PullImage only uses its io.ReadCloser aspect.
type ImagePuller interface {
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// PullImage pulls ref and drains the stream, returning the first error the
// daemon reports in it (a pull that fails after its first progress message
// still answers 200). errPrefix namespaces the wrapped errors per caller
// package (e.g. "docker" or "envbuild"), matching each package's existing
// error-message convention.
func PullImage(ctx context.Context, cli ImagePuller, ref, errPrefix string) error {
	rc, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("%s: pull %s: %w", errPrefix, ref, err)
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: drain pull %s: %w", errPrefix, ref, err)
		}
		if reported := cmp.Or(msg.ErrorDetail.Message, msg.Error); reported != "" {
			return fmt.Errorf("%s: pull %s: %s", errPrefix, ref, reported)
		}
	}
}
