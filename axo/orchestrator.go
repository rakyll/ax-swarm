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
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// maxTaskIDLen bounds task ids so `<id>-r<n>` still fits in maxAXStepLen.
const maxTaskIDLen = 20

// TaskStatus is the orchestrator-level state of one task.
type TaskStatus string

const (
	StatusPending  TaskStatus = "pending"
	StatusStarting TaskStatus = "starting"
	StatusRunning  TaskStatus = "running"
	StatusDone     TaskStatus = "done"
	StatusBlocked  TaskStatus = "blocked"
	StatusFailed   TaskStatus = "failed"
	StatusSkipped  TaskStatus = "skipped"
)

func (s TaskStatus) terminal() bool {
	switch s {
	case StatusDone, StatusBlocked, StatusFailed, StatusSkipped:
		return true
	}
	return false
}

// LogLevel styles an orchestrator log line.
type LogLevel int

const (
	LevelInfo LogLevel = iota
	LevelOK
	LevelWarn
	LevelErr
	LevelAction
)

func defaultIcon(l LogLevel) string {
	switch l {
	case LevelOK:
		return "✓"
	case LevelWarn:
		return "!"
	case LevelErr:
		return "✗"
	case LevelAction:
		return "▸"
	default:
		return "◌"
	}
}

// EventKind classifies orchestrator events consumed by the TUI/headless UI.
type EventKind int

const (
	// EvStep is a setup milestone (Gateway/Workspace).
	EvStep EventKind = iota
	// EvStatus updates the live spinner label.
	EvStatus
	// EvSetupDone marks the single Gateway and Workspace as ready.
	EvSetupDone
	// EvPhase updates the current orchestration phase.
	EvPhase
	// EvLog is a progress line.
	EvLog
	// EvTasks carries a snapshot of all tasks.
	EvTasks
	// EvText is a chunk of the final answer.
	EvText
	// EvDone is the final event; Err is the run error, if any.
	EvDone
)

// Event is one orchestrator progress event.
type Event struct {
	Kind  EventKind
	Level LogLevel
	Icon  string
	Text  string
	Tasks []TaskSnapshot
	Err   error
}

// TaskSnapshot is a read-only view of one task for rendering.
type TaskSnapshot struct {
	ID       string
	Title    string
	AXName   string
	Status   TaskStatus
	Round    int
	Attempt  int
	Since    time.Time
	Activity string
}

type taskRun struct {
	ID        string
	Title     string
	Goal      string
	DependsOn []string
	Round     int

	AXName   string
	Status   TaskStatus
	Attempt  int
	Since    time.Time
	Started  time.Time
	Activity string
	Report   TaskReport
	span     *SpanRef
}

// Orchestrator drives one run: setup, plan, execute/review rounds, synthesize.
type Orchestrator struct {
	cfg Config
	env *Env
	agy AgyRunner

	// TaskStartTimeout bounds how long a new AX Task may take to reach Running.
	TaskStartTimeout time.Duration

	// Trace records execution spans for the live trace viewer; nil disables
	// tracing.
	Trace *Tracer

	emitFn    func(Event)
	axBase    string
	root      *SpanRef
	roundSpan *SpanRef

	mu    sync.Mutex
	tasks map[string]*taskRun
	order []string
	title string
	final strings.Builder
}

// NewOrchestrator returns an orchestrator for cfg using env and agy.
func NewOrchestrator(cfg Config, env *Env, agy AgyRunner) *Orchestrator {
	return &Orchestrator{
		cfg:              cfg,
		env:              env,
		agy:              agy,
		TaskStartTimeout: 5 * time.Minute,
		axBase:           env.AX.ExecCommandLine(),
		tasks:            make(map[string]*taskRun),
	}
}

// FinalAnswer returns the synthesized answer text.
func (o *Orchestrator) FinalAnswer() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.final.String()
}

func (o *Orchestrator) emit(ev Event) {
	if o.emitFn != nil {
		o.emitFn(ev)
	}
}

func (o *Orchestrator) log(level LogLevel, icon, text string) {
	o.emit(Event{Kind: EvLog, Level: level, Icon: icon, Text: text})
	o.root.Event(levelName(level), text)
}

func levelName(l LogLevel) string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warn"
	case LevelErr:
		return "error"
	default:
		return "info"
	}
}

// spanStatus maps a task outcome to a span status.
func spanStatus(s TaskStatus) string {
	switch s {
	case StatusDone:
		return StatusSpanOK
	case StatusBlocked:
		return StatusSpanWarn
	case StatusSkipped:
		return StatusSpanSkipped
	default:
		return StatusSpanError
	}
}

func (o *Orchestrator) currentRound() *SpanRef {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.roundSpan != nil {
		return o.roundSpan
	}
	return o.root
}

