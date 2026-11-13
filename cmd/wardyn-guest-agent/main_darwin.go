// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-guest-agent runs as root, from a LaunchDaemon, inside a macOS VM sandbox and is how
// wardyn-runnerd reaches it: the harness launch, terminals and exec streams, managed files, the proxy
// CA and the guest's Screen Sharing. A macOS guest has no virtio-serial port, so the agent dials the
// host over vsock (control on one port, the desktop tunnel on another) and keeps one idle connection
// open on each; it opens no network listener. Every exec runs as the guest's agent account. It also
// dials the tools port, where the host listens only on a base's stage-2 boot (tools mode, as root).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func main() {
	// Stage 2 pushes this binary to /usr/local/bin/agent-run too: by that name it is the run launcher.
	if filepath.Base(os.Args[0]) == filepath.Base(guestagent.MacAgentRunPath) {
		os.Exit(guestagent.AgentRunMain(os.Args[1:]))
	}
	// The SSH gateway's `-L` forward and the UI-app relay exec this mode: a VM has no socat.
	if len(os.Args) > 1 && os.Args[1] == guestagent.ForwardVerb {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		code := guestagent.ForwardMain(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == guestagent.TLSCheckVerb {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		code := guestagent.TLSCheckMain(ctx, os.Args[2:], os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == guestagent.HostPrefsCarryVerb {
		os.Exit(guestagent.HostPrefsCarryMain(os.Args[2:], os.Stdout))
	}
	out := flag.String("out", "/var/run/wardyn-guest", "directory for detached execs' output (cleared at boot)")
	plist := flag.String("plist", guestagent.MacAgentPlistPath, "this agent's LaunchDaemon plist, which tools mode may replace")
	flag.Parse()
	if err := run(*out, *plist); err != nil {
		slog.Error("wardyn-guest-agent", "err", err)
		os.Exit(1)
	}
}

func run(out, plist string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runner := guestagent.ExecRunner{}
	ids, err := guestagent.LookupAgentIDs(ctx, runner, guestagent.MacAgentUser)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fmt.Errorf("this agent's binary: %w", err)
	}
	desktop := &guestagent.Desktop{Run: runner, Console: guestagent.ConsoleOwner{}, AgentUID: ids.UID}
	dial := &guestagent.ToolsDialRecord{}
	cred := &syscall.Credential{Uid: ids.UID, Gid: ids.GID, Groups: ids.Groups}
	keychain := guestagent.MacKeychain{Run: runner, Dir: out, RecordDir: guestagent.MacRunCARecordDir}
	scrub := guestagent.MacScrub{OutDir: out, MarkerDir: "/var/run", AgentUID: int(ids.UID), AgentGID: int(ids.GID),
		Keychain: keychain, SetDesktopPassword: desktop.SetPassword}
	srv := &guestagent.Server{
		Version:    version.String(),
		Cred:       cred,
		Home:       guestagent.MacAgentHome,
		User:       guestagent.MacAgentUser,
		OutDir:     out,
		MarkerDir:  "/var/run",
		Desktop:    desktop,
		ToolsDial:  dial,
		TrustRunCA: keychain.TrustRunCA,
		Mount:      guestagent.MacShares{Run: runner, Home: guestagent.MacAgentHome, UID: int(ids.UID), GID: int(ids.GID), Probe: guestagent.ShareProbe{Cred: cred}.Read}.Mount,
		Disk:       guestagent.MacDisks{Run: runner, Home: guestagent.MacAgentHome, UID: int(ids.UID), GID: int(ids.GID)}.Attach,
	}
	srv.ScreenLockOff = guestagent.ScreenLock{Run: runner, Stdin: runner, Console: guestagent.ConsoleOwner{}}.Off
	srv.Scrub, srv.RecordMCPTables = scrub.Scrub, scrub.RecordMCPTables
	carry := guestagent.HostPrefsCarry{Run: runner, Console: guestagent.ConsoleOwner{}, MarkerDir: srv.MarkerDir, Cred: cred, Self: self}
	// The sign-in hold runs the carry itself. With its switch on it can restart the guest before serving.
	hold := guestagent.LoginHold{Run: runner, Console: guestagent.ConsoleOwner{}, MarkerDir: srv.MarkerDir, AgentUID: ids.UID}
	hostPrefs, held := hold.Start(ctx, carry.Carry)
	slog.Info("wardyn-guest-agent host prefs", "status", hostPrefs, "login_hold", held.Path, "reason", held.Reason)
	if held.Restarting {
		return nil
	}
	// Before the agent serves, so before any Init: what a past run wrote is gone, whether it stopped
	// gracefully or not. A restart of this service after Init skips it (BootScrubDue).
	if guestagent.BootScrubDue(srv.MarkerDir) {
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		r := scrub.Scrub(sctx, guestagent.ScrubAtBoot)
		cancel()
		srv.BootScrub = &r
		slog.Info("wardyn-guest-agent boot scrub", "outcome", r.Outcome, "removed", r.Removed, "ca_sha256", r.CAs)
		if err := guestagent.WriteHostPrefsStatus(guestagent.MacHostPrefsStatusPath, hostPrefs); err != nil {
			slog.Warn("wardyn-guest-agent host prefs status", "err", err)
		}
		// After the scrub, which removes the status folder: waits for the agent at the console, then
		// leaves auto-login off again when the hold is on, or arms the disk once its switch files appear.
		go func() {
			write := func(line string) {
				slog.Info("wardyn-guest-agent login hold", "status", line)
				if err := guestagent.WriteHostPrefsStatus(guestagent.MacLoginHoldStatusPath, line); err != nil {
					slog.Warn("wardyn-guest-agent login hold status", "err", err)
				}
			}
			write(hold.Settle(ctx, held))
			if held.Path == "off" {
				if line := hold.Watch(ctx); line != "" {
					write(line)
				}
			}
		}()
	}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	// Without the base's key the agent can pair on no port, so it does not start.
	key, err := guestagent.ReadPairKey(guestagent.MacPairKeyPath)
	if err != nil {
		return err
	}
	slog.Info("wardyn-guest-agent serving", "version", version.String())
	tools := &guestagent.Tools{Desktop: desktop, AgentBinary: self, AgentPlist: plist}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = guestagent.ServeVsock(ctx, hostDialer(guestagent.VsockControlPort, key), h)
	}()
	go func() {
		defer wg.Done()
		_ = guestagent.SpliceDesktop(ctx, hostDialer(guestagent.VsockDesktopPort, key), screenSharing, desktop.Track, desktop.ServeConsole)
	}()
	go func() {
		defer wg.Done()
		_ = guestagent.ServeTools(ctx, hostDialer(guestagent.VsockToolsPort, key), tools.ToolsHandler(), dial)
	}()
	wg.Wait()
	return nil
}

// hostDialer dials the host on port and answers its pairing challenge with key, so every channel's
// session, open byte or tools request starts only on a connection the helper has paired.
func hostDialer(port uint32, key []byte) guestagent.DialFunc {
	return guestagent.PairedDial(func(ctx context.Context) (net.Conn, error) { return guestagent.DialHost(ctx, port) }, port, key)
}

// screenSharing dials the guest's own Screen Sharing, the desktop tunnel's only destination.
func screenSharing(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", guestagent.DesktopTarget)
	if err != nil {
		return nil, fmt.Errorf("Screen Sharing: %w", err)
	}
	return c, nil
}
