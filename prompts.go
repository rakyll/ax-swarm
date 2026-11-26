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
	"encoding/json"
	"fmt"
	"strings"
)

// Role headers identify which orchestrator role a prompt is for.
const (
	plannerHeader     = "# AX Orchestrator: Planner"
	workerHeader      = "# AX Orchestrator: Worker"
	reviewerHeader    = "# AX Orchestrator: Reviewer"
	synthesizerHeader = "# AX Orchestrator: Synthesizer"
)

// plannerSchema forces the planner's final answer into a task graph.
const plannerSchema = `{"type":"object","properties":{"title":{"type":"string"},"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"goal":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}}},"required":["id","title","goal"]}}},"required":["title","tasks"]}`

// reviewerSchema forces the reviewer's final answer into a completion verdict
// plus optional follow-up tasks.
const reviewerSchema = `{"type":"object","properties":{"complete":{"type":"boolean"},"reason":{"type":"string"},"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"goal":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}}},"required":["id","title","goal"]}}},"required":["complete","reason"]}`

// maxReportChars bounds how much of one task's report is embedded in later prompts.
const maxReportChars = 6000

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n > 1 && len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func writeEnvironment(b *strings.Builder, cfg Config) {
	b.WriteString("## AX Environment\n\n")
	b.WriteString("Exactly one AX Workspace and one AX Gateway exist for this whole run. Every task runs in its own AX Task that reuses them:\n")
	fmt.Fprintf(b, "- Shared Workspace: `%s` (mounted at `/workspace` in every AX Task)\n", cfg.Name)
	fmt.Fprintf(b, "- Shared Gateway: `%s` (HTTPS egress for GitHub, PyPI, npm, Go proxy", cfg.Name)
	if len(cfg.AllowHosts) > 0 {
		fmt.Fprintf(b, ", %s", strings.Join(cfg.AllowHosts, ", "))
	}
	b.WriteString(")\n")
	ws := WorkspaceManifest(cfg.Name, cfg.Image, cfg.WorkspaceSpec)
	if repos := workspaceRepos(ws); len(repos) > 0 {
		b.WriteString("- Repositories cloned into `/workspace`:\n")
		for _, r := range repos {
			fmt.Fprintf(b, "  - `%s`\n", r)
		}
	}
	if cmds := workspaceCommands(ws); len(cmds) > 0 {
		b.WriteString("- Setup commands already applied to `/workspace`:\n")
		for _, c := range cmds {
			fmt.Fprintf(b, "  - `%s`\n", c)
		}
	}
	b.WriteString("- Each AX Task gets its own isolated copy of `/workspace` from the shared Workspace snapshot: file changes made by one task are NOT visible to other tasks. A task that changes files reports a unified diff, and dependent tasks receive that diff and can apply it with `git apply`.\n\n")
}

func writeTaskRules(b *strings.Builder, cfg Config, maxTasks int) {
	b.WriteString("## Task Rules\n\n")
	fmt.Fprintf(b, "- Return at most %d tasks. Prefer a few substantial tasks over many tiny ones.\n", maxTasks)
	fmt.Fprintf(b, "- Up to %d tasks run in parallel; maximize independent work that can run concurrently.\n", cfg.Parallel)
	b.WriteString("- `id`: short and unique; lowercase letters, digits, and `-`; starts with a letter; at most 20 characters.\n")
	b.WriteString("- `goal`: a complete, self-contained brief for a worker that has never seen this conversation. Include what to inspect or run, and what evidence to report.\n")
	b.WriteString("- `depends_on`: ids of tasks that must finish first (their reports are handed to this task). Only reference tasks listed earlier, or tasks from earlier rounds. Omit or leave empty for independent tasks.\n")
	b.WriteString("- Every task must be doable by running shell commands inside `/workspace`. Do not create tasks for writing the final answer; the orchestrator synthesizes it from the task reports.\n\n")
}

