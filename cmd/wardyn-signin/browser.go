// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, a := range args {
		if a == "--headless" {
			flags = append(flags, "--headless=new")
		} else {
			flags = append(flags, a)
		}
	}
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
	b := exec.Command("chromium", flags...)
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
	go serveCtl(ln, c)
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
