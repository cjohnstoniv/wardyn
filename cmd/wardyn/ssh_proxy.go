// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/loopback"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// SSH over a single-use ticket (WARDYN_SSH_TICKETS): `run ssh`, `exec` and `forward` mint the ticket and hand it to
// their ssh(1) child, whose ProxyCommand is `wardyn run ssh-proxy`; `run editor` writes a Host block whose
// ProxyCommand mints for each connection. The proxy carries the SSH bytes over GET /api/v1/runs/{id}/ssh.

// sshTicketEnv carries the ticket from a parent command to the proxy, in ssh(1)'s environment only: the parent never
// sets it in its own, and the proxy unsets it before it dials.
const sshTicketEnv = "WARDYN_SSH_TICKET"

// sshKnownHosts is where the ticket path pins the gateway's host key, under gatewayHostKeyAlias.
const sshKnownHosts = "~/.wardyn/known_hosts"

// sshTicketPurposes is the ticket purpose each ProxyCommand verb mints.
var sshTicketPurposes = map[string]string{
	"ssh": "ssh-shell", "exec": "ssh-exec", "forward": "ssh-forward", "editor": "ssh-editor",
}

// sshProxyCmd returns `wardyn run ssh-proxy <run-id> --for ssh|exec|forward|editor`: the ProxyCommand ssh(1) runs on
// the ticket path. Its stdout carries SSH bytes only.
func sshProxyCmd(client clientFn) *cobra.Command {
	var verb, ports string
	cmd := &cobra.Command{
		Use:   "ssh-proxy <run-id> --for ssh|exec|forward|editor",
		Short: "Carry one SSH connection to a run over the HTTPS API (run by ssh as a ProxyCommand)",
		Long: `Carry one SSH connection to a RUNNING sandbox over the HTTPS API
(GET /api/v1/runs/{id}/ssh), with a single-use SSH ticket. ssh(1) runs this
command as its ProxyCommand; 'wardyn run ssh', 'exec', 'forward' and 'editor'
write that line for you when the deployment turns SSH tickets on
(WARDYN_SSH_TICKETS).

The ticket comes from the command that started ssh, through its environment,
and is cleared before the connection is made. With none handed over (VS Code,
or 'ssh wardyn-<run-id>' on a Host block 'run editor' wrote), this command asks
for one with your sign-in (wardyn login). Standard output carries the SSH
connection and nothing else.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHProxy(cmd, client(), args[0], verb, ports)
		},
	}
	cmd.Flags().StringVar(&verb, "for", "", "the command this connection serves: ssh, exec, forward or editor")
	cmd.Flags().StringVar(&ports, "ports", "", "with --for forward and no ticket handed over, the sandbox ports to ask a ticket for (a comma list)")
	return cmd
}

func runSSHProxy(cmd *cobra.Command, c *sdk.Client, runID, verb, ports string) error {
	// First, so nothing this process dials or starts later carries the ticket in its environment.
	tok := os.Getenv(sshTicketEnv)
	if err := os.Unsetenv(sshTicketEnv); err != nil {
		return fmt.Errorf("ssh-proxy: %w", err)
	}
	purpose, ok := sshTicketPurposes[verb]
	if !ok {
		return errors.New("ssh-proxy: --for must be ssh, exec, forward or editor")
	}
	id, err := parseID("run", runID)
	if err != nil {
		return err
	}
	runID = id.String()
	door, err := sshDoorURL(c.BaseURL, runID)
	if err != nil {
		return fmt.Errorf("ssh-proxy: %w", err)
	}
	if tok == "" {
		if tok, err = proxyMint(cmd.Context(), c, runID, verb, purpose, ports); err != nil {
			return err
		}
	}
	return sshTunnel(cmd.Context(), c.BaseURL, door, tok, cmd.InOrStdin(), cmd.OutOrStdout())
}

// proxyMint is the proxy's own mint, when no ticket was handed over: the deployment must offer tickets, the gateway's
// host key is pinned again, and the ticket must be the one asked for.
func proxyMint(ctx context.Context, c *sdk.Client, runID, verb, purpose, ports string) (string, error) {
	if c.Token == "" {
		return "", errors.New("ssh-proxy: sign in first (wardyn login)")
	}
	q := url.Values{"purpose": {purpose}}
	switch {
	case verb == "forward" && ports == "":
		return "", errors.New("ssh-proxy: --for forward with no ticket handed over needs --ports")
	case verb == "forward":
		q.Set("ports", ports)
	case ports != "":
		return "", errors.New("ssh-proxy: --ports goes only with --for forward")
	}
	gw, err := resolveSSHGateway(ctx, c)
	if err != nil {
		return "", err
	}
	if !gw.tickets {
		return "", fmt.Errorf("%s: this deployment does not offer SSH tickets (WARDYN_SSH_TICKETS); run again with --key", verb)
	}
	if err := refreshGatewayHostKey(verb, c.BaseURL, gw); err != nil {
		return "", err
	}
	return mintSSHTicket(ctx, c, verb, runID, q)
}

// proxyRefusal is the door's own refusal: its sentence, and the API error that sets the exit code.
type proxyRefusal struct {
	msg string
	err error
}

func (e *proxyRefusal) Error() string { return "ssh-proxy: " + e.msg }
func (e *proxyRefusal) Unwrap() error { return e.err }

// sshTunnel dials the ticket door, with the ticket in a header and never in the URL, and copies bytes both ways until
// either side ends.
func sshTunnel(ctx context.Context, base, door, tok string, in io.Reader, out io.Writer) error {
	ws, resp, err := websocket.Dial(ctx, door, &websocket.DialOptions{
		HTTPHeader: http.Header{"Wardyn-Ticket": []string{tok}},
		HTTPClient: orgClient(base, 0),
	})
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			ae := sdk.NewAPIError(resp.StatusCode, body)
			return &proxyRefusal{msg: apiSentence(ae), err: ae}
		}
		return fmt.Errorf("ssh-proxy: %w", err)
	}
	// NetConn sets no read limit. Either direction ending ends the connection: ssh closes its side when it is done.
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer nc.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.CopyBuffer(nc, in, make([]byte, 32<<10)); done <- struct{}{} }()
	go func() { _, _ = io.CopyBuffer(out, nc, make([]byte, 32<<10)); done <- struct{}{} }()
	<-done
	return nil
}

// mintSSHTicket mints a ticket of q's purpose for verb and refuses one the server did not mint for that purpose: an
// older server ignores the purpose and mints a terminal ticket. It never falls back to a dial with no ticket.
func mintSSHTicket(ctx context.Context, c *sdk.Client, verb, runID string, q url.Values) (string, error) {
	tok, echo, err := mintTicket(ctx, c, runID, q)
	if err != nil {
		return "", ticketMintError(verb, err)
	}
	if echo != q.Get("purpose") {
		return "", fmt.Errorf("%s: this server minted a ticket that does not open SSH; run again with --key", verb)
	}
	return tok, nil
}

// ticketMintError is one line for a refused mint. A support helper and a portal or service are pointed to --key; a
// person denied ssh_key gets the server's sentence and no --key, since that deny is the operator's.
func ticketMintError(verb string, err error) error {
	var ae *sdk.APIError
	if !errors.As(err, &ae) {
		return fmt.Errorf("%s: could not get an SSH ticket: %w", verb, err)
	}
	switch ae.Reason {
	case "support_door_not_granted":
		return fmt.Errorf("%s: SSH tickets are for the run's owner; a support helper connects with a registered key: run again with --key", verb)
	case "ssh_ticket_unsupported_caller":
		return fmt.Errorf("%s: SSH tickets need a person signed in directly (wardyn login); run again with --key to use a registered key", verb)
	case "capability_feature":
		return errors.New(verb + ": " + apiSentence(ae))
	}
	return fmt.Errorf("%s: %w", verb, err)
}

// runTicketSSH is the ticket path of `run ssh`, `exec` and `forward`: pin the gateway's host key, build the
// ProxyCommand, mint, then run ssh(1) with the ticket in its environment only. front goes before the connection
// options, tail after the target.
func runTicketSSH(ctx context.Context, c *sdk.Client, gw sshGateway, verb, runID string, q url.Values, front, tail []string) error {
	runID = uuid.MustParse(runID).String() // resolveSSHAccess or runSSH checked it
	if err := refreshGatewayHostKey(verb, c.BaseURL, gw); err != nil {
		return err
	}
	conn, err := ticketSSHArgs(verb, c.BaseURL, runID)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	tok, err := mintSSHTicket(ctx, c, verb, runID, q)
	if err != nil {
		return err
	}
	args := append(append(append([]string{}, front...), conn...), tail...)
	return runSSHBinaryEnv(ctx, args, []string{sshTicketEnv + "=" + tok})
}

// ticketExecArgs is `run exec`'s ticket-path argv around the connection options: no terminal, and each word quoted
// to arrive as one argument, as execSSHArgs does.
func ticketExecArgs(argv []string) (front, tail []string) {
	words := make([]string, len(argv))
	for i, w := range argv {
		words[i] = remoteWord(w)
	}
	return []string{"-T"}, []string{"--", strings.Join(words, " ")}
}

// ticketPorts is the sandbox ports a forward ticket names: the pairs' remote ports, each once, at most 16.
func ticketPorts(pairs []forwardPair) (string, error) {
	var ports []string
	seen := map[int]bool{}
	for _, p := range pairs {
		if !seen[p.remote] {
			seen[p.remote] = true
			ports = append(ports, strconv.Itoa(p.remote))
		}
	}
	if len(ports) > 16 {
		return "", errors.New("forward: a ticket covers at most 16 sandbox ports; split the command")
	}
	return strings.Join(ports, ","), nil
}

// ticketSSHArgs is ssh(1)'s connection options on the ticket path: the ProxyCommand, the pinned host key, no
// multiplexing (a master would skip the ProxyCommand, and an ssh-exec ticket serves one command), and the target.
// No key is offered.
func ticketSSHArgs(verb, base, runID string) ([]string, error) {
	line, err := thisProxyCommand(verb, base, runID)
	if err != nil {
		return nil, err
	}
	return []string{
		"-o", "ProxyCommand=" + line,
		"-o", "HostKeyAlias=" + gatewayHostKeyAlias(base),
		"-o", "UserKnownHostsFile=" + sshKnownHosts,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		runID + "@" + sshHostAlias(runID),
	}, nil
}

// thisProxyCommand is proxyCommandLine for this program on this OS.
func thisProxyCommand(verb, base, runID string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot find this program's own path for the ProxyCommand (%w); run again with --key", err)
	}
	return proxyCommandLine(runtime.GOOS, exe, runID, verb, base)
}

// editorTicketBlock pins the gateway's host key and returns `run editor`'s Host block on the ticket path.
func editorTicketBlock(base string, gw sshGateway, runID string) (string, error) {
	runID = uuid.MustParse(runID).String() // resolveSSHAccess checked it
	if err := refreshGatewayHostKey("editor", base, gw); err != nil {
		return "", err
	}
	line, err := thisProxyCommand("editor", base, runID)
	if err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	return ticketConfigBlock(base, runID, line), nil
}

// ticketConfigBlock is `run editor`'s Host block on the ticket path: the proxy mints for each connection, so the
// block holds no ticket and no key.
func ticketConfigBlock(base, runID, line string) string {
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = u.Hostname()
	}
	return fmt.Sprintf("Host %s\n  HostName %s\n  User %s\n  ProxyCommand %s\n  HostKeyAlias %s\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n",
		sshHostAlias(runID), host, runID, line, gatewayHostKeyAlias(base), sshKnownHosts)
}

// proxyCommandLine is the ProxyCommand that runs `wardyn run ssh-proxy`: every variable word quoted for goos's
// rules, then every % doubled, since ssh expands % tokens anywhere in the line.
func proxyCommandLine(goos, exe, runID, verb, base string) (string, error) {
	line, err := proxyCommandWords(goos, exe, runID, verb, base)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(line, "%", "%%"), nil
}

// proxyCommandWords is proxyCommandLine before the % doubling: the words a shell (POSIX) or the C runtime's argument
// rules (Windows) split back.
func proxyCommandWords(goos, exe, runID, verb, base string) (string, error) {
	if exe == "" || strings.ContainsAny(exe, `'"`) || strings.IndexFunc(exe, unicode.IsControl) >= 0 {
		return "", errors.New("this program's path holds a quote or a control character, so it cannot go in a ProxyCommand; run again with --key")
	}
	if _, err := checkTicketBase(base); err != nil {
		return "", err
	}
	quote, prog := shellQuote, shellQuote
	if goos == "windows" {
		quote, prog = windowsArgQuote, windowsProgQuote
	}
	return strings.Join([]string{prog(exe), "run", "ssh-proxy", quote(runID), "--for", quote(verb), "--url", quote(base)}, " "), nil
}

