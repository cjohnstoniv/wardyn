// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	syncSubsystem    = "wardyn-sync"
	syncDialTimeout  = 15 * time.Second
	syncCloseTimeout = 10 * time.Second
)

// syncWatchEvery is how often --watch looks again.
var syncWatchEvery = 2 * time.Second

type syncFlags struct {
	remoteDir       string
	pull            bool
	watch           bool
	asJSON          bool
	advertisedProxy bool
}

func syncCmd(client clientFn) *cobra.Command {
	var f syncFlags
	cmd := &cobra.Command{
		Use:   "sync <run-id> <local-dir>",
		Short: "Copy a local directory into a running sandbox over the SSH gateway",
		Long: `Copy a local directory into a RUNNING sandbox over the SSH gateway's wardyn-sync
subsystem, using the SSH key from 'wardyn ssh-key ensure' (~/.wardyn/id_ed25519).

The direction is laptop to sandbox. --pull also brings the sandbox's changes
back, for this run only. Safety rules, all enforced here and none optional:
nothing is ever deleted; symlinks are refused both ways; pulled files never get
an exec bit; .git, .envrc, .direnv, .vscode/tasks.json, .vscode/launch.json,
.idea/runConfigurations and .idea/workspace.xml are never synced either way; a
file changed on both sides since the last sync is not pulled (the laptop's copy
is pushed) and is listed as a conflict. State is kept in
~/.local/state/wardyn/sync/<run-id>.json, outside the synced directory.

--remote-dir (default /home/agent/work) must exist in the sandbox. The
directory is where the transfer starts, not a boundary: the sandbox's sftp
server can reach whatever the agent user can.

When the deployment advertises a ProxyCommand, connecting runs it on this
computer, so it needs --advertised-proxy (same as 'wardyn run ssh').
`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(cmd, client(), args[0], args[1], f)
		},
	}
	cmd.Flags().StringVar(&f.remoteDir, "remote-dir", "", "absolute directory in the sandbox, under /home/agent/ (default /home/agent/work)")
	cmd.Flags().BoolVar(&f.pull, "pull", false, "also pull the sandbox's changes to this computer")
	cmd.Flags().BoolVar(&f.watch, "watch", false, "keep syncing every couple of seconds until interrupted")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "print each pass as JSON")
	cmd.Flags().BoolVar(&f.advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

func runSync(cmd *cobra.Command, c *sdk.Client, runID, localDir string, f syncFlags) error {
	if _, err := parseID("run", runID); err != nil {
		return err
	}
	root, err := filepath.Abs(localDir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("sync: %s is not a directory", localDir)
	}
	remote, err := syncRemoteDir(f.remoteDir)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	stateFile, err := syncStatePath(runID)
	if err != nil {
		return err
	}
	if err := syncStateOutside(root, stateFile); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	st, err := loadSyncState(stateFile, root, remote)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	gw, err := resolveSSHGateway(cmd.Context(), c)
	if err != nil {
		return err
	}
	if gw.proxy != "" && !f.advertisedProxy {
		return fmt.Errorf("sync: this deployment advertises a ProxyCommand, which would run on this computer:\n  %s\n"+
			"run again with --advertised-proxy to connect through it", gw.proxy)
	}
	if gw.fingerprint == "" {
		return errors.New("sync: the gateway publishes no host key fingerprint, so its identity cannot be checked; ask your operator")
	}
	signer, err := loadSyncSigner()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	sshc, err := dialSyncGateway(ctx, gw, runID, signer)
	if err != nil {
		return err
	}
	defer sshc.Close()

	direction := "push"
	if f.pull {
		direction = "pull"
	}
	sess, err := openSyncSession(sshc, remote, direction)
	if err != nil {
		return err
	}
	sy := &syncer{c: sess.sftp, o: syncOptions{root: root, remote: remote, pull: f.pull}, st: st, stateFile: stateFile}
	runErr := runSyncLoop(ctx, cmd, sy, f)
	if cerr := sess.close(); runErr == nil {
		runErr = cerr
	}
	return runErr
}

func runSyncLoop(ctx context.Context, cmd *cobra.Command, sy *syncer, f syncFlags) error {
	for first := true; ; first = false {
		rep, err := sy.pass()
		if err != nil {
			return fmt.Errorf("sync: %w", err)
		}
		if f.asJSON {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(rep); err != nil {
				return err
			}
		} else if first || !rep.empty() {
			printSyncReport(cmd.OutOrStdout(), rep)
		}
		if !f.watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(syncWatchEvery):
		}
	}
}

