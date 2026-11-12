// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// wallPresetsCmd is `wardyn walls presets`: named walls a person picks at launch, kept for themselves, a
// group or the organisation, with three built in. A bare invocation lists them. See
// internal/api/wall_presets.go.
func wallPresetsCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "presets",
		Short: "List, show, save or delete wall presets: named walls you pick when you start a run",
		Long: "A wall preset is a named set of walls that only narrows what a run may do. Three are built\n" +
			"in (open, read-only and sealed); you can keep your own, and a group's template managers or\n" +
			"the organisation's administrators can keep one for a group or everyone. Start a run with one\n" +
			"using `wardyn run --wall-preset <name|id>`. A name is looked up across the audiences open to\n" +
			"you; if it is in more than one, use --mine, --group or --org, or the preset's id.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := client().ListWallPresets(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), list)
			}
			return printWallPresets(cmd.OutOrStdout(), list)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(wallPresetShowCmd(client), wallPresetSetCmd(client), wallPresetDeleteCmd(client))
	return cmd
}

// wallPresetAudience is the --mine, --group and --org choice that picks one audience's preset.
type wallPresetAudience struct {
	mine, org bool
	group     string
}

func (a *wallPresetAudience) register(cmd *cobra.Command, withMine bool) {
	if withMine {
		cmd.Flags().BoolVar(&a.mine, "mine", false, "the preset you keep for yourself")
	}
	cmd.Flags().StringVar(&a.group, "group", "", "the preset kept for this group (its id or label)")
	cmd.Flags().BoolVar(&a.org, "org", false, "the preset kept for the organisation")
	if withMine {
		cmd.MarkFlagsMutuallyExclusive("mine", "group", "org")
	} else {
		cmd.MarkFlagsMutuallyExclusive("group", "org")
	}
}

// matches reports whether p is in the chosen audience; with none chosen, every audience matches.
func (a wallPresetAudience) matches(p sdk.WallPreset, groupID string) bool {
	switch {
	case a.mine:
		return p.Scope == types.WallPresetScopePerson
	case a.org:
		return p.Scope == types.WallPresetScopeOrg
	case a.group != "":
		return p.Scope == types.WallPresetScopeGroup && p.GroupID == groupID
	}
	return true
}

