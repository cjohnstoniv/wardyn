// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// machines.go — `wardyn machines`: the macOS VMs you kept on your own Mac between runs. A run keeps or resumes
// one with `wardyn run --keep-machine` and `--resume-machine`. Every refusal is the server's own sentence, printed
// as the error it is.

// machinesCmd lists your kept machines; `delete` removes one from its Mac.
func machinesCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "machines",
		Short: "List the macOS VMs you kept on your own Mac between runs, or delete one",
		Long: "Lists the macOS VMs you kept on your own Mac when a run stopped: its state, the Mac, the base it was made from, " +
			"when it was last used and when it expires, how much it has written, and whether Wardyn has removed the last run's settings and certificates from it.\n\n" +
			"Keep one with 'wardyn run --experience interactive --machine-kind macos_vm --keep-machine [--machine-name NAME]'. " +
			"Start a run from one with '--resume-machine <id|name>', and start it over from the Mac's current base with '--fresh'.\n\n" +
			"'wardyn machines delete <id|name>' deletes one, with its files, tools and sign-ins.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mine, err := client().ListMyMachines(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), mine)
			}
			return writeMachines(cmd.OutOrStdout(), mine.Machines)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {machines, allowed, reason, count, limit, worst_bytes} as JSON")
	cmd.AddCommand(machinesDeleteCmd(client))
	return cmd
}

// writeMachines prints the list. A machine whose delete has started is shown as deleting (expired, when it ran
// out its idle time), with a sentence under the table saying what is left to happen; nothing says it is gone
// until the server stops listing it.
func writeMachines(w io.Writer, ms []sdk.KeptMachine) error {
	if len(ms) == 0 {
		_, err := fmt.Fprintln(w, "no kept machines")
		return err
	}
	tw := newTab(w)
	fmt.Fprintln(tw, "NAME\tSTATE\tMAC\tBASE\tLAST USED\tEXPIRES\tWRITTEN\tSCRUBBED")
	for _, m := range ms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, machineState(m), machineMac(m), orDash(m.Base),
			machineDate(m.LastUsedAt), machineDate(m.ExpiresAt), machineGB(m.DiskBytes), yesNo(m.Scrubbed))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, m := range ms {
		switch {
		case m.State != types.KeptMachineDeleting:
		case m.DeleteReason == sdk.KeptMachineDeletedByExpired:
			fmt.Fprintf(w, "machine %s: This machine expired and will be deleted.\n", m.Name)
		default:
			fmt.Fprintf(w, "Your machine %s is being deleted.\n", m.Name)
		}
	}
	return nil
}

func machineState(m sdk.KeptMachine) string {
	if m.State == types.KeptMachineDeleting && m.DeleteReason == sdk.KeptMachineDeletedByExpired {
		return "expired"
	}
	return string(m.State)
}

// machineMac is the Mac's name, or its runner id when the server sent no name.
func machineMac(m sdk.KeptMachine) string {
	if m.RunnerName != "" {
		return m.RunnerName
	}
	return m.RunnerID.String()
}

// machineDate is a day in the person's own time zone.
func machineDate(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02")
}

// machineGB is what a machine has written, in the whole GB the console and the Mac's status line use.
func machineGB(bytes int64) string {
	switch gb := (bytes + 1<<29) >> 30; {
	case bytes <= 0:
		return "-"
	case gb == 0:
		return "<1 GB"
	default:
		return fmt.Sprintf("%d GB", gb)
	}
}

// machinesDeleteCmd is `wardyn machines delete <id|name>`: it asks first, unless --yes, and says what the
// server did: deleted, or still waiting for its Mac. A refusal (a run holds it, a delete has started, the
// Mac could not be reached or would not) is the server's sentence, unchanged.
func machinesDeleteCmd(client clientFn) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete one of your kept machines from its Mac, with its files, tools and sign-ins",
		Long: "Deletes a kept machine from the Mac it is kept on, with the files, tools and sign-ins on it. Nothing undoes it.\n\n" +
			"Asks first at a terminal. Without a terminal, --yes is required. A machine a run holds is refused until that run ends. " +
			"If the Mac is offline the machine stays listed as being deleted, and is deleted when the Mac is back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMachinesDelete(cmd, client(), strings.TrimSpace(args[0]), yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete without asking (required without a terminal)")
	return cmd
}

