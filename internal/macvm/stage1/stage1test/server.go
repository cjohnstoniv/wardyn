// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package stage1test is a fake macOS guest's Remote Login for stage 1's
// tests: an in-process SSH server (x/crypto/ssh) that records every exec's
// command line and stdin, and acts out the few commands whose effect a later
// step depends on (sudo's password line, dscl's password change, dd's file
// write, shasum, Remote Login off). Dial stands in for the prep relay.
package stage1test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// Exec is one command the guest ran.
type Exec struct {
	Line  string
	Argv  []string
	Stdin []byte
}

// Server is the fake guest.
type Server struct {
	// Fail, when it returns true for an argv (sudo stripped), makes that
	// command exit 1 and echo its stdin on stderr, as a careless tool might.
	Fail func(argv []string) bool
	// DropOnOff ends the session of Remote Login off with no exit status.
	DropOnOff bool
	// StaysOn keeps Remote Login answering after it was turned off.
	StaysOn bool
	// RejectPasswd makes dscl's passwd fail the way interactive dscl does:
	// an error on stderr, and exit 0 at the end of its input.
	RejectPasswd bool
	// ExtraAccounts are local users with a UniqueID of 500 or more beyond the
	// admin and the agent, as a person's setup by hand could have left.
	ExtraAccounts []string
	// KeyboardOnly takes the password through keyboard-interactive (PAM)
	// only, as one hidden prompt.
	KeyboardOnly bool

	mu       sync.Mutex
	sharing  bool // Screen Sharing loaded: port 5900 sends its RFB banner
	muteRFB  bool // port 5900 accepts, then sends nothing
	rfb      net.Listener
	user     string
	password string
	key      ssh.Signer
	on       bool
	execs    []Exec
	users    []string // the accounts added by sysadminctl
	files    map[string][]byte
	dials    []int
	ln       net.Listener
}

// New starts a guest whose account user has password, with Remote Login on.
func New(t testing.TB, user, password string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rfb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{user: user, password: password, on: true, files: map[string][]byte{}, ln: ln, rfb: rfb}
	s.NewHostKey(t)
	t.Cleanup(func() { _ = ln.Close(); _ = rfb.Close() })
	go s.accept()
	go func() {
		for {
			c, err := rfb.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			mute := s.muteRFB
			s.mu.Unlock()
			if !mute {
				_, _ = c.Write([]byte("RFB 003.889\n"))
			}
			_ = c.Close()
		}
	}()
	return s
}

// NewHostKey gives the guest a new host key for every later connection.
func (s *Server) NewHostKey(t testing.TB) ssh.PublicKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.key = k
	s.mu.Unlock()
	return k.PublicKey()
}

// Refused is the prep relay's refusal of a dial, as its netstack (gonet)
// reports a guest's RST.
func Refused() error {
	return &net.OpError{Op: "connect", Net: "tcp", Err: errors.New((&tcpip.ErrConnectionRefused{}).String())}
}

// Dial is the prep relay's DialGuest: port 22 while Remote Login is on, port
// 5900 (an RFB banner) once Screen Sharing is loaded; any other is refused.
func (s *Server) Dial(ctx context.Context, port int) (net.Conn, error) {
	s.mu.Lock()
	s.dials = append(s.dials, port)
	addr := ""
	switch {
	case port == 22 && s.on:
		addr = s.ln.Addr().String()
	case port == 5900 && s.sharing:
		addr = s.rfb.Addr().String()
	}
	s.mu.Unlock()
	if addr == "" {
		return nil, Refused()
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// LoadSharing loads Screen Sharing before stage 1 runs, as a person turning it on in System Settings does.
func (s *Server) LoadSharing() {
	s.mu.Lock()
	s.sharing = true
	s.mu.Unlock()
}

// SetPassword sets the account's password, as the provisioning boot does.
func (s *Server) SetPassword(pw string) {
	s.mu.Lock()
	s.password = pw
	s.mu.Unlock()
}

// MuteRFB makes port 5900 accept a connection and then send nothing.
func (s *Server) MuteRFB(on bool) {
	s.mu.Lock()
	s.muteRFB = on
	s.mu.Unlock()
}

// Execs is every command run so far.
func (s *Server) Execs() []Exec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exec(nil), s.execs...)
}

// Dials is every port dialled so far.
func (s *Server) Dials() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.dials...)
}

// Password is the account's password now.
func (s *Server) Password() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.password
}

// File is what dd wrote to path.
func (s *Server) File(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[path]
}

