// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// The version notice (design ATTACH 11.7, #3310): after a successful call, at most once a day per
// organisation URL, login and whoami compare /healthz.version with this CLI's release and print one stderr
// line when they differ. The last check per URL is cached in <UserCacheDir>/wardyn/healthz.json (nothing
// secret). A development build (a version with a +commit suffix) never checks.

const (
	noticeCacheFile = "healthz.json"
	noticeInterval  = 24 * time.Hour
)

// cliVersion is this CLI's version; a var so a test can be any build.
var cliVersion = version.String

type noticeCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

// versionNotice prints the notice for origin on w when it is due. server is /healthz.version when the
// command has already read it, else "" and it is read here. Every failure is silent: the notice is advice.
func versionNotice(ctx context.Context, w io.Writer, origin, server string) {
	cli := cliVersion()
	if strings.Contains(cli, "+") {
		return
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir := filepath.Join(base, "wardyn")
	path := filepath.Join(dir, noticeCacheFile)
	checks := map[string]noticeCheck{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &checks)
	}
	if c, ok := checks[origin]; ok && time.Since(c.CheckedAt) < noticeInterval {
		return
	}
	if server == "" {
		var h struct {
			Version string `json:"version"`
		}
		if cliCall(ctx, origin, http.MethodGet, "/healthz", "", nil, &h) != nil || h.Version == "" {
			return
		}
		server = h.Version
	}
	checks[origin] = noticeCheck{CheckedAt: time.Now().UTC(), Version: server}
	if body, err := json.Marshal(checks); err == nil && os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(path, body, 0o644)
	}
	if line := noticeLine(cli, origin, server); line != "" {
		fmt.Fprintln(w, line)
	}
	if line := runnerOfferLine(ctx, server); line != "" {
		fmt.Fprintln(w, line)
	}
}

// noticeLine is the notice for a CLI at cli and an organisation at server, "" when they are the same
// release. The server's own +commit suffix is not part of its release.
func noticeLine(cli, origin, server string) string {
	release, _, _ := strings.Cut(server, "+")
	if release == cli {
		return ""
	}
	if older(release, cli) {
		return fmt.Sprintf(copyNoticeServerOlder, cli, origin, server)
	}
	return fmt.Sprintf(copyNoticeServerNewer, cli, origin, server)
}

// older reports whether release a sorts before b by semver precedence: major.minor.patch, then a release
// ahead of any of its own prereleases (0.10.0-rc.18 is older than 0.10.0). A version that is not a
// release (a build suffix, a word, an empty identifier, a leading zero) is unknown, and unknown is not older.
func older(a, b string) bool {
	pa, okA := parseRelease(a)
	pb, okB := parseRelease(b)
	if !okA || !okB {
		return false
	}
	for i := range pa.core {
		if c := cmpNum(pa.core[i], pb.core[i]); c != 0 {
			return c < 0
		}
	}
	if len(pa.pre) == 0 || len(pb.pre) == 0 {
		return len(pa.pre) != 0 && len(pb.pre) == 0
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePrereleaseID(pa.pre[i], pb.pre[i]); c != 0 {
			return c < 0
		}
	}
	return len(pa.pre) < len(pb.pre)
}

type release struct {
	core [3]string // digits with no leading zero
	pre  []string
}

// parseRelease reads major.minor.patch[-prerelease]; ok is false for anything else, "+build" included.
func parseRelease(v string) (release, bool) {
	var r release
	core, pre, hasPre := strings.Cut(v, "-")
	f := strings.Split(core, ".")
	if len(f) != 3 {
		return r, false
	}
	for i, s := range f {
		if !isNumericID(s) {
			return r, false
		}
		r.core[i] = s
	}
	if hasPre {
		r.pre = strings.Split(pre, ".")
		for _, id := range r.pre {
			if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" ||
				(isDigits(id) && !isNumericID(id)) {
				return r, false
			}
		}
	}
	return r, true
}

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// isNumericID is digits with no leading zero (a lone 0 is fine).
func isNumericID(s string) bool { return isDigits(s) && (s == "0" || s[0] != '0') }

// cmpNum compares two digit strings with no leading zero by value, whatever their length.
func cmpNum(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b)) }

// comparePrereleaseID orders two prerelease identifiers: numbers by value, below words, words by text.
func comparePrereleaseID(a, b string) int {
	na, nb := isDigits(a), isDigits(b)
	switch {
	case na && nb:
		return cmpNum(a, b)
	case na:
		return -1
	case nb:
		return 1
	}
	return cmp.Compare(a, b)
}
