// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestEmulationWarningNamesTheArchitecturesTheSourceAndTheFix(t *testing.T) {
	const ref = "mirror.example/agent:1"
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	d, f := archDriver("aarch64", map[string]string{ref: "amd64"})
	f.archProbeExit, f.archProbeLog = 0, ""
	if err := d.ensureImage(context.Background(), ref, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{ref, "amd64 image", "arm64 engine", "WARDYN_AGENT_IMAGES", "published default",
		"docker buildx build --platform linux/amd64,linux/arm64", "<key>@arm64", "docker buildx imagetools inspect " + ref} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning lacks %q: %s", want, buf.String())
		}
	}
}