// windowsProgQuote quotes the program path, the first word, which the C runtime reads with no backslash rules: a
// quote only groups. The path holds no quote (proxyCommandWords).
func windowsProgQuote(s string) string { return `"` + s + `"` }

// windowsArgQuote quotes a later word by the C runtime's rules: a run of backslashes is literal unless a quote
// follows, so only the run before the closing quote is doubled. The word holds no quote (proxyCommandWords,
// checkTicketBase).
func windowsArgQuote(s string) string {
	n := len(s) - len(strings.TrimRight(s, `\`))
	return `"` + s + strings.Repeat(`\`, n) + `"`
}

// checkTicketBase is the server URL a ticket path may use: one that parses, https or http on a loopback host
// (refusePlainHTTPGateway's rule), with no quote and no control character, since it goes into a ProxyCommand.
func checkTicketBase(base string) (*url.URL, error) {
	if strings.ContainsAny(base, `'"`) || strings.IndexFunc(base, unicode.IsControl) >= 0 {
		return nil, errors.New("the server URL holds a quote or a control character, so it cannot go in a ProxyCommand")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("the server URL does not parse: %w", err)
	}
	switch {
	case u.Host == "":
		return nil, errors.New("the server URL has no host")
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback.Host(u.Hostname()):
	default:
		return nil, fmt.Errorf("the server URL must be https:// (or http:// on a loopback address), not %q", u.Scheme+"://")
	}
	return u, nil
}

// sshDoorURL is the ticket door's WebSocket URL: the server URL's host and path, no query.
func sshDoorURL(base, runID string) (string, error) {
	u, err := checkTicketBase(base)
	if err != nil {
		return "", err
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/") + "/api/v1/runs/" + runID + "/ssh", nil
}

// gatewayHostKeyAlias is the name the gateway's host key is pinned under: one per API host, so a rotated key
// replaces its own line.
func gatewayHostKeyAlias(base string) string {
	host := base
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}
	return "wardyn-gateway-" + strings.ReplaceAll(strings.ToLower(host), ":", "-")
}

// refreshGatewayHostKey checks /healthz's host_key against its fingerprint and rewrites that key's one line in
// ~/.wardyn/known_hosts, keeping every other line.
func refreshGatewayHostKey(verb, base string, gw sshGateway) error {
	if gw.hostKey == "" {
		return fmt.Errorf("%s: the gateway publishes no host key for SSH tickets, so its identity cannot be checked; ask your operator, or run again with --key", verb)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(gw.hostKey))
	if err != nil {
		return fmt.Errorf("%s: the gateway's published host key does not parse: %w", verb, err)
	}
	if got := ssh.FingerprintSHA256(key); got != gw.fingerprint {
		return fmt.Errorf("%s: the gateway's published host key %s is not its advertised fingerprint %s; ask your operator", verb, got, gw.fingerprint)
	}
	path, err := expandHome(sshKnownHosts)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: read %s: %w", verb, path, err)
	}
	alias := gatewayHostKeyAlias(base)
	var b strings.Builder
	for _, ln := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(ln); len(f) == 0 || f[0] == alias {
			continue
		}
		b.WriteString(ln + "\n")
	}
	b.WriteString(alias + " " + string(ssh.MarshalAuthorizedKey(key)))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	if err := writeFileAtomic(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("%s: write %s: %w", verb, path, err)
	}
	return nil
}
