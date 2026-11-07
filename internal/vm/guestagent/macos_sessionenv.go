// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The window's GUI session and Terminal get the run's settings: the proxy, the CA bundle, the provider's
// sign-in files and placeholders, the git identity. They are the half of the run's environment that is
// public by contract (Init.Env); SecretEnv is never read here, so an env_secret value reaches neither the
// Terminal nor a GUI app. Two routes carry the settings, since each covers what the other misses:
//
//   - a shell file, written before Init answers and sourced through a hook in /etc/zshenv by the agent
//     account's zsh only (root's and the admin's zsh read /etc/zshenv too, and never need the run's proxy);
//   - launchd's user domain, set once the agent account is at the console (applySessionEnv).
//
// Neither ever fails the Init. Where the second stands is reported as SessionEnvStatus, so the host can
// hold the window until the settings are in, and say so when they could not be.

// The states of a run's settings in the GUI session.
const (
	SessionEnvPending = "pending" // the agent account is not at the console yet, or the setter is running
	SessionEnvSet     = "set"
	SessionEnvFailed  = "failed"

	// The reasons a failed state names.
	SessionEnvNotSignedIn  = "not_signed_in" // nobody signed in as the agent account within the wait
	SessionEnvSetenvFailed = "setenv_failed" // launchctl did not take the variables, as root or as the user
	sessionEnvViaNone      = "none"          // nothing to set
	sessionEnvViaRetry     = "retry"         // the person's own retry from the window's Terminal
	sessionEnvHookMarker   = "# Wardyn run settings"
	sessionEnvMaxFile      = 1 << 20
	sessionEnvCheckKey     = "WARDYN_RUN_ID" // the variable read back to prove the session holds the set
)

// Defaults of the console wait: the agent account may still be auto-logging in when Init arrives.
const (
	sessionEnvPollEvery = 2 * time.Second
	sessionEnvPollWait  = 10 * time.Minute
)

// SessionEnvStatus is where a run's settings stand in the GUI session. It holds counts and names, never a
// value.
type SessionEnvStatus struct {
	State  string
	Reason string // failed only
	Via    string // set only: asuser, user, retry or none
	Vars   int    // set only
}

// Line is the status file's text: one session_env= line, without the newline.
func (s SessionEnvStatus) Line() string {
	switch s.State {
	case SessionEnvSet:
		return fmt.Sprintf("session_env=set vars=%d via=%s", s.Vars, s.Via)
	case SessionEnvFailed:
		return "session_env=failed reason=" + s.Reason
	}
	return "session_env=pending"
}

// ParseSessionEnvStatus reads Line's text back; anything else is an error.
func ParseSessionEnvStatus(b []byte) (SessionEnvStatus, error) {
	f := strings.Fields(string(b))
	bad := fmt.Errorf("not a session_env status line: %q", strings.TrimSpace(string(b)))
	if len(f) == 0 || len(b) > 512 {
		return SessionEnvStatus{}, bad
	}
	state, ok := strings.CutPrefix(f[0], "session_env=")
	if !ok {
		return SessionEnvStatus{}, bad
	}
	st := SessionEnvStatus{State: state}
	seen := 0
	for _, kv := range f[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return SessionEnvStatus{}, bad
		}
		switch {
		case k == "vars" && state == SessionEnvSet:
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return SessionEnvStatus{}, bad
			}
			st.Vars = n
		case k == "via" && state == SessionEnvSet:
			st.Via = v
		case k == "reason" && state == SessionEnvFailed:
			st.Reason = v
		default:
			return SessionEnvStatus{}, bad
		}
		seen++
	}
	switch {
	case state == SessionEnvPending && seen == 0,
		state == SessionEnvSet && seen == 2 && st.Via != "",
		state == SessionEnvFailed && seen == 1:
		return st, nil
	}
	return SessionEnvStatus{}, bad
}

// SessionEnvPaths are the files the settings use. A zero field is the guest's own path (MacSessionEnvDir
// and the Mac* paths); a test names its own.
type SessionEnvPaths struct{ Dir, Script, Status, Zshenv string }

