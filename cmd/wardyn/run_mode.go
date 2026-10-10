// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runModeFlags are `wardyn run`'s typed flags for the run-mode contract. They
// are the new client's spelling of what --agent, --task, --task-mode,
// --interactive and --model-provider say, and a request never mixes the two: with
// --experience set the CLI moves those flags into the carriers, and the server
// refuses any older field that rides beside them.
type runModeFlags struct {
	experience  string
	command     string
	startup     string
	startFolder string
	tools       []string
	noRepos     bool
}

func (f *runModeFlags) bind(fs *pflag.FlagSet) {
	fs.StringVar(&f.experience, "experience", "", "what you are starting: background (an agent task or a command, with no terminal, SSH or web access) or interactive (a sandbox you work in). Never inferred, and required by the other run-mode flags. --agent and --task become the agent task, or --agent the one included tool")
	fs.StringVar(&f.command, "command", "", "background command to run in the governed sandbox (with --experience background; no agent). The same as --task-mode exec")
	fs.StringArrayVar(&f.tools, "tool", nil, "interactive: an included tool as ID or ID=MODEL_PROVIDER, for example claude-code=bedrock-team. Repeatable where the server allows more than one; --agent is the same as one --tool")
	fs.StringVar(&f.startup, "startup", "", "interactive: what starts on its own: none, harness:ID (one included tool) or command:COMMAND. One choice; unset is none")
	fs.StringVar(&f.startFolder, "start-folder", "", "the run's one starting folder: image-default, or ATTACHMENT[:SUBPATH] where ATTACHMENT is a workspace id or drive")
	fs.BoolVar(&f.noRepos, "no-repos", false, "explicitly attach no repositories or drives. Nothing in the sandbox is kept when the run ends unless you push or copy it out")
}

func (f runModeFlags) used() bool {
	return f.experience != "" || f.command != "" || len(f.tools) > 0 || f.startup != "" || f.startFolder != "" || f.noRepos
}

// applyRunModeFlags rewrites a request the older flags filled into the run-mode
// carriers. It chooses nothing: an unset experience with another run-mode flag is
// an error here, as it is at the server.
func applyRunModeFlags(body *sdk.CreateRunRequest, f runModeFlags) error {
	if !f.used() {
		return nil
	}
	if f.noRepos {
		body.NoRepositoriesOrDrives = true
	}
	if f.startFolder != "" {
		folder, err := parseStartFolder(f.startFolder)
		if err != nil {
			return err
		}
		body.StartFolder = folder
	}
	if f.experience == "" {
		if f.command != "" || len(f.tools) > 0 || f.startup != "" {
			return errors.New("--command, --tool and --startup need --experience background or interactive: the run mode is never inferred")
		}
		return nil
	}
	if body.Interactive || body.TaskMode != "" {
		return errors.New("--experience replaces --interactive and --task-mode")
	}
	body.Experience = sdk.Experience(f.experience)
	switch body.Experience {
	case sdk.ExperienceBackground:
		return backgroundFlags(body, f)
	case sdk.ExperienceInteractive:
		return interactiveFlags(body, f)
	}
	return fmt.Errorf("--experience %q is not one of background, interactive", f.experience)
}

func backgroundFlags(body *sdk.CreateRunRequest, f runModeFlags) error {
	if f.startup != "" || len(f.tools) > 0 {
		return errors.New("--startup and --tool are for --experience interactive: a background run starts its workload")
	}
	switch {
	case f.command != "" && (body.Agent != "" || body.Task != ""):
		return errors.New("--command runs no agent: drop --agent and --task, or use an agent task instead")
	case f.command != "":
		body.Workload = &sdk.RunWorkload{Kind: sdk.WorkloadCommand, Command: f.command}
	default:
		tool := sdk.IncludedTool{ID: body.Agent, Kind: sdk.IncludedToolHarness, ModelProvider: body.ModelProvider}
		body.Workload = &sdk.RunWorkload{Kind: sdk.WorkloadAgentTask, Agent: body.Agent, Task: body.Task}
		if body.Agent != "" {
			body.Tools = []sdk.IncludedTool{tool}
		}
	}
	body.Agent, body.Task, body.ModelProvider = "", "", ""
	return nil
}

func interactiveFlags(body *sdk.CreateRunRequest, f runModeFlags) error {
	if f.command != "" || body.Task != "" {
		return errors.New("--command and --task are for a background run: start a command in an interactive run with --startup command:COMMAND")
	}
	tools := f.tools
	if body.Agent != "" {
		if len(tools) > 0 {
			return errors.New("--agent is the same as one --tool: use one or the other")
		}
		tools = []string{body.Agent}
		if body.ModelProvider != "" {
			tools[0] += "=" + body.ModelProvider
		}
	}
	for _, spec := range tools {
		id, provider, _ := strings.Cut(spec, "=")
		body.Tools = append(body.Tools, sdk.IncludedTool{ID: id, Kind: sdk.IncludedToolHarness, ModelProvider: provider})
	}
	if f.startup != "" {
		startup, err := parseStartup(f.startup)
		if err != nil {
			return err
		}
		body.Startup = startup
	}
	body.Agent, body.ModelProvider = "", ""
	return nil
}

func parseStartup(s string) (*sdk.RunStartup, error) {
	kind, value, _ := strings.Cut(s, ":")
	switch sdk.StartupKind(kind) {
	case sdk.StartupNone:
		if value != "" {
			return nil, errors.New("--startup none takes no value")
		}
		return &sdk.RunStartup{Kind: sdk.StartupNone}, nil
	case sdk.StartupHarness:
		return &sdk.RunStartup{Kind: sdk.StartupHarness, Tool: value}, nil
	case sdk.StartupCommand:
		return &sdk.RunStartup{Kind: sdk.StartupCommand, Command: value}, nil
	}
	return nil, fmt.Errorf("--startup %q is not none, harness:ID or command:COMMAND", s)
}

func parseStartFolder(s string) (*sdk.StartFolder, error) {
	if s == "image-default" {
		return &sdk.StartFolder{Kind: sdk.StartFolderImageDefault}, nil
	}
	attachment, subpath, _ := strings.Cut(s, ":")
	if attachment == "" {
		return nil, fmt.Errorf("--start-folder %q is not image-default or ATTACHMENT[:SUBPATH]", s)
	}
	return &sdk.StartFolder{Kind: sdk.StartFolderAttachment, Attachment: attachment, Subpath: subpath}, nil
}
