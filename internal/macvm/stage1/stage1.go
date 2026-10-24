// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package stage1 is the macOS base image's stage 1 (MAC-VM §3.4 step 7): over
// SSH to the provisioned guest it installs the guest agent, creates the agent
// user, turns on Screen Sharing, sets the guest's power, lock and update
// settings, and turns Remote Login off, last.
//
// The guest is reached only through BootstrapInput.Dial, the helper's own
// prep relay, which dials the fixed lease of the VM it just booted; there is
// no address to configure. Its host key is trusted on first use for this one
// boot and pinned in memory only, so nothing outlives the call. Every remote
// command is a fixed argv from the commands table; passwords and the one-time
// code go over stdin only, and never into an argv, an event or an error.
package stage1

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"golang.org/x/crypto/ssh"
)

// guestAddr labels the guest for SSH: the prep relay's fixed lease. It is
// never dialled; the connection comes from BootstrapInput.Dial.
const guestAddr = "10.0.2.15:22"

var (
	// ErrHostKeyChanged: the guest presented another host key than the one
	// trusted on this boot's first connection.
	ErrHostKeyChanged = errors.New("stage 1: the guest's SSH host key changed during this boot")
	// ErrRemoteLoginStillOn: after Remote Login was turned off, the guest
	// still answered on port 22.
	ErrRemoteLoginStillOn = errors.New("stage 1: the guest still answers on port 22 after Remote Login was turned off")
	// ErrUnsafePassword: a password or code holds a character that stdin's
	// line-based readers could split on.
	ErrUnsafePassword = errors.New("stage 1: a password holds a character other than letters, digits, '-' or '_'")
	// ErrAgentChecksum: the installed guest agent is not the file sent.
	ErrAgentChecksum = errors.New("stage 1: the installed guest agent's SHA-256 does not match")
)

// StepError is a failed stage-1 command. Its text names the step and the
// command, never a value sent to it.
type StepError struct {
	Step, Command string
	Err           error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("stage 1 step %s (%s): %v", e.Step, e.Command, e.Err)
}
func (e *StepError) Unwrap() error { return e.Err }

// Config is stage 1's fixed input.
type Config struct {
	// AgentPath is the darwin/arm64 wardyn-guest-agent to install.
	AgentPath    string
	AgentVersion string
	// OffWait bounds how long Remote Login may keep answering after it was
	// turned off; 0 is 30 s.
	OffWait time.Duration
}

// Bootstrapper is macvm.Bootstrapper over SSH.
type Bootstrapper struct{ cfg Config }

// New returns stage 1 for prepare's PrepareOptions.Bootstrap.
func New(c Config) *Bootstrapper {
	if c.OffWait == 0 {
		c.OffWait = 30 * time.Second
	}
	return &Bootstrapper{cfg: c}
}

// AgentPath is the guest agent stage 1 installs.
func (b *Bootstrapper) AgentPath() string { return b.cfg.AgentPath }

var stepNames = map[string]string{
	"6 D1(c)": "replacing the one-time code",
	"7a":      "installing the guest agent",
	"7b":      "creating the agent user",
	"7c":      "turning on Screen Sharing",
	"7d":      "setting power, screen lock and updates",
	"7e":      "turning Remote Login off",
}

