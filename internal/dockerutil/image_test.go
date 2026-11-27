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

// countingPuller counts the pulls that reach it and answers like fakePuller.
type countingPuller struct{ calls int }

func (c *countingPuller) ImagePull(ctx context.Context, ref string, o client.ImagePullOptions) (client.ImagePullResponse, error) {
	c.calls++
	return fakePuller{}.ImagePull(ctx, ref, o)
}

// A ref in a namespace Wardyn builds locally is refused before the daemon is asked, in every spelling of the default
// registry's host; a ref that merely contains the name elsewhere, or sits on another registry, still pulls.
func TestPullImageRefusesWardynLocalBuildNamespaces(t *testing.T) {
	for _, ref := range []string{
		"wardyn-byoi/reuse:abc",
		"wardyn-devcontainer/run:latest",
		"wardyn-workspace/w:built",
		"docker.io/wardyn-byoi/x:1",
		"index.docker.io/wardyn-workspace/w:1",
		"registry-1.docker.io/wardyn-devcontainer/x:1",
	} {
		p := &countingPuller{}
		err := PullImage(t.Context(), p, ref, "docker")
		if err == nil || !strings.Contains(err.Error(), ref) || p.calls != 0 {
			t.Errorf("PullImage(%q) err=%v calls=%d, want a refusal naming the ref and 0 pulls", ref, err, p.calls)
		}
	}
	for _, ref := range []string{"img:1", "ghcr.io/wardyn-byoi/x:1", "registry.example/wardyn-workspace/w:1", "wardyn-agent:dev"} {
		p := &countingPuller{}
		if err := PullImage(t.Context(), p, ref, "docker"); err != nil || p.calls != 1 {
			t.Errorf("PullImage(%q) err=%v calls=%d, want a pull", ref, err, p.calls)
		}
	}
}