func (o *Orchestrator) step(level LogLevel, text string) {
	o.emit(Event{Kind: EvStep, Level: level, Text: text})
}

func (o *Orchestrator) status(text string) {
	o.emit(Event{Kind: EvStatus, Text: text})
}

func (o *Orchestrator) phase(text string) {
	o.emit(Event{Kind: EvPhase, Text: text})
}

// Run executes the whole orchestration. Task failures are not fatal: they are
// reported, reviewed, and reflected in the final answer. Run returns an error
// only when setup or synthesis fails, or ctx is canceled.
func (o *Orchestrator) Run(ctx context.Context, emit func(Event)) (err error) {
	o.emitFn = emit
	o.root = o.Trace.Start(nil, "axo "+o.cfg.Name, SpanRun, map[string]string{
		"name":       o.cfg.Name,
		"goal":       o.cfg.Goal,
		"model":      o.cfg.Model,
		"atespace":   o.cfg.Atespace,
		"parallel":   strconv.Itoa(o.cfg.Parallel),
		"max_rounds": strconv.Itoa(o.cfg.MaxRounds),
		"repos":      strings.Join(workspaceRepos(map[string]any{"spec": o.cfg.WorkspaceSpec}), ", "),
	})
	defer func() {
		if err != nil && ctx.Err() != nil {
			o.Trace.CloseOpen(StatusSpanWarn, "canceled")
		} else {
			o.Trace.CloseOpen(StatusSpanWarn, "still running when the run ended")
		}
		o.root.EndErr(err)
	}()

	setupSpan := o.Trace.Start(o.root, "setup", SpanPhase, nil)
	err = o.setup(ctx, setupSpan)
	setupSpan.EndErr(err)
	if err != nil {
		return err
	}
	o.emit(Event{Kind: EvSetupDone})

	o.phase("planning")
	o.status("agy is planning tasks")
	o.addTasks(o.plan(ctx), 1, o.cfg.MaxTasks)
	if err := ctx.Err(); err != nil {
		return err
	}

	for round := 1; ; round++ {
		o.phase(fmt.Sprintf("round %d", round))
		o.status(fmt.Sprintf("round %d: running tasks", round))
		rs := o.Trace.Start(o.root, fmt.Sprintf("round %d", round), SpanPhase, nil)
		o.mu.Lock()
		o.roundSpan = rs
		o.mu.Unlock()
		o.executePending(ctx)
		rs.End(StatusSpanOK)
		if err := ctx.Err(); err != nil {
			return err
		}
		if round >= o.cfg.MaxRounds {
			if round > 1 || o.cfg.MaxRounds > 1 {
				o.log(LevelInfo, "◌", fmt.Sprintf("reached --max-rounds=%d", o.cfg.MaxRounds))
			}
			break
		}
		remaining := o.cfg.MaxTasks - o.taskCount()
		if remaining <= 0 {
			o.log(LevelInfo, "◌", fmt.Sprintf("reached --max-tasks=%d", o.cfg.MaxTasks))
			break
		}
		o.phase(fmt.Sprintf("review %d", round))
		o.status(fmt.Sprintf("agy is reviewing round %d", round))
		verdict, err := o.review(ctx, round, remaining)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			o.log(LevelWarn, "!", "review failed, synthesizing with current results: "+oneLinePreview(err.Error(), 120))
			break
		}
		complete := verdict.Complete != nil && *verdict.Complete
		if complete || len(verdict.Tasks) == 0 {
			o.log(LevelOK, "◆", "review: goal complete — "+oneLinePreview(verdict.Reason, 140))
			break
		}
		o.log(LevelAction, "◆", fmt.Sprintf("review: %d follow-up task(s) — %s", len(verdict.Tasks), oneLinePreview(verdict.Reason, 120)))
		if o.addTasks(verdict.Tasks, round+1, remaining) == 0 {
			break
		}
	}

	o.phase("synthesis")
	o.status("agy is synthesizing the answer")
	return o.synthesize(ctx)
}

