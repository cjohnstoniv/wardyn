// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	runnerunits "github.com/cjohnstoniv/wardyn/deploy/runner"
)

const (
	serviceLabel = "com.wardyn.runnerd"
	serviceUnit  = "wardyn-runnerd.service"
)

// setupHost is everything setup reads from or does to the computer it runs on, so a test runs it in a
// temporary home with a recording command runner and a fake Docker.
type setupHost struct {
	goos, home string
	// xdgConfig and xdgData are the login shell's XDG_CONFIG_HOME and XDG_DATA_HOME, "" when unset.
	xdgConfig, xdgData string
	dockerHost         string
	uid                int
	exe                string
	run                func(ctx context.Context, name string, args ...string) ([]byte, error)
	// docker checks that the engine answers as this user, the proxy bridge exists (creating it when
	// missing) and a probe container reaches the relay pair serve will use (findRelay), narrowed by want.
	docker func(ctx context.Context, image, network string, want relayPair) (dockerEngine, relayPair, error)
}

func osSetupHost() (setupHost, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return setupHost{}, fmt.Errorf("unsupported OS: %s (on Windows, run setup inside WSL2)", runtime.GOOS)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return setupHost{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return setupHost{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return setupHost{}, err
	}
	return setupHost{goos: runtime.GOOS, home: home, xdgConfig: os.Getenv("XDG_CONFIG_HOME"), xdgData: os.Getenv("XDG_DATA_HOME"),
		dockerHost: os.Getenv("DOCKER_HOST"), uid: os.Geteuid(), exe: exe, run: runCommand, docker: prepareSetupDocker}, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// setupPaths are where setup puts things.
type setupPaths struct{ installed, stateDir, envFile, unitFile, plistFile, logFile string }

func (p setupPaths) identity() string { return filepath.Join(p.stateDir, "runner.json") }

// paths follows install.sh: the binary under the data directory; on Linux the unit and env file under
// $HOME/.config, because the systemd user manager never sees a login shell's XDG_CONFIG_HOME, with the
// identity in the XDG directory only when an earlier install already registered there.
func (h setupHost) paths() (setupPaths, error) {
	switch h.goos {
	case "linux":
		data := filepath.Join(cmp.Or(h.xdgData, filepath.Join(h.home, ".local", "share")), "wardyn")
		cfg := filepath.Join(h.home, ".config")
		p := setupPaths{installed: filepath.Join(data, "bin", "wardyn-runnerd"), stateDir: filepath.Join(cfg, "wardyn", "runner"),
			envFile: filepath.Join(cfg, "wardyn", "runnerd.env"), unitFile: filepath.Join(cfg, "systemd", "user", serviceUnit)}
		if h.xdgConfig != "" {
			xdg := filepath.Join(h.xdgConfig, "wardyn", "runner")
			if _, err := os.Stat(p.identity()); err != nil {
				if _, err := os.Stat(filepath.Join(xdg, "runner.json")); err == nil {
					p.stateDir = xdg
				}
			}
		}
		return p, nil
	case "darwin":
		data := filepath.Join(h.home, "Library", "Application Support", "wardyn")
		return setupPaths{installed: filepath.Join(data, "bin", "wardyn-runnerd"), stateDir: filepath.Join(data, "runner"),
			plistFile: filepath.Join(h.home, "Library", "LaunchAgents", serviceLabel+".plist"), logFile: filepath.Join(h.home, "Library", "Logs", "wardyn", "runnerd.log")}, nil
	}
	return setupPaths{}, fmt.Errorf("unsupported OS: %s", h.goos)
}

// checkServiceManager refuses, before the token is spent, a Linux user manager that would not find the unit.
func (h setupHost) checkServiceManager(ctx context.Context, p setupPaths) error {
	if h.goos != "linux" {
		return nil
	}
	out, _ := h.run(ctx, "systemctl", "--user", "show", "-p", "UnitPath", "--value")
	dir := filepath.Dir(p.unitFile)
	if !strings.Contains(" "+strings.TrimSpace(string(out))+" ", " "+dir+" ") {
		return fmt.Errorf("the systemd user manager does not search %s for units, so it could not find the service: no token was spent", dir)
	}
	return nil
}

// installBinary stops a running copy and replaces the installed binary with this one, atomically. It
// leaves a binary that is already the installed one alone, so setup also works run from there.
func (h setupHost) installBinary(ctx context.Context, dst string) error {
	if h.goos == "linux" {
		_, _ = h.run(ctx, "systemctl", "--user", "stop", serviceUnit)
	}
	if a, aerr := os.Stat(h.exe); aerr == nil {
		if b, berr := os.Stat(dst); berr == nil && os.SameFile(a, b) {
			return nil
		}
	}
	src, err := os.Open(h.exe)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(dst, 0o755, func(w io.Writer) error { _, err := io.Copy(w, src); return err }); err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

// writeAtomic writes a sibling temporary file and renames it over path, so a reader never sees half of it.
func writeAtomic(path string, mode os.FileMode, write func(io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = errors.Join(write(f), f.Chmod(mode)); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// startService writes the service definition and (re)starts it.
func (h setupHost) startService(ctx context.Context, plan setupPlan, out io.Writer) error {
	if h.goos == "darwin" {
		return h.startLaunchAgent(ctx, plan, out)
	}
	p := plan.paths
	env := fmt.Sprintf("WARDYN_RUNNER_ORG_URL=%s\nWARDYN_PROXY_IMAGE=%s\nWARDYN_INTERNAL_NETWORK=%s\nWARDYN_RUNNER_STATE_DIR=%s\n", plan.org, plan.proxyImage, plan.network, p.stateDir)
	if plan.relayListen != "" {
		env += "WARDYN_RELAY_LISTEN=" + plan.relayListen + "\n"
	}
	if plan.relayHostTarget != "" {
		env += "WARDYN_RELAY_HOST_TARGET=" + plan.relayHostTarget + "\n"
	}
	if plan.dockerHost != "" {
		env += "DOCKER_HOST=" + plan.dockerHost + "\n"
	}
	unit := strings.ReplaceAll(runnerunits.SystemdUnit, "__WARDYN_RUNNERD__", p.installed)
	for _, f := range []struct {
		path, text string
		mode       os.FileMode
	}{{p.envFile, env, 0o600}, {p.unitFile, unit, 0o644}} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(f.path, f.mode, func(w io.Writer) error { _, err := io.WriteString(w, f.text); return err }); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", serviceUnit}} {
		if b, err := h.run(ctx, "systemctl", append([]string{"--user"}, args...)...); err != nil {
			return fmt.Errorf("systemctl --user %s: %w: %s", args[0], err, strings.TrimSpace(string(b)))
		}
	}
	fmt.Fprintln(out, "Started the wardyn-runnerd user service. Logs: journalctl --user -u wardyn-runnerd")
	fmt.Fprintln(out, "To keep it running when you are logged out: loginctl enable-linger $USER")
	if h.xdgConfig != "" {
		fmt.Fprintf(out, "XDG_CONFIG_HOME is set here, so pass --state-dir %s to wardyn runner claim.\n", p.stateDir)
	}
	return nil
}

// plistComment is the template's documentation, which is not written into the user's LaunchAgents.
var plistComment = regexp.MustCompile(`(?s)<!--.*?-->\n`)

// renderLaunchAgent fills the embedded plist, dropping the DOCKER_HOST, WARDYN_RELAY_LISTEN and
// WARDYN_RELAY_HOST_TARGET keys when there is no value rather than setting them empty.
func renderLaunchAgent(plan setupPlan) string {
	tpl := plistComment.ReplaceAllString(runnerunits.LaunchAgent, "")
	for key, val := range map[string]string{"DOCKER_HOST": plan.dockerHost, "WARDYN_RELAY_LISTEN": plan.relayListen, "WARDYN_RELAY_HOST_TARGET": plan.relayHostTarget} {
		if val == "" {
			tpl = regexp.MustCompile(`[ \t]*<key>`+key+`</key>\n[ \t]*<string>[^<]*</string>\n`).ReplaceAllString(tpl, "")
		}
	}
	return strings.NewReplacer(
		"__WARDYN_RUNNERD__", html.EscapeString(plan.paths.installed),
		"__WARDYN_ORG_URL__", html.EscapeString(plan.org),
		"__WARDYN_PROXY_IMAGE__", html.EscapeString(plan.proxyImage),
		"__WARDYN_INTERNAL_NETWORK__", html.EscapeString(plan.network),
		"__DOCKER_HOST__", html.EscapeString(plan.dockerHost),
		"__RELAY_LISTEN__", html.EscapeString(plan.relayListen),
		"__RELAY_HOST_TARGET__", html.EscapeString(plan.relayHostTarget),
		"__WARDYN_LOG__", html.EscapeString(plan.paths.logFile),
	).Replace(tpl)
}

func (h setupHost) startLaunchAgent(ctx context.Context, plan setupPlan, out io.Writer) error {
	p := plan.paths
	for _, dir := range []string{filepath.Dir(p.plistFile), filepath.Dir(p.logFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := writeAtomic(p.plistFile, 0o644, func(w io.Writer) error { _, err := io.WriteString(w, renderLaunchAgent(plan)); return err }); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", h.uid)
	_, _ = h.run(ctx, "launchctl", "bootout", domain+"/"+serviceLabel)
	if b, err := h.run(ctx, "launchctl", "bootstrap", domain, p.plistFile); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(out, "Started the wardyn-runnerd LaunchAgent. Log: %s\n", p.logFile)
	return nil
}