// BuildPlannerPrompt asks agy to decompose the goal into a task graph.
func BuildPlannerPrompt(cfg Config) string {
	var b strings.Builder
	b.WriteString(plannerHeader + "\n\n")
	b.WriteString("You are the planner of a custom task orchestrator. Break the user's goal into a dependency graph of tasks. The orchestrator will run each task in its own AX Task with a dedicated worker agent, review the results, and synthesize the final answer.\n\n")
	b.WriteString("## Goal\n\n")
	b.WriteString(strings.TrimSpace(cfg.Goal) + "\n\n")
	writeEnvironment(&b, cfg)
	writeTaskRules(&b, cfg, min(cfg.MaxTasks, 3*cfg.Parallel))
	b.WriteString("## Output\n\n")
	b.WriteString("Do not call any tools and do not run any commands; you cannot see the workspace. Plan from the goal and environment above (early tasks can explore). Respond with only a JSON object of the form:\n\n")
	b.WriteString("```json\n{\"title\": \"<short title for the run>\", \"tasks\": [{\"id\": \"explore\", \"title\": \"<short title>\", \"goal\": \"<self-contained brief>\", \"depends_on\": []}]}\n```\n")
	return b.String()
}

// TaskReport is the outcome of one task, shared with later prompts.
type TaskReport struct {
	ID      string
	Title   string
	Goal    string
	Round   int
	Status  TaskStatus
	Summary string
	Details string
	Patch   string
	Error   string
}

func writeReport(b *strings.Builder, r TaskReport) {
	fmt.Fprintf(b, "### Task `%s`: %s (round %d, %s)\n\n", r.ID, r.Title, r.Round, r.Status)
	fmt.Fprintf(b, "Goal: %s\n\n", truncate(oneLine(r.Goal), 600))
	if r.Summary != "" {
		fmt.Fprintf(b, "Summary: %s\n\n", truncate(r.Summary, 1200))
	}
	if r.Details != "" {
		fmt.Fprintf(b, "Details:\n\n%s\n\n", truncate(r.Details, maxReportChars))
	}
	if r.Patch != "" {
		fmt.Fprintf(b, "Patch:\n\n```diff\n%s\n```\n\n", truncate(r.Patch, maxReportChars))
	}
	if r.Error != "" {
		fmt.Fprintf(b, "Error: %s\n\n", truncate(oneLine(r.Error), 600))
	}
}

// WorkerTask is the task a worker is asked to complete.
type WorkerTask struct {
	ID      string
	Title   string
	Goal    string
	AXName  string
	Attempt int
	// PrevError explains why the previous attempt failed (retries only).
	PrevError string
}