func (o *Orchestrator) setup(ctx context.Context, sp *SpanRef) error {
	name := o.cfg.Name
	o.status(fmt.Sprintf("Creating Gateway %s...", name))
	gwSpan := o.Trace.Start(sp, "CreateGateway "+name, SpanAX, map[string]string{
		"rpc": "ax.v1alpha1.AX/CreateGateway", "gateway": name, "extra_hosts": strings.Join(o.cfg.AllowHosts, ", "),
	})
	err := o.env.CreateGateway(ctx, o.cfg.AllowHosts)
	gwSpan.EndErr(err)
	if err != nil {
		o.step(LevelErr, "Failed to create Gateway "+name)
		return err
	}
	o.step(LevelOK, "Created Gateway "+name)

	poll := o.env.AX.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	start := time.Now()
	label := func(phase string) string {
		if phase == "" {
			return "pending"
		}
		return phase
	}

	// CreateWorkspace returns only once the Workspace is pre-warmed, so run it
	// in the background and keep the status line fresh with GetWorkspace.
	o.status(fmt.Sprintf("Creating Workspace %s...", name))
	wsSpan := o.Trace.Start(sp, "CreateWorkspace "+name, SpanAX, map[string]string{
		"rpc": "ax.v1alpha1.AX/CreateWorkspace", "workspace": name, "image": o.cfg.Image,
		"repos":    strings.Join(workspaceRepos(map[string]any{"spec": o.cfg.WorkspaceSpec}), ", "),
		"commands": strings.Join(workspaceCommands(map[string]any{"spec": o.cfg.WorkspaceSpec}), "\n"),
	})
	created := make(chan error, 1)
	go func() { created <- o.env.CreateWorkspace(ctx, o.cfg) }()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for waiting := true; waiting; {
		select {
		case err := <-created:
			wsSpan.EndErr(err)
			if err != nil {
				o.step(LevelErr, "Failed to create Workspace "+name)
				return err
			}
			waiting = false
		case <-ticker.C:
			if phase, _, err := o.env.AX.WorkspacePhase(ctx, name); err == nil {
				o.status(fmt.Sprintf("Creating Workspace %s (%s · %s)", name, label(phase), formatElapsed(time.Since(start))))
			}
		case <-ctx.Done():
			<-created
			return ctx.Err()
		}
	}
	o.step(LevelOK, "Created Workspace "+name)

	readySpan := o.Trace.Start(sp, "wait for Workspace Ready", SpanAX, map[string]string{"rpc": "ax.v1alpha1.AX/GetWorkspace"})
	deadline := start.Add(o.cfg.SetupTimeout)
	for {
		phase, setupErr, err := o.env.AX.WorkspacePhase(ctx, name)
		readySpan.SetAttr("phase", phase)
		if err != nil || phase == v1alpha1.WorkspacePhaseFailed || time.Now().After(deadline) {
			e := err
			if e == nil {
				e = fmt.Errorf("workspace is %s: %s", phase, setupErr)
			}
			readySpan.EndErr(e)
		} else if phase == v1alpha1.WorkspacePhaseReady {
			readySpan.End(StatusSpanOK)
		}
		if err != nil {
			o.step(LevelErr, "Failed while checking Workspace "+name)
			return err
		}
		if phase == v1alpha1.WorkspacePhaseReady {
			o.step(LevelOK, fmt.Sprintf("Workspace %s ready (%s)", name, formatElapsed(time.Since(start))))
			return nil
		}
		if phase == v1alpha1.WorkspacePhaseFailed {
			o.step(LevelErr, "Workspace "+name+" failed setup")
			if setupErr == "" {
				setupErr = "no setup error reported"
			}
			return fmt.Errorf("workspace %s failed setup: %s", name, setupErr)
		}
		if time.Now().After(deadline) {
			o.step(LevelErr, "Workspace "+name+" setup timed out")
			return fmt.Errorf("workspace %s is %s after %s", name, label(phase), o.cfg.SetupTimeout)
		}
		o.status(fmt.Sprintf("Waiting for Workspace %s (%s · %s)", name, label(phase), formatElapsed(time.Since(start))))
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// noticeLogger returns an agy event callback that surfaces agy's own notices
// (e.g. headless permission denials) once each, tagged with who.
func (o *Orchestrator) noticeLogger(who string, sp *SpanRef) func(AgyEvent) {
	seen := make(map[string]bool)
	var mu sync.Mutex
	return func(ev AgyEvent) {
		if ev.Kind != AgyEventStderr || ev.ErrorText == "" {
			return
		}
		mu.Lock()
		dup := seen[ev.ErrorText]
		seen[ev.ErrorText] = true
		mu.Unlock()
		sp.Event("warn", "agy: "+ev.ErrorText)
		if !dup {
			o.log(LevelWarn, "!", fmt.Sprintf("[%s] agy: %s", who, oneLinePreview(ev.ErrorText, 140)))
		}
	}
}

func (o *Orchestrator) plan(ctx context.Context) []plannedTaskJ {
	prompt := BuildPlannerPrompt(o.cfg)
	args := BuildAgyArgs(o.cfg, prompt, agyCall{Schema: plannerSchema})
	sp := o.Trace.Start(o.root, "plan", SpanAgy, map[string]string{"role": "planner", "model": o.cfg.Model, "prompt": prompt})
	start := time.Now()
	out, err := o.agy.Run(ctx, args, o.noticeLogger("planner", sp))
	sp.SetAttr("response", out.Text, "structured_output", out.Structured)
	p, parsed := parsePlan(out)
	switch {
	case ctx.Err() != nil:
		sp.EndErr(ctx.Err())
		return nil
	case err != nil:
		sp.EndErr(err)
		o.log(LevelWarn, "!", "planner failed ("+oneLinePreview(err.Error(), 100)+"); running the goal as a single task")
	case !parsed:
		sp.SetAttr("note", "no parseable task graph; running the goal as a single task")
		sp.End(StatusSpanWarn)
		o.log(LevelWarn, "!", "planner returned no tasks; running the goal as a single task")
	default:
		sp.SetAttr("title", p.Title, "tasks", strconv.Itoa(len(p.Tasks)))
		sp.End(StatusSpanOK)
		title := strings.TrimSpace(p.Title)
		o.mu.Lock()
		o.title = title
		o.mu.Unlock()
		label := fmt.Sprintf("plan ready in %s: %d task(s)", formatElapsed(time.Since(start)), len(p.Tasks))
		if title != "" {
			label = title + " — " + label
		}
		o.log(LevelAction, "◆", label)
		return p.Tasks
	}
	return []plannedTaskJ{{ID: "main", Title: "Complete the goal", Goal: o.cfg.Goal}}
}

func (o *Orchestrator) taskCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.order)
}

