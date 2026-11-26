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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var workerTaskRe = regexp.MustCompile("Complete exactly one task, `([^`]+)`")

// fakeAgy plays every orchestrator role based on the prompt header.
type fakeAgy struct {
	mu            sync.Mutex
	allArgs       [][]string
	workerPrompts map[string][]string
	reviewCalls   int
	running       int
	maxRunning    int
	// failFirst makes the first attempt of these tasks return an error.
	failFirst map[string]bool
	// failAlways makes these tasks always report failed.
	failAlways map[string]bool
	plan       string
	reviews    []string
}

func newFakeAgy() *fakeAgy {
	return &fakeAgy{
		workerPrompts: map[string][]string{},
		failFirst:     map[string]bool{},
		failAlways:    map[string]bool{},
		plan: `{"title":"Demo","tasks":[
			{"id":"a","title":"Task A","goal":"do a"},
			{"id":"b","title":"Task B","goal":"do b"},
			{"id":"c","title":"Task C","goal":"combine","depends_on":["a","b"]}]}`,
		reviews: []string{`{"complete":true,"reason":"all good"}`},
	}
}

func (f *fakeAgy) Run(ctx context.Context, args []string, onEvent func(AgyEvent)) (AgyOutcome, error) {
	prompt := args[len(args)-1]
	f.mu.Lock()
	f.allArgs = append(f.allArgs, args)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(prompt, plannerHeader):
		return AgyOutcome{Text: f.plan}, nil
	case strings.HasPrefix(prompt, reviewerHeader):
		f.mu.Lock()
		i := min(f.reviewCalls, len(f.reviews)-1)
		f.reviewCalls++
		f.mu.Unlock()
		return AgyOutcome{Text: f.reviews[i]}, nil
	case strings.HasPrefix(prompt, synthesizerHeader):
		onEvent(AgyEvent{Kind: AgyEventText, Text: "Final answer\n"})
		return AgyOutcome{Text: "Final answer\n"}, nil
	case strings.HasPrefix(prompt, workerHeader):
		id := workerTaskRe.FindStringSubmatch(prompt)[1]
		f.mu.Lock()
		f.workerPrompts[id] = append(f.workerPrompts[id], prompt)
		attempt := len(f.workerPrompts[id])
		f.running++
		f.maxRunning = max(f.maxRunning, f.running)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.running--
			f.mu.Unlock()
		}()
		onEvent(AgyEvent{Kind: AgyEventTool, ToolName: "run_command", ToolState: "ACTIVE", ToolSummary: "ax ssh x -- ls"})
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return AgyOutcome{}, ctx.Err()
		}
		if f.failFirst[id] && attempt == 1 {
			return AgyOutcome{}, errors.New("worker crashed")
		}
		if f.failAlways[id] {
			return AgyOutcome{Text: "```json\n{\"status\":\"failed\",\"summary\":\"nope\"}\n```"}, nil
		}
		return AgyOutcome{Text: fmt.Sprintf("Did it.\n```json\n{\"status\":\"done\",\"summary\":\"%s ok\",\"details\":\"evidence for %s\"}\n```", id, id)}, nil
	}
	return AgyOutcome{}, fmt.Errorf("unexpected prompt: %.40s", prompt)
}

func testConfig() Config {
	return Config{
		Goal:          "Audit the repo",
		Image:         "localhost:5001/ax-runner@sha256:" + strings.Repeat("a", 64),
		Atespace:      "default",
		Name:          "ax-o-test",
		Parallel:      2,
		MaxRounds:     2,
		MaxRetries:    1,
		MaxTasks:      10,
		SetupTimeout:  time.Minute,
		WorkerTimeout: time.Minute,
		AXBin:         "/usr/local/bin/ax",
		AgyBin:        "agy",
		Model:         "test-model",
		Color:         "never",
		WorkspaceSpec: map[string]any{"setup": map[string]any{"git": []any{map[string]any{"repo": "https://github.com/chalk/chalk"}}}},
	}
}

func newTestOrchestrator(cfg Config, fax *fakeAX, fagy *fakeAgy) (*Orchestrator, *Env) {
	ax := newAX(fax, cfg)
	ax.PollInterval = time.Millisecond
	env := &Env{AX: ax, Name: cfg.Name}
	return NewOrchestrator(cfg, env, fagy), env
}

