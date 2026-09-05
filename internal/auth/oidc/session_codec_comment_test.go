// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"os"
	"regexp"
	"testing"
)

// TestSessionCommentsMatchTheCodec is the CODE half of F032. Three shipped
// documents said a pre-upgrade session cookie stays valid across the upgrade
// with no forced re-login; docs-ops corrected the documents, but all three were
// written FROM a comment in this package, which said the same thing beside code
// that does the opposite:
//
//	decodeSession is deliberately NOT widened to require this field: a
//	pre-0.6 cookie stays VALID and nobody is forced to re-login by an upgrade.
//
// decodeSession compares the payload's version EXACTLY (`sess.V !=
// SessionCodecVersion`), and a pre-0.7 cookie carries no "v" key at all, so it
// decodes to 0 and is refused. Upgrading signs every SSO human out once.
// Correcting the documents and leaving the comment would just re-seed them.
//
// DERIVED FROM THE CONST, exactly like the sibling doc guard: if the codec is
// ever relaxed to tolerate an older payload, the claim becomes true again and
// this stops demanding otherwise, instead of pinning a sentence to a rule that
// has moved.
func TestSessionCommentsMatchTheCodec(t *testing.T) {
	codec, err := os.ReadFile("session_codec.go")
	if err != nil {
		t.Fatalf("read session_codec.go: %v", err)
	}
	if !regexp.MustCompile(`sess\.V != SessionCodecVersion`).Match(codec) {
		t.Skip("decodeSession no longer requires an exact codec version — an older cookie may genuinely survive now")
	}

	// "a pre-0.N cookie ... stays valid" / "... is still valid", in any comment
	// in this package. Bounded so it cannot span paragraphs and match two
	// unrelated sentences.
	staleClaim := regexp.MustCompile(`(?i)pre-0\.[0-9]+[^.\n]{0,160}(stays|is still|remains) valid`)
	for _, name := range []string{"oidc.go", "oidc_callback.go", "session_codec.go", "session_groups.go"} {
		b, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if m := staleClaim.FindString(string(b)); m != "" {
			t.Errorf("%s still says %q — decodeSession refuses any payload whose codec version differs, so that cookie is not a stale-groups session, it is not a session. "+
				"This is the comment three shipped documents were written from; say what the code does.", name, m)
		}
	}
}
