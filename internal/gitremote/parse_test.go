// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// B11a-F8/F9/F10. Every one of these fails SAFE — the wrong answer is a dropped
// repo or an extra line in a warning, never a widened grant — but F8/F9/F10 all
// POISON the operator's "other hosts" warning, which is the string a human reads
// to decide whether a workspace is safe to run. A warning that says the remote
// host is "https" or "file" or "b@github.com" is worse than no warning.
func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		name, url, wantHost, wantRepo string
	}{
		// B11a-F8: the scheme switch was case-sensitive, so an upper-case
		// scheme fell into the default (scp-like) arm and the SCHEME itself
		// came back as the host.
		{"uppercase https", "HTTPS://github.com/acme/web", "github.com", "acme/web"},
		{"mixed case ssh", "SSH://git@github.com/acme/web.git", "github.com", "acme/web"},
		// B11a-F8: a URL whose scheme this package does not handle is NOT an
		// scp target — git's own rule is that "://" makes it a URL — so it must
		// be dropped rather than parsed into the host "file".
		{"file url", "file:///srv/mirrors/acme.git", "", ""},
		{"unknown scheme", "ftp://example.com/acme/web.git", "", ""},
		// B11a-F9: userinfo was split at the FIRST "@", so a value with two
		// landed on the host "b@github.com"; git and net/url both split at the
		// LAST one.
		{"double at ssh url", "ssh://a@b@github.com/acme/web", "github.com", "acme/web"},
		{"double at scp", "a@b@github.com:acme/web.git", "github.com", "acme/web"},
		// Ordinary forms stay exactly as they were.
		{"https", "https://github.com/acme/web.git", "github.com", "acme/web"},
		{"scp", "git@github.com:acme/web.git", "github.com", "acme/web"},
		{"https other host", "https://gitlab.example.com/acme/web.git", "gitlab.example.com", "acme/web"},
		{"https with port", "https://git.example.com:8443/acme/web.git", "git.example.com", "acme/web"},
		// Same class as the git helper's B11a-F11: a bracketed IPv6 host was
		// truncated at the first colon to "[2001", which is what the operator
		// would have read in the warning.
		{"ipv6 with port", "ssh://git@[2001:db8::1]:2222/acme/web.git", "2001:db8::1", "acme/web"},
		{"ipv6 no port", "https://[2001:db8::1]/acme/web.git", "2001:db8::1", "acme/web"},
		// R-02: the scp-like arm took the FIRST colon, which is INSIDE the
		// bracket — `git clone git@[::1]:repo.git` is valid git syntax, and it
		// produced the same bogus "[2001" the URL arm had already been fixed of.
		{"scp ipv6", "git@[2001:db8::1]:acme/web.git", "2001:db8::1", "acme/web"},
		{"scp ipv6 no user", "[2001:db8::1]:acme/web.git", "2001:db8::1", "acme/web"},
		{"scp ipv6 unterminated", "git@[2001:db8::1:acme/web.git", "", ""},
		{"no path", "https://github.com", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, repo := parseRemoteURL(c.url)
			if host != c.wantHost || repo != c.wantRepo {
				t.Fatalf("parseRemoteURL(%q) = (%q, %q), want (%q, %q)", c.url, host, repo, c.wantHost, c.wantRepo)
			}
		})
	}
}

// End-to-end through a real tree: the detected GitHub set and the operator's
// "other hosts" warning are what actually ship, so pin them, not just the
// parser. Every URL here used to contribute a bogus host.
func TestDetect_PoisonedHostFormsAreNotWarnedAbout(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root,
		"HTTPS://github.com/acme/upper",
		"ssh://a@b@github.com/acme/doubleat",
		"file:///srv/mirrors/local.git",
		"https://gitlab.example.com/acme/real",
	)
	gh, other := DetectGitHubRepos(root)
	if !reflect.DeepEqual(gh, []string{"acme/doubleat", "acme/upper"}) {
		t.Fatalf("github = %v, want [acme/doubleat acme/upper]", gh)
	}
	if !reflect.DeepEqual(other, []string{"gitlab.example.com"}) {
		t.Fatalf("otherHosts = %v, want only the real non-GitHub host [gitlab.example.com]", other)
	}
}

