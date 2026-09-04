// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// sshHealthz is /healthz's "ssh" field, decoded straight from the anonymous
// GET the gateway pane also reads — see internal/api/sshgateway.go's
// sshGatewayHealthz and the console's ConnectSSHCard (run-detail-ssh.tsx),
// whose command/config strings this command reproduces byte-for-byte.
type sshHealthz struct {
	Enabled            bool   `json:"enabled"`
	AdvertiseAddr      string `json:"advertise_addr"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
}

// sshCmd returns the cobra command for `wardyn ssh <run-id>`. It is a
// SEPARATE command from `wardyn attach` on purpose: attach uses the admin
// bearer over a WebSocket, ssh uses a registered public key over the real SSH
// protocol — different credentials that must never silently switch on a flag.
func sshCmd(client clientFn) *cobra.Command {
	var doPrint bool
	var doConfig bool
	var doJSON bool
	cmd := &cobra.Command{
		Use:   "ssh <run-id>",
		Short: "Connect to a running sandbox over the SSH gateway",
		Long: `Connect to a RUNNING Wardyn sandbox over the SSH gateway
(WARDYN_SSH_LISTEN), authenticating with a registered SSH key.

Reads the gateway's advertised address from GET /healthz (anonymous, no
token needed) and execs the local ssh(1) binary. --print emits the command
instead of running it; --config emits an ssh_config Host block; --json emits
the target (host, port, username, host key fingerprint) for a script or an
external tool that dials the sandbox itself.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modes := 0
			for _, on := range []bool{doPrint, doConfig, doJSON} {
				if on {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("ssh: --print, --config and --json are mutually exclusive")
			}
			return runSSH(cmd, client(), args[0], doPrint, doConfig, doJSON)
		},
	}
	cmd.Flags().BoolVar(&doPrint, "print", false, "print the ssh command instead of running it")
	cmd.Flags().BoolVar(&doConfig, "config", false, "print an ssh_config Host block instead of running it")
	cmd.Flags().BoolVar(&doJSON, "json", false, "print the SSH target as JSON instead of connecting (host, port, username, host_key_fingerprint, command)")
	return cmd
}

// sshTarget is `wardyn ssh --json`'s output: everything an external tool needs
// to dial a sandbox over the gateway. Port is always populated (22 when the
// advertised address names none) so a consumer never has to apply ssh's
// default itself; Command is the exact `ssh` invocation --print would show.
type sshTarget struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	Command            string `json:"command"`
}

func runSSH(cmd *cobra.Command, c *sdk.Client, runID string, doPrint, doConfig, doJSON bool) error {
	// The run id is ARGV: it is spliced into ssh(1)'s "<run-id>@<host>"
	// argument, into the ssh_config User/Host block --config emits, and into
	// the command string --print/--json hand an operator to paste. Unvalidated,
	// a leading "-o..." became an ssh OPTION rather than a username
	// (ProxyCommand = arbitrary execution), an embedded newline injected a
	// fresh ssh_config directive, and a ";" rode into a pasted shell line.
	// Validated HERE, before the /healthz call and before any of the four
	// output paths, because all four route through this one function.
	//
	// A UUID is exactly what the gateway itself accepts (sshAuth's uuid.Parse,
	// internal/api/sshgateway.go) — anything else could never have connected,
	// so refusing it costs no legitimate use.
	if _, err := parseID("run", runID); err != nil {
		return err
	}
	raw, err := c.Healthz(cmd.Context())
	if err != nil {
		return err
	}
	var health struct {
		SSH *sshHealthz `json:"ssh"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		return fmt.Errorf("decode /healthz: %w", err)
	}
	if health.SSH == nil || !health.SSH.Enabled {
		return errors.New("ssh: gateway is off on this deployment (an operator enables it by setting WARDYN_SSH_LISTEN and WARDYN_SSH_ADVERTISE where wardynd starts)")
	}

	host, port := splitHostPort(health.SSH.AdvertiseAddr)
	// The gateway is up but publishes no reachable address (WARDYN_SSH_ADVERTISE
	// unset — /healthz carries it verbatim). Without this the command built
	// "ssh <run>@ -p" and handed ssh(1) an empty hostname, so the operator got
	// ssh's own resolver error for a Wardyn misconfiguration.
	if host == "" {
		return errors.New("ssh: the gateway is enabled but advertises no address — set WARDYN_SSH_ADVERTISE (the externally-reachable host[:port], e.g. \"wardyn.example.com:2222\") where wardynd runs")
	}
	target := fmt.Sprintf("%s@%s", runID, host)
	args := []string{target}
	if port != "" {
		args = append(args, "-p", port)
	}

	if doConfig {
		shortID := shortRunID(runID)
		configPort := port
		if configPort == "" {
			configPort = "22"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Host wardyn-%s\n  HostName %s\n  Port %s\n  User %s\n",
			shortID, host, configPort, runID)
		return nil
	}

	if doPrint {
		fmt.Fprintln(cmd.OutOrStdout(), "ssh "+strings.Join(args, " "))
		return nil
	}

	if doJSON {
		portNum := 22
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil {
				return fmt.Errorf("ssh: advertised port %q is not a number", port)
			}
			portNum = n
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(sshTarget{
			Host: host, Port: portNum, Username: runID,
			HostKeyFingerprint: health.SSH.HostKeyFingerprint,
			Command:            "ssh " + strings.Join(args, " "),
		})
	}

	sub := exec.CommandContext(cmd.Context(), "ssh", args...)
	// ssh(1) is a third-party binary that runs the operator's own
	// ProxyCommand/LocalCommand children and can SendEnv to the remote host.
	// It has no use for the admin bearer, the age master key or an API key.
	sub.Env = cliutil.ScrubChildEnv(os.Environ())
	sub.Stdin = os.Stdin
	sub.Stdout = os.Stdout
	sub.Stderr = os.Stderr
	if err := sub.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &exitError{code: exitErr.ExitCode(), err: fmt.Errorf("ssh: %w", err)}
		}
		return fmt.Errorf("ssh: %w", err)
	}
	return nil
}

// shortRunID mirrors the console's Host-block label (run-detail-ssh.tsx):
// the "run_" prefix stripped, then the first 8 characters.
func shortRunID(runID string) string {
	id := strings.TrimPrefix(runID, "run_")
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// bracketedHostPort matches a bracketed IPv6 literal with an optional port,
// e.g. "[::1]:2222" or "[2001:db8::1]".
var bracketedHostPort = regexp.MustCompile(`^\[([^\]]+)\](?::(\d+))?$`)

// splitHostPort divides an advertise_addr "host:port" (WARDYN_SSH_ADVERTISE)
// into its parts. A bare host with no port returns an empty port string
// (callers render the conventional default rather than guessing). Mirrors
// ui/src/app/components/screens/run-detail-ssh.tsx's splitHostPort exactly,
// including its bracketed-IPv6 and bare-IPv6 handling: a naive last-colon
// split mangles both a bracketed "[::1]:2222" and a bare "::1"/"2001:db8::1"
// literal, which (unlike a hostname or IPv4 host) always has 2+ colons of its
// own.
func splitHostPort(addr string) (host, port string) {
	if m := bracketedHostPort.FindStringSubmatch(addr); m != nil {
		return m[1], m[2]
	}
	if strings.Count(addr, ":") > 1 {
		return addr, ""
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, ""
	}
	return addr[:i], addr[i+1:]
}