// Bootstrap runs stage 1. Any failure stops it with a StepError or one of the
// errors above, and prepare then publishes no base.
func (b *Bootstrapper) Bootstrap(ctx context.Context, in macvm.BootstrapInput) (macvm.BootstrapResult, error) {
	agent, err := os.ReadFile(b.cfg.AgentPath)
	if err != nil {
		return macvm.BootstrapResult{}, &StepError{Step: "7a", Command: "read the guest agent", Err: err}
	}
	s, err := newSession(in, agent)
	if err != nil {
		return macvm.BootstrapResult{}, err
	}
	defer s.zero()
	if err := s.connect(ctx); err != nil {
		return macvm.BootstrapResult{}, err
	}
	defer func() { _ = s.client.Close() }()
	step := ""
	for i, c := range commands {
		if c.OnlyWithCode && s.code == nil {
			continue
		}
		if c.Step != step {
			step = c.Step
			s.emit(stepNames[step], float64(i)/float64(len(commands)))
		}
		if err := s.run(ctx, c); err != nil {
			return macvm.BootstrapResult{}, &StepError{Step: c.Step, Command: c.ID, Err: err}
		}
		if c.OnlyWithCode {
			if err := s.codeReplaced(ctx); err != nil {
				return macvm.BootstrapResult{}, err
			}
		}
	}
	_ = s.client.Close()
	if err := s.confirmOff(ctx, b.cfg.OffWait); err != nil {
		return macvm.BootstrapResult{}, err
	}
	s.emit("Remote Login is off", 1)
	return macvm.BootstrapResult{AgentVersion: b.cfg.AgentVersion}, nil
}

// session is one stage-1 run's state. Every secret is a byte slice zeroed at
// the end, best effort: copies the SSH library or Go strings made are not
// reachable from here.
type session struct {
	in       macvm.BootstrapInput
	username string
	admin    []byte // the admin password the guest has now
	code     []byte // the one-time code, on the D1(c) path only
	next     []byte // its replacement
	agentPw  []byte
	agent    []byte
	agentSHA string
	pin      []byte // the host key trusted on this boot's first connection
	client   *ssh.Client
}

func newSession(in macvm.BootstrapInput, agent []byte) (*session, error) {
	s := &session{in: in, username: in.Username, agent: agent}
	sum := sha256.Sum256(agent)
	s.agentSHA = hex.EncodeToString(sum[:])
	if !safeWord(in.Username) {
		return nil, errors.New("stage 1: the admin account name holds a character other than letters, digits, '-' or '_'")
	}
	pw, err := randomPassword()
	if err != nil {
		return nil, err
	}
	s.agentPw = pw
	s.admin = []byte(in.Password)
	if in.NewPassword != "" {
		s.code, s.next = []byte(in.Password), []byte(in.NewPassword)
	}
	for _, v := range [][]byte{s.admin, s.next} {
		if v != nil && !safeWord(string(v)) {
			return nil, ErrUnsafePassword
		}
	}
	return s, nil
}

func safeWord(v string) bool {
	return v != "" && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
}

func randomPassword() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	out := make([]byte, base64.RawURLEncoding.EncodedLen(len(b)))
	base64.RawURLEncoding.Encode(out, b)
	clear(b)
	return out, nil
}

func (s *session) emit(detail string, fraction float64) {
	if s.in.Progress != nil {
		s.in.Progress(macvm.PrepareEvent{Phase: macvm.PhaseBootstrap, Fraction: fraction, Detail: detail})
	}
}

// hostKey trusts the first key it sees on this boot and refuses any other.
func (s *session) hostKey(_ string, _ net.Addr, key ssh.PublicKey) error {
	if s.pin == nil {
		s.pin = key.Marshal()
		s.emit("guest host key "+ssh.FingerprintSHA256(key)+", trusted for this boot only", 0)
		return nil
	}
	if !bytes.Equal(s.pin, key.Marshal()) {
		return ErrHostKeyChanged
	}
	return nil
}

