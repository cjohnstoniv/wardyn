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
// command is a fixed argv from the commands table; passwords, the one-time
// code and the base's pairing key go over stdin only, and never into an argv,
// an event or an error.
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
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"golang.org/x/crypto/ssh"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// guestAddr labels the guest for SSH: the prep relay's fixed lease. It is
// never dialled; the connection comes from BootstrapInput.Dial.
const guestAddr = "10.0.2.15:22"

var (
	// ErrHostKeyChanged: the guest presented another host key than the one
	// trusted on this boot's first connection.
	ErrHostKeyChanged = errors.New("stage 1: the guest's SSH host key changed during this boot")
	// ErrRemoteLoginStillOn: after Remote Login was turned off, no fresh dial
	// of the guest's port 22 was refused: one connected, or none got an
	// answer at all.
	ErrRemoteLoginStillOn = errors.New("stage 1: the guest's port 22 did not refuse a connection after Remote Login was turned off")
	// ErrGuestLivenessUnknown: stage 1 could not show that the guest it
	// turned Remote Login off in is the one that refused port 22: the VM
	// stopped, its boot epoch changed (it asked for its lease again), or the
	// helper cannot read the epoch. Unknown fails closed.
	ErrGuestLivenessUnknown = errors.New("stage 1: could not show that the guest stayed up, on the same boot, while Remote Login was turned off and checked")
	// ErrUnsafePassword: a password or code holds a character that stdin's
	// line-based readers could split on.
	ErrUnsafePassword = errors.New("stage 1: a password holds a character other than letters, digits, '-' or '_'")
	// ErrUnexpectedAccounts: the guest's local users of UniqueID 500 or more are not exactly the admin and the
	// agent, or the list could not be read as dscl prints it.
	ErrUnexpectedAccounts = errors.New("stage 1: the guest's local accounts are not exactly the admin and the agent")
	// ErrSignInRefused: the guest answered, but refused the admin account's password.
	ErrSignInRefused = macvm.ErrSignInRefused
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
	for i, c := range commands {
		if c.OnlyWithCode && s.code == nil {
			continue
		}
		if c.Event != "" {
			s.emit(c.Event, float64(i)/float64(len(commands)))
		}
		if c.MayDropSession {
			if err := s.beforeOff(ctx, b.cfg.OffWait); err != nil {
				return macvm.BootstrapResult{}, err
			}
		}
		if c.SkipIfOK != nil && s.run(ctx, command{ID: c.ID + "-probe", Step: c.Step, Root: c.Root, Argv: c.SkipIfOK}) == nil {
			continue
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
	pairKey  []byte // the base's pairing key: raw bytes for dd, so never through safeWord
	agent    []byte
	agentSHA string
	pin      []byte // the host key trusted on this boot's first connection
	client   *ssh.Client
	epoch    uint64 // the guest's boot epoch just before Remote Login off
}

// guestEpoch is the guest's boot epoch (BootstrapInput.GuestEpoch); any
// failure to read it is ErrGuestLivenessUnknown.
func (s *session) guestEpoch() (uint64, error) {
	if s.in.GuestEpoch == nil {
		return 0, ErrGuestLivenessUnknown
	}
	e, err := s.in.GuestEpoch()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrGuestLivenessUnknown, err)
	}
	return e, nil
}

// sameBoot says whether the guest is still on the boot whose epoch was taken
// before Remote Login off.
func (s *session) sameBoot() error {
	e, err := s.guestEpoch()
	if err != nil {
		return err
	}
	if e != s.epoch {
		return fmt.Errorf("%w: the guest asked for its network lease again", ErrGuestLivenessUnknown)
	}
	return nil
}

