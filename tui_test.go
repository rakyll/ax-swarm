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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func testTUIConfig(goal string) Config {
	return Config{
		Goal:         goal,
		Name:         "swarm-tui01",
		Atespace:     "default",
		Image:        "localhost:5001/ax-runner@sha256:" + strings.Repeat("a", 64),
		Repos:        []string{"https://github.com/chalk/chalk"},
		Parallel:     8,
		SetupTimeout: 30 * time.Second,
		Color:        "never",
		AXBin:        "ax",
		AgyBin:       "agy",
	}
}

func noopAgyStarter(_ context.Context, _ string, _ []string) (<-chan AgyEvent, error) {
	ch := make(chan AgyEvent, 1)
	ch <- AgyEvent{Kind: AgyEventDone}
	close(ch)
	return ch, nil
}

func TestTUIGoalInputAndValidation(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner, PollInterval: time.Millisecond}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01"}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	m := newTUIModel(testTUIConfig(""), env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()

	if m.stage != stageGoalInput {
		t.Fatalf("expected stageGoalInput, got %v", m.stage)
	}
	if view := m.View(); !strings.Contains(view, "What should the swarm do?") {
		t.Fatalf("goal input view missing prompt: %q", view)
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(tuiModel)
	if m.input.Width != 88 {
		t.Fatalf("input width = %d, want 88", m.input.Width)
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(tuiModel)
	if m.stage != stageGoalInput || cmd != nil {
		t.Fatalf("empty enter should stay in stageGoalInput without cmd")
	}
	if !strings.Contains(m.View(), "Enter a goal to start the swarm.") {
		t.Fatalf("expected validation hint in view: %q", m.View())
	}

	for _, r := range "analyze chalk" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(tuiModel)
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(tuiModel)
	if m.stage != stageCreateGateway {
		t.Fatalf("expected stageCreateGateway after Enter, got %v", m.stage)
	}
	if m.cfg.Goal != "analyze chalk" {
		t.Fatalf("cfg.Goal = %q, want %q", m.cfg.Goal, "analyze chalk")
	}
	if cmd == nil {
		t.Fatalf("expected gateway creation command after submitting goal")
	}
	if strings.Contains(m.View(), "\x1b[") {
		t.Fatalf("color=never view should not contain ANSI escape codes: %q", m.View())
	}
}

func TestTUIGoalInputEscCancels(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01"}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	m := newTUIModel(testTUIConfig(""), env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(tuiModel)
	if !m.cancelled || m.stage != stageDone {
		t.Fatalf("expected cancelled stageDone on Esc, got cancelled=%v stage=%v", m.cancelled, m.stage)
	}
	if cmd == nil {
		t.Fatalf("expected tea.Quit command on Esc")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected zero AX calls on goal cancel, got %v", runner.calls)
	}
}

func TestTUIHappyPathBackgroundAgyStreaming(t *testing.T) {
	var polls int
	runner := &fakeRunner{
		handler: func(c fakeCall) (string, string, error) {
			if slices.Contains(c.args, "workspaces") {
				polls++
				phase := "Preparing"
				if polls >= 2 {
					phase = "Ready"
				}
				return "NAME ATESPACE PHASE\nswarm-tui01 default " + phase + "\n", "", nil
			}
			if slices.Contains(c.args, "tasks") {
				return "NAME ATESPACE STATE\nswarm-tui01-worker1 default Running\n", "", nil
			}
			return "", "", nil
		},
	}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner, PollInterval: time.Millisecond}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01"}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	streamCh := make(chan AgyEvent, 8)
	var launchedBin string
	var launchedArgs []string
	m := newTUIModel(testTUIConfig("audit chalk"), env, styles, func(_ context.Context, bin string, args []string) (<-chan AgyEvent, error) {
		launchedBin = bin
		launchedArgs = slices.Clone(args)
		return streamCh, nil
	})
	defer m.cancelSetup()
	defer m.cancelAgy()

	// Step 1: Create Gateway
	updated, cmd := m.Update(m.createGatewayCmd()())
	m = updated.(tuiModel)
	if m.stage != stageCreateWorkspace || !env.GatewayCreated {
		t.Fatalf("expected stageCreateWorkspace with GatewayCreated, got stage=%v created=%v", m.stage, env.GatewayCreated)
	}

	// Step 2: Create Workspace
	updated, cmd = m.Update(cmd())
	m = updated.(tuiModel)
	if m.stage != stageWaitWorkspace || !env.WorkspaceCreated {
		t.Fatalf("expected stageWaitWorkspace with WorkspaceCreated, got stage=%v created=%v", m.stage, env.WorkspaceCreated)
	}

	// Step 3: Poll Workspace (Preparing -> tick -> Ready -> start background agy)
	updated, _ = m.Update(cmd())
	m = updated.(tuiModel)
	if m.stage != stageWaitWorkspace || m.workspacePhase != "Preparing" {
		t.Fatalf("expected Preparing wait state, got stage=%v phase=%q", m.stage, m.workspacePhase)
	}
	updated, cmd = m.Update(pollTickMsg{})
	m = updated.(tuiModel)
	updated, cmd = m.Update(cmd())
	m = updated.(tuiModel)
	if m.stage != stageStartingAgy {
		t.Fatalf("expected stageStartingAgy after Ready, got %v", m.stage)
	}

	// Start background agy process
	updated, _ = m.Update(cmd())
	m = updated.(tuiModel)
	if m.stage != stageRunningAgy {
		t.Fatalf("expected stageRunningAgy, got %v", m.stage)
	}
	if launchedBin != "agy" || !slices.Contains(launchedArgs, "stream-json") || launchedArgs[len(launchedArgs)-2] != "-p" || !strings.HasPrefix(launchedArgs[len(launchedArgs)-1], "/swarm audit chalk\n") {
		t.Fatalf("unexpected background agy invocation: bin=%q args=%v", launchedBin, launchedArgs)
	}

	// Feed stream events from background agy: init, task poll, partial text, newline, result, done.
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventInit, Model: "gemini-4-neon-high"}, ok: true})
	m = updated.(tuiModel)
	updated, _ = m.Update(taskCountsMsg{running: 1, total: 1})
	m = updated.(tuiModel)
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventText, Text: "Line 1\nPartial"}, ok: true})
	m = updated.(tuiModel)

	runningView := m.View()
	if strings.Contains(runningView, "Created Gateway") {
		t.Fatalf("running view should only show agy output and minimal status, got:\n%s", runningView)
	}
	if !strings.Contains(runningView, "Partial") || !strings.Contains(runningView, "1 AX tasks running") || !strings.Contains(runningView, "gemini-4-neon-high") {
		t.Fatalf("running view missing streaming partial line or status:\n%s", runningView)
	}

	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventText, Text: " line 2\n"}, ok: true})
	m = updated.(tuiModel)
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventResult, Status: "SUCCESS", Response: "Line 1\nPartial line 2\n"}, ok: true})
	m = updated.(tuiModel)
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventDone}, ok: true})
	m = updated.(tuiModel)
	if m.stage != stageCleanup {
		t.Fatalf("expected stageCleanup after agy done, got %v", m.stage)
	}
	if m.outputText() != "Line 1\nPartial line 2\n" {
		t.Fatalf("outputText() = %q", m.outputText())
	}

	updated, _ = m.Update(m.cleanupCmd()())
	m = updated.(tuiModel)
	if m.stage != stageDone || !m.cleanupRan {
		t.Fatalf("expected stageDone with cleanupRan=true, got stage=%v cleanupRan=%v", m.stage, m.cleanupRan)
	}
	if !strings.Contains(m.View(), "Deleted 1 task, Workspace, and Gateway (swarm-tui01*)") {
		t.Fatalf("final view missing cleanup summary:\n%s", m.View())
	}
}

