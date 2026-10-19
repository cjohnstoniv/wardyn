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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	syncSubsystem    = "wardyn-sync"
	syncDialTimeout  = 15 * time.Second
	syncCloseTimeout = 10 * time.Second
	// syncStderrCap bounds the gateway's stderr kept for an error message; the rest is read and dropped.
	syncStderrCap = 64 << 10
)

// syncHandshakeTimeout bounds the SSH handshake: a gateway that accepts the connection and never speaks
// must not hang the sync.
var syncHandshakeTimeout = syncDialTimeout

// syncWatchEvery is how often --watch looks again.
var syncWatchEvery = 2 * time.Second

type syncFlags struct {
	remoteDir       string
	pull            bool
	watch           bool
	asJSON          bool
	advertisedProxy bool
	maxPullBytes    int64
	pullDeletes     bool
	maxPullDeletes  int
	git             bool
	pullCommits     bool
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
symlinks are refused both ways; pulled files never get an exec bit; .git,
.envrc, .direnv, .vscode/tasks.json, .vscode/launch.json,
.idea/runConfigurations and .idea/workspace.xml are never synced either way; a
file changed on both sides since the last sync is not pulled (the laptop's copy
is pushed) and is listed as a conflict. State is kept in
~/.local/state/wardyn/sync/<run-id>.json, outside the synced directory.

Ignore files: every .gitignore and .wardynignore in the tree is honoured both
ways, in git's syntax; a .wardynignore line wins over a .gitignore one in the
same directory, so "!name" there sends a git-ignored file. Your organisation's
own patterns, if it set any, apply on top and no ignore file can undo them.

Deletes and renames: a file deleted or renamed on this computer is deleted or
renamed in the sandbox, but only while the sandbox's copy is still the one this
sync sent (a sandbox edit is kept and listed as a conflict). A sandbox delete
never removes a file here unless --pull-deletes is given: those deletes are
listed before they are applied, at most --max-pull-deletes of them in one pass
(over that, none), and each file is moved into
~/.local/state/wardyn/sync/<run-id>-deleted/ rather than removed.

Git history: .git is never synced. --git sends this computer's current branch
as a git bundle (commits and the branch name, no hooks, no config) after the
first pass; a sandbox directory with no repository gets a new one with that
branch checked out over the synced files, one with a repository gets it as
refs/remotes/laptop/<branch>. --pull-commits brings the sandbox's commits
since then back as a bundle and fetches them into the branch
wardyn/sync-<run-id prefix> here, for you to review; your working tree and other
branches are never touched. Both need the top of a git work tree, and both
run git in the sandbox over an SSH exec channel.

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
	cmd.Flags().BoolVar(&f.pullDeletes, "pull-deletes", false, "with --pull, let a file deleted in the sandbox be removed here too (moved into the run's sync state folder)")
	cmd.Flags().IntVar(&f.maxPullDeletes, "max-pull-deletes", 20, "most files --pull-deletes may remove here in one pass; over it, none is removed")
	cmd.Flags().BoolVar(&f.git, "git", false, "after the first pass, send this computer's current branch and its history to the sandbox as a git bundle")
	cmd.Flags().BoolVar(&f.pullCommits, "pull-commits", false, "after the first pass, fetch the sandbox's new commits into the branch wardyn/sync-<run> here (never into your working tree)")
	cmd.Flags().Int64Var(&f.maxPullBytes, "max-pull-bytes", defaultMaxPullBytes, "most bytes one pass may pull from the sandbox (a single file over 256 MiB is always refused)")
	cmd.Flags().BoolVar(&f.advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

func runSync(cmd *cobra.Command, c *sdk.Client, runID, localDir string, f syncFlags) error {
	id, err := parseID("run", runID)
	if err != nil {
		return err
	}
	if err := refusePlainHTTPGateway("sync", c.BaseURL); err != nil {
		return err
	}
	if err := f.check(); err != nil {
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
		return fmt.Errorf("sync: this deployment advertises a ProxyCommand, which would run on this computer as:\n  %s\n"+
			"run again with --advertised-proxy to connect through it", expandProxyCommand(gw.proxy, gw.host, portOr22(gw.port), runID))
	}
	if gw.fingerprint == "" {
		return errors.New("sync: the gateway publishes no host key fingerprint, so its identity cannot be checked; ask your operator")
	}
	rules, err := c.GetRunSyncRules(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("sync: read the run's sync rules: %w", err)
	}
	signer, err := loadSyncSigner()
	if err != nil {
		return err
	}
	// The first Ctrl-C cancels ctx; stop() then restores the default, so a
	// second one ends the process even if the wind-down hangs.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); stop() }()
	sshc, err := dialSyncGateway(ctx, gw, runID, signer)
	if err != nil {
		return err
	}
	defer sshc.Close()
	// Closing the client unblocks any sftp call stuck on a sandbox that stalls
	// (a file swapped for a FIFO, a trickling read).
	defer context.AfterFunc(ctx, func() { _ = sshc.Close() })()

	direction := "push"
	if f.pull {
		direction = "pull"
	}
	sess, err := openSyncSession(sshc, remote, direction)
	if err != nil {
		return err
	}
	lroot, err := os.OpenRoot(root)
	if err != nil {
		_ = sess.close()
		return fmt.Errorf("sync: %w", err)
	}
	defer lroot.Close()
	sy := &syncer{c: sess.sftp, meter: sess.meter, root: lroot, st: st, stateFile: stateFile, o: syncOptions{
		remote: remote, pull: f.pull, maxPullBytes: f.maxPullBytes, orgDeny: rules.Deny,
		pullDeletes: f.pullDeletes, maxPullDeletes: f.maxPullDeletes, notify: cmd.ErrOrStderr(),
		trash: filepath.Join(filepath.Dir(stateFile), runID+"-deleted", time.Now().UTC().Format("20060102T150405.000000000Z")),
	}}
	var gitStep func() error
	if f.git || f.pullCommits {
		if err := syncGitCheck(ctx, sy, root); err != nil {
			_ = sess.close()
			return fmt.Errorf("sync: %w", err)
		}
		w := cmd.OutOrStdout()
		if f.asJSON {
			w = cmd.ErrOrStderr() // stdout stays one JSON object per pass
		}
		gitStep = func() error {
			if f.git {
				if err := sendGit(ctx, w, sy, sshc, runID, root); err != nil {
					return err
				}
			}
			if f.pullCommits {
				return pullCommits(ctx, w, sy, sshc, runID, root)
			}
			return nil
		}
	}
	runErr := runSyncLoop(ctx, cmd, sy, sess, f, gitStep)
	if cerr := sess.close(); runErr == nil {
		runErr = cerr
	}
	return runErr
}

// refusePlainHTTPGateway refuses a plain-http, non-loopback server: the gateway's
// address, host key fingerprint and ProxyCommand all come from its /healthz, so
// anyone on the path could supply all three. Shared by `run ssh` and `sync`; verb
// is the command name that leads the error.
func refusePlainHTTPGateway(verb, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s: %s is plain http: the gateway's address, host key fingerprint and ProxyCommand are read from its /healthz, so anyone on the path could supply all three; use https://", verb, host)
}

func portOr22(port string) string {
	if port == "" {
		return "22"
	}
	return port
}

func runSyncLoop(ctx context.Context, cmd *cobra.Command, sy *syncer, sess *syncSession, f syncFlags, gitStep func() error) error {
	for first := true; ; first = false {
		rep, err := sy.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("sync: interrupted")
			}
			sess.report(syncPassReport{Error: syncShown(err.Error())})
			return fmt.Errorf("sync: %w", err)
		}
		sess.report(syncPassReport{Complete: true, DeletedOnLaptop: len(rep.DeletedLocal)})
		if f.asJSON {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(rep); err != nil {
				return err
			}
		} else if first || !rep.empty() {
			printSyncReport(cmd.OutOrStdout(), rep)
		}
		if first && gitStep != nil {
			if err := gitStep(); err != nil {
				if ctx.Err() != nil {
					return errors.New("sync: interrupted")
				}
				return fmt.Errorf("sync: %w", err)
			}
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
		fmt.Fprintf(w, "conflict (changed on both sides; the laptop's copy wins, the sandbox's is not pulled): %s\n", syncShown(p))
	}
	for _, p := range r.Renamed {
		fmt.Fprintf(w, "renamed in the sandbox: %s -> %s\n", syncShown(p.From), syncShown(p.To))
	}
	for _, p := range r.DeletedRemote {
		fmt.Fprintf(w, "deleted in the sandbox (deleted here): %s\n", syncShown(p))
	}
	for _, p := range r.DeletedLocal {
		fmt.Fprintf(w, "deleted here (deleted in the sandbox; kept in %s): %s\n", syncShown(r.KeptIn), syncShown(p))
	}
	for _, x := range r.Refused {
		fmt.Fprintf(w, "refused (%s): %s\n", syncShown(x.Reason), syncShown(x.Path))
	}
	if r.Ignored > 0 {
		fmt.Fprintf(w, "left out by ignore files: %d\n", r.Ignored)
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
	port := portOr22(gw.port)
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
	// Closing the connection ends the handshake on either transport: a ProxyCommand's
	// pipes take no deadline, so a socket deadline alone would not bound it.
	timer := time.AfterFunc(syncHandshakeTimeout, func() { _ = conn.Close() })
	cc, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(gw.host, port), cfg)
	if !timer.Stop() && err == nil {
		err = fmt.Errorf("no answer within %s", syncHandshakeTimeout)
		_ = cc.Close()
	}
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

// expandProxyCommand expands the same %h, %p, %r and %% tokens ssh_config does.
// resolveSSHGateway has already limited host and port to characters that mean
// nothing to a shell.
func expandProxyCommand(proxy, host, port, user string) string {
	return strings.NewReplacer("%%", "%", "%h", host, "%p", port, "%r", user).Replace(proxy)
}

// dialProxyCommand runs the advertised ProxyCommand with the child environment
// scrubbed as for ssh(1).
func dialProxyCommand(ctx context.Context, proxy, host, port, user string) (net.Conn, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", expandProxyCommand(proxy, host, port, user))
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
	meter   *listMeter
	status  int           // the exit-status, valid once exited is closed; -1 when none came
	exited  chan struct{} // closed when the channel's requests end
	errDone chan struct{} // closed when stderr is drained
	stderr  cappedBuffer
}

// cappedBuffer keeps the first syncStderrCap bytes written to it and discards the rest, so the channel
// still drains and the gateway is never blocked on a full stderr.
type cappedBuffer struct{ buf bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := syncStderrCap - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

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
	s.meter = &listMeter{r: ch}
	s.sftp, err = sftp.NewClientPipe(s.meter, stdinEOF{ch})
	if err != nil {
		return nil, fmt.Errorf("sync: %w", s.refusal())
	}
	if got, err := s.sftp.RealPath("."); err != nil || got != dir {
		_ = s.close()
		if err != nil {
			return nil, fmt.Errorf("sync: ask the sandbox for its start directory: %w", err)
		}
		return nil, fmt.Errorf("sync: the sandbox started in %s, not %s; does %s exist in the sandbox?", syncShown(got), syncShown(dir), syncShown(dir))
	}
	return s, nil
}

// syncPassRequest is the channel request `wardyn sync` sends after each pass:
// the gateway counts the laptop deletes into the session's ssh.sync.transfer
// row and, for a run that holds its agent for this folder, takes a complete
// pass as the first sync landing (or an error as it failing). It is the
// client's own account; a gateway that predates it refuses it, which is ignored.
const syncPassRequest = "wardyn-sync-pass"

type syncPassReport struct {
	Complete        bool   `json:"complete"`
	Error           string `json:"error,omitempty"`
	DeletedOnLaptop int    `json:"deleted_on_laptop"`
}

// report sends one pass's account and reports whether the gateway took it.
func (s *syncSession) report(r syncPassReport) bool {
	b, err := json.Marshal(r)
	if err != nil {
		return false
	}
	return s.request(syncPassRequest, ssh.Marshal(struct{ Report string }{string(b)})) == nil
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
		return errors.New(syncShown(msg))
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
	return syncExitError(s.status, s.stderr.String())
}

// syncExitError reads the channel's exit-status: 0 is a clean end; anything
// else, including no status at all (-1, the channel closed without one), is a
// failure, because the gateway then records the sync as failed too.
func syncExitError(status int, stderr string) error {
	switch status {
	case 0:
		return nil
	case -1:
		return errors.New("sync: the channel ended without an exit status from the sandbox's sftp-server")
	}
	return fmt.Errorf("sync: the sandbox's sftp-server exited %d: %s", status, syncShown(strings.TrimSpace(stderr)))
}

func (f syncFlags) check() error {
	if f.maxPullBytes <= 0 {
		return errors.New("sync: --max-pull-bytes must be positive")
	}
	if f.pullDeletes && !f.pull {
		return errors.New("sync: --pull-deletes needs --pull")
	}
	if f.maxPullDeletes <= 0 {
		return errors.New("sync: --max-pull-deletes must be positive")
	}
	return nil
}

// localFolderRequest is --local-folder as the create request names it: the
// folder's base name, never its path on this computer. No dir is no request.
func localFolderRequest(dir, remoteDir string, createJSON bool) (*types.RunLocalFolder, error) {
	if dir == "" {
		return nil, nil
	}
	if createJSON {
		return nil, errors.New("--local-folder prints the sync's progress, so it does not go with --json")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("--local-folder: %s is not a directory", dir)
	}
	if _, err := syncRemoteDir(remoteDir); err != nil {
		return nil, fmt.Errorf("--local-folder-remote-dir: %w", err)
	}
	return &types.RunLocalFolder{Name: filepath.Base(abs), RemoteDir: remoteDir}, nil
}

// seedThenWait seeds a new run's local folder, if it has one, then waits for
// the run when --wait asks.
func seedThenWait(cmd *cobra.Command, c *sdk.Client, id uuid.UUID, dir, remoteDir string, wait bool, timeout time.Duration) error {
	if err := seedLocalFolder(cmd, c, id, dir, remoteDir, timeout); err != nil {
		return err
	}
	if wait {
		return waitForRun(cmd.Context(), cmd.ErrOrStderr(), c, id, timeout)
	}
	return nil
}

// seedLocalFolder waits for the new run's sandbox, then runs one sync pass of
// dir into it, which releases the held agent. On a failure the run stays held
// until its seed wait ends, so the error names the command that seeds it. A
// run without one has nothing to seed.
func seedLocalFolder(cmd *cobra.Command, c *sdk.Client, id uuid.UUID, dir, remoteDir string, timeout time.Duration) error {
	if dir == "" {
		return nil
	}
	errW := cmd.ErrOrStderr()
	fmt.Fprintf(errW, "waiting for run %s's sandbox to seed %s into it\n", id, dir)
	deadline := time.Now().Add(timeout)
	for {
		run, err := c.GetRun(cmd.Context(), id)
		switch {
		case err != nil:
			return err
		case run.State == types.RunRunning:
			err := runSync(cmd, c, id.String(), dir, syncFlags{remoteDir: remoteDir, maxPullBytes: defaultMaxPullBytes, maxPullDeletes: 20})
			if err != nil {
				return fmt.Errorf("%w\nthe agent of run %s is held for this folder: seed it with 'wardyn sync %s %s' before the run's seed wait ends", err, id, id, dir)
			}
			fmt.Fprintf(errW, "seeded %s; the agent of run %s starts now. Keep it in step with 'wardyn sync %s %s --watch'\n", dir, id, id, dir)
			return nil
		case run.State.IsTerminal():
			return &exitError{code: 1, err: fmt.Errorf("run %s ended %s before its folder was seeded", id, run.State)}
		case time.Now().After(deadline):
			return &exitError{code: 124, err: fmt.Errorf("run %s's sandbox was not up within --timeout %s, so %s was not seeded", id, timeout, dir)}
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(waitPollInterval):
		}
	}
}
