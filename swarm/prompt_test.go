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
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBuildSwarmPrompt(t *testing.T) {
	cfg := Config{
		Goal:     "Analyze chalk's API\nand weak spots",
		Name:     "swarm-abc123",
		Atespace: "default",
		Parallel: 6,
		WorkspaceSpec: map[string]any{"setup": map[string]any{
			"git":      []any{map[string]any{"repo": "https://github.com/chalk/chalk"}},
			"commands": []any{"npm ci"},
		}},
		AutoResume: true,
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
		"Create and start an AX Task (ONE shell call)",
		"ax-swarm resumes them automatically",
		"T=swarm-abc123-<step>",
		"/Users/jbd/ax/bin/ax -a default apply -f - <<'EOF'",
		"/Users/jbd/ax/bin/ax -a default get tasks | awk",
		"/Users/jbd/ax/bin/ax -a default resume task \"$T\"",
		"Execute Commands inside `/workspace` (batch them)",
		"/Users/jbd/ax/bin/ax -a default ssh swarm-abc123-<step> -- sh -lc 'cd /workspace && <command 1> && <command 2>'",
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

// TestCreateAndStartBlockRuns executes the one-shot create-and-start block from
// the prompt against a fake ax that reports Suspended, then Running after a
// resume, and checks that the loop resumes the task and exits on Running.
func TestCreateAndStartBlockRuns(t *testing.T) {
	dir := t.TempDir()
	fakeAX := dir + "/ax"
	state := dir + "/state"
	script := `#!/bin/sh
case "$*" in
  *"apply -f -"*) cat >/dev/null; echo Suspended > "` + state + `"; echo created ;;
  *"get tasks"*) echo "NAME ATESPACE STATE"; echo "swarm-abc123-probe default $(cat "` + state + `")" ;;
  *"resume task"*) echo Running > "` + state + `"; echo resumed ;;
esac
`
	if err := os.WriteFile(fakeAX, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	prompt := BuildSwarmPrompt(Config{Goal: "x", Name: "swarm-abc123", Atespace: "default", Parallel: 1, AutoResume: true}, fakeAX)
	start := strings.Index(prompt, "```bash\nT=")
	if start < 0 {
		t.Fatalf("create-and-start block not found in prompt")
	}
	block := prompt[start+len("```bash\n"):]
	block = block[:strings.Index(block, "```")]
	block = strings.ReplaceAll(block, "<step>", "probe")
	block = strings.ReplaceAll(block, "<concise step goal>", "probe")

	out, err := exec.Command("/bin/sh", "-c", block).CombinedOutput()
	if err != nil {
		t.Fatalf("block failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "swarm-abc123-probe: Running") {
		t.Fatalf("block did not reach Running:\n%s", out)
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