// sanitizeID turns a planner-provided id into a safe AX name suffix.
func sanitizeID(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "task"
	}
	if id[0] < 'a' || id[0] > 'z' {
		id = "t-" + id
	}
	if len(id) > maxTaskIDLen {
		id = strings.TrimRight(id[:maxTaskIDLen], "-")
	}
	return id
}

// uniqueIDLocked returns id, or id with a numeric suffix, not yet in use.
func (o *Orchestrator) uniqueIDLocked(id string) string {
	if _, taken := o.tasks[id]; !taken {
		return id
	}
	for n := 2; ; n++ {
		suf := fmt.Sprintf("-%d", n)
		base := id
		if len(base)+len(suf) > maxTaskIDLen {
			base = strings.TrimRight(base[:maxTaskIDLen-len(suf)], "-")
		}
		if _, taken := o.tasks[base+suf]; !taken {
			return base + suf
		}
	}
}

// addTasks validates and registers planned tasks for round, adding at most
// limit. Dependencies may only point at tasks registered before the dependent
// one, which keeps the graph acyclic; other references are dropped.
func (o *Orchestrator) addTasks(planned []plannedTaskJ, round, limit int) int {
	added := 0
	idMap := make(map[string]string)
	var lines []string
	o.mu.Lock()
	for _, pt := range planned {
		if added >= limit {
			break
		}
		goal := strings.TrimSpace(pt.Goal)
		if goal == "" {
			continue
		}
		title := strings.TrimSpace(pt.Title)
		if title == "" {
			title = truncate(oneLine(goal), 60)
		}
		raw := pt.ID
		if strings.TrimSpace(raw) == "" {
			raw = title
		}
		id := o.uniqueIDLocked(sanitizeID(raw))
		var deps []string
		seen := make(map[string]bool)
		for _, d := range pt.DependsOn {
			ref, ok := idMap[d]
			if !ok {
				if _, exists := o.tasks[sanitizeID(d)]; exists {
					ref, ok = sanitizeID(d), true
				}
			}
			if ok && ref != id && !seen[ref] {
				seen[ref] = true
				deps = append(deps, ref)
			}
		}
		idMap[pt.ID] = id
		o.tasks[id] = &taskRun{
			ID: id, Title: title, Goal: goal, DependsOn: deps, Round: round,
			Status: StatusPending, Since: time.Now(),
		}
		o.order = append(o.order, id)
		added++
		line := fmt.Sprintf("task %s: %s", id, title)
		if len(deps) > 0 {
			line += " (after " + strings.Join(deps, ", ") + ")"
		}
		lines = append(lines, line)
	}
	o.mu.Unlock()
	for _, l := range lines {
		o.log(LevelOK, "+", l)
	}
	o.emitTasks()
	return added
}

func (o *Orchestrator) snapshotLocked() []TaskSnapshot {
	out := make([]TaskSnapshot, 0, len(o.order))
	for _, id := range o.order {
		t := o.tasks[id]
		out = append(out, TaskSnapshot{
			ID: t.ID, Title: t.Title, AXName: t.AXName, Status: t.Status,
			Round: t.Round, Attempt: t.Attempt, Since: t.Since, Activity: t.Activity,
		})
	}
	return out
}

// Snapshot returns the current task table.
func (o *Orchestrator) Snapshot() []TaskSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snapshotLocked()
}

func (o *Orchestrator) emitTasks() {
	o.emit(Event{Kind: EvTasks, Tasks: o.Snapshot()})
}

func (o *Orchestrator) update(t *taskRun, fn func(t *taskRun)) {
	o.mu.Lock()
	prev := t.Status
	fn(t)
	if t.Status != prev {
		t.Since = time.Now()
	}
	o.mu.Unlock()
	o.emitTasks()
}

