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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	runnerunits "github.com/cjohnstoniv/wardyn/deploy/runner"
)

// defaultDockerSocket is where the Docker SDK connects when DOCKER_HOST is unset. Setup writes the engine it
// checked into the service every time, this included, so the service never has to look at the person's docker
// CLI context: a later `docker context use` cannot move it to another engine or stop it starting.
const defaultDockerSocket = "unix:///var/run/docker.sock"

const (
	serviceLabel = "com.wardyn.runnerd"
	// managedServiceLabel is the agent the macOS package installs; it must not run beside the setup agent.
	managedServiceLabel = serviceLabel + ".managed"
	serviceUnit         = "wardyn-runnerd.service"
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
	// engine checks that the engine answers as this user and the proxy bridge exists (creating it when
	// missing); relay then checks that a probe container reaches the relay pair serve will use
	// (findRelay), narrowed by want, with the proxy image serve will run.
	engine func(ctx context.Context, network string, want relayPair) (dockerEngine, error)
	relay  func(ctx context.Context, image, network string, eng dockerEngine, want relayPair) (relayPair, error)
}

func osSetupHost() (setupHost, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return setupHost{}, fmt.Errorf("unsupported OS: %s (on Windows, run setup inside WSL2)", runtime.GOOS)
	}
	if err := checkUntransportedDockerEnv(os.Getenv); err != nil {
		return setupHost{}, err
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
		dockerHost: cmp.Or(os.Getenv("DOCKER_HOST"), defaultDockerSocket), uid: os.Geteuid(), exe: exe, run: runCommand, engine: readySetupEngine, relay: probeSetupRelay}, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// setupPaths are where setup puts things.
type setupPaths struct {
	installed, stateDir, envFile, unitFile, plistFile, logFile string
	// cliProfile is the directory of the CLI profile the wardyn command reads: wherever os.UserConfigDir puts it
	// for this person.
	cliProfile string
}

func (p setupPaths) identity() string { return filepath.Join(p.stateDir, "runner.json") }

// configFile is runnerd.json, where every setting the service runs with lives; the service definition
// names only this path and the binary.
func (p setupPaths) configFile() string { return filepath.Join(p.cliProfile, runnerConfigName) }

// keptStateDir is the identity directory the service's own record names: runnerd.json, else the
// environment file or LaunchAgent a setup before it wrote.
func (p setupPaths) keptStateDir() string {
	f, _ := loadRunnerFile(p.configFile(), false)
	return cmp.Or(f.StateDir, p.setting("WARDYN_RUNNER_STATE_DIR"))
}

// paths: the binary under the data directory; on Linux the env file under $HOME/.config, where the unit's
// EnvironmentFile looks, and the unit where checkServiceManager finds the user manager looks. The identity
// is the one the service's own record (the env file an earlier setup wrote) names; without one, the one of
// $HOME/.config/wardyn/runner and $XDG_CONFIG_HOME/wardyn/runner that holds an identity ($HOME when
// neither does). Both holding one with no record is refused before anything is stopped or written: setup
// never guesses which identity the service runs.
func (h setupHost) paths() (setupPaths, error) {
	switch h.goos {
	case "linux":
		data := filepath.Join(cmp.Or(h.xdgData, filepath.Join(h.home, ".local", "share")), "wardyn")
		cfg := filepath.Join(h.home, ".config")
		p := setupPaths{installed: filepath.Join(data, "bin", "wardyn-runnerd"), stateDir: filepath.Join(cfg, "wardyn", "runner"),
			envFile: filepath.Join(cfg, "wardyn", "runnerd.env"), unitFile: filepath.Join(cfg, "systemd", "user", serviceUnit),
			cliProfile: filepath.Join(cmp.Or(h.xdgConfig, cfg), "wardyn")}
		holds := func(dir string) bool { _, err := os.Stat(filepath.Join(dir, "runner.json")); return err == nil }
		if dir := p.keptStateDir(); filepath.IsAbs(dir) && holds(dir) {
			p.stateDir = dir
			return p, nil
		}
		if h.xdgConfig == "" {
			return p, nil
		}
		switch xdg := filepath.Join(h.xdgConfig, "wardyn", "runner"); {
		case holds(xdg) && holds(p.stateDir) && xdg != p.stateDir:
			return setupPaths{}, fmt.Errorf("two runner identities are on this computer, in %s and in %s, and no earlier setup recorded "+
				"which one the service runs: move the one this computer should not use out of the way and run setup again "+
				"(nothing was changed and no token was spent)", p.stateDir, xdg)
		case holds(xdg):
			p.stateDir = xdg
		}
		return p, nil
	case "darwin":
		data := filepath.Join(h.home, "Library", "Application Support", "wardyn")
		return setupPaths{installed: filepath.Join(data, "bin", "wardyn-runnerd"), stateDir: filepath.Join(data, "runner"),
			plistFile: filepath.Join(h.home, "Library", "LaunchAgents", serviceLabel+".plist"), logFile: filepath.Join(h.home, "Library", "Logs", "wardyn", "runnerd.log"),
			cliProfile: data}, nil
	}
	return setupPaths{}, fmt.Errorf("unsupported OS: %s", h.goos)
}

