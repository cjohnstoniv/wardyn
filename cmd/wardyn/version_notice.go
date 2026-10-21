// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// older reports whether release a sorts before b as major.minor.patch; anything unreadable is not older.
func older(a, b string) bool {
	pa, pb := releaseParts(a), releaseParts(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func releaseParts(v string) []int {
	f := strings.Split(v, ".")
	if len(f) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}