type depState int

const (
	depsWaiting depState = iota
	depsReady
	depsBroken
)

func (o *Orchestrator) depStateLocked(t *taskRun) (depState, string) {
	state := depsReady
	for _, d := range t.DependsOn {
		dep, ok := o.tasks[d]
		if !ok {
			continue
		}
		switch dep.Status {
		case StatusDone:
		case StatusBlocked, StatusFailed, StatusSkipped:
			return depsBroken, d
		default:
			state = depsWaiting
		}
	}
	return state, ""
}

// executePending runs every pending task, respecting dependencies and
// --parallel. Tasks whose dependencies did not finish are skipped.
func (o *Orchestrator) executePending(ctx context.Context) {
	done := make(chan struct{})
	running := 0
	for {
		if ctx.Err() == nil {
			var ready []*taskRun
			var skipped []string
			o.mu.Lock()
			for changed := true; changed; {
				changed = false
				ready = ready[:0]
				for _, id := range o.order {
					t := o.tasks[id]
					if t.Status != StatusPending {
						continue
					}
					st, broken := o.depStateLocked(t)
					switch st {
					case depsReady:
						ready = append(ready, t)
					case depsBroken:
						t.Status, t.Since = StatusSkipped, time.Now()
						t.Report = TaskReport{
							ID: t.ID, Title: t.Title, Goal: t.Goal, Round: t.Round, Status: StatusSkipped,
							Error: fmt.Sprintf("dependency %s did not finish", broken),
						}
						skipped = append(skipped, fmt.Sprintf("task %s skipped: dependency %s did not finish", t.ID, broken))
						ss := o.Trace.Start(o.roundSpan, "task "+t.ID, SpanTask, map[string]string{
							"id": t.ID, "title": t.Title, "goal": t.Goal, "error": t.Report.Error,
						})
						ss.End(StatusSpanSkipped)
						changed = true
					}
				}
			}
			var launch []*taskRun
			for _, t := range ready {
				if running+len(launch) >= o.cfg.Parallel {
					break
				}
				t.Status, t.Since, t.Activity = StatusStarting, time.Now(), "queued"
				launch = append(launch, t)
			}
			o.mu.Unlock()
			for _, s := range skipped {
				o.log(LevelInfo, "○", s)
			}
			if len(skipped) > 0 || len(launch) > 0 {
				o.emitTasks()
			}
			for _, t := range launch {
				running++
				go func(t *taskRun) {
					o.runTask(ctx, t)
					done <- struct{}{}
				}(t)
			}
		}
		if running == 0 {
			return
		}
		<-done
		running--
	}
}

func (o *Orchestrator) runTask(ctx context.Context, t *taskRun) {
	span := o.Trace.Start(o.currentRound(), "task "+t.ID, SpanTask, map[string]string{
		"id": t.ID, "title": t.Title, "goal": t.Goal, "round": strconv.Itoa(t.Round),
		"depends_on": strings.Join(t.DependsOn, ", "),
	})
	o.mu.Lock()
	t.Started = time.Now()
	t.span = span
	o.mu.Unlock()
	prevErr := ""
	attempts := 1 + o.cfg.MaxRetries
	for attempt := 1; attempt <= attempts; attempt++ {
		report, retryable := o.attempt(ctx, t, attempt, prevErr)
		if ctx.Err() != nil {
			report.Status = StatusFailed
			report.Error = "canceled"
			o.finish(t, report)
			return
		}
		if !retryable || attempt == attempts {
			o.finish(t, report)
			return
		}
		prevErr = report.Error
		if prevErr == "" {
			prevErr = report.Summary
		}
		o.log(LevelWarn, "↻", fmt.Sprintf("task %s attempt %d failed, retrying: %s", t.ID, attempt, oneLinePreview(prevErr, 100)))
	}
}

func (o *Orchestrator) finish(t *taskRun, r TaskReport) {
	o.update(t, func(t *taskRun) {
		t.Status = r.Status
		t.Report = r
		t.Activity = ""
	})
	t.span.SetAttr("status", string(r.Status), "summary", r.Summary, "details", r.Details, "patch", r.Patch, "error", r.Error)
	t.span.End(spanStatus(r.Status))
	o.mu.Lock()
	took := formatElapsed(time.Since(t.Started))
	o.mu.Unlock()
	detail := r.Summary
	if detail == "" {
		detail = r.Error
	}
	detail = oneLinePreview(detail, 140)
	switch r.Status {
	case StatusDone:
		o.log(LevelOK, "✓", fmt.Sprintf("task %s done in %s: %s", t.ID, took, detail))
	case StatusBlocked:
		o.log(LevelWarn, "!", fmt.Sprintf("task %s blocked after %s: %s", t.ID, took, detail))
	default:
		o.log(LevelErr, "✗", fmt.Sprintf("task %s failed after %s: %s", t.ID, took, detail))
		if hint := axErrorHint(r.Error, o.cfg.Name); hint != "" {
			o.log(LevelInfo, " ", hint)
		}
	}
}