// checkServiceManager sets the unit's place on Linux from the user manager's own search path, never from
// the shell's environment, which the manager need not share: of the user configuration directories
// $XDG_CONFIG_HOME/systemd/user and $HOME/.config/systemd/user, the first in UnitPath that already holds
// the unit (the one the manager loads, rewritten in place), else the first in UnitPath. A manager that
// searches neither is refused before the token is spent.
func (h setupHost) checkServiceManager(ctx context.Context, p *setupPaths) error {
	if h.goos != "linux" {
		return nil
	}
	out, err := h.run(ctx, "systemctl", "--user", "show", "-p", "UnitPath", "--value")
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		return fmt.Errorf("the systemd user manager did not answer (%w), so setup cannot start the service: no token was spent", err)
	}
	candidates := []string{filepath.Join(h.home, ".config", "systemd", "user")}
	if xdg := filepath.Join(h.xdgConfig, "systemd", "user"); h.xdgConfig != "" && xdg != candidates[0] {
		candidates = append([]string{xdg}, candidates...)
	}
	dir := ""
	for _, d := range strings.Fields(string(out)) {
		if !slices.Contains(candidates, d) {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, serviceUnit)); err == nil {
			dir = d
			break
		}
		dir = cmp.Or(dir, d)
	}
	if dir == "" {
		return fmt.Errorf("the systemd user manager does not search %s for units, so it could not find the service: run setup with "+
			"XDG_CONFIG_HOME set as the user manager has it (systemctl --user show-environment): no token was spent", strings.Join(candidates, " or "))
	}
	p.unitFile = filepath.Join(dir, serviceUnit)
	return nil
}

