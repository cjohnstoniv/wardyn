// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// The agent's browser: its own profile, separate from the person's, which
// loads every login the person shared (~/.wardyn/logins/<site>.json) at start
// and whenever a file changes. The agent may run any browser it likes; the
// logins directory is the contract, and this launcher is one reader of it.

func agentProfileDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "wardyn-browser")
}

func agentCtlSock() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wardyn", "browser.sock")
}

// cmdBrowser runs the agent's browser until it exits. --headless runs it
// without a display. As the agent.
func cmdBrowser(args []string) error {
	if err := checkAgentUID(); err != nil {
		return err
	}
	flags := browserFlags(agentProfileDir())
	extra, err := browserArgs(args)
	if err != nil {
		return err
	}
	flags = append(flags, extra...)
	if err := os.MkdirAll(filepath.Dir(agentCtlSock()), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(agentProfileDir(), 0o700); err != nil {
		return err
	}
	r3, w3, err := os.Pipe()
	if err != nil {
		return err
	}
	r4, w4, err := os.Pipe()
	if err != nil {
		return err
	}
	bin, err := browserBinary(browserBinaries, os.Stat)
	if err != nil {
		return err
	}
	b := exec.Command(bin, flags...)
	b.Env = browserEnv(os.Environ())
	b.ExtraFiles = []*os.File{r3, w4}
	if err := b.Start(); err != nil {
		return err
	}
	_ = r3.Close()
	_ = w4.Close()
	c := newCDP(w3, r4)
	ln, err := listenCtl(agentCtlSock())
	if err != nil {
		_ = b.Process.Kill()
		return err
	}
	defer ln.Close()
	go serveCtl(ln, c, true)
	exited := make(chan error, 1)
	go func() { exited <- b.Wait() }()
	applied := map[string]time.Time{}
	for {
		applyLogins(c, applied)
		select {
		case err := <-exited:
			return err
		case <-time.After(2 * time.Second):
		}
	}
}

// browserArgs are the flags cmdBrowser adds for its arguments: --headless, and a start URL. Anything
// else is refused, so no argument can open a debugging port or load an extension (#2970).
func browserArgs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		switch {
		case a == "--headless":
			out = append(out, "--headless=new")
		case a == "about:blank" || strings.HasPrefix(a, "https://") || strings.HasPrefix(a, "http://"):
			out = append(out, a)
		default:
			return nil, fmt.Errorf("browser takes --headless and a start URL only; %q is refused, so no flag can open a debugging port or load an extension", a)
		}
	}
	return out, nil
}

// browserBinaries are the Chromium binaries cmdBrowser may run, first found wins: Debian's own binary,
// which reads no CHROMIUM_FLAGS, then its wrapper. Never a PATH lookup: PATH is the agent's (#2970).
var browserBinaries = []string{"/usr/lib/chromium/chromium", "/usr/bin/chromium"}

// browserBinary is the first of bins that is a regular file owned by root and writable by nobody else.
func browserBinary(bins []string, stat func(string) (fs.FileInfo, error)) (string, error) {
	for _, p := range bins {
		fi, err := stat(p)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() && st.Uid == 0 && fi.Mode().Perm()&0o022 == 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("no root-owned Chromium at %s; the browser was not started", strings.Join(bins, " or "))
}

// browserEnvKeep are the only variables of the agent's environment its browser gets: its display, locale,
// home and temp, and the run's proxy and CA. Anything else, CHROMIUM_FLAGS and LD_PRELOAD among them,
// could add a flag or code the launcher refuses (#2970).
var browserEnvKeep = []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "TMPDIR", "XDG_RUNTIME_DIR",
	"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// browserEnv is env cut to browserEnvKeep, with a fixed PATH.
func browserEnv(env []string) []string {
	out := []string{"PATH=/usr/bin:/bin"}
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); slices.Contains(browserEnvKeep, k) {
			out = append(out, kv)
		}
	}
	return out
}

// applyLogins sets the cookies of every logins file new or changed since the
// last pass.
func applyLogins(c *cdp, applied map[string]time.Time) {
	files, _ := filepath.Glob(filepath.Join(agentLoginsDir(), "*.json"))
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || applied[f].Equal(fi.ModTime()) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var st storageState
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		params := struct {
			Cookies []map[string]any `json:"cookies"`
		}{Cookies: []map[string]any{}}
		for _, ck := range st.Cookies {
			p := map[string]any{"name": ck.Name, "value": ck.Value, "domain": ck.Domain, "path": ck.Path,
				"secure": ck.Secure, "httpOnly": ck.HTTPOnly, "sameSite": ck.SameSite}
			if ck.Expires > 0 {
				p["expires"] = ck.Expires
			}
			params.Cookies = append(params.Cookies, p)
		}
		raw, _ := json.Marshal(params)
		if _, err := c.call("Storage.setCookies", raw); err == nil {
			applied[f] = fi.ModTime()
		}
	}
}