func runMachinesDelete(cmd *cobra.Command, c *sdk.Client, ref string, yes bool) error {
	out := cmd.OutOrStdout()
	mine, err := c.ListMyMachines(cmd.Context())
	if err != nil {
		return err
	}
	// Every delete this command sends is of a machine in your own list, asked about first: a reference that
	// names none is refused here, never sent to the server to see what it says.
	m, found := findMachine(mine.Machines, ref)
	if !found {
		//lint:ignore ST1005 the server's sentence for it (kept_machine_not_found), as the person reads it
		return fmt.Errorf("You have no kept machine %s.", ref)
	}
	if !yes {
		if err := confirmMachineDelete(cmd, m); err != nil {
			return err
		}
	}
	if err := c.DeleteMyMachine(cmd.Context(), m.ID); err != nil {
		return err
	}
	// A confirmed delete leaves no record; one waiting for its Mac stays, as deleting.
	after, err := c.ListMyMachines(cmd.Context())
	if err != nil {
		fmt.Fprintf(out, "machine %s: the delete was accepted; run 'wardyn machines' to see whether it is done\n", m.Name)
		return nil
	}
	if left, still := findMachine(after.Machines, m.ID.String()); still {
		fmt.Fprintf(out, "machine %s: %s is offline; it is deleted when it's back\n", left.Name, machineMac(left))
		return nil
	}
	fmt.Fprintf(out, "deleted machine %s from %s\n", m.Name, machineMac(m))
	return nil
}

// findMachine is the machine whose id or name is ref. An id is compared as a UUID, so any spelling of it
// (upper case, no dashes, in braces, urn:uuid:) finds the machine its canonical form does.
func findMachine(ms []sdk.KeptMachine, ref string) (sdk.KeptMachine, bool) {
	if id, err := uuid.Parse(ref); err == nil {
		for _, m := range ms {
			if m.ID == id {
				return m, true
			}
		}
	}
	// The server matches a machine's name case-blind, so the CLI does too.
	for _, m := range ms {
		if strings.EqualFold(m.Name, ref) {
			return m, true
		}
	}
	return sdk.KeptMachine{}, false
}

// confirmMachineDelete asks a person at a terminal; without one it refuses, so a script says --yes.
func confirmMachineDelete(cmd *cobra.Command, m sdk.KeptMachine) error {
	what := fmt.Sprintf("This deletes it from %s, with the files, tools and sign-ins on it. It can't be undone.", machineMac(m))
	if !stdinIsTerminal() {
		return fmt.Errorf("nothing was deleted: without a terminal, pass --yes to delete machine %s. %s", m.Name, what)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Delete machine %s? %s [y/N] ", m.Name, what)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("nothing was deleted")
	}
	return nil
}

// printMachineCreatedLine prints machineCreatedLine under a created run, and nothing for a run that keeps none.
func printMachineCreatedLine(cmd *cobra.Command, c *sdk.Client, m *sdk.RunMachine) {
	if line := machineCreatedLine(cmd.Context(), c, m); line != "" {
		fmt.Fprintln(cmd.OutOrStdout(), line)
	}
}

// machineCreatedLine is what `wardyn run` prints under its created line for a run that keeps or resumes a
// machine, "" for any other. A start is "will" until the Mac confirms it, so nothing here says it happened.
// A new machine's Mac is chosen when the run starts, and an unnamed one is named then; a resumed machine's name,
// date and Mac come from your list when it can be read, and from the flag as typed when not.
func machineCreatedLine(ctx context.Context, c *sdk.Client, m *sdk.RunMachine) string {
	const kept = ", kept when this run stops"
	switch {
	case m == nil:
		return ""
	case m.Keep && m.Name != "":
		return "machine " + m.Name + ": new" + kept
	case m.Keep:
		return "machine: new" + kept + "; it is named when the run starts"
	}
	name, date, mac := m.Resume, "", "its Mac"
	if mine, err := c.ListMyMachines(ctx); err == nil {
		if km, ok := findMachine(mine.Machines, m.Resume); ok {
			name, date, mac = km.Name, machineDate(km.LastUsedAt), machineMac(km)
		}
	}
	if m.Fresh {
		return "machine " + name + ": will start fresh from " + mac + "'s current base" + kept
	}
	if date == "" {
		return "machine " + name + ": will resume" + kept
	}
	return "machine " + name + ": will resume (last used " + date + ")" + kept
}
