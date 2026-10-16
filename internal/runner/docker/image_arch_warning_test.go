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
		"docker buildx build --platform linux/amd64,linux/arm64", "<key>@arm64", "used by runs on a registered arm64 runner", "docker buildx imagetools inspect " + ref} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning lacks %q: %s", want, buf.String())
		}
	}
}

func TestEmulationWarningNamesTheProxyImageSetting(t *testing.T) {
	const ref = "mirror.example/proxy:1"
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	d, f := archDriver("aarch64", map[string]string{ref: "amd64"})
	f.archProbeExit, f.archProbeLog = 0, ""
	d.cfg.ProxyImage = ref
	if err := d.ensureImage(context.Background(), ref, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WARDYN_PROXY_IMAGE_ARM64", "WARDYN_PROXY_IMAGE or"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("proxy warning lacks %q: %s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "WARDYN_AGENT_IMAGES") {
		t.Errorf("proxy warning names WARDYN_AGENT_IMAGES: %s", buf.String())
	}
}