func printSyncReport(w io.Writer, r *syncReport) {
	fmt.Fprintf(w, "pushed %d file(s) (%d bytes), pulled %d file(s) (%d bytes)\n", len(r.Pushed), r.BytesOut, len(r.Pulled), r.BytesIn)
	for _, p := range r.Conflicts {
		fmt.Fprintf(w, "conflict (changed on both sides; the laptop's copy wins, the sandbox's is not pulled): %s\n", p)
	}
	for _, x := range r.Refused {
		fmt.Fprintf(w, "refused (%s): %s\n", x.Reason, x.Path)
	}
}

// loadSyncSigner reads the CLI's own gateway key. Its public half is the one
// 'wardyn ssh-key ensure' registered.
func loadSyncSigner() (ssh.Signer, error) {
	file, err := expandHome(defaultSSHKeyPath)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("sync: no SSH key at %s; run 'wardyn ssh-key ensure' first", file)
	}
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("sync: read %s: %w", file, err)
	}
	return signer, nil
}

// dialSyncGateway connects to the gateway as the run, trusting only the host
// key whose fingerprint /healthz advertised.
func dialSyncGateway(ctx context.Context, gw sshGateway, runID string, signer ssh.Signer) (*ssh.Client, error) {
	port := gw.port
	if port == "" {
		port = "22"
	}
	var conn net.Conn
	var err error
	if gw.proxy != "" {
		conn, err = dialProxyCommand(ctx, gw.proxy, gw.host, port, runID)
	} else {
		d := net.Dialer{Timeout: syncDialTimeout}
		conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(gw.host, port))
	}
	if err != nil {
		return nil, fmt.Errorf("sync: connect to the gateway: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User: runID,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != gw.fingerprint {
				return fmt.Errorf("gateway host key %s is not the advertised %s", got, gw.fingerprint)
			}
			return nil
		},
		Timeout: syncDialTimeout,
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(gw.host, port), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sync: ssh handshake with the gateway: %w", err)
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

// procConn is the connection a ProxyCommand provides: its stdout and stdin.
type procConn struct {
	cmd *exec.Cmd
	r   io.ReadCloser
	w   io.WriteCloser
}

func (p *procConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *procConn) Close() error {
	_ = p.w.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return nil
}
func (p *procConn) LocalAddr() net.Addr              { return procAddr{} }
func (p *procConn) RemoteAddr() net.Addr             { return procAddr{} }
func (p *procConn) SetDeadline(time.Time) error      { return nil }
func (p *procConn) SetReadDeadline(time.Time) error  { return nil }
func (p *procConn) SetWriteDeadline(time.Time) error { return nil }

type procAddr struct{}

func (procAddr) Network() string { return "proxycommand" }
func (procAddr) String() string  { return "proxycommand" }

// dialProxyCommand runs the advertised ProxyCommand, expanding the same %h,
// %p, %r and %% tokens ssh_config does, with the child environment scrubbed
// as for ssh(1).
func dialProxyCommand(ctx context.Context, proxy, host, port, user string) (net.Conn, error) {
	line := strings.NewReplacer("%%", "%", "%h", host, "%p", port, "%r", user).Replace(proxy)
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Env = cliutil.ScrubChildEnv(os.Environ())
	cmd.Stderr = os.Stderr
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procConn{cmd: cmd, r: r, w: w}, nil
}

// syncSession is one open wardyn-sync channel with an sftp client over it. It
// drives the raw channel because ssh.Session cannot report a subsystem's
// exit-status.
type syncSession struct {
	ch      ssh.Channel
	sftp    *sftp.Client
	status  int           // the exit-status, valid once exited is closed; -1 when none came
	exited  chan struct{} // closed when the channel's requests end
	errDone chan struct{} // closed when stderr is drained
	stderr  bytes.Buffer
}

