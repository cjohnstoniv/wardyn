// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
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
	ProxyCommand       string `json:"proxy_command"`
}

// sshCmd returns the cobra command for `wardyn run ssh <run-id>`. It is a
// SEPARATE command from `wardyn run attach` on purpose: attach uses the admin
// bearer over a WebSocket, ssh uses a registered public key over the real SSH
// protocol — different credentials that must never silently switch on a flag.
func sshCmd(client clientFn) *cobra.Command {
	var doPrint bool
	var doConfig bool
	var doJSON bool
	var advertisedProxy bool
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

When the deployment advertises a ProxyCommand (WARDYN_SSH_PROXY_COMMAND, for
a gateway reached through a TLS-terminating listener on 443), all three
outputs carry it. Connecting runs that command on this computer, so it needs
--advertised-proxy; without the flag the command is shown and nothing runs.
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
			return runSSH(cmd, client(), args[0], sshModes{doPrint, doConfig, doJSON, advertisedProxy})
		},
	}
	cmd.Flags().BoolVar(&doPrint, "print", false, "print the ssh command instead of running it")
	cmd.Flags().BoolVar(&doConfig, "config", false, "print an ssh_config Host block instead of running it")
	cmd.Flags().BoolVar(&doJSON, "json", false, "print the SSH target as JSON instead of connecting (host, port, username, host_key_fingerprint, proxy_command, command)")
	cmd.Flags().BoolVar(&advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

// sshGateway is where the gateway is reached, as /healthz advertises it.
type sshGateway struct {
	host, port, fingerprint, proxy string
}

// resolveSSHGateway reads GET /healthz and returns the advertised gateway,
// refusing a gateway that is off, advertises no address, or publishes a
// ProxyCommand that breaks out of a quoted one-liner. Shared by `run ssh` and
// `sync`, so both reach the gateway the same way.
func resolveSSHGateway(ctx context.Context, c *sdk.Client) (sshGateway, error) {
	raw, err := c.Healthz(ctx)
	if err != nil {
		return sshGateway{}, err
	}
	var health struct {
		SSH *sshHealthz `json:"ssh"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		return sshGateway{}, fmt.Errorf("decode /healthz: %w", err)
	}
	if health.SSH == nil || !health.SSH.Enabled {
		return sshGateway{}, errors.New("ssh: gateway is off on this deployment (an operator enables it by setting WARDYN_SSH_LISTEN and WARDYN_SSH_ADVERTISE where wardynd starts)")
	}
	host, port := splitHostPort(health.SSH.AdvertiseAddr)
	// The gateway is up but publishes no reachable address (WARDYN_SSH_ADVERTISE
	// unset — /healthz carries it verbatim). Without this check, the command
	// would build "ssh <run>@ -p" and hand ssh(1) an empty hostname, so the
	// operator would see ssh's own resolver error for a Wardyn misconfiguration.
	if host == "" {
		return sshGateway{}, errors.New("ssh: the gateway is enabled but advertises no address — set WARDYN_SSH_ADVERTISE (the externally-reachable host[:port], e.g. \"wardyn.example.com:2222\") where wardynd runs")
	}
	if err := checkGatewayAddr(host, port); err != nil {
		return sshGateway{}, err
	}
	proxy := health.SSH.ProxyCommand
	// The daemon refuses these at boot; a skewed or hostile one could still
	// publish a value that breaks out of the quoted one-liner or the config line.
	if err := cliutil.CheckSSHProxyCommand(proxy); err != nil {
		return sshGateway{}, fmt.Errorf("ssh: this deployment advertises a ProxyCommand that %v, so it is not shown or run; ask your operator", err)
	}
	return sshGateway{host: host, port: port, fingerprint: health.SSH.HostKeyFingerprint, proxy: proxy}, nil
}

// checkGatewayAddr refuses an advertised address that is not a DNS name or IP
// literal with a numeric port. The value is spliced into an ssh_config line, a
// pasted shell line and a ProxyCommand's %h and %p, so anything else (a
// newline, a $(...), a quote) would run or inject something.
func checkGatewayAddr(host, port string) error {
	if a, err := netip.ParseAddr(host); err != nil || a.Zone() != "" {
		if len(host) > 253 || !gatewayDNSName.MatchString(host) {
			return fmt.Errorf("ssh: the gateway advertises the host %q, which is not a DNS name or an IP address, so it is not used; ask your operator", host)
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
			return fmt.Errorf("ssh: the gateway advertises the port %q, which is not a number from 1 to 65535, so it is not used; ask your operator", port)
		}
	}
	return nil
}

var gatewayDNSName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// sshTarget is `wardyn run ssh --json`'s output: everything an external tool needs
// to dial a sandbox over the gateway. Port is always populated (22 when the
// advertised address names none) so a consumer never has to apply ssh's
// default itself; Command is the exact `ssh` invocation --print would show.
type sshTarget struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	ProxyCommand       string `json:"proxy_command,omitempty"`
	Command            string `json:"command"`
}

// sshModes is the command's flags: at most one of print, config and json, and
// advertisedProxy, the consent to run a server-supplied ProxyCommand.
type sshModes struct {
	print, config, json, advertisedProxy bool
}

func runSSH(cmd *cobra.Command, c *sdk.Client, runID string, m sshModes) error {
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
	gw, err := resolveSSHGateway(cmd.Context(), c)
	if err != nil {
		return err
	}
	host, port, proxy := gw.host, gw.port, gw.proxy
	target := fmt.Sprintf("%s@%s", runID, host)
	args := []string{target}
	printed := "ssh "
	if proxy != "" {
		args = append([]string{"-o", "ProxyCommand=" + proxy}, args...)
		printed += "-o ProxyCommand='" + proxy + "' "
	}
	printed += target
	if port != "" {
		args = append(args, "-p", port)
		printed += " -p " + port
	}

	if m.config {
		shortID := shortRunID(runID)
		configPort := port
		if configPort == "" {
			configPort = "22"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Host wardyn-%s\n  HostName %s\n  Port %s\n  User %s\n",
			shortID, host, configPort, runID)
		if proxy != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  ProxyCommand %s\n", proxy)
		}
		return nil
	}

	if m.print {
		fmt.Fprintln(cmd.OutOrStdout(), printed)
		return nil
	}

	if m.json {
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
			HostKeyFingerprint: gw.fingerprint,
			ProxyCommand:       proxy,
			Command:            printed,
		})
	}

	if proxy != "" && !m.advertisedProxy {
		return fmt.Errorf("ssh: this deployment advertises a ProxyCommand, which ssh would run on this computer:\n  %s\n"+
			"run again with --advertised-proxy to connect through it, or use --print or --config to copy it", proxy)
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
			// docs/CI.md documents ssh's exit status passing straight through
			// as this command's own exit code. ExitCode() is documented to
			// return -1 for a signal-killed child (a supervisor's SIGTERM, an
			// operator's Ctrl-C reaching ssh directly) — that is not part of
			// the documented taxonomy (every code there is >= 0), and
			// os.Exit(-1) in main.go would not do what a negative "exit
			// code" implies, so it is clamped to the generic local-failure
			// code instead of leaking out unchanged.
			code := exitErr.ExitCode()
			if code < 0 {
				code = 1
			}
			return &exitError{code: code, err: fmt.Errorf("ssh: %w", err)}
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