// DefaultSessionEnvPaths is the guest's own.
func DefaultSessionEnvPaths() SessionEnvPaths { return SessionEnvPaths{}.resolved() }

func (p SessionEnvPaths) resolved() SessionEnvPaths {
	p.Dir = cmp.Or(p.Dir, MacSessionEnvDir)
	p.Script = cmp.Or(p.Script, MacSessionEnvPath)
	p.Status = cmp.Or(p.Status, MacSessionEnvStatusPath)
	p.Zshenv = cmp.Or(p.Zshenv, MacZshEnvPath)
	return p
}

var (
	sessionEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// sessionEnvOwn are the session's own variables, which the run's never replace.
	sessionEnvOwn = []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TERM", "TMPDIR"}
	// sessionEnvStartup are variables a shell reads at start, and sessionEnvLoader the prefixes the dynamic
	// loader reads: through them the run would choose what a shell or an app loads, so none is passed on.
	sessionEnvStartup = []string{"ZDOTDIR", "ENV", "BASH_ENV", "FPATH", "PS4", "PROMPT_COMMAND"}
	sessionEnvLoader  = []string{"DYLD_", "LD_"}
)

// SessionEnv is the settings for the GUI session and the shell file, KEY=VALUE and sorted. It starts from
// env, the non-secret half, and never reads secret except to drop a key that appears in both (fail
// closed). When ca is set, env's CA variables give way to it, as initEnv does, so a Linux path never
// leaks into the GUI. Prepare inputs (keys ending _B64), the managed-files list, the session's own
// variables, shell and loader startup variables, a key that is not a shell name and a value with a NUL or a
// newline are left out.
func SessionEnv(env, secret map[string]string, ca []string) []string {
	var out []string
	for k, v := range env {
		if _, dup := secret[k]; dup ||
			(len(ca) > 0 && isCAKey(k)) ||
			strings.HasSuffix(k, "_B64") || k == EnvManagedFiles || slices.Contains(sessionEnvOwn, k) ||
			slices.Contains(sessionEnvStartup, k) || slices.ContainsFunc(sessionEnvLoader, func(pre string) bool { return strings.HasPrefix(k, pre) }) ||
			!sessionEnvKey.MatchString(k) || strings.ContainsAny(v, "\x00\n") {
			continue
		}
		out = append(out, k+"="+v)
	}
	out = append(out, ca...)
	slices.Sort(out)
	return out
}

// SessionEnvFile writes env as `export K='v'` lines to p.Script, root's at 0644 through replaceFile. Every
// zsh on the clone, root's and the admin's included, sources it, so the directory must be root's and
// nobody else's to change: it is made 0755 if absent, and refused when it, or any parent, is another
// user's or writable by group or others.
func SessionEnvFile(p SessionEnvPaths, env []string) error {
	p = p.resolved()
	if err := makeDirAll(p.Dir, 0o755, nil); err != nil {
		return fmt.Errorf("the settings directory: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(p.Dir, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o022 != 0 || (st.Uid != 0 && st.Uid != trustedOwnerUID) {
		return fmt.Errorf("the settings directory %s is not root's alone: %w", p.Dir, ErrUnsafeParent)
	}
	return replaceFile(p.Script, 0o644, func(f *os.File) error {
		var b bytes.Buffer
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			b.WriteString("export " + k + "='" + strings.ReplaceAll(v, "'", `'\''`) + "'\n")
		}
		_, err := f.Write(b.Bytes())
		return err
	})
}

// parseSessionEnvScript is SessionEnvFile's text back as KEY=VALUE.
func parseSessionEnvScript(b []byte) ([]string, error) {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		kv, ok := strings.CutPrefix(line, "export ")
		k, v, ok2 := strings.Cut(kv, "=")
		if !ok || !ok2 || !sessionEnvKey.MatchString(k) || len(v) < 2 || v[0] != '\'' || v[len(v)-1] != '\'' {
			return nil, errors.New("the settings file is not one this agent wrote")
		}
		out = append(out, k+"="+strings.ReplaceAll(v[1:len(v)-1], `'\''`, "'"))
	}
	return out, nil
}

