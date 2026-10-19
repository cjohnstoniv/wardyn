// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Git over SSH signs in through the run proxy's ssh agent, so an ssh_key
// grant's private key never enters the sandbox (internal/egress/proxy
// ssh_agent.go):
//
//	wardyn-git-helper ssh-exec --agent <name>=<grant id>... -- ssh <args>
//	  git's GIT_SSH_COMMAND. Listens on <dir>/<name>.sock for each grant in a
//	  fresh 0700 directory, runs ssh with WARDYN_SSH_AGENT_DIR=<dir> (the ssh
//	  config's IdentityAgent), and removes the directory when ssh exits, so
//	  the sockets exist only for that one git operation. Each agent frame is
//	  relayed to the proxy's ssh agent route for the grant.
//	wardyn-git-helper ssh-known-hosts <grant id>
//	  ssh's KnownHostsCommand: prints the grant's operator-supplied known_hosts.
const (
	sshAgentRoute = "/wardyn/v1/ssh-agent/"
	// maxAgentFrame matches the proxy's bound on one agent request.
	maxAgentFrame = 64 << 10
	// sshAgentCallTimeout covers the proxy's first-use mint of the key.
	sshAgentCallTimeout = 150 * time.Second
)

var agentName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// agentFailure is SSH_AGENT_FAILURE, answered when the proxy cannot be asked.
var agentFailure = []byte{0, 0, 0, 1, 5}

// runSSHVerb runs an ssh verb and returns its exit status.
func runSSHVerb(verb string, args []string, stdout, stderr io.Writer) int {
	proxyURL := strings.TrimRight(os.Getenv("WARDYN_PROXY_URL"), "/")
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: sshAgentCallTimeout}
	switch verb {
	case "ssh-exec":
		return sshExec(client, proxyURL, args, stderr)
	case "ssh-known-hosts":
		if len(args) != 1 || uuid.Validate(args[0]) != nil {
			fmt.Fprintln(stderr, "wardyn-git-helper: usage: wardyn-git-helper ssh-known-hosts <grant id>")
			return 2
		}
		return sshKnownHosts(client, proxyURL, args[0], stdout, stderr)
	}
	return 2
}

func sshExec(client *http.Client, proxyURL string, args []string, stderr io.Writer) int {
	grants := map[string]string{}
	i := 0
	for ; i < len(args) && args[i] != "--"; i += 2 {
		name, grant, ok := strings.Cut(argAt(args, i+1), "=")
		if args[i] != "--agent" || !ok || !agentName.MatchString(name) || uuid.Validate(grant) != nil {
			fmt.Fprintln(stderr, "wardyn-git-helper: usage: wardyn-git-helper ssh-exec --agent <name>=<grant id>... -- <command> [args]")
			return 2
		}
		grants[name] = grant
	}
	if i+1 >= len(args) || len(grants) == 0 {
		fmt.Fprintln(stderr, "wardyn-git-helper: usage: wardyn-git-helper ssh-exec --agent <name>=<grant id>... -- <command> [args]")
		return 2
	}
	command := args[i+1:]

	dir, err := os.MkdirTemp("", "wardyn-ssh-agent-")
	if err != nil {
		fmt.Fprintln(stderr, "wardyn-git-helper: ssh agent directory:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, grant := range grants {
		l, err := net.Listen("unix", filepath.Join(dir, name+".sock"))
		if err != nil {
			fmt.Fprintln(stderr, "wardyn-git-helper: ssh agent socket:", err)
			return 1
		}
		defer func() { _ = l.Close() }()
		go serveAgent(l, client, proxyURL+sshAgentRoute+grant)
	}

	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // git's own ssh command line
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "WARDYN_SSH_AGENT_DIR="+dir)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "wardyn-git-helper:", err)
		return 1
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode()
	default:
		fmt.Fprintln(stderr, "wardyn-git-helper:", err)
		return 255 // what ssh itself exits with on a failure of its own
	}
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func serveAgent(l net.Listener, client *http.Client, route string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go bridgeAgent(c, client, route)
	}
}

// bridgeAgent relays each agent frame on c to the proxy and its answer back.
func bridgeAgent(c net.Conn, client *http.Client, route string) {
	defer func() { _ = c.Close() }()
	for {
		var head [4]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(head[:])
		if n == 0 || n > maxAgentFrame {
			return
		}
		frame := make([]byte, 4+n)
		copy(frame, head[:])
		if _, err := io.ReadFull(c, frame[4:]); err != nil {
			return
		}
		if _, err := c.Write(askAgent(client, route, frame)); err != nil {
			return
		}
	}
}

// askAgent is the proxy's answer frame, or SSH_AGENT_FAILURE.
func askAgent(client *http.Client, route string, frame []byte) []byte {
	resp, err := client.Post(route, "application/octet-stream", bytes.NewReader(frame))
	if err != nil {
		return agentFailure
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentFrame+4))
	if err != nil || resp.StatusCode != http.StatusOK || len(reply) < 5 || int(binary.BigEndian.Uint32(reply)) != len(reply)-4 {
		return agentFailure
	}
	return reply
}

func sshKnownHosts(client *http.Client, proxyURL, grant string, stdout, stderr io.Writer) int {
	resp, err := client.Get(proxyURL + sshAgentRoute + grant + "/known_hosts")
	if err != nil {
		fmt.Fprintln(stderr, "wardyn-git-helper: known_hosts:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAgentFrame))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "wardyn-git-helper: known_hosts: proxy answered %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	_, _ = stdout.Write(body)
	return 0
}
