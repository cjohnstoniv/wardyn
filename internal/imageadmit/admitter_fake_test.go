// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
)

const bundleV03 = "application/vnd.dev.sigstore.bundle.v0.3+json"

// fakeRegistry serves one repository from memory and counts every read.
type fakeRegistry struct {
	mu        sync.Mutex
	tags      map[string]string                  // tag -> digest
	manifests map[string]*imageadmit.Manifest    // digest -> manifest
	referrers map[string][]imageadmit.Descriptor // artifact type -> referrers of the image
	legacy    map[string]*imageadmit.Manifest    // legacy tag suffix (".sig", ".att") -> its manifest
	blobs     map[string][]byte                  // digest -> bytes
	errs      map[string]error                   // method, or method+":"+id, -> error to return
	calls     map[string]int                     // method and method+":"+id -> count
	gate      chan struct{}                      // when set, Referrers waits for it to close
	refs      []imageadmit.Ref                   // every Ref a read named
}

func newFake() *fakeRegistry {
	return &fakeRegistry{tags: map[string]string{}, manifests: map[string]*imageadmit.Manifest{},
		referrers: map[string][]imageadmit.Descriptor{}, legacy: map[string]*imageadmit.Manifest{}, blobs: map[string][]byte{}, errs: map[string]error{},
		calls: map[string]int{}}
}

func sha(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

// image makes tag name the image at digest d.
func (f *fakeRegistry) image(tag, d string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[tag] = d
	f.manifests[d] = &imageadmit.Manifest{Digest: d, MediaType: "application/vnd.oci.image.index.v1+json"}
}

// bundle publishes b as a Sigstore bundle referrer of the image, as cosign v3 and GitHub attestations push one.
func (f *fakeRegistry) bundle(b []byte) {
	layer := imageadmit.Descriptor{MediaType: bundleV03, Digest: sha(b), Size: int64(len(b))}
	f.referrer(bundleV03, []imageadmit.Descriptor{layer}, b)
}

// referrer publishes a referrer manifest of artifact type at with layers; blob, when set, is every layer's bytes.
func (f *fakeRegistry) referrer(at string, layers []imageadmit.Descriptor, blob []byte) imageadmit.Descriptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := sha([]byte(fmt.Sprintf("referrer-%d-%s", len(f.manifests), at)))
	f.manifests[md] = &imageadmit.Manifest{Digest: md, MediaType: "application/vnd.oci.image.manifest.v1+json", ArtifactType: at, Layers: layers}
	for _, l := range layers {
		if blob != nil {
			f.blobs[l.Digest] = blob
		}
	}
	d := imageadmit.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: md, ArtifactType: at}
	f.referrers[at] = append(f.referrers[at], d)
	return d
}

func (f *fakeRegistry) count(method, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	f.calls[method+":"+id]++
	if err := f.errs[method+":"+id]; err != nil {
		return err
	}
	return f.errs[method]
}

func (f *fakeRegistry) n(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeRegistry) total() int {
	return f.n("Manifest") + f.n("Referrers") + f.n("LegacyTag") + f.n("Blob")
}

func (f *fakeRegistry) setErr(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, key)
		return
	}
	f.errs[key] = err
}

func (f *fakeRegistry) note(ref imageadmit.Ref) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
}

func (f *fakeRegistry) Manifest(_ context.Context, ref imageadmit.Ref) (*imageadmit.Manifest, error) {
	f.note(ref)
	id := ref.Digest
	if id == "" {
		id = ref.Tag
	}
	if err := f.count("Manifest", id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := ref.Digest
	if d == "" {
		if d = f.tags[ref.Tag]; d == "" {
			return nil, imageadmit.ErrNotFound
		}
	}
	m, ok := f.manifests[d]
	if !ok {
		return nil, imageadmit.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (f *fakeRegistry) Referrers(ctx context.Context, ref imageadmit.Ref, subject, artifactType string) ([]imageadmit.Descriptor, error) {
	f.note(ref)
	if err := f.count("Referrers", artifactType); err != nil {
		return nil, err
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if subject != digest { // only the test image has referrers
		return nil, nil
	}
	return append([]imageadmit.Descriptor(nil), f.referrers[artifactType]...), nil
}

func (f *fakeRegistry) LegacyTag(_ context.Context, ref imageadmit.Ref, subject, suffix string) (*imageadmit.Manifest, error) {
	f.note(ref)
	if err := f.count("LegacyTag", subject+suffix); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.legacy[suffix]
	if !ok || subject != digest {
		return nil, imageadmit.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

// legacyTag publishes ls as the layers of the image's legacy cosign tag with suffix, as cosign v2 pushes them.
func (f *fakeRegistry) legacyTag(suffix string, ls ...imageadmit.LegacyLayer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &imageadmit.Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json"}
	for _, l := range ls {
		m.Layers = append(m.Layers, l.Descriptor)
		if l.Blob != nil { // a layer with no blob is one the registry has lost
			f.blobs[l.Descriptor.Digest] = l.Blob
		}
	}
	f.legacy[suffix] = m
}

func (f *fakeRegistry) Blob(_ context.Context, ref imageadmit.Ref, d imageadmit.Descriptor) ([]byte, error) {
	f.note(ref)
	if err := f.count("Blob", d.Digest); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[d.Digest]
	if !ok {
		return nil, imageadmit.ErrNotFound
	}
	return b, nil
}

var _ imageadmit.Registry = (*fakeRegistry)(nil)

// The real client is what the admitter reads through.
var _ imageadmit.Registry = (*imageadmit.RegistryClient)(nil)

// admit runs Require then Admit for imageRef, the way the run-create check does.
func admit(t *testing.T, a *imageadmit.Admitter, p *imageadmit.Policy, imageRef string) (imageadmit.Verdict, error) {
	t.Helper()
	req, refused := p.Require(imageRef, true, nil)
	if refused != nil {
		return *refused, nil
	}
	return a.Admit(context.Background(), req)
}
