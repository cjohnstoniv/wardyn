// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package releasesig_test

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

const version = "0.10.0"

var sums = []byte("0123  ./wardyn-runnerd-linux-amd64\n")

func release() releasesigtest.Options {
	return releasesigtest.Options{Identity: releasesig.Identity(version), Issuer: releasesig.Issuer, Repository: releasesig.Repository}
}

func TestVerifySumsAcceptsTheReleaseWorkflowAtItsTag(t *testing.T) {
	s := releasesigtest.New(t)
	if err := releasesig.VerifySums(s.TrustedMaterial(), sums, s.Sign(sums, release()), version); err != nil {
		t.Fatalf("the release's own bundle was refused: %v", err)
	}
	if got := releasesig.Identity(version); got != "https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.10.0" {
		t.Fatalf("pinned identity = %s", got)
	}
}

func TestVerifySumsRefusesAnotherSigner(t *testing.T) {
	for name, edit := range map[string]func(*releasesigtest.Options){
		"another workflow": func(o *releasesigtest.Options) {
			o.Identity = releasesig.Repository + "/.github/workflows/ci.yml@refs/tags/v" + version
		},
		"a branch ref": func(o *releasesigtest.Options) {
			o.Identity = releasesig.Repository + "/.github/workflows/release.yml@refs/heads/main"
		},
		"another tag":        func(o *releasesigtest.Options) { o.Identity = releasesig.Identity("0.9.9") },
		"another issuer":     func(o *releasesigtest.Options) { o.Issuer = "https://accounts.google.com" },
		"another repository": func(o *releasesigtest.Options) { o.Repository = "https://github.com/someone/wardyn" },
	} {
		t.Run(name, func(t *testing.T) {
			s := releasesigtest.New(t)
			o := release()
			edit(&o)
			err := releasesig.VerifySums(s.TrustedMaterial(), sums, s.Sign(sums, o), version)
			if !errors.Is(err, releasesig.ErrIdentity) {
				t.Fatalf("signed by %+v: err = %v, want ErrIdentity", o, err)
			}
		})
	}
}

func TestVerifySumsRefusesTamperedSums(t *testing.T) {
	s := releasesigtest.New(t)
	b := s.Sign(sums, release())
	err := releasesig.VerifySums(s.TrustedMaterial(), []byte("9999  ./wardyn-runnerd-linux-amd64\n"), b, version)
	if !errors.Is(err, releasesig.ErrSignature) {
		t.Fatalf("tampered SHA256SUMS: err = %v, want ErrSignature", err)
	}
}

func TestVerifySumsRequiresATransparencyLogEntry(t *testing.T) {
	s := releasesigtest.New(t)
	o := release()
	o.NoTlog = true
	if err := releasesig.VerifySums(s.TrustedMaterial(), sums, s.Sign(sums, o), version); err == nil {
		t.Fatal("a bundle with no transparency-log entry was accepted")
	}
}

func TestVerifySumsRefusesAnUnreadableBundle(t *testing.T) {
	s := releasesigtest.New(t)
	if err := releasesig.VerifySums(s.TrustedMaterial(), sums, []byte("<html>not found</html>"), version); !errors.Is(err, releasesig.ErrBundle) {
		t.Fatalf("err = %v, want ErrBundle", err)
	}
}

// The embedded root is the public-good one: a refresh that dropped Rekor or Fulcio would refuse every release.
func TestEmbeddedTrustedRoot(t *testing.T) {
	tm, err := releasesig.TrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(tm.RekorLogs()) == 0 || len(tm.FulcioCertificateAuthorities()) == 0 {
		t.Fatalf("embedded root has %d Rekor logs and %d Fulcio CAs", len(tm.RekorLogs()), len(tm.FulcioCertificateAuthorities()))
	}
}
