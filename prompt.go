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
	"fmt"
	"strings"
)

// BuildSwarmPrompt constructs the `/swarm ...` prompt passed to `agy -p`.
// The prompt places `/swarm` at byte 0 so agy activates its native swarm mode,
// and instructs the swarm to own and drive all task execution directly inside
// AX Tasks via raw `ax` CLI commands.
func BuildSwarmPrompt(cfg Config, axBaseCmd string) string {
	goalLine := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(cfg.Goal, "\r\n", " "), "\n", " "))

	var b strings.Builder
	fmt.Fprintf(&b, "/swarm %s\n\n", goalLine)

	b.WriteString("## Goal\n\n")
	b.WriteString(strings.TrimSpace(cfg.Goal))
	b.WriteString("\n\n")

	b.WriteString("## AX Execution Environment\n\n")
	b.WriteString("Exactly one shared AX Workspace and one shared AX Gateway have been provisioned and verified Ready for this entire swarm:\n")
	fmt.Fprintf(&b, "- Swarm name / task prefix: `%s`\n", cfg.Name)
	fmt.Fprintf(&b, "- Atespace: `%s`\n", cfg.Atespace)
	fmt.Fprintf(&b, "- Shared Workspace (single per swarm): `%s` (Phase: `Ready`, mounted at `/workspace` inside every AX Task)\n", cfg.Name)
	fmt.Fprintf(&b, "- Shared Gateway (single per swarm): `%s` (HTTPS egress for GitHub, PyPI, npm, Go proxy)\n", cfg.Name)
	fmt.Fprintf(&b, "- Base `ax` CLI command: `%s`\n", axBaseCmd)
	fmt.Fprintf(&b, "- Target max concurrent AX Tasks: `%d`\n", cfg.Parallel)
	if len(cfg.Repos) > 0 {
		b.WriteString("- Cloned repositories in `/workspace`:\n")
		for _, repo := range cfg.Repos {
			fmt.Fprintf(&b, "  - `%s`\n", repo)
		}
	}
	if len(cfg.SetupCmds) > 0 {
		b.WriteString("- Setup commands already applied to `/workspace`:\n")
		for _, cmd := range cfg.SetupCmds {
			fmt.Fprintf(&b, "  - `%s`\n", cmd)
		}
	}
	b.WriteString("\n")

	b.WriteString("## Mandatory Swarm Execution & Task Control Rules\n\n")
	b.WriteString("1. **The swarm controls all AX task execution.** Plan the work, decompose it into independent parallel workstreams, spawn swarm agents concurrently, and have the swarm directly create, resume, execute commands inside, inspect, suspend, retry, and delete AX Tasks via the `ax` CLI.\n")
	fmt.Fprintf(&b, "2. **Single Workspace and single Gateway per swarm.** Every AX Task in this swarm MUST reuse the single pre-provisioned Workspace (`%s`) and single pre-provisioned Gateway (`%s`). NEVER create, modify, or delete `Workspace` or `Gateway` resources (`kind: Workspace` or `kind: Gateway`); only create and control `kind: Task` resources.\n", cfg.Name, cfg.Name)
	b.WriteString("3. **Run ALL substantive work inside AX Tasks, never on the local host.** Do not inspect, build, test, or modify local host files to answer the goal. Local shell calls must only invoke `ax` commands to control AX Tasks. Every exploration, file read, build, test, or experiment must execute inside an AX Task's `/workspace` via `ax ssh`.\n")
	fmt.Fprintf(&b, "4. **Pass AX control details to every swarm worker.** Spawned swarm agents do not automatically inherit this initial prompt. Whenever delegating work to a swarm agent, include the exact base `ax` command (`%s`), the single shared Workspace and Gateway name (`%s` — tell them NOT to create new Workspaces or Gateways), its assigned unique AX Task name (`%s-<step>`), and the `apply` / `resume` / `ssh` / `delete` workflow in its instructions so each agent drives its own AX Task directly.\n", axBaseCmd, cfg.Name, cfg.Name)
	fmt.Fprintf(&b, "5. **Strict task naming.** Name every AX Task `%s-<step>`, where `<step>` uses only lowercase letters, digits, and `-`, starts with a letter, is at most 24 characters long, and is unique across the run. Keeping this prefix lets `ax-swarm` clean up any leftover tasks automatically.\n", cfg.Name)
	fmt.Fprintf(&b, "6. **Parallelism and isolation.** Fan out independent files, components, hypotheses, or checks across parallel swarm agents, up to %d concurrent AX Tasks. Each AX Task receives its own isolated copy of `/workspace` from the single shared Workspace snapshot; filesystem changes in one task are not visible in other tasks. Share results and coordinate fan-in through the swarm.\n", cfg.Parallel)
	b.WriteString("7. **Handle failures actively.** If an AX Task enters `Failed` or `Crashed`, inspect it with `describe task`, diagnose the cause, and adjust the approach rather than blindly repeating the same command.\n")
	if cfg.Keep {
		b.WriteString("8. **Keep tasks (`--keep` is set).** Do not delete AX Tasks when finished unless cleaning up a broken attempt; the user requested preserving swarm resources for inspection.\n")
	} else {
		fmt.Fprintf(&b, "8. **Clean up completed tasks.** Delete each AX Task with `%s delete task <task-name>` once its work is collected (or suspend idle tasks with `suspend task` if you plan to reuse them). Any remaining `%s-*` tasks, along with the shared Workspace and Gateway, will also be deleted automatically when this session exits.\n", axBaseCmd, cfg.Name)
	}
	b.WriteString("\n")

	b.WriteString("## AX CLI Task Lifecycle Reference\n\n")
	b.WriteString("Use these exact commands to control each AX Task:\n\n")

	b.WriteString("### 1. Create an AX Task\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "%s apply -f - <<'EOF'\n", axBaseCmd)
	b.WriteString("apiVersion: ax.io/v1alpha1\n")
	b.WriteString("kind: Task\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s-<step>\n", cfg.Name)
	b.WriteString("spec:\n")
	b.WriteString("  workspace:\n")
	fmt.Fprintf(&b, "    name: %s\n", cfg.Name)
	b.WriteString("  gateway:\n")
	fmt.Fprintf(&b, "    name: %s\n", cfg.Name)
	b.WriteString("  goal:\n")
	b.WriteString("    text: \"<concise step goal>\"\n")
	b.WriteString("  config:\n")
	fmt.Fprintf(&b, "    swarm: %s\n", cfg.Name)
	b.WriteString("    step: <step>\n")
	b.WriteString("  debug: true\n")
	b.WriteString("EOF\n")
	b.WriteString("```\n\n")
	b.WriteString("`debug: true` is required so `ax ssh` can run commands inside the task container.\n\n")

	b.WriteString("### 2. Start / Resume the Task until `Running`\n\n")
	b.WriteString("A newly applied Task typically starts in `Suspended` state once seeded from the Workspace snapshot. Poll `get tasks` until `STATE` is `Suspended` or `Running`, resume it if `Suspended`, and confirm `STATE` is `Running` before calling `ssh`:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "%s get tasks\n", axBaseCmd)
	fmt.Fprintf(&b, "%s resume task %s-<step>\n", axBaseCmd, cfg.Name)
	b.WriteString("```\n\n")

	b.WriteString("### 3. Execute Commands inside `/workspace`\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "%s ssh %s-<step> -- sh -lc 'cd /workspace && <command>'\n", axBaseCmd, cfg.Name)
	b.WriteString("```\n\n")

	b.WriteString("### 4. Inspect, Suspend, or Delete Tasks\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "%s describe task %s-<step>\n", axBaseCmd, cfg.Name)
	fmt.Fprintf(&b, "%s suspend task %s-<step>\n", axBaseCmd, cfg.Name)
	fmt.Fprintf(&b, "%s delete task %s-<step>\n", axBaseCmd, cfg.Name)
	b.WriteString("```\n\n")

	b.WriteString("Finish by synthesizing the evidence gathered from the AX Tasks into a clear, well-supported answer for the user.\n")

	return b.String()
}