// BuildWorkerPrompt asks agy to complete one task inside its AX Task.
func BuildWorkerPrompt(cfg Config, axBase string, t WorkerTask, deps []TaskReport) string {
	var b strings.Builder
	b.WriteString(workerHeader + "\n\n")
	fmt.Fprintf(&b, "You are a worker in a custom task orchestrator. Complete exactly one task, `%s`, using your dedicated AX Task, then report.\n\n", t.ID)
	b.WriteString("## Overall Goal (context only)\n\n")
	b.WriteString(truncate(cfg.Goal, 2000) + "\n\n")
	fmt.Fprintf(&b, "## Your Task: %s\n\n", t.Title)
	b.WriteString(strings.TrimSpace(t.Goal) + "\n\n")
	if t.Attempt > 1 && t.PrevError != "" {
		fmt.Fprintf(&b, "This is attempt %d. The previous attempt failed with: %s\nAdjust your approach instead of repeating it.\n\n", t.Attempt, truncate(oneLine(t.PrevError), 800))
	}
	if len(deps) > 0 {
		b.WriteString("## Reports From Tasks You Depend On\n\n")
		for _, d := range deps {
			writeReport(&b, d)
		}
		b.WriteString("Patches from these tasks are NOT applied to your `/workspace`. If you need their changes, write the patch to a file inside your AX Task and `git apply` it.\n\n")
	}
	writeEnvironment(&b, cfg)
	b.WriteString("## How To Work\n\n")
	fmt.Fprintf(&b, "Your AX Task `%s` is already Running and dedicated to you. Run every command inside it, never on the local host:\n\n", t.AXName)
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "%s ssh %s -- sh -lc 'cd /workspace && <command 1> && <command 2>'\n", axBase, t.AXName)
	b.WriteString("```\n\n")
	b.WriteString("- Batch related commands into one `ssh` call; every separate shell call costs a full turn.\n")
	b.WriteString("- Always call `run_command` with `BypassSandbox` set to true for these `ax` commands: they must reach the AX cluster, and the default command sandbox blocks that network access.\n")
	b.WriteString("- Use only that `ssh` command form. Do not create, resume, suspend, or delete any AX resource; the orchestrator owns the Task, Workspace, and Gateway lifecycle.\n")
	b.WriteString("- Do not read, build, or modify files on the local host.\n")
	b.WriteString("- If you change files, capture the change with `git diff` (inside the AX Task) and include it as `patch`.\n")
	b.WriteString("- If a command fails, diagnose and adapt. If you cannot finish, report `blocked` or `failed` with evidence rather than guessing.\n\n")
	b.WriteString("## Report\n\n")
	b.WriteString("End your final response with exactly one fenced JSON block of this form (the orchestrator parses it):\n\n")
	b.WriteString("```json\n{\"status\": \"done|blocked|failed\", \"summary\": \"<one or two sentences>\", \"details\": \"<findings with concrete evidence: commands, outputs, file/line references>\", \"patch\": \"<unified diff, or empty>\"}\n```\n")
	return b.String()
}

// BuildReviewerPrompt asks agy whether the goal is complete and, if not, which
// follow-up tasks to run next.
func BuildReviewerPrompt(cfg Config, reports []TaskReport, round, remainingTasks int) string {
	var b strings.Builder
	b.WriteString(reviewerHeader + "\n\n")
	fmt.Fprintf(&b, "You are the reviewer of a custom task orchestrator. Round %d of at most %d just finished. Decide whether the task reports below are enough to fully and confidently answer the goal.\n\n", round, cfg.MaxRounds)
	b.WriteString("## Goal\n\n")
	b.WriteString(strings.TrimSpace(cfg.Goal) + "\n\n")
	b.WriteString("## Task Reports So Far\n\n")
	for _, r := range reports {
		writeReport(&b, r)
	}
	writeEnvironment(&b, cfg)
	writeTaskRules(&b, cfg, remainingTasks)
	b.WriteString("## Output\n\n")
	b.WriteString("Do not call any tools and do not run any commands. If the goal is fully addressed, set `complete` to true and return no tasks. Otherwise set `complete` to false and return only the follow-up tasks needed to close the gaps (retry failed or blocked work with a better approach, verify unproven claims, or go deeper). Use new ids that do not repeat earlier ones. Respond with only a JSON object of the form:\n\n")
	b.WriteString("```json\n{\"complete\": false, \"reason\": \"<what is missing or why it is done>\", \"tasks\": [{\"id\": \"verify-fix\", \"title\": \"<short title>\", \"goal\": \"<self-contained brief>\", \"depends_on\": [\"<earlier id>\"]}]}\n```\n")
	return b.String()
}

// BuildSynthesisPrompt asks agy to write the final answer from task reports.
func BuildSynthesisPrompt(cfg Config, reports []TaskReport) string {
	var b strings.Builder
	b.WriteString(synthesizerHeader + "\n\n")
	b.WriteString("You are the synthesizer of a custom task orchestrator. Worker agents ran the tasks below, each inside its own isolated AX Task. Write the final answer to the goal for the user.\n\n")
	b.WriteString("## Goal\n\n")
	b.WriteString(strings.TrimSpace(cfg.Goal) + "\n\n")
	b.WriteString("## Task Reports\n\n")
	for _, r := range reports {
		writeReport(&b, r)
	}
	b.WriteString("## Instructions\n\n")
	b.WriteString("- Do not call any tools and do not run any commands; use only the reports above.\n")
	b.WriteString("- Answer the goal directly in Markdown. Ground claims in the reported evidence (commands, outputs, file/line references) and include patches when they are part of the answer.\n")
	b.WriteString("- Clearly state anything that failed, stayed blocked, or remains unverified.\n")
	b.WriteString("- Do not describe the orchestration process itself.\n")
	return b.String()
}

