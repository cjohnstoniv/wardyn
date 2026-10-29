// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// wallsCmd is a person's own walls: limits they set on every run they start, which only narrow what
// their organisation allows. A bare `wardyn walls` shows them; see internal/api/personal_walls.go.
func wallsCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "walls",
		Short: "Show, set or clear your own walls: limits on every run you start",
		Long: "Your walls are limits you put on every run you start. They only narrow what your\n" +
			"organisation allows and no one else reads or changes them. `wardyn walls` prints\n" +
			"them as the YAML `wardyn walls set --file` takes back.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := client().GetMyWalls(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), w)
			}
			return printWalls(cmd.OutOrStdout(), cmd.ErrOrStderr(), w)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")

	var file string
	set := &cobra.Command{
		Use:   "set",
		Short: "Replace your walls with the ones in a YAML or JSON file",
		Long: "Replace your walls with the document in --file: the knobs you want, at the top level\n" +
			"(allowed_domains, denied_domains, allow_all_egress: false, first_use_approval,\n" +
			"min_confinement_class, desktop, withheld_grants). A knob the file leaves out is no\n" +
			"limit. The whole document is replaced, so `wardyn walls` first shows what to keep.\n" +
			"The server refuses anything that would widen a run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := readWallsSpec(cmd.InOrStdin(), file)
			if err != nil {
				return err
			}
			if _, err := client().PutMyWalls(cmd.Context(), spec); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "saved your walls: runs you start from now on use them")
			return nil
		},
	}
	set.Flags().StringVarP(&file, "file", "f", "", "the walls: YAML or JSON ('-' for stdin)")
	_ = set.MarkFlagRequired("file")

	clearWalls := &cobra.Command{
		Use:   "clear",
		Short: "Remove all your walls",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := client().ClearMyWalls(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed your walls")
			return nil
		},
	}

	cmd.AddCommand(set, clearWalls)
	return cmd
}

// readWallsSpec reads the walls from file ('-' for stdin). The YAML is decoded into the typed spec, not
// into plain data, so a host that reads as a number or a date stays the text the file wrote; a key the
// spec does not have is refused. A file that names no limit is refused too: it would clear the walls,
// and `walls clear` says that.
func readWallsSpec(stdin io.Reader, file string) (types.PersonalWallsSpec, error) {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return types.PersonalWallsSpec{}, fmt.Errorf("read the walls file: %w", err)
	}
	if err := checkHostsAreText(raw); err != nil {
		return types.PersonalWallsSpec{}, err
	}
	var spec types.PersonalWallsSpec
	if err := yaml.UnmarshalStrict(raw, &spec); err != nil {
		return types.PersonalWallsSpec{}, fmt.Errorf("the walls file does not parse (accepts JSON or YAML): %w", err)
	}
	if spec.Empty() {
		return types.PersonalWallsSpec{}, errors.New("the walls file names no limit: to remove your walls, run `wardyn walls clear`")
	}
	return spec, nil
}

// checkHostsAreText refuses a site that YAML reads as a number, a date or a boolean: decoding it into the
// text field would write it back in another spelling (1.10 as 1.1), which is a different site.
func checkHostsAreText(raw []byte) error {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		return nil // the typed decode below says what is wrong with it
	}
	top := doc.Content[0].Content
	for i := 0; i+1 < len(top); i += 2 {
		if k := top[i].Value; k != "allowed_domains" && k != "denied_domains" {
			continue
		}
		for _, host := range top[i+1].Content {
			if host.Kind == yamlv3.ScalarNode && host.ShortTag() != "!!str" {
				return fmt.Errorf("%s: %q reads as %s, not text: put it in quotes", top[i].Value, host.Value, host.ShortTag())
			}
		}
	}
	return nil
}

// printWalls writes the spec as YAML on out, in the form `walls set` reads, and the note on errOut so a
// pipe into a file stays a clean document.
func printWalls(out, errOut io.Writer, w sdk.PersonalWalls) error {
	if w.Spec.Empty() {
		fmt.Fprintln(errOut, "you have no walls set")
		_, err := fmt.Fprintln(out, "{}")
		return err
	}
	y, err := yaml.Marshal(w.Spec)
	if err != nil {
		return fmt.Errorf("render your walls as YAML: %w", err)
	}
	if w.UpdatedAt != nil {
		fmt.Fprintf(errOut, "your walls, last saved %s\n", w.UpdatedAt.Format(time.RFC3339))
	}
	_, err = out.Write(y)
	return err
}