func collect(t *testing.T, o *Orchestrator) ([]Event, error) {
	t.Helper()
	var mu sync.Mutex
	var events []Event
	err := o.Run(context.Background(), func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	return events, err
}

func TestOrchestratorSingleGatewayAndWorkspace(t *testing.T) {
	cfg := testConfig()
	fax, fagy := newFakeAX(), newFakeAgy()
	o, env := newTestOrchestrator(cfg, fax, fagy)

	events, err := collect(t, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	kinds := fax.appliedKinds()
	if kinds["Gateway"] != 1 || kinds["Workspace"] != 1 {
		t.Fatalf("applied Gateways=%d Workspaces=%d, want exactly 1 each", kinds["Gateway"], kinds["Workspace"])
	}
	if kinds["Task"] != 3 {
		t.Fatalf("applied Tasks=%d, want 3", kinds["Task"])
	}
	for _, m := range fax.applied {
		if m["kind"] != "Task" {
			continue
		}
		spec := m["spec"].(map[string]any)
		ws := spec["workspace"].(map[string]any)["name"]
		gw := spec["gateway"].(map[string]any)["name"]
		if ws != cfg.Name || gw != cfg.Name {
			t.Errorf("task %v uses workspace=%v gateway=%v, want shared %s", m["metadata"], ws, gw, cfg.Name)
		}
		if spec["debug"] != true {
			t.Errorf("task %v missing debug: true", m["metadata"])
		}
	}

	// c depends on a and b, so its worker must see both reports.
	cPrompt := fagy.workerPrompts["c"][0]
	for _, want := range []string{"Task `a`", "Task `b`", "evidence for a", "ssh ax-o-test-c"} {
		if !strings.Contains(cPrompt, want) {
			t.Errorf("worker prompt for c missing %q", want)
		}
	}
	if fagy.maxRunning > cfg.Parallel {
		t.Errorf("max concurrent workers = %d, want <= %d", fagy.maxRunning, cfg.Parallel)
	}
	if got := o.FinalAnswer(); got != "Final answer\n" {
		t.Errorf("FinalAnswer = %q", got)
	}
	var setupDone, text bool
	for _, ev := range events {
		setupDone = setupDone || ev.Kind == EvSetupDone
		text = text || (ev.Kind == EvText && strings.Contains(ev.Text, "Final answer"))
	}
	if !setupDone || !text {
		t.Errorf("events missing setupDone=%v text=%v", setupDone, text)
	}
	for _, snap := range o.Snapshot() {
		if snap.Status != StatusDone {
			t.Errorf("task %s status %s, want done", snap.ID, snap.Status)
		}
	}

	// Every agy call is a plain -p call: never /swarm, slash commands off.
	for _, args := range fagy.allArgs {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "/swarm") {
			t.Errorf("agy args use /swarm: %v", args[:len(args)-1])
		}
		if !strings.Contains(joined, "--disable-slash-commands") {
			t.Errorf("agy args missing --disable-slash-commands")
		}
	}

	// Each AX Task is deleted once its worker finishes; cleanup removes the rest.
	if len(fax.tasks) != 0 {
		t.Errorf("tasks left after run: %v", fax.tasks)
	}
	res := env.CleanupWithResult(context.Background())
	if res.Err != nil || !res.WorkspaceDeleted || !res.GatewayDeleted {
		t.Errorf("cleanup = %+v", res)
	}
}

func TestOrchestratorRetriesAndSkipsDependents(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRounds = 1
	fax, fagy := newFakeAX(), newFakeAgy()
	fagy.failFirst["a"] = true
	fagy.failAlways["b"] = true
	o, _ := newTestOrchestrator(cfg, fax, fagy)

	if _, err := collect(t, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status := map[string]TaskStatus{}
	for _, s := range o.Snapshot() {
		status[s.ID] = s.Status
	}
	if status["a"] != StatusDone || status["b"] != StatusFailed || status["c"] != StatusSkipped {
		t.Fatalf("statuses = %v, want a=done b=failed c=skipped", status)
	}
	if n := len(fagy.workerPrompts["a"]); n != 2 {
		t.Errorf("a attempts = %d, want 2", n)
	}
	if !strings.Contains(fagy.workerPrompts["a"][1], "worker crashed") || !strings.Contains(fagy.workerPrompts["a"][1], "ssh ax-o-test-a-r2") {
		t.Errorf("retry prompt lacks previous error or retry task name")
	}
	if _, ok := fagy.workerPrompts["c"]; ok {
		t.Errorf("c ran even though dependency b failed")
	}
	if fagy.reviewCalls != 0 {
		t.Errorf("reviewer called %d times with --max-rounds=1", fagy.reviewCalls)
	}
}

func TestOrchestratorReviewAddsFollowUps(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRounds = 3
	fax, fagy := newFakeAX(), newFakeAgy()
	fagy.reviews = []string{
		`{"complete":false,"reason":"needs verification","tasks":[{"id":"verify","title":"Verify","goal":"verify c","depends_on":["c"]}]}`,
		`{"complete":true,"reason":"verified"}`,
	}
	o, _ := newTestOrchestrator(cfg, fax, fagy)
	if _, err := collect(t, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fagy.reviewCalls != 2 {
		t.Errorf("review calls = %d, want 2", fagy.reviewCalls)
	}
	p := fagy.workerPrompts["verify"]
	if len(p) != 1 || !strings.Contains(p[0], "Task `c`") {
		t.Fatalf("verify worker did not receive c's report")
	}
	if k := fax.appliedKinds(); k["Gateway"] != 1 || k["Workspace"] != 1 || k["Task"] != 4 {
		t.Errorf("applied = %v, want 1 Gateway, 1 Workspace, 4 Tasks", k)
	}
}

func TestOrchestratorPlannerFallback(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRounds = 1
	fax, fagy := newFakeAX(), newFakeAgy()
	fagy.plan = "I could not plan."
	o, _ := newTestOrchestrator(cfg, fax, fagy)
	if _, err := collect(t, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snaps := o.Snapshot()
	if len(snaps) != 1 || snaps[0].ID != "main" || snaps[0].Status != StatusDone {
		t.Fatalf("snapshots = %+v, want single done task main", snaps)
	}
}

func TestAddTasksSanitizesAndDropsForwardDeps(t *testing.T) {
	cfg := testConfig()
	o, _ := newTestOrchestrator(cfg, newFakeAX(), newFakeAgy())
	n := o.addTasks([]plannedTaskJ{
		{ID: "First Step!", Title: "one", Goal: "g1", DependsOn: []string{"second"}},
		{ID: "second", Title: "two", Goal: "g2", DependsOn: []string{"First Step!", "missing"}},
		{ID: "second", Title: "dup", Goal: "g3"},
		{ID: "empty", Title: "no goal"},
		{ID: "9lives-and-a-very-long-identifier", Title: "long", Goal: "g4"},
	}, 1, 10)
	if n != 4 {
		t.Fatalf("added %d, want 4", n)
	}
	snaps := o.Snapshot()
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	want := []string{"first-step", "second", "second-2", "t-9lives-and-a-very"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if deps := o.tasks["first-step"].DependsOn; len(deps) != 0 {
		t.Errorf("forward dep kept: %v", deps)
	}
	if deps := o.tasks["second"].DependsOn; len(deps) != 1 || deps[0] != "first-step" {
		t.Errorf("second deps = %v, want [first-step]", deps)
	}
	for _, id := range ids {
		if len(id) > maxTaskIDLen || len(cfg.Name)+1+len(id)+3 > 63 {
			t.Errorf("id %q too long", id)
		}
	}
}

func TestParsePlanVerdictAndWorkerReport(t *testing.T) {
	p, ok := parsePlan(AgyOutcome{Text: "Here:\n```json\n{\"title\":\"x\",\"tasks\":[{\"id\":\"a\",\"title\":\"A\",\"goal\":\"g\"}]}\n```\nbye"})
	if !ok || len(p.Tasks) != 1 {
		t.Fatalf("fenced plan not decoded: %+v", p)
	}
	// Real agy output: response holds a draft object followed by the
	// schema-conforming one, and structured_output holds the latter.
	concatenated := "{\n  \"answer\": true\n}\n{\"complete\":true,\"reason\":\"ok\"}\n"
	v, ok := parseVerdict(AgyOutcome{Text: concatenated})
	if !ok || v.Complete == nil || !*v.Complete || v.Reason != "ok" {
		t.Fatalf("verdict from concatenated text = %+v ok=%v", v, ok)
	}
	v, ok = parseVerdict(AgyOutcome{Text: "garbage", Structured: `{"complete":false,"reason":"gap"}`})
	if !ok || v.Complete == nil || *v.Complete || v.Reason != "gap" {
		t.Fatalf("verdict from structured output = %+v ok=%v", v, ok)
	}
	if _, ok := parsePlan(AgyOutcome{Text: `{"answer": true}`}); ok {
		t.Errorf("object without tasks accepted as a plan")
	}
	r := parseWorkerReport("notes\n```json\n{\"status\":\"BLOCKED\",\"summary\":\"no net\"}\n```")
	if r.Status != "blocked" || r.Summary != "no net" {
		t.Errorf("report = %+v", r)
	}
	r = parseWorkerReport("I ran {a:1} then found:\n{\"status\":\"done\",\"summary\":\"s\",\"details\":\"d\"}")
	if r.Status != "done" || r.Details != "d" {
		t.Errorf("trailing report = %+v", r)
	}
	r = parseWorkerReport("just prose findings")
	if r.Status != "done" || r.Details != "just prose findings" {
		t.Errorf("prose report = %+v", r)
	}
}

func TestBuildAgyArgs(t *testing.T) {
	cfg := testConfig()
	cfg.AllowRules = []string{"mcp(github/*)"}
	cfg.DangerouslySkipPermissions = true

	worker := BuildAgyArgs(cfg, "P", agyCall{AllowAX: true})
	joined := strings.Join(worker, " ")
	for _, want := range []string{"--output-format stream-json", "--disable-slash-commands", "command(/usr/local/bin/ax)", "mcp(github/*)", "--model test-model", "--dangerously-skip-permissions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("worker args missing %q: %v", want, worker)
		}
	}
	if worker[len(worker)-2] != "-p" || worker[len(worker)-1] != "P" {
		t.Errorf("prompt not last: %v", worker)
	}

	planner := strings.Join(BuildAgyArgs(cfg, "P", agyCall{Schema: plannerSchema}), " ")
	if strings.Contains(planner, "--settings") || strings.Contains(planner, "--dangerously-skip-permissions") {
		t.Errorf("planner gets tool permissions: %s", planner)
	}
	if !strings.Contains(planner, "--json-schema") {
		t.Errorf("planner missing --json-schema")
	}
	var schema map[string]any
	for _, s := range []string{plannerSchema, reviewerSchema} {
		if err := json.Unmarshal([]byte(s), &schema); err != nil {
			t.Errorf("schema is not valid JSON: %v", err)
		}
	}
}

func TestPromptsNeverUseSwarm(t *testing.T) {
	cfg := testConfig()
	cfg.Goal = "/swarm do things"
	reports := []TaskReport{{ID: "a", Title: "A", Goal: "g", Status: StatusDone, Summary: "s"}}
	prompts := map[string]string{
		"planner":  BuildPlannerPrompt(cfg),
		"worker":   BuildWorkerPrompt(cfg, "ax -a default", WorkerTask{ID: "a", Title: "A", Goal: "g", AXName: "ax-o-test-a", Attempt: 1}, reports),
		"reviewer": BuildReviewerPrompt(cfg, reports, 1, 5),
		"synth":    BuildSynthesisPrompt(cfg, reports),
	}
	for name, p := range prompts {
		if strings.HasPrefix(p, "/") {
			t.Errorf("%s prompt starts with a slash command", name)
		}
		if !strings.Contains(p, "https://github.com/chalk/chalk") && name != "synth" {
			t.Errorf("%s prompt missing workspace repo", name)
		}
	}
	if !strings.Contains(prompts["worker"], "Do not create, resume, suspend, or delete any AX resource") {
		t.Errorf("worker prompt missing AX ownership rule")
	}
}

func TestRunHeadless(t *testing.T) {
	cfg := testConfig()
	fax, fagy := newFakeAX(), newFakeAgy()
	o, env := newTestOrchestrator(cfg, fax, fagy)
	starter := func(ctx context.Context, cfg Config) <-chan Event { return startOrchestrator(ctx, o) }

	var stdout, stderr bytes.Buffer
	code := runHeadless(cfg, env, strings.NewReader(""), &stdout, &stderr, false, starter)
	if code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr.String())
	}
	if stdout.String() != "Final answer\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	log := stderr.String()
	for _, want := range []string{"Created Gateway ax-o-test", "Created Workspace ax-o-test", "task c done"} {
		if !strings.Contains(log, want) {
			t.Errorf("stderr missing %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log, "deleted Workspace and Gateway (ax-o-test*)") {
		t.Errorf("stderr missing cleanup summary:\n%s", log)
	}
	if len(fax.gateways) != 0 || len(fax.workspaces) != 0 {
		t.Errorf("resources left: gateways=%v workspaces=%v", fax.gateways, fax.workspaces)
	}
}

func TestTUIModelRendersTasks(t *testing.T) {
	cfg := testConfig()
	fax := newFakeAX()
	env := &Env{AX: newAX(fax, cfg), Name: cfg.Name}
	m := newTUIModel(cfg, env, newTUIStyles(&bytes.Buffer{}, "never", false), func(context.Context, Config) <-chan Event { return nil })

	step := func(ev Event) {
		next, _ := m.Update(orchEventMsg{ev: ev, ok: true})
		m = next.(tuiModel)
	}
	step(Event{Kind: EvStep, Level: LevelOK, Text: "Created Gateway ax-o-test"})
	if v := m.View(); !strings.Contains(v, progName) || !strings.Contains(v, "Created Gateway ax-o-test") {
		t.Fatalf("setup view:\n%s", v)
	}
	step(Event{Kind: EvSetupDone})
	step(Event{Kind: EvTasks, Tasks: []TaskSnapshot{
		{ID: "a", Status: StatusRunning, AXName: "ax-o-test-a", Since: time.Now(), Activity: "ax ssh"},
		{ID: "b", Status: StatusPending, Since: time.Now()},
	}})
	v := m.View()
	for _, want := range []string{"TASK", "AX TASK", "ax-o-test-a", "running", "pending"} {
		if !strings.Contains(v, want) {
			t.Errorf("running view missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "ACTIVITY") || strings.Contains(v, "ax ssh") {
		t.Errorf("table shows worker activity:\n%s", v)
	}
}

func TestValidateConfigResourcePrefix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"audit", "ax-o-audit"},
		{"ax-o-audit", "ax-o-audit"},
		{"2026-run", "ax-o-2026-run"},
		{strings.Repeat("a", 33), "ax-o-" + strings.Repeat("a", 33)},
	} {
		cfg := testConfig()
		cfg.Name = tc.in
		if err := validateConfig(&cfg, "/nonexistent"); err != nil {
			t.Errorf("--name %q: %v", tc.in, err)
			continue
		}
		if cfg.Name != tc.want {
			t.Errorf("--name %q -> %q, want %q", tc.in, cfg.Name, tc.want)
		}
	}
	cfg := testConfig()
	cfg.Name = ""
	if err := validateConfig(&cfg, "/nonexistent"); err != nil || !regexp.MustCompile(`^ax-o-[0-9a-f]{6}$`).MatchString(cfg.Name) {
		t.Errorf("random name = %q (err %v), want ax-o-<hex>", cfg.Name, err)
	}
	for _, bad := range []string{strings.Repeat("a", 34), "Bad_Name", "trailing-"} {
		cfg := testConfig()
		cfg.Name = bad
		if err := validateConfig(&cfg, "/nonexistent"); err == nil {
			t.Errorf("--name %q accepted", bad)
		}
	}
	cfg = testConfig()
	cfg.MaxRetries = 9
	if err := validateConfig(&cfg, "/nonexistent"); err == nil {
		t.Errorf("--max-retries=9 accepted")
	}
}

func TestParseAgyStreamLine(t *testing.T) {
	lines := []string{
		`{"event":"init","init":{"model":"m1"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi"}}`,
		`{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE","tool_name":"run_command","tool_info":{"parameters":{"CommandLine":"ax -a default ssh t -- ls"}}}}`,
		`{"event":"result","result":{"status":"DONE","response":"final","structured_output":{"complete":true}}}`,
	}
	var kinds []AgyEventKind
	for _, l := range lines {
		ev, ok := parseAgyStreamLine(l)
		if !ok {
			t.Fatalf("not parsed: %s", l)
		}
		kinds = append(kinds, ev.Kind)
		if ev.Kind == AgyEventTool && ev.ToolSummary != "ax -a default ssh t -- ls" {
			t.Errorf("tool summary = %q", ev.ToolSummary)
		}
		if ev.Kind == AgyEventResult && (ev.Response != "final" || ev.Structured != `{"complete":true}`) {
			t.Errorf("result = %+v", ev)
		}
	}
	want := []AgyEventKind{AgyEventInit, AgyEventText, AgyEventTool, AgyEventResult}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}