// stdinEOF makes the sftp client's Close send EOF on the channel's stdin
// instead of closing the channel.
type stdinEOF struct{ ch ssh.Channel }

func (c stdinEOF) Write(p []byte) (int, error) { return c.ch.Write(p) }
func (c stdinEOF) Close() error                { return c.ch.CloseWrite() }

// openSyncSession opens the wardyn-sync subsystem for dir and checks the
// sandbox really started there. The gateway refuses a bad directory by acking
// the subsystem and ending the channel with exit 1, so a refusal arrives here
// as the sftp handshake failing; and a directory that does not exist starts
// sftp-server somewhere else, which the RealPath check catches.
func openSyncSession(c *ssh.Client, dir, direction string) (*syncSession, error) {
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		return nil, fmt.Errorf("sync: open a channel: %w", err)
	}
	s := &syncSession{ch: ch, status: -1, exited: make(chan struct{}), errDone: make(chan struct{})}
	go func() {
		defer close(s.exited)
		for r := range reqs {
			if r.Type == "exit-status" && len(r.Payload) == 4 {
				s.status = int(binary.BigEndian.Uint32(r.Payload))
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	go func() {
		defer close(s.errDone)
		_, _ = io.Copy(&s.stderr, ch.Stderr())
	}()
	for _, kv := range [][2]string{{"WARDYN_SYNC_DIR", dir}, {"WARDYN_SYNC_DIRECTION", direction}} {
		if err := s.request("env", ssh.Marshal(struct{ Name, Value string }{kv[0], kv[1]})); err != nil {
			_ = ch.Close()
			return nil, fmt.Errorf("sync: the gateway does not accept %s (is it a Wardyn gateway of this version?): %w", kv[0], err)
		}
	}
	if err := s.request("subsystem", ssh.Marshal(struct{ Subsystem string }{syncSubsystem})); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("sync: the gateway refused the %s subsystem: %w", syncSubsystem, err)
	}
	s.sftp, err = sftp.NewClientPipe(ch, stdinEOF{ch})
	if err != nil {
		return nil, fmt.Errorf("sync: %w", s.refusal())
	}
	if got, err := s.sftp.RealPath("."); err != nil || got != dir {
		_ = s.close()
		if err != nil {
			return nil, fmt.Errorf("sync: ask the sandbox for its start directory: %w", err)
		}
		return nil, fmt.Errorf("sync: the sandbox started in %s, not %s; does %s exist in the sandbox?", got, dir, dir)
	}
	return s, nil
}

func (s *syncSession) request(typ string, payload []byte) error {
	ok, err := s.ch.SendRequest(typ, true, payload)
	if err == nil && !ok {
		err = errors.New("request refused")
	}
	return err
}

// finish waits for the channel's exit-status, then closes it, giving up after
// syncCloseTimeout. True when the exit-status arrived in time.
func (s *syncSession) finish() bool {
	select {
	case <-s.exited:
	case <-time.After(syncCloseTimeout):
		_ = s.ch.Close()
		return false
	}
	_ = s.ch.Close()
	<-s.errDone
	return true
}

// refusal explains a channel that ended before the sftp handshake: the
// gateway's own message on stderr when there is one.
func (s *syncSession) refusal() error {
	s.finish()
	if msg := strings.TrimSpace(s.stderr.String()); msg != "" {
		return errors.New(msg)
	}
	return errors.New("the gateway ended the sync channel without a message")
}

// close ends the session the way the gateway's contract asks: stdin EOF, wait
// for the channel's exit-status, then close. Closing the channel first can
// cancel the sandbox's sftp-server before it exits cleanly, and the audit row
// would then read as a failure.
func (s *syncSession) close() error {
	closed := make(chan struct{})
	go func() { defer close(closed); _ = s.sftp.Close() }()
	select {
	case <-closed:
	case <-time.After(syncCloseTimeout):
		_ = s.ch.Close()
		return errors.New("sync: the sandbox did not end the sync session in time")
	}
	if !s.finish() {
		return errors.New("sync: the sandbox did not report the end of the sync session in time")
	}
	if s.status > 0 {
		return fmt.Errorf("sync: the sandbox's sftp-server exited %d: %s", s.status, strings.TrimSpace(s.stderr.String()))
	}
	return nil
}
