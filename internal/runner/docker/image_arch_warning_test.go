// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/image"
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

// emulationWarning runs ensureImage on an emulated amd64 image with meta overlaid on its inspect
// result and returns the log text.
func emulationWarning(t *testing.T, ref string, meta image.InspectResponse) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	d, f := archDriver("aarch64", map[string]string{ref: "amd64"})
	f.archProbeExit, f.archProbeLog = 0, ""
	f.imageMeta[ref] = meta
	if err := d.ensureImage(context.Background(), ref, "", nil); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// withDescriptor sets meta.Descriptor to a descriptor of the given media type and digest. The
// descriptor type is an indirect dependency, so it is built without naming it.
func withDescriptor(meta *image.InspectResponse, mediaType, dgst string) {
	meta.Descriptor = allocOf(meta.Descriptor)
	meta.Descriptor.MediaType = mediaType
	reflect.ValueOf(&meta.Descriptor.Digest).Elem().SetString(dgst)
}

func allocOf[T any](*T) *T { return new(T) }

func TestEmulationWarningDiagnosesTheSelectedImage(t *testing.T) {
	const (
		ref     = "mirror.example/agent:1"
		dgst    = "sha256:" + "aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11"
		other   = "sha256:" + "bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22"
		index   = "application/vnd.oci.image.index.v1+json"
		single  = "application/vnd.oci.image.manifest.v1+json"
		pinned  = "mirror.example/agent@" + dgst
		notThis = "other.example/agent@" + other
	)
	tests := []struct {
		name    string
		meta    func() image.InspectResponse
		want    []string
		notWant []string
	}{
		{"single-arch manifest", func() image.InspectResponse {
			m := image.InspectResponse{RepoDigests: []string{notThis, pinned}, Os: "linux", Variant: "v2"}
			withDescriptor(&m, single, dgst)
			return m
		}, []string{"Selected image: " + pinned + ", a single-platform manifest", "platform linux/amd64/v2"}, []string{other, "cannot tell"}},
		{"index", func() image.InspectResponse {
			m := image.InspectResponse{RepoDigests: []string{pinned}, Os: "linux"}
			withDescriptor(&m, index, dgst)
			return m
		}, []string{pinned + ", a multi-arch index", "platform linux/amd64"}, []string{"cannot tell"}},
		{"classic store cannot tell", func() image.InspectResponse {
			return image.InspectResponse{RepoDigests: []string{pinned}, Os: "linux"}
		}, []string{pinned + "; platform linux/amd64; cannot tell from the local store whether " + pinned + " is a multi-arch index; check with docker buildx imagetools inspect " + ref}, nil},
		{"descriptor for another digest is not trusted", func() image.InspectResponse {
			m := image.InspectResponse{RepoDigests: []string{pinned}, Os: "linux"}
			withDescriptor(&m, index, other)
			return m
		}, []string{"cannot tell from the local store"}, []string{"a multi-arch index,"}},
		{"locally built", func() image.InspectResponse {
			return image.InspectResponse{Os: "linux", RepoDigests: []string{notThis}}
		}, []string{"no registry digest (built or loaded locally); platform linux/amd64"}, []string{other, "cannot tell"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emulationWarning(t, ref, tc.meta())
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("warning lacks %q: %s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("warning has %q: %s", w, got)
				}
			}
		})
	}
}
