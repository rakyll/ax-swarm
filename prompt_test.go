// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"strings"
	"testing"
)

func TestBuildSwarmPrompt(t *testing.T) {
	cfg := Config{
		Goal:      "Analyze chalk's API\nand weak spots",
		Name:      "swarm-abc123",
		Atespace:  "default",
		Parallel:  6,
		Repos:     []string{"https://github.com/chalk/chalk"},
		SetupCmds: []string{"npm ci"},
	}

	prompt := BuildSwarmPrompt(cfg, "/Users/jbd/ax/bin/ax -a default")

	if !strings.HasPrefix(prompt, "/swarm Analyze chalk's API and weak spots\n") {
		t.Fatalf("prompt must start with single-line /swarm command, got: %q", prompt[:min(len(prompt), 80)])
	}

	requiredSnippets := []string{
		"Swarm name / task prefix: `swarm-abc123`",
		"Shared Workspace (single per swarm): `swarm-abc123`",
		"Shared Gateway (single per swarm): `swarm-abc123`",
		"Base `ax` CLI command: `/Users/jbd/ax/bin/ax -a default`",
		"Target max concurrent AX Tasks: `6`",
		"https://github.com/chalk/chalk",
		"npm ci",
		"The swarm controls all AX task execution.",
		"Single Workspace and single Gateway per swarm.",
		"NEVER create, modify, or delete `Workspace` or `Gateway` resources",
		"Run ALL substantive work inside AX Tasks, never on the local host.",
		"Pass AX control details to every swarm worker.",
		"tell them NOT to create new Workspaces or Gateways",
		"name: swarm-abc123-<step>",
		"debug: true",
		"/Users/jbd/ax/bin/ax -a default apply -f -",
		"/Users/jbd/ax/bin/ax -a default get tasks",
		"/Users/jbd/ax/bin/ax -a default resume task swarm-abc123-<step>",
		"/Users/jbd/ax/bin/ax -a default ssh swarm-abc123-<step> -- sh -lc 'cd /workspace && <command>'",
		"/Users/jbd/ax/bin/ax -a default suspend task swarm-abc123-<step>",
		"/Users/jbd/ax/bin/ax -a default delete task swarm-abc123-<step>",
		"Clean up completed tasks.",
	}
	for _, s := range requiredSnippets {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing expected snippet %q", s)
		}
	}
}

func TestBuildSwarmPromptKeepMode(t *testing.T) {
	cfg := Config{
		Goal:     "Inspect repo",
		Name:     "swarm-keep1",
		Atespace: "dev",
		Parallel: 4,
		Keep:     true,
	}
	prompt := BuildSwarmPrompt(cfg, "ax -a dev")
	if !strings.Contains(prompt, "Keep tasks (`--keep` is set).") {
		t.Fatalf("expected --keep guidance in prompt, got:\n%s", prompt)
	}
}
