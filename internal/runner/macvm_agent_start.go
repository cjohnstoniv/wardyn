// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// MacVMAgentStart refuses a macOS VM run that asks its terminal to open the agent CLI. The runner says it
// at the start, and the control plane at Review, in the same words.
const MacVMAgentStart = "A macOS VM's terminal opens a shell, not the agent, in this release. Turn off \"Start the run in this harness\", or send interactive_start: \"shell\" in the API."

// MacVMNoIdlePause refuses a macOS VM run under a profile that pauses idle runs: a macOS VM has no pause.
const MacVMNoIdlePause = "A macOS VM can't be paused when idle, so your profile's idle pause can't apply to it. Choose a container or a Linux VM, or ask an admin to turn idle pause off."