// RemoteLogin says whether Remote Login is on.
func (s *Server) RemoteLogin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	s.mu.Lock()
	check := func(user, pw string) (*ssh.Permissions, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if user == s.user && pw == s.password {
			return nil, nil
		}
		return nil, errors.New("wrong password")
	}
	cfg := &ssh.ServerConfig{}
	if s.KeyboardOnly {
		cfg.KeyboardInteractiveCallback = func(m ssh.ConnMetadata, ask ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			a, err := ask(m.User(), "", []string{"Password:"}, []bool{false})
			if err != nil || len(a) != 1 {
				return nil, errors.New("no answer")
			}
			return check(m.User(), a[0])
		}
	} else {
		cfg.PasswordCallback = func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) { return check(m.User(), string(pw)) }
	}
	cfg.AddHostKey(s.key)
	s.mu.Unlock()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for r := range reqs {
		if r.Type != "exec" {
			_ = r.Reply(false, nil)
			continue
		}
		var p struct{ Line string }
		if err := ssh.Unmarshal(r.Payload, &p); err != nil {
			_ = r.Reply(false, nil)
			return
		}
		_ = r.Reply(true, nil)
		stdin, _ := io.ReadAll(ch)
		code, stdout, stderr, drop := s.exec(p.Line, stdin)
		_, _ = ch.Write(stdout)
		_, _ = ch.Stderr().Write(stderr)
		if drop {
			return
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

var sudo = []string{"/usr/bin/sudo", "-k", "-S", "-p", "", "--"}

func (s *Server) exec(line string, stdin []byte) (code int, stdout, stderr []byte, drop bool) {
	argv := Split(line)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs = append(s.execs, Exec{Line: line, Argv: argv, Stdin: stdin})
	rest := stdin
	if len(argv) >= len(sudo) && slices.Equal(argv[:len(sudo)], sudo) {
		pw, after, _ := bytes.Cut(stdin, []byte("\n"))
		if string(pw) != s.password {
			return 1, nil, []byte("Sorry, try again.\n"), false
		}
		argv, rest = argv[len(sudo):], after
	}
	if s.Fail != nil && s.Fail(argv) {
		return 1, nil, append([]byte("failed; stdin was: "), stdin...), false
	}
	switch {
	case slices.Equal(argv, []string{"/usr/bin/dscl", "."}):
		f := strings.Fields(strings.TrimSpace(string(rest)))
		if s.RejectPasswd || len(f) != 4 || f[0] != "passwd" || f[1] != "/Users/"+s.user || f[2] != s.password {
			return 0, nil, append([]byte("passwd: DS error: eDSAuthFailed; input: "), rest...), false
		}
		s.password = f[3]
	case slices.Equal(argv, []string{"/usr/bin/dscl", ".", "-list", "/Users", "UniqueID"}):
		out := "root 0\ndaemon 1\nnobody -2\n" + s.user + " 501\n"
		for i, u := range append(slices.Clone(s.users), s.ExtraAccounts...) {
			out += fmt.Sprintf("%s %d\n", u, 502+i)
		}
		return 0, []byte(out), nil, false
	case len(argv) >= 3 && argv[0] == "/usr/sbin/sysadminctl" && argv[1] == "-addUser":
		s.users = append(s.users, argv[2])
	case len(argv) == 2 && argv[0] == "/bin/dd" && strings.HasPrefix(argv[1], "of="):
		s.files[strings.TrimPrefix(argv[1], "of=")] = append([]byte(nil), rest...)
	case len(argv) == 4 && argv[0] == "/usr/bin/shasum":
		sum := sha256.Sum256(s.files[argv[3]])
		return 0, []byte(hex.EncodeToString(sum[:]) + "  " + argv[3] + "\n"), nil, false
	case len(argv) >= 3 && argv[0] == "/bin/launchctl":
		code, stderr = s.launchctl(argv)
		return code, nil, stderr, false
	case len(argv) == 4 && argv[0] == "/usr/sbin/systemsetup" && argv[2] == "-setremotelogin" && argv[3] == "off":
		s.on = s.StaysOn
		return 0, nil, nil, s.DropOnOff
	}
	return 0, nil, nil, false
}

// launchctl answers stage 1's Screen Sharing probe and bootstrap; any other launchctl line succeeds.
func (s *Server) launchctl(argv []string) (code int, stderr []byte) {
	switch {
	case len(argv) == 3 && argv[1] == "print" && argv[2] == "system/com.apple.screensharing":
		if !s.sharing {
			return 113, []byte("Could not find service \"com.apple.screensharing\" in domain for system\n")
		}
	case len(argv) == 4 && argv[1] == "bootstrap" && strings.HasSuffix(argv[3], "com.apple.screensharing.plist"):
		if s.sharing {
			return 5, []byte("Bootstrap failed: 5: Input/output error\n")
		}
		s.sharing = true
	}
	return 0, nil
}

// Split is a command line's words: blanks separate them, and '...' quotes
// (a quote inside is written quote, backslash, quote, quote), the only quoting stage 1 writes.
func Split(line string) []string {
	var out []string
	var w strings.Builder
	inWord, quoted := false, false
	r := bufio.NewReader(strings.NewReader(line))
	for {
		c, err := r.ReadByte()
		if err != nil {
			break
		}
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			w.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\':
			if n, err := r.ReadByte(); err == nil {
				w.WriteByte(n)
				inWord = true
			}
		case c == ' ':
			if inWord {
				out = append(out, w.String())
				w.Reset()
				inWord = false
			}
		default:
			w.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, w.String())
	}
	return out
}