// wallPresetGroupID resolves --group to a group id: the id itself, or the label of a group you may save
// for, compared without regard to case. Two groups with the same label are refused, listing their ids,
// rather than one being picked.
func wallPresetGroupID(g string, publish sdk.TemplatePublish) (string, error) {
	for _, id := range publish.Groups {
		if id == g {
			return id, nil
		}
	}
	var ids []string
	for id, label := range publish.GroupLabels {
		if strings.EqualFold(label, g) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	switch len(ids) {
	case 0:
		return g, nil
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("more than one group is labelled %q (ids %s): use the group's id", g, strings.Join(ids, ", "))
}

// wallPresetAudienceWord names a preset's audience in the words the console uses.
func wallPresetAudienceWord(p sdk.WallPreset, publish sdk.TemplatePublish) string {
	switch p.Scope {
	case types.WallPresetScopeBuiltin:
		return "built in"
	case types.WallPresetScopeOrg:
		return "organisation"
	case types.WallPresetScopeGroup:
		if l := publish.GroupLabels[p.GroupID]; l != "" {
			return l + " group"
		}
		return "group " + p.GroupID
	}
	return "yours"
}

// resolveWallPreset finds the preset a name or id means among the list. A built-in's name always means the
// built-in. A uuid not in the list is passed through for the server to answer. A name in more than one
// audience is refused, listing them, unless an audience flag or an id picks one; flagsHint is the sentence
// ending that says how.
func resolveWallPreset(list sdk.WallPresetList, ref string, aud wallPresetAudience, flagsHint string) (sdk.WallPreset, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return sdk.WallPreset{}, errors.New("name the wall preset (a name or an id)")
	}
	if b, ok := types.BuiltinWallPreset(strings.ToLower(ref)); ok {
		for _, p := range list.Presets {
			if p.ID == b.ID {
				return p, nil
			}
		}
		return b, nil
	}
	if _, err := uuid.Parse(ref); err == nil {
		for _, p := range list.Presets {
			if p.ID == ref {
				return p, nil
			}
		}
		return sdk.WallPreset{ID: ref, Name: ref}, nil
	}
	gid := ""
	if aud.group != "" {
		var err error
		if gid, err = wallPresetGroupID(aud.group, list.Publish); err != nil {
			return sdk.WallPreset{}, err
		}
	}
	var found []sdk.WallPreset
	for _, p := range list.Presets {
		if p.Scope != types.WallPresetScopeBuiltin && strings.EqualFold(p.Name, ref) && aud.matches(p, gid) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return sdk.WallPreset{}, fmt.Errorf("wall preset %q does not exist or is not open to you", ref)
	case 1:
		return found[0], nil
	}
	words := make([]string, len(found))
	for i, p := range found {
		words[i] = wallPresetAudienceWord(p, list.Publish)
	}
	return sdk.WallPreset{}, fmt.Errorf("wall preset %q is in more than one of your audiences (%s); %s", ref, strings.Join(words, ", "), flagsHint)
}

const wallPresetAudienceHint = "add --mine, --group or --org, or use its id"

func wallPresetShowCmd(client clientFn) *cobra.Command {
	var asJSON bool
	var aud wallPresetAudience
	cmd := &cobra.Command{
		Use:   "show <name|id>",
		Short: "Show a wall preset's walls as the YAML `walls presets set --file` takes back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			list, err := c.ListWallPresets(cmd.Context())
			if err != nil {
				return err
			}
			p, err := resolveWallPreset(list, args[0], aud, wallPresetAudienceHint)
			if err != nil {
				return err
			}
			if p, err = c.GetWallPreset(cmd.Context(), p.ID); err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wall preset %s (%s, version %d)\n", p.Name, wallPresetAudienceWord(p, list.Publish), p.Version)
			if p.Description != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), p.Description)
			}
			return printWallsSpec(cmd.OutOrStdout(), p.Spec)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	aud.register(cmd, true)
	return cmd
}

func wallPresetSetCmd(client clientFn) *cobra.Command {
	var file, description string
	var aud wallPresetAudience
	cmd := &cobra.Command{
		Use:   "set <name> -f FILE [--group G | --org] [--description D]",
		Short: "Create a wall preset, or replace the one of that name, from a YAML or JSON file",
		Long: "Save the walls in --file as the wall preset <name>, for yourself, or for a group with --group\n" +
			"or the organisation with --org. If your audience already has a preset of that name it is\n" +
			"replaced, and the save names the version it read: if someone saved it since, the save is\n" +
			"refused and you run it again. The file is the one `wardyn walls set` reads, and it must name\n" +
			"at least one limit. Without --description the existing description stays.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if types.IsBuiltinWallPresetName(name) {
				return fmt.Errorf("%q is a built-in wall preset's name: pick another, or copy it with `wardyn walls presets show %s`", name, name)
			}
			spec, err := readWallsSpec(cmd.InOrStdin(), file)
			if err != nil {
				return err
			}
			c := client()
			list, err := c.ListWallPresets(cmd.Context())
			if err != nil {
				return err
			}
			req := sdk.WallPresetSaveRequest{Scope: types.WallPresetScopePerson, Name: name, Description: description, Spec: spec}
			switch {
			case aud.org:
				req.Scope = types.WallPresetScopeOrg
			case aud.group != "":
				gid, err := wallPresetGroupID(aud.group, list.Publish)
				if err != nil {
					return err
				}
				req.Scope, req.GroupID = types.WallPresetScopeGroup, gid
			}
			id := ""
			for _, p := range list.Presets {
				if p.Scope == req.Scope && p.GroupID == req.GroupID && strings.EqualFold(p.Name, name) {
					v := p.Version
					id, req.ExpectedVersion = p.ID, &v
					if !cmd.Flags().Changed("description") {
						req.Description = p.Description
					}
				}
			}
			saved, err := c.SaveWallPreset(cmd.Context(), id, req)
			var apiErr *sdk.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Reason == "wall_preset_version_conflict" {
				return fmt.Errorf("someone saved wall preset %q since you read it: run the command again", name)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved wall preset %s (%s, version %d)\n", saved.Name, wallPresetAudienceWord(saved, list.Publish), saved.Version)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the walls: YAML or JSON ('-' for stdin)")
	_ = cmd.MarkFlagRequired("file")
	cmd.Flags().StringVar(&description, "description", "", "what the preset is for")
	aud.register(cmd, false)
	return cmd
}

