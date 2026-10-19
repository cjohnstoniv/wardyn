// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// wardynSSHConfig is the ssh_config file Wardyn owns. ~/.ssh/config reaches it
// with one Include line, added only after the user says yes.
const wardynSSHConfig = "~/.wardyn/ssh_config"

// runEditorCmd returns `wardyn run editor <run-id>`: write the run's Host block
// to ~/.wardyn/ssh_config and open VS Code Remote-SSH on it over the SSH gateway.
func runEditorCmd(client clientFn) *cobra.Command {
	var dir string
	var advertisedProxy bool
	cmd := &cobra.Command{
		Use:   "editor <run-id>",
		Short: "Open VS Code on a running sandbox over the SSH gateway",
		Long: `Open VS Code (Remote-SSH) on a RUNNING sandbox over the SSH gateway
(WARDYN_SSH_LISTEN), with your registered SSH key.

Writes the run's Host block to ~/.wardyn/ssh_config, a file Wardyn owns, then
runs: code --remote ssh-remote+wardyn-<short-id> <dir>. VS Code reads your
~/.ssh/config, so that file needs one line, "Include ~/.wardyn/ssh_config", at
the top; this command asks before adding it and, if you decline, prints the
line and stops. The Host block has the same HostName, Port, User and
ProxyCommand as 'wardyn run ssh --config', plus your key.

VS Code needs "remote.SSH.localServerDownload": "always" in its settings (the
sandbox has no internet); see docs/SSH.md.

Without the gateway or a registered key it stops with one line saying so. The
key is made by 'wardyn ssh-key ensure', never by this command.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\r\n") {
				return fmt.Errorf("editor: --dir %q must be an absolute path in the sandbox", dir)
			}
			acc, err := resolveSSHAccess(cmd.Context(), client(), "editor", args[0], advertisedProxy)
			if err != nil {
				return err
			}
			cfg, err := expandHome(wardynSSHConfig)
			if err != nil {
				return err
			}
			identity := ""
			if acc.identity != "" {
				identity = defaultSSHKeyPath
			}
			if err := upsertSSHHost(cfg, sshConfigBlock(acc.gw, args[0], identity)); err != nil {
				return fmt.Errorf("editor: write %s: %w", cfg, err)
			}
			included, err := ensureSSHInclude(cmd, cfg)
			if err != nil {
				return err
			}
			if !included {
				return nil
			}
			c := exec.CommandContext(cmd.Context(), "code", "--remote", "ssh-remote+wardyn-"+shortRunID(args[0]), dir)
			c.Env = cliutil.ScrubChildEnv(os.Environ())
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := c.Run(); err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					return fmt.Errorf("editor: `code` is not on your PATH; in VS Code, Remote-SSH: Connect to Host, then wardyn-%s", shortRunID(args[0]))
				}
				return fmt.Errorf("editor: code: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "/home/agent/work", "folder in the sandbox to open")
	cmd.Flags().BoolVar(&advertisedProxy, "advertised-proxy", false, "write the ProxyCommand this deployment advertises into the Host block (VS Code runs it on this computer)")
	return cmd
}

// upsertSSHHost writes block into the Wardyn-owned file at path, replacing the
// block for the same Host and keeping the others.
func upsertSSHHost(path, block string) error {
	host := strings.SplitN(block, "\n", 2)[0]
	var keep []string
	if raw, err := os.ReadFile(path); err == nil {
		var cur []string
		flush := func() {
			if len(cur) > 0 && cur[0] != host {
				keep = append(keep, strings.Join(cur, "\n")+"\n")
			}
			cur = nil
		}
		for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if strings.HasPrefix(ln, "Host ") {
				flush()
			}
			cur = append(cur, ln)
		}
		flush()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(append(keep, block), "\n")), 0o600)
}

// ensureSSHInclude reports whether ~/.ssh/config includes the Wardyn file,
// adding the line (first in the file, where ssh applies it to every host) only
// after the user agrees. A decline prints the line and returns false.
func ensureSSHInclude(cmd *cobra.Command, cfg string) (bool, error) {
	sshCfg, err := expandHome("~/.ssh/config")
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(sshCfg)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("editor: read %s: %w", sshCfg, err)
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && strings.EqualFold(f[0], "Include") && (f[1] == wardynSSHConfig || f[1] == cfg) {
			return true, nil
		}
	}
	line := "Include " + wardynSSHConfig
	fmt.Fprintf(cmd.ErrOrStderr(), "Add %q to the top of %s so VS Code can find the host? [y/N] ", line, sshCfg)
	ans, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		fmt.Fprintf(cmd.ErrOrStderr(), "\nNot changed. Put this line at the top of %s, then run the command again:\n  %s\n", sshCfg, line)
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(sshCfg), 0o700); err != nil {
		return false, fmt.Errorf("editor: %w", err)
	}
	mode := os.FileMode(0o600)
	if fi, serr := os.Stat(sshCfg); serr == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(sshCfg, append([]byte(line+"\n\n"), raw...), mode); err != nil {
		return false, fmt.Errorf("editor: write %s: %w", sshCfg, err)
	}
	return true, nil
}
