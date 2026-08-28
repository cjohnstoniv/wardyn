// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// defaultSSHKeyPath is where `ssh-key ensure` keeps the CLI's own keypair.
// Its own file, not ~/.ssh/id_ed25519: a key an external tool dials sandboxes
// with should be revocable (delete it in the console) without touching the
// human's everyday identity.
const defaultSSHKeyPath = "~/.wardyn/id_ed25519"

// sshKeyCmd returns `wardyn ssh-key`: the keys the SSH gateway trusts for the
// caller's own principal (docs/SSH.md §1). `ensure` is the scriptable half of
// the console's Account -> SSH keys page — a tool that needs to dial a sandbox
// calls it once per machine and gets back the identity file to use.
func sshKeyCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh-key",
		Short: "Manage the SSH keys the gateway trusts for your account",
	}
	cmd.AddCommand(sshKeyEnsureCmd(client), sshKeyListCmd(client))
	return cmd
}

// sshKeyEnsureResult is `ssh-key ensure --json`'s output.
type sshKeyEnsureResult struct {
	// Path is the private key file; Path+".pub" holds the authorized_keys line.
	Path        string `json:"path"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	// Generated is true when this call created the keypair.
	Generated bool `json:"generated"`
	// Registered is true when this call registered the key; false means the
	// fingerprint was already on the account (the call is idempotent).
	Registered bool `json:"registered"`
}

func sshKeyEnsureCmd(client clientFn) *cobra.Command {
	var path, name string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Create the CLI's SSH keypair if missing and register its public key (idempotent)",
		Long: `Make sure this machine holds an SSH key the gateway trusts for your account.

Generates an ed25519 keypair at --path when none exists, then registers the
public key via POST /api/v1/me/ssh-keys unless its fingerprint is already on
the account. Safe to run on every launch: a second call changes nothing.

Under SSO the key must be registered by YOU, not by the deployment's admin
token (a key on the admin-token principal can never open a run you created):
authenticate the CLI with your own API token (WARDYN_TOKEN) or add the key in
the console under Account -> SSH keys instead.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHKeyEnsure(cmd, client(), path, name, asJSON)
		},
	}
	cmd.Flags().StringVar(&path, "path", defaultSSHKeyPath, "private key file to create/use (its .pub sibling is the registered key)")
	cmd.Flags().StringVar(&name, "name", "", "display name for a newly registered key (default: wardyn-cli@<hostname>)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {path, public_key, fingerprint, generated, registered} as JSON")
	return cmd
}

func runSSHKeyEnsure(cmd *cobra.Command, c *sdk.Client, path, name string, asJSON bool) error {
	path, err := expandHome(path)
	if err != nil {
		return err
	}
	pub, generated, err := ensureLocalSSHKey(path)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	fp := ssh.FingerprintSHA256(pub)

	keys, err := c.ListSSHKeys(cmd.Context())
	if err != nil {
		return fmt.Errorf("ssh-key: list registered keys: %w", err)
	}
	registered := false
	known := false
	for _, k := range keys {
		if k.Fingerprint == fp {
			known = true
			break
		}
	}
	if !known {
		if name == "" {
			host, _ := os.Hostname()
			if host == "" {
				host = "unknown-host"
			}
			name = "wardyn-cli@" + host
		}
		if _, err := c.AddSSHKey(cmd.Context(), name, line); err != nil {
			return fmt.Errorf("ssh-key: register %s: %w", fp, err)
		}
		registered = true
	}

	res := sshKeyEnsureResult{Path: path, PublicKey: line, Fingerprint: fp, Generated: generated, Registered: registered}
	if asJSON {
		return emitJSON(res)
	}
	out := cmd.OutOrStdout()
	switch {
	case generated:
		fmt.Fprintf(out, "generated %s\n", path)
	default:
		fmt.Fprintf(out, "using %s\n", path)
	}
	if registered {
		fmt.Fprintf(out, "registered %s as %q\n", fp, name)
	} else {
		fmt.Fprintf(out, "already registered: %s\n", fp)
	}
	return nil
}

func sshKeyListCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the SSH keys registered for your account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			keys, err := client().ListSSHKeys(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				if keys == nil {
					keys = []sdk.SSHPublicKey{}
				}
				return emitJSON(keys)
			}
			tw := newTab()
			fmt.Fprintln(tw, "FINGERPRINT\tNAME\tROLE\tCREATED")
			for _, k := range keys {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Fingerprint, k.Name, k.Role, k.CreatedAt.Format("2006-01-02"))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// ensureLocalSSHKey loads the ed25519 key at path, generating it (0600, plus a
// 0644 .pub sibling) when absent. It refuses to overwrite: an existing file
// that does not parse is an error, never silently replaced — the registered
// fingerprint would no longer match anything on disk.
func ensureLocalSSHKey(path string) (pub ssh.PublicKey, generated bool, err error) {
	if raw, rerr := os.ReadFile(path); rerr == nil {
		key, perr := ssh.ParseRawPrivateKey(raw)
		if perr != nil {
			return nil, false, fmt.Errorf("ssh-key: %s exists but is not a readable private key (%v) — move it aside or pass --path", path, perr)
		}
		signer, serr := ssh.NewSignerFromKey(key)
		if serr != nil {
			return nil, false, fmt.Errorf("ssh-key: %s: %w", path, serr)
		}
		return signer.PublicKey(), false, nil
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return nil, false, fmt.Errorf("ssh-key: read %s: %w", path, rerr)
	}

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("ssh-key: generate: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privKey, "wardyn-cli")
	if err != nil {
		return nil, false, fmt.Errorf("ssh-key: encode: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, false, fmt.Errorf("ssh-key: public key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("ssh-key: create %s: %w", filepath.Dir(path), err)
	}
	// O_EXCL: two concurrent first runs must not both "win" and register two
	// different keys under one path.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("ssh-key: create %s: %w", path, err)
	}
	if err := pem.Encode(f, block); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("ssh-key: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("ssh-key: write %s: %w", path, err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return nil, false, fmt.Errorf("ssh-key: write %s.pub: %w", path, err)
	}
	return sshPub, true, nil
}

// expandHome resolves a leading "~/" against the user's home directory.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve ~: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}
