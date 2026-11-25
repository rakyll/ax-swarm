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
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeCall struct {
	bin   string
	args  []string
	stdin string
}

type fakeRunner struct {
	calls   []fakeCall
	handler func(call fakeCall) (stdout, stderr string, err error)
}

func (f *fakeRunner) Run(_ context.Context, bin string, args []string, stdin []byte) (string, string, error) {
	c := fakeCall{
		bin:   bin,
		args:  slices.Clone(args),
		stdin: string(stdin),
	}
	f.calls = append(f.calls, c)
	if f.handler != nil {
		return f.handler(c)
	}
	return "", "", nil
}

func TestParseTableColumnAndPrefix(t *testing.T) {
	wsOut := `NAME             ATESPACE   PHASE       GIT-REPOS   MCP-SERVERS
swarm-231ece     default    Preparing   0           0
swarm-6c4bcd     default    Ready       1           0
`
	if got := parseTableColumn(wsOut, "swarm-6c4bcd", "PHASE"); got != "Ready" {
		t.Fatalf("got phase %q, want Ready", got)
	}
	if got := parseTableColumn(wsOut, "swarm-231ece", "PHASE"); got != "Preparing" {
		t.Fatalf("got phase %q, want Preparing", got)
	}
	if got := parseTableColumn(wsOut, "missing", "PHASE"); got != "" {
		t.Fatalf("got %q for missing workspace, want empty", got)
	}
	if got := parseTableColumn("", "swarm-6c4bcd", "PHASE"); got != "" {
		t.Fatalf("got %q for empty output, want empty", got)
	}

	taskOut := `NAME                              ATESPACE   STATE       ACTOR                             AGE
demo-task                         default    Suspended   demo-task                         9h
swarm-6c4bcd-analyze-testing      default    Running     swarm-6c4bcd-analyze-testing      19h
swarm-6c4bcd-analyze-api          default    Suspended   swarm-6c4bcd-analyze-api          19h
swarm-other-task                  default    Running     swarm-other-task                  1h
`
	got := parseTableNamesWithPrefix(taskOut, "swarm-6c4bcd-")
	want := []string{"swarm-6c4bcd-analyze-testing", "swarm-6c4bcd-analyze-api"}
	if !slices.Equal(got, want) {
		t.Fatalf("got tasks %v, want %v", got, want)
	}
	running, total := parseTableTaskCountsWithPrefix(taskOut, "swarm-6c4bcd-")
	if running != 1 || total != 2 {
		t.Fatalf("counts = (%d, %d), want (1, 2)", running, total)
	}
	if got := parseTableNamesWithPrefix("No tasks found.\n", "swarm-6c4bcd-"); len(got) != 0 {
		t.Fatalf("expected empty tasks for non-table output, got %v", got)
	}
}

func TestBaseCommandLineQuoting(t *testing.T) {
	ax := newAX("/Users/my user/bin/ax", "team-a", "prod ctx", "http://127.0.0.1:54380")
	got := ax.BaseCommandLine()
	want := "'/Users/my user/bin/ax' -a team-a --context 'prod ctx' --server http://127.0.0.1:54380"
	if got != want {
		t.Fatalf("BaseCommandLine() = %q, want %q", got, want)
	}
}