func wallPresetDeleteCmd(client clientFn) *cobra.Command {
	var aud wallPresetAudience
	cmd := &cobra.Command{
		Use:   "delete <name|id> [--group G | --org | --mine]",
		Short: "Delete a wall preset you may change",
		Long:  "Delete a wall preset. Runs already launched with it keep it. A built-in cannot be deleted.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			list, err := c.ListWallPresets(cmd.Context())
			if err != nil {
				return err
			}
			p, err := resolveWallPreset(list, args[0], aud, wallPresetAudienceHint)
			if err != nil {
				return err
			}
			if err := c.DeleteWallPreset(cmd.Context(), p.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted wall preset %s: runs already launched with it keep it\n", p.Name)
			return nil
		},
	}
	aud.register(cmd, true)
	return cmd
}

// wallPresetNarrows names the knobs a spec sets, as the YAML spells them; push_rules shows the part set.
func wallPresetNarrows(spec types.PersonalWallsSpec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	var knobs map[string]json.RawMessage
	if json.Unmarshal(raw, &knobs) != nil {
		return ""
	}
	var names []string
	for k, v := range knobs {
		if k != "push_rules" {
			names = append(names, k)
			continue
		}
		var sub map[string]json.RawMessage
		if json.Unmarshal(v, &sub) == nil {
			for s := range sub {
				names = append(names, k+"."+s)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

func printWallPresets(out io.Writer, list sdk.WallPresetList) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tAUDIENCE\tVERSION\tNARROWS")
	for _, p := range list.Presets {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", p.Name, wallPresetAudienceWord(p, list.Publish), p.Version, wallPresetNarrows(p.Spec))
	}
	return tw.Flush()
}

// printWallsSpec writes a spec as the YAML `walls set` reads; a spec that names no limit is {}.
func printWallsSpec(out io.Writer, spec types.PersonalWallsSpec) error {
	if spec.Empty() {
		_, err := fmt.Fprintln(out, "{}")
		return err
	}
	y, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("render the walls as YAML: %w", err)
	}
	_, err = out.Write(y)
	return err
}

// runWallPreset is `wardyn run --wall-preset`: the preset is looked up when the command starts, so a name in
// more than one audience or a missing one is refused before the run is built, and the run is sent the
// preset's id. The line saying which preset bound the run is read back from the run, not from that lookup.
type runWallPreset struct {
	ref, id string
	publish sdk.TemplatePublish
}

func (w *runWallPreset) register(cmd *cobra.Command, client clientFn) {
	cmd.Flags().StringVar(&w.ref, "wall-preset", "", "narrow this run with a wall preset, by name or id (see `wardyn walls presets`); also goes with --file")
	addPreRunE(cmd, func(cmd *cobra.Command, _ []string) error {
		if w.ref == "" {
			return nil
		}
		list, err := client().ListWallPresets(cmd.Context())
		if err != nil {
			return err
		}
		p, err := resolveWallPreset(list, w.ref, wallPresetAudience{}, "use its id")
		if err != nil {
			return err
		}
		w.id, w.publish = p.ID, list.Publish
		return nil
	})
}

// printLine says which wall preset, and which version of it, the server bound the run to. A run read that
// fails or names no preset prints nothing: the line is never filled in from the earlier lookup.
func (w *runWallPreset) printLine(ctx context.Context, c *sdk.Client, out io.Writer, run uuid.UUID) {
	if w.id == "" {
		return
	}
	rp, err := c.GetRunWallPreset(ctx, run)
	if err != nil || rp == nil || rp.Name == "" {
		return
	}
	p := sdk.WallPreset{Scope: types.WallPresetScope(rp.Scope), GroupID: rp.GroupID, Name: rp.Name, Version: rp.Version}
	fmt.Fprintf(out, "  Wall preset: %s (%s, version %d).\n", rp.Name, wallPresetAudienceWord(p, w.publish), rp.Version)
}
