// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// startConfig is what wardynd hands the person's stack on stdin. It is never
// on a command line (the agent can read every /proc/<pid>/cmdline) and never
// in an environment.
type startConfig struct {
	// Credential is the sign-in lane credential: 32 random bytes, base64url.
	Credential string `json:"credential"`
	// Proxy is the run proxy's sign-in listener, host:port.
	Proxy string `json:"proxy"`
	// URL is the page the person's browser opens on, "" for a blank one.
	URL string `json:"url,omitempty"`
}

func (c startConfig) validate() error {
	if b, err := base64.RawURLEncoding.DecodeString(c.Credential); err != nil || len(b) != 32 {
		return errors.New("the sign-in credential is malformed")
	}
	if _, _, err := net.SplitHostPort(c.Proxy); err != nil {
		return errors.New("the sign-in proxy must be host:port")
	}
	if c.URL != "" && !strings.HasPrefix(c.URL, "https://") {
		return errors.New("the start page must be an https URL")
	}
	return nil
}

// readyWithin bounds how long start waits for the display to come up.
const readyWithin = 45 * time.Second

// cmdStart starts the person's sign-in display, detached, and returns once it
// answers. As the person.
func cmdStart(in io.Reader) error {
	if err := checkPerson(); err != nil {
		return err
	}
	var cfg startConfig
	if err := json.NewDecoder(io.LimitReader(in, 4096)).Decode(&cfg); err != nil {
		return fmt.Errorf("read the start config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if _, err := ctlCall(ctlSock, "Browser.getVersion", nil); err == nil {
		return errors.New("a sign-in display is already running")
	}
	if err := mkdirs(); err != nil {
		return err
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(credPath, b, 0o600); err != nil {
		return err
	}
	logf, err := os.OpenFile(serveLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "serve")
	cmd.Env = personEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(readyWithin)
	for {
		if _, err := os.Stat(wsSock); err == nil {
			if _, err := ctlCall(ctlSock, "Browser.getVersion", nil); err == nil {
				return nil
			}
		}
		select {
		case err := <-exited:
			return fmt.Errorf("the sign-in display stopped while starting (%v): %s", err, logTail())
		case <-deadline:
			return fmt.Errorf("the sign-in display did not come up within %s: %s", readyWithin, logTail())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func logTail() string {
	b, _ := os.ReadFile(serveLog)
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	return strings.TrimSpace(string(b))
}

// personEnv is the whole environment of every person-side process: nothing
// inherited from the container (whose environment is the agent's).
func personEnv() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + homeDir,
		"TMPDIR=" + tmpDir,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"LANG=C.UTF-8",
		"DBUS_SESSION_BUS_ADDRESS=disabled:",
		"NO_AT_BRIDGE=1",
	}
}

// cmdServe runs the person's display until the browser exits: the forwarder,
// cage (headless wlroots, pixman), wayvnc and websockify on unix sockets in
// the store, and the browser on its devtools pipe.
func cmdServe() error {
	if err := checkPerson(); err != nil {
		return err
	}
	b, err := os.ReadFile(credPath)
	if err != nil {
		return err
	}
	var cfg startConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	fwd, err := newForwarder(cfg.Proxy, cfg.Credential)
	if err != nil {
		return err
	}
	go fwd.serve(personUID)

	var procs []*exec.Cmd
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
		}
	}()
	launch := func(name string, args []string, env []string, extra ...*os.File) (*exec.Cmd, error) {
		cmd := exec.Command(name, args...)
		cmd.Env = append(personEnv(), env...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		cmd.ExtraFiles = extra
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		procs = append(procs, cmd)
		return cmd, nil
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(displayPath)
	if _, err := launch("cage", []string{"--", self, "hold"},
		[]string{"WLR_BACKENDS=headless", "WLR_RENDERER=pixman", "WLR_LIBINPUT_NO_DEVICES=1"}); err != nil {
		return err
	}
	display, err := waitFile(displayPath, 15*time.Second)
	if err != nil {
		return errors.New("the compositor did not start")
	}
	wl := "WAYLAND_DISPLAY=" + display
	_ = os.Remove(vncSock)
	if _, err := launch("wayvnc", []string{"--unix-socket", vncSock}, []string{wl}); err != nil {
		return err
	}
	if _, err := waitFile(vncSock, 15*time.Second); err != nil {
		return errors.New("the VNC server did not start")
	}
	_ = os.Remove(wsSock)
	if _, err := launch("/usr/local/lib/websockify/run", []string{"--web", "/usr/local/lib/novnc",
		"--unix-listen=" + wsSock, "--unix-target=" + vncSock}, nil); err != nil {
		return err
	}

	// The devtools pipe: the browser reads fd 3 (r3) and writes fd 4 (w4).
	r3, w3, err := os.Pipe()
	if err != nil {
		return err
	}
	r4, w4, err := os.Pipe()
	if err != nil {
		return err
	}
	args := append(browserFlags(profileDir), "--ozone-platform=wayland",
		"--proxy-server=http://127.0.0.1:"+strconv.Itoa(fwd.port()),
		// Loopback too goes through the sign-in lane, where the proxy refuses it.
		"--proxy-bypass-list=<-loopback>")
	args = append(args, cmpOr(cfg.URL, "about:blank"))
	browser := exec.Command("chromium", args...)
	browser.Env = append(personEnv(), wl)
	// The browser's own output can carry page URLs: it goes nowhere.
	browser.Stdout, browser.Stderr = nil, nil
	browser.ExtraFiles = []*os.File{r3, w4}
	if err := browser.Start(); err != nil {
		return fmt.Errorf("start the browser: %w", err)
	}
	procs = append(procs, browser)
	_ = r3.Close()
	_ = w4.Close()
	c := newCDP(w3, r4)
	ln, err := listenCtl(ctlSock)
	if err != nil {
		return err
	}
	defer ln.Close()
	go serveCtl(ln, c)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	exited := make(chan error, 1)
	go func() { exited <- browser.Wait() }()
	fmt.Fprintln(os.Stderr, "wardyn-signin: the sign-in display is up")
	select {
	case <-exited:
	case <-sigs:
		_, _ = c.call("Browser.close", nil)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

// browserFlags are the flags both browsers share. --no-sandbox: the
// container's seccomp profile refuses the user namespaces Chromium's sandbox
// needs, and no-new-privileges refuses its setuid helper. Never a debugging
// port: loopback is shared.
func browserFlags(profile string) []string {
	return []string{
		"--user-data-dir=" + profile,
		"--remote-debugging-pipe",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--password-store=basic",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-sync",
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// waitFile waits for path to exist and returns its contents (trimmed).
func waitFile(path string, within time.Duration) (string, error) {
	deadline := time.Now().Add(within)
	for {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b)), nil
		} else if fi, serr := os.Stat(path); serr == nil && fi.Mode()&os.ModeSocket != 0 {
			return "", nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not appear", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cmdHold is cage's application: it records the display name and keeps the
// compositor up; the browser is serve's child so serve holds its devtools pipe.
func cmdHold() error {
	if err := checkPerson(); err != nil {
		return err
	}
	if err := os.WriteFile(displayPath, []byte(os.Getenv("WAYLAND_DISPLAY")), 0o600); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	<-sigs
	return nil
}

// stopWithin bounds how long stop waits for the person's processes to go.
const stopWithin = 5 * time.Second

// cmdStop closes the person's browser (so its cookies reach the profile),
// kills every other process of the person's uid, and proves none is left. The
// profile stays in the store until the machine ends. As the person.
func cmdStop() error {
	if err := checkPerson(); err != nil {
		return err
	}
	_, _ = ctlCall(ctlSock, "Browser.close", nil)
	self := os.Getpid()
	deadline := time.Now().Add(stopWithin)
	for browserRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(credPath)
	deadline = time.Now().Add(stopWithin)
	for {
		if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("end the sign-in display: %w", err)
		}
		if len(alive(listTasks("/proc"), personUID, self)) == 0 {
			for _, s := range []string{ctlSock, wsSock, vncSock} {
				_ = os.Remove(s)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the sign-in display's processes did not end")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// browserRunning reports whether the person's browser process is still up.
func browserRunning() bool {
	for _, t := range listTasks("/proc") {
		if t.ruid != personUID || t.tid != t.pid {
			continue
		}
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(t.pid) + "/comm"); err == nil && strings.HasPrefix(string(b), "chromium") {
			return true
		}
	}
	return false
}

// alive lists uid's live (not zombie) pids other than self.
func alive(tasks []task, uid, self int) []int {
	var out []int
	for _, t := range tasks {
		if t.pid != self && t.tid == t.pid && signalled(t, uid) && t.state != 'Z' && t.state != 'X' && t.state != 'x' {
			out = append(out, t.pid)
		}
	}
	return out
}