func TestTUICancelDuringRunningAgyCancelsContextAndCleansUp(t *testing.T) {
	runner := &fakeRunner{
		handler: func(c fakeCall) (string, string, error) {
			if slices.Contains(c.args, "tasks") {
				return "NAME ATESPACE STATE\nswarm-tui01-w1 default Running\n", "", nil
			}
			return "", "", nil
		},
	}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01", GatewayCreated: true, WorkspaceCreated: true}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	m := newTUIModel(testTUIConfig("audit"), env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()
	m.stage = stageRunningAgy
	m.agyRan = true

	// Ctrl+C cancels background agy context
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(tuiModel)
	if !m.cancelled || m.agyCtx.Err() == nil {
		t.Fatalf("expected cancelled=true and agyCtx canceled")
	}

	// Once background agy exits, cleanup begins
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventDone, Err: errors.New("signal: terminated")}, ok: true})
	m = updated.(tuiModel)
	if m.stage != stageCleanup {
		t.Fatalf("expected stageCleanup after canceled agy exits, got %v", m.stage)
	}

	updated, _ = m.Update(m.cleanupCmd()())
	m = updated.(tuiModel)
	if m.stage != stageDone || !strings.Contains(m.View(), "Canceled.") {
		t.Fatalf("expected Canceled. in final view:\n%s", m.View())
	}
}