func (o *Orchestrator) depReports(t *taskRun) []TaskReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []TaskReport
	for _, d := range t.DependsOn {
		if dep, ok := o.tasks[d]; ok {
			out = append(out, dep.Report)
		}
	}
	return out
}

// attempt runs one try of task t in a fresh AX Task. It reports whether a
// failure is worth retrying.
func (o *Orchestrator) attempt(ctx context.Context, t *taskRun, attempt int, prevErr string) (TaskReport, bool) {
	report := TaskReport{ID: t.ID, Title: t.Title, Goal: t.Goal, Round: t.Round}
	axName := o.cfg.Name + "-" + t.ID
	if attempt > 1 {
		axName = fmt.Sprintf("%s-r%d", axName, attempt)
	}
	o.update(t, func(t *taskRun) {
		t.AXName, t.Attempt, t.Status, t.Activity = axName, attempt, StatusStarting, "creating ax task"
	})
	as := o.Trace.Start(t.span, fmt.Sprintf("attempt %d", attempt), SpanTry, map[string]string{
		"ax_task": axName, "previous_error": prevErr,
	})
	done := func(r TaskReport, retry bool) (TaskReport, bool) {
		as.SetAttr("status", string(r.Status), "error", r.Error)
		as.End(spanStatus(r.Status))
		return r, retry
	}

	goal := truncate(oneLine(t.Title+": "+t.Goal), 500)
	cs := o.Trace.Start(as, "CreateTask "+axName, SpanAX, map[string]string{
		"rpc": "ax.v1alpha1.AX/CreateTask", "task": axName, "workspace": o.cfg.Name, "gateway": o.cfg.Name, "goal": goal,
	})
	err := o.env.AX.CreateTask(ctx, axName, o.cfg.Name, t.ID, goal)
	cs.EndErr(err)
	if err != nil {
		report.Status, report.Error = StatusFailed, err.Error()
		return done(report, true)
	}
	o.log(LevelOK, "+", fmt.Sprintf("ax task %s created for task %s", axName, t.ID))
	o.update(t, func(t *taskRun) { t.Activity = "starting ax task" })
	ws := o.Trace.Start(as, "start "+axName, SpanAX, map[string]string{"rpc": "ax.v1alpha1.AX/ResumeTask + GetTask", "task": axName})
	err = o.env.AX.WaitTaskRunning(ctx, axName, o.TaskStartTimeout)
	ws.EndErr(err)
	if err != nil {
		o.deleteAXTask(ctx, as, axName)
		report.Status, report.Error = StatusFailed, err.Error()
		return done(report, true)
	}
	o.update(t, func(t *taskRun) { t.Status, t.Activity = StatusRunning, "agy worker started" })
	o.log(LevelOK, "▶", fmt.Sprintf("task %s running on ax task %s", t.ID, axName))

	prompt := BuildWorkerPrompt(o.cfg, o.axBase, WorkerTask{
		ID: t.ID, Title: t.Title, Goal: t.Goal, AXName: axName, Attempt: attempt, PrevError: prevErr,
	}, o.depReports(t))
	args := BuildAgyArgs(o.cfg, prompt, agyCall{AllowAX: true})

	wspan := o.Trace.Start(as, "worker "+t.ID, SpanAgy, map[string]string{
		"role": "worker", "model": o.cfg.Model, "ax_task": axName, "prompt": prompt,
	})
	onEvent, closeTools := o.workerEvents(t, wspan)
	wctx, cancel := context.WithTimeout(ctx, o.cfg.WorkerTimeout)
	out, err := o.agy.Run(wctx, args, onEvent)
	timedOut := errors.Is(wctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	closeTools()
	wspan.SetAttr("response", out.Text)
	if timedOut {
		err = fmt.Errorf("worker timed out after %s", o.cfg.WorkerTimeout)
	}
	o.deleteAXTask(ctx, as, axName)

	if err != nil {
		wspan.EndErr(err)
		report.Status = StatusFailed
		report.Error = err.Error()
		if strings.TrimSpace(out.Text) != "" {
			report.Details = strings.TrimSpace(out.Text)
		}
		return done(report, true)
	}
	r := parseWorkerReport(out.Text)
	report.Status = TaskStatus(r.Status)
	report.Summary = strings.TrimSpace(r.Summary)
	report.Details = strings.TrimSpace(r.Details)
	report.Patch = strings.TrimSpace(r.Patch)
	wspan.SetAttr("reported_status", r.Status)
	wspan.End(spanStatus(report.Status))
	return done(report, report.Status == StatusFailed)
}

func (o *Orchestrator) deleteAXTask(ctx context.Context, parent *SpanRef, name string) {
	if o.cfg.Keep || ctx.Err() != nil {
		return
	}
	sp := o.Trace.Start(parent, "DeleteTask "+name, SpanAX, map[string]string{"rpc": "ax.v1alpha1.AX/DeleteTask", "task": name})
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := o.env.AX.Delete(dctx, "task", name); err != nil && !isNotFound(err) {
		sp.EndErr(err)
		o.log(LevelWarn, "!", fmt.Sprintf("could not delete ax task %s: %s", name, oneLinePreview(err.Error(), 100)))
		return
	}
	sp.End(StatusSpanOK)
	o.log(LevelInfo, "✓", "ax task "+name+" deleted")
}

// workerEvents surfaces a worker's shell activity as progress lines and as
// one trace span per tool call under wspan. closeOpen ends tool spans that
// never reported completion.
func (o *Orchestrator) workerEvents(t *taskRun, wspan *SpanRef) (onEvent func(AgyEvent), closeOpen func()) {
	notices := o.noticeLogger(t.ID, wspan)
	var mu sync.Mutex
	tools := make(map[int]*SpanRef)
	toolSpan := func(ev AgyEvent, summary string) *SpanRef {
		mu.Lock()
		defer mu.Unlock()
		if sp, ok := tools[ev.StepIndex]; ok {
			return sp
		}
		name := summary
		if name == "" {
			name = ev.ToolName
		}
		sp := o.Trace.Start(wspan, name, SpanTool, map[string]string{
			"tool": ev.ToolName, "command": strings.ReplaceAll(ev.ToolCommand, o.axBase, "ax"),
		})
		tools[ev.StepIndex] = sp
		return sp
	}
	endTool := func(ev AgyEvent, summary string) {
		sp := toolSpan(ev, summary)
		mu.Lock()
		delete(tools, ev.StepIndex)
		mu.Unlock()
		if ev.ToolOutputFull != "" {
			sp.SetAttr("output", ev.ToolOutputFull)
		}
		if ev.HasExitCode {
			sp.SetAttr("exit_code", strconv.Itoa(ev.ToolExitCode))
		}
		switch {
		case ev.ErrorText != "":
			sp.SetAttr("error", ev.ErrorText)
			sp.End(StatusSpanError)
		case ev.HasExitCode && ev.ToolExitCode != 0:
			sp.End(StatusSpanError)
		default:
			sp.End(StatusSpanOK)
		}
	}
	closeOpen = func() {
		mu.Lock()
		defer mu.Unlock()
		for k, sp := range tools {
			sp.End(StatusSpanWarn)
			delete(tools, k)
		}
	}
	onEvent = func(ev AgyEvent) {
		switch ev.Kind {
		case AgyEventStderr:
			notices(ev)
		case AgyEventTool:
			summary := strings.ReplaceAll(ev.ToolSummary, o.axBase, "ax")
			if ev.ToolState == "ACTIVE" {
				toolSpan(ev, summary)
			} else {
				endTool(ev, summary)
			}
			switch {
			case ev.ToolState == "ACTIVE":
				if ev.ToolName == "run_command" && summary != "" {
					o.log(LevelAction, "▸", fmt.Sprintf("[%s] %s", t.ID, summary))
				}
				if summary != "" {
					o.update(t, func(t *taskRun) { t.Activity = truncate(summary, 60) })
				}
			case ev.ErrorText != "":
				o.log(LevelWarn, "!", fmt.Sprintf("[%s] %s: %s", t.ID, ev.ToolName, oneLinePreview(ev.ErrorText, 120)))
			case ev.ToolName == "run_command" && ev.HasExitCode && ev.ToolExitCode != 0:
				msg := fmt.Sprintf("exit %d", ev.ToolExitCode)
				if ev.ToolOutput != "" {
					msg += ": " + ev.ToolOutput
				}
				o.log(LevelErr, "✗", fmt.Sprintf("[%s] %s", t.ID, msg))
			}
		}
	}
	return onEvent, closeOpen
}

// reports returns the reports of all finished tasks in plan order.
func (o *Orchestrator) reports() []TaskReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []TaskReport
	for _, id := range o.order {
		if t := o.tasks[id]; t.Status.terminal() {
			out = append(out, t.Report)
		}
	}
	return out
}

