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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// operatorIPSW writes a restore image the operator downloaded and returns its path and SHA-256.
func operatorIPSW(t *testing.T, body string) (path, sha string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "UniversalMac_27.0_26A1_Restore.ipsw")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(body))
	return path, hex.EncodeToString(h[:])
}

// cacheNames lists the download cache.
func cacheNames(t *testing.T, f *prepFixture) []string {
	t.Helper()
	des, err := os.ReadDir(f.opts.Layout.CacheDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func cachedPath(f *prepFixture) string {
	return filepath.Join(f.opts.Layout.CacheDir(), "UniversalMac_27.0_26A1_Restore.ipsw")
}

func placeInCache(t *testing.T, f *prepFixture, body, record string) {
	t.Helper()
	if err := os.MkdirAll(f.opts.Layout.CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedPath(f), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if record != "" {
		if err := os.WriteFile(cachedPath(f)+".sha256", []byte(record+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// --ipsw with the right hash installs from a copy in the cache, never asks Apple for the newest image's
// URL, never downloads, leaves the operator's file as it is, and the success cleanup deletes the copy.
func TestPrepareIPSWMatchingHashInstallsFromACopy(t *testing.T) {
	f := newPrep(t)
	src, sha := operatorIPSW(t, "operator-downloaded-restore-image")
	f.opts.IPSW, f.opts.IPSWSHA256 = src, strings.ToUpper(sha)
	var installedFrom string
	var duringInstall []string
	f.eng.InstallFunc = func(_ context.Context, _ *macvmtest.Machine, ipsw string, progress func(float64)) error {
		installedFrom, duringInstall = ipsw, cacheNames(t, f)
		a, _ := os.Stat(src)
		b, err := os.Stat(ipsw)
		if err != nil || os.SameFile(a, b) {
			t.Errorf("install ran from %q (same file as the operator's: %v, %v)", ipsw, err == nil, err)
		}
		progress(1)
		return nil
	}
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.eng.URLCalls(); n != 0 {
		t.Fatalf("LatestRestoreImageURL ran %d times with --ipsw", n)
	}
	if n := f.srv.reqs.Load(); n != 0 {
		t.Fatalf("%d downloads with --ipsw", n)
	}
	if f.eng.Installs() != 1 || m.IPSWSHA256 != sha {
		t.Fatalf("installs %d, manifest hash %q, want %q", f.eng.Installs(), m.IPSWSHA256, sha)
	}
	if filepath.Dir(installedFrom) != f.opts.Layout.CacheDir() || len(duringInstall) != 2 {
		t.Fatalf("installed from %q with the cache holding %v, want the image and its record", installedFrom, duringInstall)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "operator-downloaded-restore-image" {
		t.Fatalf("the operator's file changed: %q %v", b, err)
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("the cache copy outlived a successful prepare: %v", left)
	}
}

// A file that is not the hash given is refused by name before it is loaded or installed, nothing of it stays
// in the cache, and the operator's file is untouched.
func TestPrepareIPSWMismatchRefusesAndKeepsNothing(t *testing.T) {
	f := newPrep(t)
	src, sha := operatorIPSW(t, "not-the-expected-image")
	want := strings.Repeat("ab", 32)
	f.opts.IPSW, f.opts.IPSWSHA256 = src, want
	before, _ := os.Stat(src)
	_, err := macvm.Prepare(context.Background(), f.opts)
	var mm *macvm.IPSWMismatchError
	if !errors.As(err, &mm) || mm.Want != want || mm.Got != sha ||
		!strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), sha) {
		t.Fatalf("mismatch: %v", err)
	}
	if f.eng.Loads() != 0 || f.eng.Installs() != 0 || f.eng.URLCalls() != 0 || f.srv.reqs.Load() != 0 {
		t.Fatalf("a refused file was used: %d loads, %d installs, %d url calls, %d downloads", f.eng.Loads(), f.eng.Installs(), f.eng.URLCalls(), f.srv.reqs.Load())
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("a refused file left %v in the cache", left)
	}
	after, err := os.Stat(src)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the operator's file changed: %v %v", after, err)
	}
}

// --ipsw without --sha256 is a usage error, and so is a hash that is not 64 hex digits or a hash with no file.
func TestPrepareIPSWWithoutSHA256IsAUsageError(t *testing.T) {
	f := newPrep(t)
	src, sha := operatorIPSW(t, "image")
	f.opts.IPSW = src
	if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrIPSWNeedsSHA256) {
		t.Fatalf("--ipsw alone: %v", err)
	}
	if f.eng.Loads() != 0 || len(cacheNames(t, f)) != 0 {
		t.Fatal("--ipsw alone touched the cache or the engine")
	}
	f.opts.IPSW, f.opts.IPSWSHA256 = "", sha
	if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrSHA256NeedsIPSW) {
		t.Fatalf("a hash with no file: %v", err)
	}
	f.opts.IPSW, f.opts.IPSWSHA256 = src, ""
	for _, tc := range []struct {
		ipsw, sha string
		want      error
	}{
		{src, "", macvm.ErrIPSWNeedsSHA256},
		{src, sha[:63], macvm.ErrIPSWNeedsSHA256},
		{src, "zz" + sha[2:], macvm.ErrIPSWNeedsSHA256},
		{"", sha, macvm.ErrSHA256NeedsIPSW},
		{src, sha, nil},
		{"", "", nil},
	} {
		if _, err := macvm.CheckIPSWFlags(tc.ipsw, tc.sha); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("CheckIPSWFlags(%q, %q) = %v, want %v", tc.ipsw, tc.sha, err, tc.want)
		}
	}
}

// A file placed in the cache by hand has no record of where it came from: prepare refuses it, names it and
// both remedies, and neither uses nor deletes it (it may be the operator's only copy).
func TestPrepareCacheFileWithoutRecordIsRefusedAndKept(t *testing.T) {
	f := newPrep(t)
	placeInCache(t, f, "placed-by-hand", "")
	_, err := macvm.Prepare(context.Background(), f.opts)
	var ue *macvm.UnrecordedImageError
	if !errors.As(err, &ue) || ue.Mismatch || ue.Path != cachedPath(f) ||
		!strings.Contains(err.Error(), cachedPath(f)) || !strings.Contains(err.Error(), "--ipsw <moved path> --sha256") || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("unrecorded cache file: %v", err)
	}
	if b, rerr := os.ReadFile(cachedPath(f)); rerr != nil || string(b) != "placed-by-hand" {
		t.Fatalf("the file was changed or deleted: %q %v", b, rerr)
	}
	if f.srv.reqs.Load() != 0 || f.eng.Loads() != 0 || f.eng.Installs() != 0 {
		t.Fatalf("a refused file led to %d downloads, %d loads, %d installs", f.srv.reqs.Load(), f.eng.Loads(), f.eng.Installs())
	}
}

// A record that does not match its file is refused the same way, and both files stay.
func TestPrepareCacheFileWithWrongRecordIsRefusedAndKept(t *testing.T) {
	f := newPrep(t)
	rec := strings.Repeat("cd", 32)
	placeInCache(t, f, "swapped-after-download", rec)
	_, err := macvm.Prepare(context.Background(), f.opts)
	var ue *macvm.UnrecordedImageError
	if !errors.As(err, &ue) || !ue.Mismatch {
		t.Fatalf("mismatched record: %v", err)
	}
	if b, rerr := os.ReadFile(cachedPath(f)); rerr != nil || string(b) != "swapped-after-download" {
		t.Fatalf("the file was changed or deleted: %q %v", b, rerr)
	}
	if b, rerr := os.ReadFile(cachedPath(f) + ".sha256"); rerr != nil || strings.TrimSpace(string(b)) != rec {
		t.Fatalf("the record was changed or deleted: %q %v", b, rerr)
	}
	if f.srv.reqs.Load() != 0 {
		t.Fatalf("%d downloads after a refusal", f.srv.reqs.Load())
	}
}

func placePart(t *testing.T, f *prepFixture, body []byte, marker bool) {
	t.Helper()
	if err := os.MkdirAll(f.opts.Layout.CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedPath(f)+".part", body, 0o600); err != nil {
		t.Fatal(err)
	}
	if marker {
		if err := os.WriteFile(cachedPath(f)+".part.pinned", nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A complete partial file placed by hand has no marker of a Wardyn download: it is removed with a warning,
// the image is fetched from byte 0, and the record is the server's bytes, not the file's.
func TestPrepareHandPlacedCompletePartIsNotTrusted(t *testing.T) {
	f := newPrep(t)
	tampered := append([]byte("X"), f.srv.body[1:]...)
	placePart(t, f, tampered, false)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.IPSWSHA256 != f.srv.sha() || f.srv.served.Load() != int64(len(f.srv.body)) {
		t.Fatalf("manifest hash %q (want %q), %d bytes served of %d", m.IPSWSHA256, f.srv.sha(), f.srv.served.Load(), len(f.srv.body))
	}
	if !strings.Contains(f.logs.String(), "UniversalMac_27.0_26A1_Restore.ipsw.part") || !strings.Contains(f.logs.String(), "no marker") {
		t.Fatalf("no warning naming the partial file:\n%s", f.logs.String())
	}
}

// A short partial file placed by hand is not appended to.
func TestPrepareHandPlacedShortPartIsNotAppendedTo(t *testing.T) {
	f := newPrep(t)
	placePart(t, f, []byte("hand-placed-bytes"), false)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.IPSWSHA256 != f.srv.sha() {
		t.Fatalf("%v, manifest hash %q (want %q)", err, m.IPSWSHA256, f.srv.sha())
	}
}

// A partial file left by an interrupted download of Wardyn's, with its marker, still resumes, and the
// marker goes when the image is complete.
func TestPrepareMarkedPartResumes(t *testing.T) {
	f := newPrep(t)
	half := len(f.srv.body) / 2
	placePart(t, f, f.srv.body[:half], true)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.IPSWSHA256 != f.srv.sha() {
		t.Fatalf("%v, manifest hash %q", err, m.IPSWSHA256)
	}
	if got := f.srv.served.Load(); got != int64(len(f.srv.body)-half) {
		t.Fatalf("%d bytes served, want only the missing %d", got, len(f.srv.body)-half)
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache: %v", left)
	}
}

// A prepare that fails after the download leaves the image and the record its download wrote, and the
// retry reuses them without downloading again.
func TestPrepareRetryReusesTheRecordedDownload(t *testing.T) {
	f := newPrep(t)
	f.eng.InstallFunc = func(context.Context, *macvmtest.Machine, string, func(float64)) error {
		return errors.New("fake: install failed")
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("install error not reported")
	}
	rec, err := os.ReadFile(cachedPath(f) + ".sha256")
	if err != nil || strings.TrimSpace(string(rec)) != f.srv.sha() {
		t.Fatalf("record after the download: %q %v", rec, err)
	}
	reqs := f.srv.reqs.Load()
	f.eng.InstallFunc = nil
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if n := f.srv.reqs.Load(); n != reqs {
		t.Fatalf("the retry downloaded again: %d requests, then %d", reqs, n)
	}
}

// The success cleanup removes the image and its record; --keep-ipsw keeps both.
func TestPrepareCleanupRemovesRecordUnlessKept(t *testing.T) {
	f := newPrep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache: %v", left)
	}
	k := newPrep(t)
	k.opts.KeepIPSW = true
	if _, err := macvm.Prepare(context.Background(), k.opts); err != nil {
		t.Fatal(err)
	}
	img, rec := cachedPath(k), cachedPath(k)+".sha256"
	if b, err := os.ReadFile(img); err != nil || !bytes.Equal(b, k.srv.body) {
		t.Fatalf("--keep-ipsw did not keep the image: %v", err)
	}
	if b, err := os.ReadFile(rec); err != nil || strings.TrimSpace(string(b)) != k.srv.sha() {
		t.Fatalf("--keep-ipsw did not keep the record: %q %v", b, err)
	}
}

// A cached image that fails validation is deleted with its record, and a bad one given with --ipsw stays out
// of the cache.
func TestPrepareValidationFailureRemovesRecord(t *testing.T) {
	f := newPrep(t)
	f.eng.LoadFailures = 2
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("two validation failures not reported")
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache after validation failed twice: %v", left)
	}
	g := newPrep(t)
	src, sha := operatorIPSW(t, "unsupported-image")
	g.opts.IPSW, g.opts.IPSWSHA256 = src, sha
	g.eng.LoadFailures = 1
	if _, err := macvm.Prepare(context.Background(), g.opts); err == nil || g.srv.reqs.Load() != 0 {
		t.Fatalf("--ipsw failing validation: %v, %d downloads", err, g.srv.reqs.Load())
	}
	if left := cacheNames(t, g); len(left) != 0 {
		t.Fatalf("left in the cache after --ipsw failed validation: %v", left)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the operator's file is gone: %v", err)
	}
}

// With a ready base prepare does nothing and does not use the file; with Reprepare it builds the new base
// from the file and still never asks for Apple's URL.
func TestPrepareIPSWWithAReadyBase(t *testing.T) {
	f := newPrep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	urlCalls, loads := f.eng.URLCalls(), f.eng.Loads()
	src, sha := operatorIPSW(t, "a-newer-image")
	f.opts.IPSW, f.opts.IPSWSHA256 = src, sha
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil || f.eng.Installs() != 1 || f.eng.Loads() != loads || len(cacheNames(t, f)) != 0 {
		t.Fatalf("ready base with --ipsw: %v, %d installs, %d loads, cache %v", err, f.eng.Installs(), f.eng.Loads(), cacheNames(t, f))
	}
	f.opts.Reprepare = true
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || f.eng.Installs() != 2 || m.IPSWSHA256 != sha || f.eng.URLCalls() != urlCalls {
		t.Fatalf("re-prepare with --ipsw: %v, %d installs, manifest hash %q, %d url calls (was %d)", err, f.eng.Installs(), m.IPSWSHA256, f.eng.URLCalls(), urlCalls)
	}
}

// A file that is already in the cache is refused: the success cleanup would delete the operator's own file.
func TestPrepareIPSWInsideTheCacheIsRefused(t *testing.T) {
	f := newPrep(t)
	placeInCache(t, f, "in-the-cache", "")
	_, sha := operatorIPSW(t, "in-the-cache")
	f.opts.IPSW, f.opts.IPSWSHA256 = cachedPath(f), sha
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "which prepare empties") {
		t.Fatalf("--ipsw inside the cache: %v", err)
	}
	if b, err := os.ReadFile(cachedPath(f)); err != nil || string(b) != "in-the-cache" {
		t.Fatalf("the file in the cache changed: %q %v", b, err)
	}
}

// A crash between the record and the rename leaves a complete marked partial file and its record. The
// next prepare gets a 416 for it, renames it and installs, without refusing.
func TestPrepareCrashAfterRecordBeforeRenameResumes(t *testing.T) {
	f := newPrep(t)
	placePart(t, f, f.srv.body, true)
	if err := os.WriteFile(cachedPath(f)+".sha256", []byte(f.srv.sha()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.IPSWSHA256 != f.srv.sha() || f.srv.served.Load() != 0 {
		t.Fatalf("%v, manifest hash %q, %d bytes served", err, m.IPSWSHA256, f.srv.served.Load())
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache: %v", left)
	}
}

// A crash after the rename leaves the recorded image and the download's marker: the image is trusted and the
// stale marker is cleared.
func TestPrepareCrashAfterRenameKeepsTheImageAndClearsTheMarker(t *testing.T) {
	f := newPrep(t)
	placeInCache(t, f, string(f.srv.body), f.srv.sha())
	if err := os.WriteFile(cachedPath(f)+".part.pinned", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.opts.KeepIPSW = true
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.IPSWSHA256 != f.srv.sha() || f.srv.reqs.Load() != 0 {
		t.Fatalf("%v, manifest hash %q, %d requests", err, m.IPSWSHA256, f.srv.reqs.Load())
	}
	if _, err := os.Stat(cachedPath(f) + ".part.pinned"); !os.IsNotExist(err) {
		t.Fatalf("the stale marker stayed: %v", err)
	}
}

// A record that cannot be written stops the download before the rename, so a retry finds a marked complete
// partial file, not an image with no record.
func TestPrepareRecordWriteFailureLeavesNoUnrecordedImage(t *testing.T) {
	f := newPrep(t)
	if err := os.MkdirAll(cachedPath(f)+".sha256", 0o700); err != nil { // a directory where the record goes
		t.Fatal(err)
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("an unwritable record not reported")
	}
	if _, err := os.Stat(cachedPath(f)); !os.IsNotExist(err) {
		t.Fatalf("the image was renamed into place without its record: %v", err)
	}
	if err := os.Remove(cachedPath(f) + ".sha256"); err != nil {
		t.Fatal(err)
	}
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.IPSWSHA256 != f.srv.sha() {
		t.Fatalf("retry: %v, manifest hash %q", err, m.IPSWSHA256)
	}
}

// A download that fetched nothing leaves no empty marked partial file behind.
func TestPrepareFailedDownloadWithNoBytesLeavesNoPart(t *testing.T) {
	f := newPrep(t)
	tr := &failingTransport{err: errors.New(`tls: failed to verify certificate: x509: “updates.cdn-apple.com” certificates do not meet pinning requirements`)}
	f.opts.HTTP = &http.Client{Transport: tr}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("pinning refusal not reported")
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache: %v", left)
	}
}

// A link outside the cache to a file inside it is refused too, and its target stays.
func TestPrepareIPSWLinkedToTheCacheIsRefused(t *testing.T) {
	f := newPrep(t)
	placeInCache(t, f, "in-the-cache", "")
	_, sha := operatorIPSW(t, "in-the-cache")
	link := filepath.Join(t.TempDir(), "UniversalMac_27.0_26A1_Restore.ipsw")
	if err := os.Symlink(cachedPath(f), link); err != nil {
		t.Skip(err)
	}
	f.opts.IPSW, f.opts.IPSWSHA256 = link, sha
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "links to") {
		t.Fatalf("--ipsw linked to the cache: %v", err)
	}
	if b, err := os.ReadFile(cachedPath(f)); err != nil || string(b) != "in-the-cache" {
		t.Fatalf("the target changed: %q %v", b, err)
	}
}

// A link planted where the copy is made is replaced, never written through.
func TestPrepareIPSWCopyDoesNotWriteThroughAPlantedLink(t *testing.T) {
	f := newPrep(t)
	src, sha := operatorIPSW(t, "operator-image")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.opts.Layout.CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, cachedPath(f)+".copy"); err != nil {
		t.Skip(err)
	}
	f.opts.IPSW, f.opts.IPSWSHA256 = src, sha
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Fatalf("the link's target was written through: %q %v", b, err)
	}
	if left := cacheNames(t, f); len(left) != 0 {
		t.Fatalf("left in the cache: %v", left)
	}
}

// The pinning refusal also tells the operator how to bring their own image, without claiming Apple
// publishes a hash.
func TestPrepareDownloadPinningRefusalNamesIPSW(t *testing.T) {
	f := newPrep(t)
	tr := &failingTransport{err: errors.New(`tls: failed to verify certificate: x509: “updates.cdn-apple.com” certificates do not meet pinning requirements`)}
	f.opts.HTTP = &http.Client{Transport: tr}
	_, err := macvm.Prepare(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), "--ipsw <file> --sha256 <hex>") || !strings.Contains(err.Error(), f.srv.url()) ||
		!strings.Contains(err.Error(), "source you trust") || strings.Contains(strings.ToLower(err.Error()), "publish") {
		t.Fatalf("pinning refusal: %v", err)
	}
}
