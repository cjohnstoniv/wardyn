// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// mirror is an HTTPS restore image mirror: it answers Range requests (http.ServeContent) unless noRange, and
// counts requests.
type mirror struct {
	*httptest.Server
	body []byte
	sha  string
	reqs atomic.Int64
}

func newMirror(t *testing.T, noRange bool) *mirror {
	t.Helper()
	m := &mirror{body: bytes.Repeat([]byte("mirrored-restore-image!"), 400)}
	h := sha256.Sum256(m.body)
	m.sha = hex.EncodeToString(h[:])
	m.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.reqs.Add(1)
		if noRange {
			_, _ = w.Write(m.body)
			return
		}
		http.ServeContent(w, r, "image.ipsw", time.Time{}, bytes.NewReader(m.body))
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mirror) url() string { return m.URL + "/internal/UniversalMac_27.0_26A1_Restore.ipsw" }

// useMirror points a prepare at m, trusting its test certificate.
func (f *prepFixture) useMirror(m *mirror, pin string) {
	f.opts.IPSWURL, f.opts.IPSWSHA256, f.opts.HTTP = m.url(), pin, m.Client()
}

// --ipsw-url downloads the image from the mirror in ranges, checks the pin, and never asks Apple.
func TestPrepareIPSWURLDownloadsFromTheMirrorAndNeverAsksApple(t *testing.T) {
	f := newPrep(t)
	m := newMirror(t, false)
	f.useMirror(m, strings.ToUpper(m.sha))
	got, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if f.eng.URLCalls() != 0 || f.srv.reqs.Load() != 0 {
		t.Fatalf("Apple was asked: %d url calls, %d downloads", f.eng.URLCalls(), f.srv.reqs.Load())
	}
	if m.reqs.Load() < 2 || f.eng.Installs() != 1 || got.IPSWSHA256 != m.sha {
		t.Fatalf("%d mirror requests, %d installs, manifest hash %q, want ranges, 1 install, %q", m.reqs.Load(), f.eng.Installs(), got.IPSWSHA256, m.sha)
	}
}

// An image that is not the pinned hash is deleted before it is loaded, and Apple's image is not fetched
// in its place.
func TestPrepareIPSWURLMismatchIsRefusedWithNoFallback(t *testing.T) {
	f := newPrep(t)
	m := newMirror(t, false)
	want := strings.Repeat("ab", 32)
	f.useMirror(m, want)
	_, err := macvm.Prepare(context.Background(), f.opts)
	var mm *macvm.MirrorMismatchError
	if !errors.As(err, &mm) || mm.Want != want || mm.Got != m.sha {
		t.Fatalf("mismatch: %v", err)
	}
	if f.eng.Loads() != 0 || f.eng.Installs() != 0 || f.eng.URLCalls() != 0 || f.srv.reqs.Load() != 0 {
		t.Fatalf("a refused image was used or Apple asked: %d loads, %d installs, %d url calls, %d downloads", f.eng.Loads(), f.eng.Installs(), f.eng.URLCalls(), f.srv.reqs.Load())
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("a refused image left %v in the cache", left)
	}
}

// A mirror that answers 200 with the whole file says it must answer Range requests, once, not after retries.
func TestPrepareIPSWURLWithoutRangeSupportSaysSo(t *testing.T) {
	f := newPrep(t)
	m := newMirror(t, true)
	f.useMirror(m, m.sha)
	_, err := macvm.Prepare(context.Background(), f.opts)
	if !errors.Is(err, macvm.ErrNoRange) || !strings.Contains(err.Error(), "must answer HTTP Range requests") {
		t.Fatalf("a mirror with no Range: %v", err)
	}
	if n := m.reqs.Load(); n != 1 {
		t.Fatalf("%d requests to a mirror that cannot serve ranges, want 1", n)
	}
	if f.eng.URLCalls() != 0 || f.srv.reqs.Load() != 0 {
		t.Fatal("Apple was asked after the mirror failed")
	}
}

// A mirror whose certificate this Mac does not trust names the organisation's CA, not Apple's pinning.
func TestPrepareIPSWURLCertificateRefusalNamesTheCA(t *testing.T) {
	f := newPrep(t)
	m := newMirror(t, false)
	f.useMirror(m, m.sha)
	f.opts.HTTP = &http.Client{} // the system roots, which do not hold the test certificate
	_, err := macvm.Prepare(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), "install your organisation's CA root") || strings.Contains(err.Error(), "pinning") {
		t.Fatalf("an untrusted mirror certificate: %v", err)
	}
	if n := m.reqs.Load(); n != 0 {
		t.Fatalf("%d requests reached the mirror past a refused certificate", n)
	}
}

// A cached image is reused only when its record is the pinned hash; one of another hash (a Wardyn download
// of some other image) is replaced from the mirror.
func TestPrepareIPSWURLCacheReuseNeedsAMatchingPin(t *testing.T) {
	m := newMirror(t, false)

	f := newPrep(t)
	f.useMirror(m, m.sha)
	placeInCache(t, f, string(m.body), m.sha)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if n := m.reqs.Load(); n != 0 {
		t.Fatalf("a cached image with the pinned record was downloaded again (%d requests)", n)
	}

	f = newPrep(t)
	f.useMirror(m, m.sha)
	other := "an-older-restore-image"
	h := sha256.Sum256([]byte(other))
	placeInCache(t, f, other, hex.EncodeToString(h[:]))
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if n := m.reqs.Load(); n == 0 {
		t.Fatal("a cached image of another hash was reused under a pin it does not match")
	}
}

func TestCheckImageFlags(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		ipsw, url, sha string
		want           error
		bad            bool
	}{
		{"", "https://mirror.example/UniversalMac_27.0_26A1_Restore.ipsw", sha, nil, false},
		{"", "https://mirror.example/x.ipsw", "", macvm.ErrIPSWURLNeedsSHA256, false},
		{"/tmp/x.ipsw", "https://mirror.example/x.ipsw", sha, macvm.ErrIPSWAndURL, false},
		{"", "http://mirror.example/x.ipsw", sha, nil, true},
		{"", "https://user:pw@mirror.example/x.ipsw", sha, nil, true},
		{"", "https://mirror.example/image.zip", sha, nil, true},
		{"", "", sha, macvm.ErrSHA256NeedsIPSW, false},
	} {
		_, err := macvm.CheckImageFlags(tc.ipsw, tc.url, tc.sha)
		switch {
		case tc.want != nil && !errors.Is(err, tc.want), tc.bad && err == nil, tc.want == nil && !tc.bad && err != nil:
			t.Errorf("CheckImageFlags(%q, %q, %.4q) = %v", tc.ipsw, tc.url, tc.sha, err)
		}
	}
}