func newSession(in macvm.BootstrapInput, agent []byte) (*session, error) {
	if len(in.PairKey) != guestagent.PairKeyLen {
		return nil, fmt.Errorf("stage 1: prepare gave no %d-byte pairing key", guestagent.PairKeyLen)
	}
	s := &session{in: in, username: in.Username, agent: agent, pairKey: bytes.Clone(in.PairKey)}
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
		if strings.Contains(err.Error(), "unable to authenticate") { // x/crypto has no typed error for it
			return fmt.Errorf("%w (%s): %w", ErrSignInRefused, s.username, err)
		}
		return fmt.Errorf("stage 1: sign in to the guest as %s: %w", s.username, err)
	}
	_ = conn.SetDeadline(time.Time{})
	s.client = ssh.NewClient(c, chans, reqs)
	return nil
}

// codeReplaced runs after the rotate command: stage 1 signs in again with the
// new password, on the same pinned host key, and only then is the code file
// deleted. Interactive dscl can exit 0 on a failed passwd, so the sign-in is
// the proof that the code was replaced.
func (s *session) codeReplaced(ctx context.Context) error {
	_ = s.client.Close()
	clear(s.admin)
	s.admin, s.next = s.next, nil
	if err := s.connect(ctx); err != nil {
		if errors.Is(err, ErrSignInRefused) { // a failed rotation, not a wrong account: keep the text, not the identity
			err = errors.New(err.Error())
		}
		return &StepError{Step: "6 D1(c)", Command: "rotate-admin-password", Err: fmt.Errorf("the new password does not sign in: %w", err)}
	}
	if s.in.Replaced != nil {
		s.in.Replaced()
	}
	s.emit("one-time code replaced", 0)
	return nil
}

// stdin is c's stdin: sudo's password line first for a root command.
//
// It is allocated once at its exact size, so no secret is left behind in an
// array a regrowth dropped; run zeroes the one array after the command.
func (s *session) stdin(c command) []byte {
	const rotate = "passwd /Users/"
	n := 0
	if c.Root {
		n += len(s.admin) + 1
	}
	switch c.In {
	case stdinRotate:
		n += len(rotate) + len(s.username) + 1 + len(s.code) + 1 + len(s.next) + 1
	case stdinAgent:
		n += len(s.agent)
	case stdinPlist:
		n += len(agentPlist)
	case stdinAgentPassword:
		n += len(s.agentPw) + 1
	case stdinKCPassword:
		n += kcpasswordLen(len(s.agentPw))
	case stdinPairKey:
		n += len(s.pairKey)
	}
	b := make([]byte, 0, n)
	if c.Root {
		b = append(append(b, s.admin...), '\n')
	}
	switch c.In {
	case stdinRotate:
		// Written piece by piece: a formatted line would leave a copy of
		// both secrets in fmt's buffer pool. The username passed safeWord.
		b = append(append(append(b, rotate...), s.username...), ' ')
		b = append(append(append(b, s.code...), ' '), s.next...)
		b = append(b, '\n')
	case stdinAgent:
		b = append(b, s.agent...)
	case stdinPlist:
		b = append(b, agentPlist...)
	case stdinAgentPassword:
		b = append(append(b, s.agentPw...), '\n')
	case stdinKCPassword:
		kc := kcpassword(s.agentPw)
		b = append(b, kc...)
		clear(kc)
	case stdinPairKey:
		b = append(b, s.pairKey...)
	}
	return b
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
	if c.CheckAccounts {
		return s.checkAccounts(out.String())
	}
	return nil
}

