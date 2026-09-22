// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// deviceCmd is the organisation admin's surface for hybrid enrolment: mint the
// single-use token a managed laptop's first boot trades for its device
// credential, see which laptops are enrolled, and cut one off. See
// internal/api/devices.go for the server side.
func deviceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Manage enrolled laptops (hybrid enrolment)",
	}

	var name string
	enrolToken := &cobra.Command{
		Use:   "enrol-token",
		Short: "Mint a single-use enrolment token for one laptop",
		Long: "Mint a single-use enrolment token for one managed laptop. Deliver it to that\n" +
			"laptop as WARDYN_ORG_ENROLMENT_TOKEN; its first boot trades it for a device\n" +
			"credential. The token is printed ONCE on stdout (the server keeps only a hash)\n" +
			"and expires unused after 72 hours. Admin only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := client().MintDeviceEnrolmentToken(cmd.Context(), name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "enrolment token for %q, single use, expires %s:\n",
				t.DeviceName, t.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintln(cmd.OutOrStdout(), t.Token)
			return nil
		},
	}
	enrolToken.Flags().StringVar(&name, "name", "", "the laptop's inventory name (required)")
	_ = enrolToken.MarkFlagRequired("name")

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List enrolled devices, revoked ones included",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			devices, err := client().ListDevices(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				if devices == nil {
					devices = []sdk.Device{}
				}
				return emitJSON(devices)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tNAME\tENROLLED BY\tCREATED\tLAST SEEN\tACKED SEQ\tSTATUS")
			for _, d := range devices {
				lastSeen, status := "never", "active"
				if d.LastSeenAt != nil {
					lastSeen = d.LastSeenAt.Format(time.RFC3339)
				}
				if d.RevokedAt != nil {
					status = "revoked " + d.RevokedAt.Format("2006-01-02")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", d.ID, d.Name, d.EnrolledBy,
					d.CreatedAt.Format("2006-01-02"), lastSeen, d.LastSeq, status)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")

	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a device: its next audit push or heartbeat is refused",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("device id: %w", err)
			}
			if err := client().RevokeDevice(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked device %s\n", id)
			return nil
		},
	}

	cmd.AddCommand(enrolToken, list, revoke)
	return subcommandGroup(cmd)
}