// installBinary stops a running copy and replaces the installed binary with this one, atomically. It
// leaves a binary that is already the installed one alone, so setup also works run from there.
func (h setupHost) installBinary(ctx context.Context, dst string, stopService bool) error {
	if stopService && h.goos == "linux" {
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

// plainShellWord is what needs no quoting in a shell or in systemd's EnvironmentFile; every setting a Linux
// unit carries passed safeSetting, which is a subset.
var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=,-]+$`)

// shellQuote is s as one shell word: unchanged when plain, else in single quotes. A macOS path holds a space
// ("Application Support"), which a bare KEY=value line or command argument would split.
func shellQuote(s string) string {
	if plainShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellUnquote undoes shellQuote for a value read back from an environment file.
func shellUnquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}

// runnerFile is runnerd.json as setup settled it: what setup owns, over what the file already held (the
// mirror, the proxies and the recording store are edited by hand and kept). The image is written only when
// someone named it: an image that follows the organisation or this release is derived again at every start,
// so replacing the binary alone moves the proxy with it.
func (p setupPlan) runnerFile() runnerFile {
	f := p.existing
	f.OrgURL, f.StateDir, f.InternalNetwork, f.DockerHost = p.org, p.paths.stateDir, p.network, p.dockerHost
	f.ProxyImage = ""
	if p.flagged {
		f.ProxyImage = p.proxyImage
	}
	f.RelayListen, f.RelayHostTarget, f.TrustedCAFile = p.relayListen, p.relayHostTarget, p.trustedCAFile
	return f
}

// dropLegacyEnv removes the environment file an earlier setup wrote for the service, or --no-service wrote:
// its settings are in runnerd.json now, and a stale copy would bring an old pin back at the next rerun.
func (p setupPaths) dropLegacyEnv() error {
	if err := os.Remove(p.noServiceEnvFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// renderSystemdUnit fills the embedded unit: the binary and the config path, and nothing else.
func renderSystemdUnit(p setupPaths) string {
	return strings.NewReplacer("__WARDYN_RUNNERD__", systemdQuote(p.installed), "__WARDYN_CONFIG__", systemdQuote(p.configFile())).Replace(runnerunits.SystemdUnit)
}

// startService writes the service definition and (re)starts it.
func (h setupHost) startService(ctx context.Context, plan setupPlan, out io.Writer) error {
	if h.goos == "darwin" {
		return h.startLaunchAgent(ctx, plan, out)
	}
	p := plan.paths
	if err := writeRunnerFile(p.configFile(), plan.runnerFile()); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.unitFile), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(p.unitFile, 0o644, func(w io.Writer) error { _, err := io.WriteString(w, renderSystemdUnit(p)); return err }); err != nil {
		return err
	}
	if err := p.dropLegacyEnv(); err != nil {
		return err
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

// installedService is the unit or LaunchAgent file already installed, or "".
func (p setupPaths) installedService() string {
	for _, f := range []string{p.unitFile, p.plistFile} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}
	return ""
}

// noServiceEnvFile is the environment file a Linux service read before runnerd.json, and on macOS, which
// had no such file, the one --no-service wrote beside the identity's directory.
func (p setupPaths) noServiceEnvFile() string {
	return cmp.Or(p.envFile, filepath.Join(filepath.Dir(p.stateDir), "runnerd.env"))
}

// writeRunConfig is --no-service: it writes runnerd.json and prints the command that runs the runner in
// the foreground, and installs no unit, plist or login item.
func (h setupHost) writeRunConfig(plan setupPlan, out io.Writer) error {
	path := plan.paths.configFile()
	if err := writeRunnerFile(path, plan.runnerFile()); err != nil {
		return err
	}
	if err := plan.paths.dropLegacyEnv(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s. No service was installed. Run the runner yourself, from a shell that keeps it running:\n  %s serve --config %s\n",
		path, shellQuote(plan.paths.installed), shellQuote(path))
	return nil
}

// systemdQuote renders s as one double-quoted systemd command-line word: a backslash or quote is
// escaped, and % (specifier) and $ (variable) are doubled, so a path with a space or % stays one argument.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// pinnedImage is the proxy image the service definition names, or "" when setup only derived this
// binary's release image: serve then picks its own release image, so replacing the binary alone
// moves the proxy with it. An image the person or the organisation named is always written, even
// when it equals the release image.
func (p setupPlan) pinnedImage() string {
	if !p.flagged && !p.fromIdentity && p.proxyImage == releaseProxyImage() {
		return ""
	}
	return p.proxyImage
}

// pinMarker is "1" when the image was named with --proxy-image (now or to an earlier setup). setup
// writes it as WARDYN_PROXY_IMAGE_PINNED beside the image, so a rerun can tell a pin from a derived
// default by provenance rather than by the image's name (DR-0202). serve ignores it.
func (p setupPlan) pinMarker() string {
	if p.flagged && p.pinnedImage() != "" {
		return "1"
	}
	return ""
}

// keptPins reads the proxy image and CA file an earlier setup wrote, so setup run again without
// --proxy-image or --trusted-ca-file keeps them: from runnerd.json, else from the service definition a
// setup before it wrote. An unmarked release image of this project in that definition is what a setup
// before 0.9.3 derived, not a pin, and is dropped.
func (p setupPaths) keptPins(file runnerFile) (image, caFile string) {
	if image = file.ProxyImage; image == "" {
		image = p.setting("WARDYN_PROXY_IMAGE")
		if tag, ok := strings.CutPrefix(image, releaseProxyRepo+":"); ok && releaseVersion.MatchString(tag) && p.setting("WARDYN_PROXY_IMAGE_PINNED") != "1" {
			image = ""
		}
	}
	return image, cmp.Or(file.TrustedCAFile, p.setting("WARDYN_TRUSTED_CA_FILE"))
}

// setting is the value a setup before runnerd.json wrote for key into the service definition, "" when there
// is none. The source is the one the service actually used: a Linux unit's environment file, or on macOS the
// LaunchAgent once one is installed. The environment file --no-service wrote there is read only while no
// LaunchAgent exists, so an older one never outranks a newer plist (or mixes with it).
func (p setupPaths) setting(key string) string {
	if p.plistFile != "" {
		if plist, err := os.ReadFile(p.plistFile); err == nil {
			if m := regexp.MustCompile(`<key>` + key + `</key>\s*<string>([^<]*)</string>`).FindSubmatch(plist); m != nil {
				return html.UnescapeString(strings.TrimSpace(string(m[1])))
			}
			return ""
		}
	}
	b, _ := os.ReadFile(p.noServiceEnvFile())
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return shellUnquote(strings.TrimSpace(v))
		}
	}
	return ""
}

// plistComment is the template's documentation, which is not written into the user's LaunchAgents.
var plistComment = regexp.MustCompile(`(?s)<!--.*?-->\n`)

// renderLaunchAgent fills the embedded plist. It carries the binary, the config path and the log path;
// every setting is in runnerd.json, which only its owner can read, not in a plist every local account can.
func renderLaunchAgent(plan setupPlan) string {
	tpl := plistComment.ReplaceAllString(runnerunits.LaunchAgent, "")
	return strings.NewReplacer(
		"__WARDYN_RUNNERD__", html.EscapeString(plan.paths.installed),
		"__WARDYN_CONFIG__", html.EscapeString(plan.paths.configFile()),
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
	if err := writeRunnerFile(p.configFile(), plan.runnerFile()); err != nil {
		return err
	}
	if err := writeAtomic(p.plistFile, 0o644, func(w io.Writer) error { _, err := io.WriteString(w, renderLaunchAgent(plan)); return err }); err != nil {
		return err
	}
	// An environment file an earlier --no-service wrote is stale once a LaunchAgent exists: delete the
	// plist later and a --no-service rerun would read its old pins back.
	if err := p.dropLegacyEnv(); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", h.uid)
	_, _ = h.run(ctx, "launchctl", "bootout", domain+"/"+managedServiceLabel)
	_, _ = h.run(ctx, "launchctl", "bootout", domain+"/"+serviceLabel)
	if b, err := h.run(ctx, "launchctl", "bootstrap", domain, p.plistFile); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(out, "Started the wardyn-runnerd LaunchAgent. Log: %s\n", p.logFile)
	return nil
}