func (o *Orchestrator) review(ctx context.Context, round, remaining int) (planJSON, error) {
	prompt := BuildReviewerPrompt(o.cfg, o.reports(), round, remaining)
	sp := o.Trace.Start(o.root, fmt.Sprintf("review %d", round), SpanAgy, map[string]string{"role": "reviewer", "model": o.cfg.Model, "prompt": prompt})
	out, err := o.agy.Run(ctx, BuildAgyArgs(o.cfg, prompt, agyCall{Schema: reviewerSchema}), o.noticeLogger("reviewer", sp))
	sp.SetAttr("response", out.Text, "structured_output", out.Structured)
	if err != nil {
		sp.EndErr(err)
		return planJSON{}, err
	}
	p, ok := parseVerdict(out)
	if !ok {
		err := fmt.Errorf("reviewer returned no verdict: %s", oneLinePreview(out.Text, 100))
		sp.EndErr(err)
		return planJSON{}, err
	}
	sp.SetAttr("complete", strconv.FormatBool(p.Complete != nil && *p.Complete), "reason", p.Reason, "follow_up_tasks", strconv.Itoa(len(p.Tasks)))
	sp.End(StatusSpanOK)
	return p, nil
}

func (o *Orchestrator) synthesize(ctx context.Context) error {
	reports := o.reports()
	prompt := BuildSynthesisPrompt(o.cfg, reports)
	sp := o.Trace.Start(o.root, "synthesis", SpanAgy, map[string]string{"role": "synthesizer", "model": o.cfg.Model, "prompt": prompt})
	defer func() { sp.SetAttr("answer", o.FinalAnswer()) }()
	notices := o.noticeLogger("synthesizer", sp)
	streamed := false
	out, err := o.agy.Run(ctx, BuildAgyArgs(o.cfg, prompt, agyCall{}), func(ev AgyEvent) {
		switch ev.Kind {
		case AgyEventText:
			if ev.Text != "" {
				streamed = true
				o.mu.Lock()
				o.final.WriteString(ev.Text)
				o.mu.Unlock()
				o.emit(Event{Kind: EvText, Text: ev.Text})
			}
		case AgyEventStderr:
			notices(ev)
		}
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			sp.EndErr(ctxErr)
			return ctxErr
		}
		if !streamed {
			o.emitText(fallbackAnswer(reports))
		}
		sp.EndErr(err)
		return fmt.Errorf("synthesis failed: %w", err)
	}
	sp.End(StatusSpanOK)
	if !streamed {
		text := out.Text
		if strings.TrimSpace(text) == "" {
			text = fallbackAnswer(reports)
		}
		o.emitText(text)
	}
	return nil
}