// EnsureZshHook makes p.Zshenv source p.Script in agentUID's zsh, once: it creates the file at 0644, or
// appends the hook line to what is there, and does nothing when the line is already in. The line checks the
// shell's effective uid first, so every other account's zsh (root's, the admin's) never reads the script. A
// link, or anything but a regular file, is refused. It is written on the run's clone, never the base. With
// no agent account (uid 0, root's, as when the agent's ids are unknown) nothing is written.
func EnsureZshHook(p SessionEnvPaths, agentUID uint32) error {
	if agentUID == 0 {
		return errors.New("no agent account to source the settings for (uid 0 is root's)")
	}
	p = p.resolved()
	line := `[ "$EUID" = ` + strconv.FormatUint(uint64(agentUID), 10) + " ] && [ -r " + p.Script + " ] && . " + p.Script + " " + sessionEnvHookMarker
	created := false
	var st unix.Stat_t
	if err := unix.Lstat(p.Zshenv, &st); errors.Is(err, unix.ENOENT) {
		created = true
	}
	fd, err := unix.Open(p.Zshenv, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", p.Zshenv, err)
	}
	f := os.NewFile(uintptr(fd), p.Zshenv)
	defer f.Close()
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s is not a regular file", p.Zshenv)
	}
	if created {
		if err := f.Chmod(0o644); err != nil {
			return err
		}
	}
	cur, err := io.ReadAll(io.LimitReader(f, sessionEnvMaxFile))
	if err != nil {
		return err
	}
	if bytes.Contains(cur, []byte(line)) {
		return nil
	}
	add := line + "\n"
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		add = "\n" + add
	}
	_, err = f.WriteString(add)
	return err
}

// ReadSessionEnvStatus reads the status file.
func ReadSessionEnvStatus(p SessionEnvPaths) (SessionEnvStatus, error) {
	b, err := os.ReadFile(p.resolved().Status)
	if err != nil {
		return SessionEnvStatus{}, err
	}
	return ParseSessionEnvStatus(b)
}

// writeSessionEnvStatus replaces the status file, root's directory and the agent account's file (so the
// person's retry can update it in place; it holds a state, never a value).
func writeSessionEnvStatus(p SessionEnvPaths, st SessionEnvStatus, owner uint32) error {
	return replaceFile(p.resolved().Status, 0o644, func(f *os.File) error {
		_ = f.Chown(int(owner), -1)
		_, err := f.WriteString(st.Line() + "\n")
		return err
	})
}