// checkAccounts reads dscl's "name UniqueID" lines strictly: a line that is not a name alone (a record with no
// UniqueID cannot sign in, and a real Mac may hold one, so it is skipped) or exactly two fields with a numeric
// ID, or an output with none, is refused. The users of UniqueID 500 or more must be the admin and
// the agent and no one else.
func (s *session) checkAccounts(listing string) error {
	var found []string
	for _, l := range strings.Split(strings.TrimSpace(listing), "\n") {
		f := strings.Fields(l)
		if len(f) == 1 {
			continue
		}
		if len(f) != 2 {
			return fmt.Errorf("%w: cannot read %q", ErrUnexpectedAccounts, l)
		}
		id, err := strconv.Atoi(f[1])
		if err != nil {
			return fmt.Errorf("%w: cannot read %q", ErrUnexpectedAccounts, l)
		}
		if id >= 500 {
			found = append(found, f[0])
		}
	}
	slices.Sort(found)
	if want := []string{agentUser, s.username}; !slices.Equal(found, slices.Sorted(slices.Values(want))) {
		var extra []string
		for _, u := range found {
			if !slices.Contains(want, u) {
				extra = append(extra, u)
			}
		}
		return fmt.Errorf("%w: found %q, extra %q", ErrUnexpectedAccounts, found, extra)
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
	for _, v := range [][]byte{s.admin, s.code, s.next, s.agentPw, s.pairKey} {
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

// rfbBanner checks that the guest's port 5900 answers with Screen Sharing's
// RFB banner: proof the guest is up and its network reaches this helper, on
// the VM's own lease address, without SSH.
func (s *session) rfbBanner(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := s.in.Dial(dctx, 5900)
	if err != nil {
		return fmt.Errorf("%w: Screen Sharing did not answer: %v", ErrGuestLivenessUnknown, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, len(rfbBanner))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != rfbBanner {
		return fmt.Errorf("%w: Screen Sharing sent no RFB banner", ErrGuestLivenessUnknown)
	}
	return nil
}

// rfbBanner is how every RFB 3.x server's first message starts.
const rfbBanner = "RFB 003."

// beforeOff runs just before Remote Login off: Screen Sharing answers with
// its banner, port 22 is open, and the guest's boot epoch is taken. Until
// wait passes it keeps trying (Screen Sharing was loaded moments ago); then
// it is unknown, and stage 1 stops with Remote Login still on.
func (s *session) beforeOff(ctx context.Context, wait time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := s.rfbBanner(wctx)
		if err == nil {
			err = s.portOpen(wctx, 22)
		}
		if err == nil {
			s.epoch, err = s.guestEpoch()
		}
		if err == nil || ctx.Err() != nil {
			return s.stepErr(ctx, err)
		}
		select {
		case <-wctx.Done():
			return s.stepErr(ctx, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *session) stepErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	return &StepError{Step: "7e", Command: "check the guest before Remote Login off", Err: err}
}

func (s *session) portOpen(ctx context.Context, port int) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := s.in.Dial(dctx, port)
	if err != nil {
		return fmt.Errorf("%w: port %d did not answer: %v", ErrGuestLivenessUnknown, port, err)
	}
	return c.Close()
}

// confirmOff checks that a fresh dial of the guest's port 22 is refused,
// trying until wait passes. Only a refusal proves Remote Login off: a dial
// that times out, or a relay that can no longer dial the guest, proves
// nothing, so stage 1 keeps trying and then fails. A refusal counts only
// when the guest is on the same boot just before and just after it: a VM
// that is down, rebooting or resetting its network can refuse too, and that
// is unknown, which fails at once.
func (s *session) confirmOff(ctx context.Context, wait time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if err := s.sameBoot(); err != nil {
			return &StepError{Step: "7e", Command: "confirm Remote Login is off", Err: err}
		}
		dctx, dcancel := context.WithTimeout(wctx, 5*time.Second)
		c, err := s.in.Dial(dctx, 22)
		dcancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if refused(err) {
			// The refusal counts only from a guest that is still up on the
			// same boot: Screen Sharing answers, and the epoch is unchanged.
			err := s.rfbBanner(ctx)
			if err == nil {
				err = s.sameBoot()
			}
			if err != nil {
				return &StepError{Step: "7e", Command: "confirm Remote Login is off", Err: err}
			}
			return nil
		}
		if c != nil {
			_ = c.Close()
		}
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

// refused says whether err is a dial refused by the guest: an RST, as the
// relay's netstack reports it (gonet's connect error carrying
// tcpip.ErrConnectionRefused), or ECONNREFUSED.
func refused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "connect" && op.Err != nil &&
		op.Err.Error() == (&tcpip.ErrConnectionRefused{}).String()
}

func (s *session) zero() {
	for _, v := range [][]byte{s.admin, s.code, s.next, s.agentPw, s.pairKey} {
		clear(v)
	}
}