// planJSON is the planner/reviewer task graph shape.
type planJSON struct {
	Title    string         `json:"title"`
	Complete *bool          `json:"complete"`
	Reason   string         `json:"reason"`
	Tasks    []plannedTaskJ `json:"tasks"`
}

type plannedTaskJ struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Goal      string   `json:"goal"`
	DependsOn []string `json:"depends_on"`
}

// workerReportJSON is the fenced JSON report a worker ends with.
type workerReportJSON struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Details string `json:"details"`
	Patch   string `json:"patch"`
}

// decodeJSON finds a JSON object in agy's output that decodes into T and
// satisfies valid. agy's text can hold several JSON objects (for example a
// draft followed by the schema-conforming answer), so every candidate is
// tried, preferring later ones: fenced blocks (last first), then every
// top-level object in the text (last first), then the whole text.
func decodeJSON[T any](text string, valid func(T) bool) (T, bool) {
	trimmed := strings.TrimSpace(text)
	candidates := fencedBlocks(trimmed)
	objs := jsonObjects(trimmed)
	for i := len(objs) - 1; i >= 0; i-- {
		candidates = append(candidates, objs[i])
	}
	candidates = append(candidates, trimmed)
	var zero T
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if !strings.HasPrefix(c, "{") {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(c), &v); err == nil && (valid == nil || valid(v)) {
			return v, true
		}
	}
	return zero, false
}

// jsonObjects returns every top-level JSON object embedded in text, in order.
func jsonObjects(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		j := strings.IndexByte(text[i:], '{')
		if j < 0 {
			break
		}
		start := i + j
		dec := json.NewDecoder(strings.NewReader(text[start:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			i = start + 1
			continue
		}
		out = append(out, string(raw))
		i = start + int(dec.InputOffset())
	}
	return out
}

// fencedBlocks returns the contents of ``` fenced blocks, last block first.
func fencedBlocks(text string) []string {
	var blocks []string
	rest := text
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			break
		}
		after := rest[start+3:]
		nl := strings.IndexByte(after, '\n')
		if nl < 0 {
			break
		}
		body := after[nl+1:]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		blocks = append(blocks, body[:end])
		rest = body[end+3:]
	}
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	return blocks
}

// parsePlan extracts a task graph, preferring agy's schema-validated
// structured output over the response text.
func parsePlan(out AgyOutcome) (planJSON, bool) {
	valid := func(p planJSON) bool { return len(p.Tasks) > 0 }
	if p, ok := decodeJSON(out.Structured, valid); ok {
		return p, true
	}
	return decodeJSON(out.Text, valid)
}

// parseVerdict extracts a reviewer verdict, preferring structured output.
func parseVerdict(out AgyOutcome) (planJSON, bool) {
	valid := func(p planJSON) bool { return p.Complete != nil }
	if p, ok := decodeJSON(out.Structured, valid); ok {
		return p, true
	}
	return decodeJSON(out.Text, valid)
}

// parseWorkerReport extracts a worker's report. Without a parseable JSON block
// the whole response becomes the details of a `done` report, so useful
// free-form output is never thrown away.
func parseWorkerReport(text string) workerReportJSON {
	r, ok := decodeJSON(text, func(r workerReportJSON) bool {
		return r.Status != "" || r.Summary != "" || r.Details != ""
	})
	if ok {
		r.Status = strings.ToLower(strings.TrimSpace(r.Status))
		switch r.Status {
		case "done", "blocked", "failed":
		default:
			r.Status = "done"
		}
		return r
	}
	return workerReportJSON{
		Status:  "done",
		Summary: truncate(oneLine(text), 200),
		Details: strings.TrimSpace(text),
	}
}