// connect opens the SSH connection as the admin account with its current
// password.
func (s *session) connect(ctx context.Context) error {
	conn, err := s.in.Dial(ctx, 22)
	if err != nil {
		return fmt.Errorf("stage 1: dial the guest's Remote Login: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cfg := &ssh.ClientConfig{
		User: s.username,
		Auth: []ssh.AuthMethod{
			ssh.PasswordCallback(func() (string, error) { return string(s.admin), nil }),
			// macOS's sshd may ask through PAM instead: the password answers
			// its hidden prompts. UNVERIFIED (external): which one it uses.
			ssh.KeyboardInteractive(func(_, _ string, qs []string, echos []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range qs {
					if !echos[i] {
						a[i] = string(s.admin)
					}
				}
				return a, nil
			}),
		},
		HostKeyCallback: s.hostKey,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, guestAddr, cfg)
	if err != nil {
		stop()
		_ = conn.Close()
		if errors.Is(err, ErrHostKeyChanged) {
			return ErrHostKeyChanged
		}
		return fmt.Errorf("stage 1: sign in to the guest as %s: %w", s.username, err)
	}
	_ = conn.SetDeadline(time.Time{})
	s.client = ssh.NewClient(c, chans, reqs)
	return nil
}

// codeReplaced runs right after the one-time code is replaced: the code file
// goes, and stage 1 signs in again with the new password, on the same pinned
// host key.
func (s *session) codeReplaced(ctx context.Context) error {
	clear(s.admin)
	s.admin, s.next = s.next, nil
	if s.in.Replaced != nil {
		s.in.Replaced()
	}
	s.emit("one-time code replaced", 0)
	_ = s.client.Close()
	return s.connect(ctx)
}

// stdin is c's stdin: sudo's password line first for a root command.
func (s *session) stdin(c command) []byte {
	var b bytes.Buffer
	if c.Root {
		b.Write(s.admin)
		b.WriteByte('\n')
	}
	switch c.In {
	case stdinRotate:
		fmt.Fprintf(&b, "passwd /Users/%s %s %s\n", s.username, s.code, s.next)
	case stdinAgent:
		b.Write(s.agent)
	case stdinPlist:
		b.WriteString(agentPlist)
	case stdinAgentPassword:
		b.Write(s.agentPw)
		b.WriteByte('\n')
	case stdinKCPassword:
		kc := kcpassword(s.agentPw)
		b.Write(kc)
		clear(kc)
	}
	return b.Bytes()
}

// run runs c once over its own SSH session.
func (s *session) run(ctx context.Context, c command) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return s.dropped(c, err)
	}
	defer func() { _ = sess.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	in := s.stdin(c)
	defer clear(in)
	var out, errb bytes.Buffer
	sess.Stdin, sess.Stdout, sess.Stderr = bytes.NewReader(in), &out, &errb
	err = sess.Run(c.line())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *ssh.ExitError
	switch {
	case errors.As(err, &exit):
		return fmt.Errorf("exited %d: %s", exit.ExitStatus(), s.scrub(errb.String()))
	case err != nil:
		return s.dropped(c, err)
	}
	if c.CheckAgentSHA && !strings.HasPrefix(out.String(), s.agentSHA) {
		return ErrAgentChecksum
	}
	return nil
}

// dropped is a session that ended with no exit status: a failure, except
// where the command may end the session itself.
func (s *session) dropped(c command, err error) error {
	if c.MayDropSession {
		return nil
	}
	return fmt.Errorf("the session ended: %w", err)
}

// scrub is a command's stderr, cut short, with every secret this run holds
// taken out.
func (s *session) scrub(text string) string {
	for _, v := range [][]byte{s.admin, s.code, s.next, s.agentPw} {
		if len(v) > 0 {
			text = strings.ReplaceAll(text, string(v), "[redacted]")
		}
	}
	text = strings.TrimSpace(text)
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

// confirmOff checks that a fresh dial of the guest's port 22 is refused,
// trying until wait passes.
func (s *session) confirmOff(ctx context.Context, wait time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		dctx, dcancel := context.WithTimeout(wctx, 5*time.Second)
		c, err := s.in.Dial(dctx, 22)
		dcancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		_ = c.Close()
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &StepError{Step: "7e", Command: "confirm Remote Login is off", Err: ErrRemoteLoginStillOn}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *session) zero() {
	for _, v := range [][]byte{s.admin, s.code, s.next, s.agentPw} {
		clear(v)
	}
}