// B11a-F10. gitremote.safe carried a FIXED whitespace list while claiming to
// mirror internal/api's repoFieldSafe, which moved to unicode.IsControl ||
// unicode.IsSpace — so the Unicode space separators repoFieldSafe rejects
// (U+2000..U+200A, U+3000, …) passed here. One predicate, one answer.
func TestFieldSafe(t *testing.T) {
	unsafe := []string{
		"https://github.com/a b/c",      // ASCII space
		"https://github.com/a\tb/c",     // tab
		"https://github.com/a\nb/c",     // newline
		"https://github.com/a\x00b/c",   // NUL
		"https://github.com/a\u0085b/c", // NEL
		"https://github.com/a\u00a0b/c", // NBSP
		"https://github.com/a\u2003b/c", // EM SPACE - the fixed list let this through
		"https://github.com/a\u3000b/c", // IDEOGRAPHIC SPACE
		"https://github.com/a\u200ab/c", // HAIR SPACE
		"https://github.com/a\u001fb/c", // unit separator
		"https://github.com/a\u009fb/c", // C1 control
	}
	for _, s := range unsafe {
		if FieldSafe(s) {
			t.Errorf("FieldSafe(%q) = true, want false", s)
		}
	}
	safeVals := []string{
		"https://github.com/acme/web.git",
		"git@github.com:acme/web.git",
		"ssh://git@[2001:db8::1]:2222/acme/web.git",
		"https://github.com/acme/w\u00e9b.git", // non-ASCII letters are fine
	}
	for _, s := range safeVals {
		if !FieldSafe(s) {
			t.Errorf("FieldSafe(%q) = false, want true", s)
		}
	}
}

// B11a-F5. The package doc promises the walk "never follows symlinks" and is
// "bounded", but readCapped did a bare os.Open: a FIFO named .gitmodules blocks
// open(2) forever waiting for a writer, and CollectFacts runs on an HTTP handler
// goroutine with NO ctx (source_scan.go's CollectFacts call) — so one such file
// in a scanned tree wedged that request permanently. The verdict's "same trust
// tier as the tree being scanned" is about the symlink half; the hang is real
// either way, because a wedged handler goroutine is a liveness bug regardless of
// who put the file there.
func TestDetect_FifoDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo is POSIX")
	}
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, ".gitmodules"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan []string, 1)
	go func() {
		gh, _ := DetectGitHubRepos(root)
		done <- gh
	}()
	select {
	case gh := <-done:
		if len(gh) != 0 {
			t.Fatalf("github = %v, want none — a FIFO is not a git config", gh)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DetectGitHubRepos never returned on a tree containing a FIFO .gitmodules — the scan handler goroutine is wedged")
	}
}

// The same hang through the OTHER readCapped call site: a repo whose
// .git/config is a FIFO. This is the path resolveConfigPath hands back.
func TestDetect_FifoGitConfigDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo is POSIX")
	}
	root := t.TempDir()
	gitDir := filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(gitDir, "config"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	writeRepo(t, filepath.Join(root, "good"), "https://github.com/acme/ok")

	done := make(chan []string, 1)
	go func() {
		gh, _ := DetectGitHubRepos(root)
		done <- gh
	}()
	select {
	case gh := <-done:
		// NEGATIVE CONTROL inside the same walk: the sibling REAL repo is still
		// detected. Failing safe on one entry must not abandon the tree.
		if !reflect.DeepEqual(gh, []string{"acme/ok"}) {
			t.Fatalf("github = %v, want [acme/ok] — the real repo beside the FIFO must still be detected", gh)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DetectGitHubRepos never returned on a repo whose .git/config is a FIFO")
	}
}

// B11a-F5, symlink half: the walk skips symlinks everywhere EXCEPT the final
// .git/config open, so a .git/config symlink was the one place the package's
// own "never follows symlinks" promise did not hold. It pointed anywhere the
// scanning uid could read.
func TestDetect_SymlinkedGitConfigIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere-config")
	if err := os.WriteFile(outside, []byte("[remote \"o\"]\n\turl = https://github.com/secret/elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(gitDir, "config")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	gh, _ := DetectGitHubRepos(root)
	if len(gh) != 0 {
		t.Fatalf("github = %v, want none — a symlinked .git/config must not be followed", gh)
	}
}

// NEGATIVE CONTROL for B11a-F5: an ordinary directory repo still detects, and a
// config LARGER than one read's worth is read whole. The bare single Read could
// short-read and silently drop every remote after the break; io.ReadFull cannot.
func TestDetect_LargeConfigReadWhole(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[remote \"origin\"]\n\turl = https://github.com/acme/first\n"
	// ~300 KiB of filler between the two remotes, well past any single read.
	for range 6000 {
		body += "\t# filler filler filler filler filler filler filler filler\n"
	}
	body += "[remote \"last\"]\n\turl = https://github.com/acme/last\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gh, _ := DetectGitHubRepos(root)
	if !reflect.DeepEqual(gh, []string{"acme/first", "acme/last"}) {
		t.Fatalf("github = %v, want both remotes [acme/first acme/last]", gh)
	}
}