func (o *Orchestrator) emitText(text string) {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	o.mu.Lock()
	o.final.WriteString(text)
	o.mu.Unlock()
	o.emit(Event{Kind: EvText, Text: text})
}

// fallbackAnswer lists raw task reports when synthesis produced nothing.
func fallbackAnswer(reports []TaskReport) string {
	var b strings.Builder
	b.WriteString("Synthesis produced no answer; raw task reports follow.\n\n")
	for _, r := range reports {
		writeReport(&b, r)
	}
	return b.String()
}

// orchStarter starts an orchestration and returns its event stream. The
// stream always ends with EvDone and is then closed.
type orchStarter func(ctx context.Context, cfg Config) <-chan Event

func defaultOrchStarter(env *Env, tr *Tracer) orchStarter {
	return func(ctx context.Context, cfg Config) <-chan Event {
		o := NewOrchestrator(cfg, env, streamAgyRunner{bin: cfg.AgyBin})
		o.Trace = tr
		return startOrchestrator(ctx, o)
	}
}

func startOrchestrator(ctx context.Context, o *Orchestrator) <-chan Event {
	ch := make(chan Event, 256)
	go func() {
		err := o.Run(ctx, func(ev Event) { ch <- ev })
		ch <- Event{Kind: EvDone, Err: err}
		close(ch)
	}()
	return ch
}

func oneLinePreview(s string, maxLen int) string {
	clean := oneLine(s)
	for strings.Contains(clean, "  ") {
		clean = strings.ReplaceAll(clean, "  ", " ")
	}
	if maxLen > 1 && len(clean) > maxLen {
		return clean[:maxLen-1] + "…"
	}
	return clean
}

func formatElapsed(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// axErrorHint explains errors that come from the AX cluster rather than the orchestrator.
func axErrorHint(msg, name string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "runsc") || strings.Contains(lower, "callateletrestore"):
		return "The AX cluster could not start a sandbox (gVisor/runsc). This is an AX cluster problem: " +
			"check `ax local status` and the atelet logs " +
			"(`kubectl -n ate-system logs ds/atelet --all-containers`), or restart the local cluster."
	case strings.Contains(lower, "preparing workspace") || strings.Contains(lower, "failed setup"):
		return fmt.Sprintf("AX failed to set up the Workspace. Rerun with --keep to inspect it with "+
			"`ax describe workspace %s`.", name)
	case strings.Contains(lower, "unimplemented") || strings.Contains(lower, "not supported yet"):
		return "The AX server does not support part of the request (version skew between axo's AX API, the workers' ax CLI, and the server). " +
			"Point --ax-bin (or AX_BIN) at an ax binary built from the same AX version as the cluster."
	}
	return ""
}
