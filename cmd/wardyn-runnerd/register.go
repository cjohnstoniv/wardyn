// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func newRegisterCommand() *cobra.Command {
	var org, tokenFile, stateDir, name string
	cmd := &cobra.Command{Use: "register", Short: "Register this host; its owner must then verify and claim it", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := federation.CheckOrgURL(org); err != nil {
			return err
		}
		if stateDir == "" {
			var err error
			stateDir, err = runneridentity.DefaultDir()
			if err != nil {
				return err
			}
		}
		if name == "" {
			var err error
			name, err = os.Hostname()
			if err != nil {
				return err
			}
		}
		token, err := readRegistrationToken(tokenFile)
		if err != nil {
			return err
		}
		key, err := runneridentity.Generate(stateDir)
		if err != nil {
			return err
		}
		client := sdk.New(strings.TrimSpace(org), "")
		client.HTTPClient = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("runner registration does not follow redirects")
		}}
		registered, err := client.RegisterRunner(cmd.Context(), sdk.RunnerRegisterRequest{Token: token, PublicKey: key.Public().(ed25519.PublicKey), Name: name})
		if err != nil {
			return errors.New("runner registration failed; the local key was preserved. Obtain a fresh token and choose a new private state directory before retrying")
		}
		fingerprint := runnerwire.Fingerprint(key.Public().(ed25519.PublicKey))
		if registered.RunnerID == uuid.Nil || registered.State != types.RunnerUnclaimed || registered.Fingerprint != fingerprint || registered.OrgURLSHA256 != federation.OrgURLSHA256(org) {
			return errors.New("registration response does not match this key and organisation; identity was not activated")
		}
		if err := runneridentity.Save(stateDir, runneridentity.Identity{RunnerID: registered.RunnerID, PrivateKey: key, Fingerprint: fingerprint, OrgURLSHA256: registered.OrgURLSHA256}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Runner %s is unclaimed.\nFingerprint: %s\nSign in as its owner and run wardyn runner claim with this state directory.\n", registered.RunnerID, fingerprint)
		return nil
	}
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file containing the single-use registration token")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&name, "name", "", "runner name (defaults to the host name)")
	_ = cmd.MarkFlagRequired("org")
	_ = cmd.MarkFlagRequired("token-file")
	return cmd
}

func readRegistrationToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open registration token file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", errors.New("read registration token file")
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 4096 || len(token) != 68 || !strings.HasPrefix(token, "wdr_") {
		return "", errors.New("registration token file must contain one wdr_ token")
	}
	return token, nil
}