func TestManifests(t *testing.T) {
	gw := GatewayManifest("swarm-abc123", []string{"api.example.com"})
	raw, err := json.Marshal(gw)
	if err != nil {
		t.Fatalf("marshal gateway: %v", err)
	}
	if !strings.Contains(string(raw), `"kind":"Gateway"`) || !strings.Contains(string(raw), `"api.example.com"`) {
		t.Fatalf("unexpected gateway manifest: %s", raw)
	}

	// Workspace without repos/setupCmds omits setup block so AX skips setup job.
	wsEmpty := WorkspaceManifest("swarm-abc123", "localhost:5001/ax-runner@sha256:"+strings.Repeat("a", 64), nil, nil)
	spec := wsEmpty["spec"].(map[string]any)
	if _, hasSetup := spec["setup"]; hasSetup {
		t.Fatalf("expected no setup block when repos and setupCmds are empty: %#v", wsEmpty)
	}

	// Workspace with repos and setupCmds sets gateway, git depth=1, and commands.
	wsFull := WorkspaceManifest("swarm-abc123", "localhost:5001/ax-runner@sha256:"+strings.Repeat("a", 64),
		[]string{"https://github.com/chalk/chalk"}, []string{"npm ci"})
	rawWS, err := json.Marshal(wsFull)
	if err != nil {
		t.Fatalf("marshal workspace: %v", err)
	}
	s := string(rawWS)
	for _, want := range []string{`"gateway":{"name":"swarm-abc123"}`, `"repo":"https://github.com/chalk/chalk"`, `"depth":1`, `"commands":["npm ci"]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("workspace manifest missing %s: %s", want, s)
		}
	}
}

func TestSwarmEnvSetupAndCleanup(t *testing.T) {
	var polls int
	runner := &fakeRunner{
		handler: func(c fakeCall) (string, string, error) {
			if slices.Contains(c.args, "workspaces") {
				polls++
				phase := "Preparing"
				if polls >= 2 {
					phase = "Ready"
				}
				return "NAME ATESPACE PHASE\nswarm-abc123 default " + phase + "\n", "", nil
			}
			if slices.Contains(c.args, "tasks") {
				return "NAME ATESPACE STATE\nswarm-abc123-step1 default Running\nswarm-abc123-step2 default Suspended\nother-task default Running\n", "", nil
			}
			return "", "", nil
		},
	}

	ax := &AX{
		Bin:          "ax",
		Atespace:     "default",
		Runner:       runner,
		PollInterval: time.Millisecond,
	}
	env := &SwarmEnv{AX: ax, Name: "swarm-abc123"}
	var logs bytes.Buffer
	ui := newUI(&logs, strings.NewReader(""), false, false)

	cfg := Config{
		Name:         "swarm-abc123",
		Atespace:     "default",
		Image:        "localhost:5001/ax-runner@sha256:" + strings.Repeat("a", 64),
		Repos:        []string{"https://github.com/chalk/chalk"},
		SetupTimeout: 5 * time.Second,
	}

	if err := env.Setup(context.Background(), cfg, ui); err != nil {
		t.Fatalf("Setup() unexpected error: %v", err)
	}
	if !env.GatewayCreated || !env.WorkspaceCreated {
		t.Fatalf("expected GatewayCreated and WorkspaceCreated to be true")
	}
	if polls != 2 {
		t.Fatalf("expected 2 workspace polls, got %d", polls)
	}

	env.Cleanup(context.Background(), false, ui)

	var deleted []string
	for _, c := range runner.calls {
		if len(c.args) >= 5 && c.args[2] == "delete" {
			deleted = append(deleted, c.args[3]+":"+c.args[4])
		}
	}
	wantDeleted := []string{
		"task:swarm-abc123-step1",
		"task:swarm-abc123-step2",
		"workspace:swarm-abc123",
		"gateway:swarm-abc123",
	}
	if !slices.Equal(deleted, wantDeleted) {
		t.Fatalf("deleted = %v, want %v", deleted, wantDeleted)
	}
}

func TestSwarmEnvSetupFailedWorkspace(t *testing.T) {
	runner := &fakeRunner{
		handler: func(c fakeCall) (string, string, error) {
			if slices.Contains(c.args, "workspaces") {
				return "NAME ATESPACE PHASE\nswarm-fail default Failed\n", "", nil
			}
			return "", "", nil
		},
	}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner, PollInterval: time.Millisecond}
	env := &SwarmEnv{AX: ax, Name: "swarm-fail"}
	var logs bytes.Buffer
	ui := newUI(&logs, strings.NewReader(""), false, false)

	err := env.Setup(context.Background(), Config{
		Name:         "swarm-fail",
		Atespace:     "default",
		Image:        "localhost:5001/ax-runner@sha256:" + strings.Repeat("b", 64),
		SetupTimeout: time.Second,
	}, ui)
	if err == nil || !strings.Contains(err.Error(), "failed setup") {
		t.Fatalf("expected failed setup error, got %v", err)
	}
}

func TestSwarmEnvCleanupKeepSkipsDeletes(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-keep", GatewayCreated: true, WorkspaceCreated: true}
	var logs bytes.Buffer
	ui := newUI(&logs, strings.NewReader(""), false, false)

	env.Cleanup(context.Background(), true, ui)
	if len(runner.calls) != 0 {
		t.Fatalf("expected no ax calls when keep=true, got %v", runner.calls)
	}
	if !strings.Contains(logs.String(), "keeping everything named swarm-keep*") {
		t.Fatalf("expected keep message in logs, got %q", logs.String())
	}
}

func TestAXRunErrorIncludesStderr(t *testing.T) {
	runner := &fakeRunner{
		handler: func(fakeCall) (string, string, error) {
			return "", "permission denied\n", errors.New("exit status 1")
		},
	}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	err := ax.Delete(context.Background(), "task", "swarm-1-step")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected stderr detail in error, got %v", err)
	}
}

func TestSingleWorkspaceAndGatewayPerSwarm(t *testing.T) {
	runner := &fakeRunner{}
	ax := &AX{Bin: "ax", Atespace: "default", Runner: runner}
	env := &SwarmEnv{AX: ax, Name: "swarm-one"}

	img := "localhost:5001/ax-runner@sha256:" + strings.Repeat("c", 64)
	repos := []string{"https://github.com/chalk/chalk", "https://github.com/google/go-cmp"}

	if err := env.CreateGateway(context.Background(), []string{"api.example.com"}); err != nil {
		t.Fatalf("first CreateGateway error: %v", err)
	}
	if err := env.CreateGateway(context.Background(), []string{"other.example.com"}); err != nil {
		t.Fatalf("second CreateGateway error: %v", err)
	}
	if err := env.CreateWorkspace(context.Background(), img, repos, []string{"npm ci"}); err != nil {
		t.Fatalf("first CreateWorkspace error: %v", err)
	}
	if err := env.CreateWorkspace(context.Background(), img, repos, []string{"npm test"}); err != nil {
		t.Fatalf("second CreateWorkspace error: %v", err)
	}

	var gateways, workspaces int
	for _, c := range runner.calls {
		if slices.Contains(c.args, "apply") {
			if strings.Contains(c.stdin, `"kind":"Gateway"`) {
				gateways++
			}
			if strings.Contains(c.stdin, `"kind":"Workspace"`) {
				workspaces++
			}
		}
	}
	if gateways != 1 || workspaces != 1 {
		t.Fatalf("created gateways=%d workspaces=%d, want 1 and 1", gateways, workspaces)
	}
}

func TestAutoResumerOnlyStartsNewTasks(t *testing.T) {
	r := newAutoResumer(3 * time.Second)
	now := time.Now()

	// A newly created Suspended task is resumed.
	due := r.due([]AXTaskInfo{{Name: "s-a", State: "Suspended"}}, now)
	if len(due) != 1 || due[0].Name != "s-a" {
		t.Fatalf("expected s-a to be resumed, got %v", due)
	}
	// Not retried before the retry interval.
	if due := r.due([]AXTaskInfo{{Name: "s-a", State: "Suspended"}}, now.Add(time.Second)); len(due) != 0 {
		t.Fatalf("expected no retry within interval, got %v", due)
	}
	// Retried after the interval if still Suspended.
	if due := r.due([]AXTaskInfo{{Name: "s-a", State: "Suspended"}}, now.Add(4*time.Second)); len(due) != 1 {
		t.Fatalf("expected retry after interval, got %v", due)
	}
	// Once it has been Running, a later Suspended (suspended on purpose) is left alone.
	r.due([]AXTaskInfo{{Name: "s-a", State: "Running"}}, now.Add(5*time.Second))
	if due := r.due([]AXTaskInfo{{Name: "s-a", State: "Suspended"}}, now.Add(20*time.Second)); len(due) != 0 {
		t.Fatalf("expected deliberately suspended task to be left alone, got %v", due)
	}
	// Failed/Crashed tasks are never resumed.
	if due := r.due([]AXTaskInfo{{Name: "s-b", State: "Crashed"}}, now); len(due) != 0 {
		t.Fatalf("expected crashed task to be left alone, got %v", due)
	}
}
