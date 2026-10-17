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

type fakePullResponse struct{ io.ReadCloser }

func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}
func (fakePullResponse) Wait(context.Context) error { return nil }

type fakePuller struct{ stream string }

func (f fakePuller) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	return fakePullResponse{io.NopCloser(strings.NewReader(f.stream))}, nil
}

// A pull that fails after its first progress message still answers 200; the error rides the stream.
func TestPullImage_StreamError(t *testing.T) {
	ok := `{"status":"Pulling"}` + "\n" + `{"status":"Download complete"}` + "\n"
	if err := PullImage(context.Background(), fakePuller{ok}, "img:1", "docker"); err != nil {
		t.Fatalf("clean stream: %v", err)
	}
	for name, failed := range map[string]string{
		"error":       `{"error":"denied: requested access is denied"}`,
		"errorDetail": `{"errorDetail":{"message":"denied: requested access is denied"},"error":"x"}`,
	} {
		err := PullImage(context.Background(), fakePuller{`{"status":"Pulling"}` + "\n" + failed + "\n"}, "img:1", "docker")
		if err == nil || !strings.Contains(err.Error(), "denied: requested access is denied") {
			t.Errorf("%s: err = %v, want the stream's error", name, err)
		}
	}
}
