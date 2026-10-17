// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package dockerutil

import (
	"context"
	"io"
	"iter"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

// pullStream is an ImagePullResponse over a canned JSON progress stream whose Wait reports an in-stream
// error the way the real client does.
type pullStream struct {
	io.ReadCloser
	failure string
}

func (pullStream) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] { return nil }
func (p pullStream) Wait(context.Context) error {
	if p.failure != "" {
		return &streamError{p.failure}
	}
	return nil
}

type streamError struct{ msg string }

func (e *streamError) Error() string { return e.msg }

type fakePuller struct{ failure string }

func (f fakePuller) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	return pullStream{io.NopCloser(strings.NewReader(`{"status":"pulling"}`)), f.failure}, nil
}

// A pull whose stream ends in an error is a failed pull: the call returned nil and the stream drained
// before, so a registry refusal inside the stream looked like success.
func TestPullImageReportsAnInStreamError(t *testing.T) {
	if err := PullImage(t.Context(), fakePuller{}, "img:1", "docker"); err != nil {
		t.Fatalf("a clean pull failed: %v", err)
	}
	err := PullImage(t.Context(), fakePuller{failure: "denied: requested access to the resource is denied"}, "img:1", "docker")
	if err == nil || !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "img:1") {
		t.Fatalf("PullImage = %v, want the in-stream refusal naming the image", err)
	}
}
