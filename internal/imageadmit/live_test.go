// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
)

const bundleArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// TestLiveWardynReleaseImages checks Wardyn's own published images against the built-in signer, through the
// real registry: the release workflow's build-provenance referrer alone passes, its legacy cosign signature
// alone passes, and both fail for the release at another tag. It reads ghcr.io anonymously and only reads, so
// it runs only with WARDYN_LIVE_REGISTRY=1.
func TestLiveWardynReleaseImages(t *testing.T) {
	if os.Getenv("WARDYN_LIVE_REGISTRY") != "1" {
		t.Skip("set WARDYN_LIVE_REGISTRY=1 to read ghcr.io (anonymous, read-only)")
	}
	const version, wrong = "0.10.0-rc.13", "0.10.0-rc.12"
	c := imageadmit.NewRegistryClient(imageadmit.RegistryOptions{})
	for _, name := range []string{"wardynd", "agent-base"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			image := "ghcr.io/cjohnstoniv/" + name + ":" + version
			r, err := imageadmit.ParseRef(image)
			if err != nil {
				t.Fatal(err)
			}
			m, err := c.Manifest(ctx, r)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			repo := r
			repo.Tag = ""
			provenance := referrerBundles(ctx, t, c, repo, m.Digest)
			all, err := imageadmit.ReadLegacy(ctx, c, repo, m.Digest)
			if err != nil {
				t.Fatalf("legacy tags: %v", err)
			}
			var sigs []imageadmit.LegacyLayer
			for _, l := range all {
				if l.Descriptor.MediaType == imageadmit.LegacySignatureMediaType {
					sigs = append(sigs, l)
				}
			}
			t.Logf("%s: digest %s, %d provenance bundles, %d legacy layers (%d .sig)", image, m.Digest, len(provenance), len(all), len(sigs))
			if len(provenance) == 0 || len(sigs) == 0 {
				t.Fatalf("want provenance and a legacy signature, got %d and %d", len(provenance), len(sigs))
			}
			forms := map[string]imageadmit.Candidates{"provenance referrer": {Bundles: provenance}, "legacy .sig": {Legacy: sigs}}
			for form, cands := range forms {
				v := liveCheck(t, version, image, m.Digest, cands)
				if !v.Admitted() || v.Signer != imageadmit.BuiltinSigner {
					t.Errorf("%s alone: verdict %+v, want admitted by %s", form, v, imageadmit.BuiltinSigner)
				}
				if v = liveCheck(t, wrong, image, m.Digest, cands); v.Reason != imageadmit.SignatureInvalid {
					t.Errorf("%s alone against the %s release: verdict %+v, want %s", form, wrong, v, imageadmit.SignatureInvalid)
				}
			}
		})
	}
}

func liveCheck(t *testing.T, version, image, digest string, c imageadmit.Candidates) imageadmit.Verdict {
	t.Helper()
	p, err := imageadmit.Compile(imageadmit.Config{}, imageadmit.Options{Version: version})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	req, refused := p.Require(image, true, nil)
	if refused != nil {
		return *refused
	}
	return imageadmit.CheckCandidates(req, digest, c)
}

// referrerBundles reads the Sigstore bundles that refer to subject.
func referrerBundles(ctx context.Context, t *testing.T, c *imageadmit.RegistryClient, repo imageadmit.Ref, subject string) [][]byte {
	t.Helper()
	descs, err := c.Referrers(ctx, repo, subject, bundleArtifactType)
	if err != nil {
		t.Fatalf("referrers: %v", err)
	}
	var out [][]byte
	for _, d := range descs {
		at := repo
		at.Digest = d.Digest
		m, err := c.Manifest(ctx, at)
		if err != nil {
			t.Fatalf("referrer %s: %v", d.Digest, err)
		}
		for _, l := range m.Layers {
			if l.MediaType != bundleArtifactType {
				continue
			}
			b, err := c.Blob(ctx, repo, l)
			if err != nil {
				t.Fatalf("bundle %s: %v", l.Digest, err)
			}
			out = append(out, b)
		}
	}
	return out
}
