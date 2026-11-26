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
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestCreateGatewayAlreadyExistsIsNotAdopted(t *testing.T) {
	cfg := testConfig()
	fax := newFakeAX()
	fax.gateways[cfg.Name] = true // someone else's Gateway
	env := &Env{AX: newAX(fax, cfg), Name: cfg.Name}

	err := env.CreateGateway(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateGateway err = %v, want already exists", err)
	}
	if env.created() {
		t.Fatalf("existing Gateway marked as created by this run")
	}
	if res := env.CleanupWithResult(context.Background()); res.GatewayDeleted {
		t.Fatalf("cleanup deleted a Gateway this run did not create")
	}
	if !fax.gateways[cfg.Name] {
		t.Fatalf("foreign Gateway was deleted")
	}
}

func TestSetupReportsWorkspaceSetupError(t *testing.T) {
	cfg := testConfig()
	fax, fagy := newFakeAX(), newFakeAgy()
	fax.workspacePhase = v1alpha1.WorkspacePhaseFailed
	o, env := newTestOrchestrator(cfg, fax, fagy)

	_, err := collect(t, o)
	if err == nil || !strings.Contains(err.Error(), "failed setup: clone failed") {
		t.Fatalf("Run err = %v, want workspace setup error", err)
	}
	if len(fagy.allArgs) != 0 {
		t.Errorf("agy ran although setup failed")
	}
	res := env.CleanupWithResult(context.Background())
	if !res.WorkspaceDeleted || !res.GatewayDeleted {
		t.Errorf("cleanup after failed setup = %+v", res)
	}
}

func TestWorkspaceProtoStrict(t *testing.T) {
	spec := map[string]any{
		"setup": map[string]any{
			"git":      []any{map[string]any{"repo": "https://github.com/chalk/chalk", "depth": 1}},
			"commands": []any{"npm ci"},
		},
		"mcp": []any{map[string]any{"name": "docs", "endpoint": "https://example.com/mcp"}},
	}
	ws, err := WorkspaceProto("run", "img@sha256:"+strings.Repeat("a", 64), spec)
	if err != nil {
		t.Fatalf("WorkspaceProto: %v", err)
	}
	if got := ws.GetSpec().GetSetup().GetGit()[0].GetDepth(); got != 1 {
		t.Errorf("git depth = %d", got)
	}
	if got := ws.GetSpec().GetSetup().GetGateway().GetName(); got != "run" {
		t.Errorf("setup.gateway = %q, want the run's Gateway", got)
	}
	if got := ws.GetSpec().GetMcp().GetServers()[0].GetName(); got != "docs" {
		t.Errorf("legacy mcp list not normalized: %q", got)
	}

	_, err = WorkspaceProto("run", "", map[string]any{"setup": map[string]any{"gti": []any{}}})
	if err == nil || !strings.Contains(err.Error(), "gti") {
		t.Errorf("misspelled field accepted: %v", err)
	}

	gw, err := GatewayProto("run", []string{"crates.io"})
	if err != nil {
		t.Fatalf("GatewayProto: %v", err)
	}
	if n := len(gw.GetSpec().GetEgress()); n != 3 {
		t.Errorf("egress rules = %d, want 3", n)
	}
}

func TestValidateConfigRejectsBadWorkspaceSpec(t *testing.T) {
	cfg := testConfig()
	cfg.Workspace = `{"setup":{"git":[{"repoo":"https://github.com/chalk/chalk"}]}}`
	if err := validateConfig(&cfg, "/nonexistent"); err == nil || !strings.Contains(err.Error(), "repoo") {
		t.Fatalf("validateConfig err = %v, want unknown field error", err)
	}
}

func TestCleanupPaginatesTasks(t *testing.T) {
	cfg := testConfig()
	fax := newFakeAX()
	env := &Env{AX: newAX(fax, cfg), Name: cfg.Name}
	ctx := context.Background()
	if err := env.CreateGateway(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.CreateWorkspace(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"a", "b", "c"} {
		if err := env.AX.CreateTask(ctx, cfg.Name+"-"+step, cfg.Name, step, "g"); err != nil {
			t.Fatal(err)
		}
	}
	// A task from another run must survive cleanup.
	if err := env.AX.CreateTask(ctx, "other-run-x", cfg.Name, "x", "g"); err != nil {
		t.Fatal(err)
	}
	res := env.CleanupWithResult(ctx)
	if res.Err != nil || len(res.TasksDeleted) != 3 || !res.WorkspaceDeleted || !res.GatewayDeleted {
		t.Fatalf("cleanup = %+v", res)
	}
	if _, ok := fax.tasks["other-run-x"]; !ok {
		t.Errorf("cleanup deleted another run's task")
	}
}

func TestWaitTaskRunningResumesAndFailsOnCrash(t *testing.T) {
	cfg := testConfig()
	fax := newFakeAX()
	ax := newAX(fax, cfg)
	ax.PollInterval = time.Millisecond
	ctx := context.Background()
	fax.workspaces[cfg.Name] = true
	if err := ax.CreateTask(ctx, "t1", cfg.Name, "s", "g"); err != nil {
		t.Fatal(err)
	}
	if err := ax.WaitTaskRunning(ctx, "t1", time.Second); err != nil {
		t.Fatalf("WaitTaskRunning: %v", err)
	}
	fax.tasks["t1"] = v1alpha1.TaskState_TASK_STATE_CRASHED
	if err := ax.WaitTaskRunning(ctx, "t1", time.Second); err == nil || !strings.Contains(err.Error(), "Crashed") {
		t.Fatalf("crashed task err = %v", err)
	}
}

func TestExecCommandLine(t *testing.T) {
	cfg := testConfig()
	cfg.KubeContext = "kind kind"
	got := newAX(nil, cfg).ExecCommandLine()
	if want := "/usr/local/bin/ax -a default --context 'kind kind'"; got != want {
		t.Errorf("ExecCommandLine = %q, want %q", got, want)
	}
}