// updateSessionEnvStatus rewrites the existing status file in place, as the agent account does.
func updateSessionEnvStatus(p SessionEnvPaths, st SessionEnvStatus) error {
	f, err := os.OpenFile(p.resolved().Status, os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(st.Line() + "\n")
	return errors.Join(werr, f.Close())
}

// StartSessionEnv gives the run's settings to the shells and the GUI session. Before it returns, the
// shell file and the zsh hook are written and the status says pending; the returned channel closes when
// the GUI step is over (set, failed, or stopped by ctx). It never fails the Init: a step that fails is
// logged and the next goes on. env and secret are Init's; only env is read for values.
func (d *Desktop) StartSessionEnv(ctx context.Context, p SessionEnvPaths, agentUID uint32, env, secret map[string]string, ca []string) <-chan struct{} {
	list := SessionEnv(env, secret, ca)
	if err := SessionEnvFile(p, list); err != nil {
		slog.Warn("guest agent: the run's settings are not in the shell file", "err", err)
	} else if err := EnsureZshHook(p, agentUID); err != nil {
		slog.Warn("guest agent: the zsh hook for the run's settings is not in place", "err", err)
	}
	if err := writeSessionEnvStatus(p, SessionEnvStatus{State: SessionEnvPending}, agentUID); err != nil {
		slog.Warn("guest agent: the settings status is not written", "err", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.applySessionEnv(ctx, p, agentUID, list, cmp.Or(d.SessionEnvEvery, sessionEnvPollEvery), cmp.Or(d.SessionEnvWait, sessionEnvPollWait))
	}()
	return done
}

// applySessionEnv waits for the agent account to be at the console, sets env in its GUI session, and
// records the outcome in the status file. wait bounds the whole step, the set included: a launchctl that
// never returns ends it failed (setenv_failed), so the status never stays pending past the host's own wait.
// A ctx that ends first (the stop scrub) leaves the status as it was.
func (d *Desktop) applySessionEnv(ctx context.Context, p SessionEnvPaths, agentUID uint32, env []string, every, wait time.Duration) SessionEnvStatus {
	pending := SessionEnvStatus{State: SessionEnvPending}
	finish := func(st SessionEnvStatus) SessionEnvStatus {
		if err := writeSessionEnvStatus(p, st, agentUID); err != nil {
			slog.Warn("guest agent: the settings status is not written", "err", err)
		}
		return st
	}
	if len(env) == 0 {
		return finish(SessionEnvStatus{State: SessionEnvSet, Via: sessionEnvViaNone})
	}
	step, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	type result struct {
		via string
		err error
	}
	var r result
	for r.err = ErrConsoleChanged; errors.Is(r.err, ErrConsoleChanged); {
		for {
			if ctx.Err() != nil {
				return pending
			}
			if uid, err := d.Console.Console(step); err == nil && uid == agentUID {
				break
			}
			select {
			case <-ctx.Done():
				return pending
			case <-step.Done():
				return finish(SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvNotSignedIn})
			case <-time.After(every):
			}
		}
		// The setter runs on its own, so one that ignores step's end still cannot hold the status. It sets
		// only in agentUID's session; a console that changed hands since (ErrConsoleChanged) waits again.
		res := make(chan result, 1)
		go func() {
			via, err := d.SetSessionEnv(step, agentUID, env)
			res <- result{via, err}
		}()
		select {
		case r = <-res:
		case <-step.Done():
			r.err = fmt.Errorf("the desktop session did not take the settings within %v: %w", wait, step.Err())
		}
	}
	switch {
	case ctx.Err() != nil:
		return pending
	case errors.Is(r.err, ErrNoConsoleUser):
		return finish(SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvNotSignedIn})
	case r.err != nil:
		slog.Warn("guest agent: the run's settings are not in the desktop session", "err", r.err)
		return finish(SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvSetenvFailed})
	}
	return finish(SessionEnvStatus{State: SessionEnvSet, Vars: len(env), Via: r.via})
}

// SessionEnvRetry is the retry a person runs from the window's Terminal (agent-run --session-env-retry):
// it reapplies the shell file's settings in their own session, with no asuser, and records the outcome.
func SessionEnvRetry(ctx context.Context, p SessionEnvPaths, run CmdRunner) (SessionEnvStatus, error) {
	p = p.resolved()
	b, err := os.ReadFile(p.Script)
	if err != nil {
		return SessionEnvStatus{}, err
	}
	env, err := parseSessionEnvScript(b)
	if err != nil {
		return SessionEnvStatus{}, err
	}
	failed := SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvSetenvFailed}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, err := run.Run(ctx, "/bin/launchctl", "setenv", k, v); err != nil {
			_ = updateSessionEnvStatus(p, failed)
			return failed, fmt.Errorf("set %s: %w", k, err)
		}
	}
	if key, want := sessionEnvCheck(env); key != "" {
		out, err := run.Run(ctx, "/bin/launchctl", "getenv", key)
		if err != nil || strings.TrimSuffix(string(out), "\n") != want {
			_ = updateSessionEnvStatus(p, failed)
			return failed, fmt.Errorf("the session does not hold %s", key)
		}
	}
	st := SessionEnvStatus{State: SessionEnvSet, Vars: len(env), Via: sessionEnvViaRetry}
	return st, updateSessionEnvStatus(p, st)
}

// sessionEnvCheck is the one variable read back after a set: the run id when there, else the first.
func sessionEnvCheck(env []string) (key, want string) {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == sessionEnvCheckKey {
			return k, v
		}
	}
	if len(env) == 0 {
		return "", ""
	}
	k, v, _ := strings.Cut(env[0], "=")
	return k, v
}