func TestTUIResultFallbackWhenNoTextDeltas(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01"}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	m := newTUIModel(testTUIConfig("audit"), env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()
	m.stage = stageRunningAgy
	m.agyRan = true

	updated, _ := m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventResult, Status: "SUCCESS", Response: "Only in result"}, ok: true})
	m = updated.(tuiModel)
	updated, _ = m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventDone}, ok: true})
	m = updated.(tuiModel)
	if m.outputText() != "Only in result" {
		t.Fatalf("outputText() = %q, want %q", m.outputText(), "Only in result")
	}
}

func TestTUIWorkspaceFailureTriggersCleanup(t *testing.T) {
	runner := &fakeRunner{
		handler: func(c fakeCall) (string, string, error) {
			if slices.Contains(c.args, "workspaces") {
				return "NAME ATESPACE PHASE\nswarm-tui01 default Failed\n", "", nil
			}
			if slices.Contains(c.args, "tasks") {
				return "NAME ATESPACE STATE\n", "", nil
			}
			return "", "", nil
		},
	}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner, PollInterval: time.Millisecond}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01", GatewayCreated: true, WorkspaceCreated: true}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	m := newTUIModel(testTUIConfig("check"), env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()
	m.stage = stageWaitWorkspace

	updated, _ := m.Update(m.pollWorkspaceCmd()())
	m = updated.(tuiModel)
	if m.err == nil || !strings.Contains(m.err.Error(), "failed setup") {
		t.Fatalf("expected workspace failed error, got %v", m.err)
	}
	if m.stage != stageCleanup {
		t.Fatalf("expected stageCleanup after workspace failure, got %v", m.stage)
	}

	updated, _ = m.Update(m.cleanupCmd()())
	m = updated.(tuiModel)
	if m.stage != stageDone || !m.cleanupRan {
		t.Fatalf("expected stageDone after cleanup, got stage=%v cleanupRan=%v", m.stage, m.cleanupRan)
	}
	if !strings.Contains(m.View(), "Deleted Workspace and Gateway (swarm-tui01*)") {
		t.Fatalf("expected cleanup summary in view:\n%s", m.View())
	}
}

func TestTUIKeepSkipsCleanup(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-tui01", GatewayCreated: true, WorkspaceCreated: true}
	var out bytes.Buffer
	styles := newTUIStyles(&out, "never", false)

	cfg := testTUIConfig("inspect")
	cfg.Keep = true
	m := newTUIModel(cfg, env, styles, noopAgyStarter)
	defer m.cancelSetup()
	defer m.cancelAgy()
	m.stage = stageRunningAgy
	m.agyRan = true

	updated, _ := m.Update(agyStreamMsg{ev: AgyEvent{Kind: AgyEventDone}, ok: true})
	m = updated.(tuiModel)
	if m.stage != stageDone || !m.cleanupRan {
		t.Fatalf("expected stageDone with cleanupRan=true when keep=true, got stage=%v", m.stage)
	}
	if !strings.Contains(m.View(), "Kept AX resources named swarm-tui01*") {
		t.Fatalf("expected kept resources message:\n%s", m.View())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected zero AX delete calls when keep=true, got %v", runner.calls)
	}
}
