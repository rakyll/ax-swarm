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
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildAgyArgs(t *testing.T) {
	minimal := BuildAgyArgs(Config{}, "/swarm hello")
	wantMinimal := []string{"--output-format", "stream-json", "-p", "/swarm hello"}
	if !slices.Equal(minimal, wantMinimal) {
		t.Fatalf("minimal args = %v, want %v", minimal, wantMinimal)
	}

	full := BuildAgyArgs(Config{
		Model:                      "gemini-4-neon-high",
		Effort:                     "high",
		Mode:                       "auto",
		Project:                    "proj-1",
		DangerouslySkipPermissions: true,
	}, "/swarm test")
	wantFull := []string{
		"--output-format", "stream-json",
		"--model", "gemini-4-neon-high",
		"--effort", "high",
		"--mode", "auto",
		"--project", "proj-1",
		"--dangerously-skip-permissions",
		"-p", "/swarm test",
	}
	if !slices.Equal(full, wantFull) {
		t.Fatalf("full args = %v, want %v", full, wantFull)
	}
}

func TestParseAgyStreamLine(t *testing.T) {
	ev, ok := parseAgyStreamLine(`{"event":"init","conversation_id":"c1","init":{"model":"gemini-4-neon-high"}}`)
	if !ok || ev.Kind != AgyEventInit || ev.Model != "gemini-4-neon-high" || ev.ConversationID != "c1" {
		t.Fatalf("unexpected init event: %+v ok=%v", ev, ok)
	}

	if _, ok := parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"user_input","state":"DONE"}}`); ok {
		t.Fatalf("expected user_input step_update to be skipped")
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"hello "}}`)
	if !ok || ev.Kind != AgyEventText || ev.Text != "hello " {
		t.Fatalf("unexpected text delta event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"ax -a default resume task swarm-1-api"}}}}`)
	if !ok || ev.Kind != AgyEventTool || ev.ToolName != "run_command" || ev.ToolState != "ACTIVE" || ev.ToolSummary != "ax -a default resume task swarm-1-api" {
		t.Fatalf("unexpected tool event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"run_command","duration_seconds":0.35,"tool_info":{"name":"run_command","parameters":{"CommandLine":"ax -a default apply -f - <<'EOF'\napiVersion: ax.io/v1alpha1\nkind: Task\nmetadata:\n  name: swarm-1-api\nspec:\n  debug: true\nEOF"},"output":"Created At: t\nCompleted At: t\nCommand exited with code 0.\n\nCommand output:\ntask.ax.io/swarm-1-api created\n"}}}`)
	if !ok || ev.Kind != AgyEventTool || ev.ToolSummary != "ax apply Task swarm-1-api" || !ev.HasExitCode || ev.ToolExitCode != 0 || ev.ToolOutput != "task.ax.io/swarm-1-api created" {
		t.Fatalf("unexpected ax apply done event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"swarm","tool_info":{"name":"swarm","parameters":{"action":"title","title":"Audit chalk"}}}}`)
	if !ok || ev.Kind != AgyEventSwarmTitle || ev.SwarmTitle != "Audit chalk" {
		t.Fatalf("unexpected swarm title event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"swarm","tool_info":{"name":"swarm","parameters":{"action":"add","name":"api-worker","brief":"Inspect public API"}}}}`)
	if !ok || ev.Kind != AgyEventSwarmWorker || ev.WorkerName != "api-worker" || ev.WorkerState != "working" || ev.WorkerBrief != "Inspect public API" {
		t.Fatalf("unexpected swarm add event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"swarm","tool_info":{"name":"swarm","parameters":{"action":"status"},"output":"Created At: t\nCompleted At: t\n{\"title\":\"Audit chalk\",\"members\":[{\"name\":\"coordinator\",\"role\":\"coordinator\",\"state\":\"working\"},{\"name\":\"api-worker\",\"role\":\"worker\",\"state\":\"waiting\"}]}"}}}`)
	if !ok || ev.Kind != AgyEventSwarmRoster || ev.SwarmTitle != "Audit chalk" || len(ev.Members) != 2 {
		t.Fatalf("unexpected swarm roster event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"read_board","tool_info":{"name":"read_board","output":"Created At: t\nCompleted At: t\n{\"messages\":[{\"seq\":2,\"agent\":\"api-worker\",\"text\":\"@coordinator [done] checked API\"}],\"head\":2}"}}}`)
	if !ok || ev.Kind != AgyEventBoardMessages || len(ev.BoardMessages) != 1 || ev.BoardMessages[0].Seq != 2 || ev.BoardMessages[0].Agent != "api-worker" || ev.BoardHead != 2 {
		t.Fatalf("unexpected read_board event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"post_message","tool_info":{"name":"post_message","parameters":{"text":"[done] all good"},"output":"Created At: t\n{\"status\":\"posted\",\"seq\":4}"}}}`)
	if !ok || ev.Kind != AgyEventBoardMessages || len(ev.BoardMessages) != 1 || ev.BoardMessages[0].Seq != 4 || ev.BoardMessages[0].Agent != "coordinator" {
		t.Fatalf("unexpected post_message event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine(`{"event":"result","result":{"status":"SUCCESS","response":"final answer\n","duration_seconds":2.5,"num_turns":2}}`)
	if !ok || ev.Kind != AgyEventResult || ev.Status != "SUCCESS" || ev.Response != "final answer\n" || ev.NumTurns != 2 {
		t.Fatalf("unexpected result event: %+v ok=%v", ev, ok)
	}

	ev, ok = parseAgyStreamLine("plain text line")
	if !ok || ev.Kind != AgyEventText || ev.Text != "plain text line\n" {
		t.Fatalf("unexpected plain text fallback: %+v ok=%v", ev, ok)
	}
}

func TestStartAgyStreamAndLargeLines(t *testing.T) {
	bigText := strings.Repeat("x", 100*1024)
	script := `printf '%s\n' '{"event":"init","conversation_id":"c1","init":{"model":"test-model"}}'` + "\n" +
		`printf '{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"%s"}}\n' "$BIG_TEXT"` + "\n" +
		`printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"done"}}'` + "\n" +
		`echo "warn line" >&2` + "\n"

	t.Setenv("BIG_TEXT", bigText)
	ch, err := StartAgyStream(context.Background(), "/bin/sh", []string{"-c", script})
	if err != nil {
		t.Fatalf("StartAgyStream() error: %v", err)
	}

	var gotInit, gotText, gotResult, gotStderr, gotDone bool
	for ev := range ch {
		switch ev.Kind {
		case AgyEventInit:
			gotInit = ev.Model == "test-model"
		case AgyEventText:
			if len(ev.Text) == len(bigText) {
				gotText = true
			}
		case AgyEventResult:
			gotResult = ev.Status == "SUCCESS" && ev.Response == "done"
		case AgyEventStderr:
			gotStderr = ev.ErrorText == "warn line"
		case AgyEventDone:
			gotDone = ev.Err == nil
		}
	}
	if !gotInit || !gotText || !gotResult || !gotStderr || !gotDone {
		t.Fatalf("missing events: init=%v text=%v result=%v stderr=%v done=%v", gotInit, gotText, gotResult, gotStderr, gotDone)
	}
}

func TestStartAgyStreamCancelStopsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := StartAgyStream(ctx, "/bin/sh", []string{"-c", "echo started; sleep 30"})
	if err != nil {
		t.Fatalf("StartAgyStream() error: %v", err)
	}

	// Wait until subprocess is running and emits its first line, then cancel.
	ev := <-ch
	if ev.Kind != AgyEventText || ev.Text != "started\n" {
		t.Fatalf("expected started line, got %+v", ev)
	}

	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind == AgyEventDone {
				if ev.Err == nil {
					t.Fatalf("expected non-nil error after cancellation")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for canceled background agy process to exit")
		}
	}
}

func TestStopAgyWaitsForProcessExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := StartAgyStream(ctx, "/bin/sh", []string{"-c", "echo started; sleep 30"})
	if err != nil {
		t.Fatalf("StartAgyStream() error: %v", err)
	}
	if ev := <-ch; ev.Kind != AgyEventText {
		t.Fatalf("expected started line, got %+v", ev)
	}
	start := time.Now()
	stopAgy(cancel, ch, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("stopAgy took %s; process was not stopped promptly", elapsed)
	}
	if _, ok := <-ch; ok {
		t.Fatalf("event channel still open after stopAgy; agy has not exited")
	}
}

// TestParseRealAgySwarmStream replays a real `agy --output-format stream-json
// -p "/swarm ..."` run. Every JSON line must decode as an event (none may fall
// back to answer text), the failed tool call must surface its error, and the
// stream must end with a result.
func TestParseRealAgySwarmStream(t *testing.T) {
	data, err := os.ReadFile("testdata/agy-swarm-run.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var sawResult, sawToolError bool
	var text strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev, ok := parseAgyStreamLine(line)
		if !ok {
			continue
		}
		if ev.Kind == AgyEventText {
			if strings.HasPrefix(ev.Text, "{") {
				t.Fatalf("JSON line was not decoded and leaked as answer text: %.120s", ev.Text)
			}
			text.WriteString(ev.Text)
		}
		if ev.Kind == AgyEventTool && ev.ToolState == "ERROR" {
			sawToolError = true
			if !strings.Contains(ev.ErrorText, "unknown tool") {
				t.Fatalf("tool error message not extracted: %q", ev.ErrorText)
			}
		}
		if ev.Kind == AgyEventResult {
			sawResult = true
			if ev.Status != "SUCCESS" || !strings.Contains(ev.Response, "probe-ok") {
				t.Fatalf("unexpected result: %+v", ev)
			}
		}
	}
	if !sawToolError || !sawResult {
		t.Fatalf("toolError=%v result=%v", sawToolError, sawResult)
	}
	if !strings.Contains(text.String(), "probe-ok") {
		t.Fatalf("streamed answer text missing: %q", text.String())
	}
}

func TestParseAgyNoticeAndErrorShapes(t *testing.T) {
	ev, ok := parseAgyStreamLine(`jetski: no output produced — a tool required the "mcp" permission that headless mode cannot prompt for, so it was auto-denied.`)
	if !ok || ev.Kind != AgyEventStderr || !strings.Contains(ev.ErrorText, "auto-denied") {
		t.Fatalf("notice not recognized: %+v", ev)
	}
	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR","tool_name":"call_mcp_tool"}}`)
	if !ok || ev.ToolState != "ERROR" || ev.ErrorText == "" {
		t.Fatalf("ERROR step without message should still carry an error: %+v", ev)
	}
	ev, ok = parseAgyStreamLine(`{"event":"result","result":{"status":"ERROR","error":{"type":"MODEL_ERROR","message":"quota exceeded"}}}`)
	if !ok || ev.Kind != AgyEventResult || ev.ErrorText != "quota exceeded" {
		t.Fatalf("object error in result not decoded: %+v", ev)
	}
}

func TestAgySessionSettingsAllowsAXAndExtraRules(t *testing.T) {
	cfg := Config{AXBin: "/Users/jbd/ax/bin/ax", AllowRules: []string{"mcp(data-agent-kit/*)", "mcp(data-agent-kit/*)"}}
	args := BuildAgyArgs(cfg, "/swarm x")
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("expected --settings in args: %v", args)
	}
	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
		t.Fatalf("settings is not valid JSON: %v (%s)", err, args[i+1])
	}
	want := []string{"command(/Users/jbd/ax/bin/ax)", "mcp(data-agent-kit/*)"}
	if !slices.Equal(settings.Permissions.Allow, want) {
		t.Fatalf("allow = %v, want %v (deduplicated, ax first)", settings.Permissions.Allow, want)
	}
	if args[len(args)-2] != "-p" {
		t.Fatalf("prompt must stay last: %v", args)
	}
}
