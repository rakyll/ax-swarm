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

	ev, ok = parseAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE","tool_name":"run_command"}}`)
	if !ok || ev.Kind != AgyEventTool || ev.ToolName != "run_command" || ev.ToolState != "ACTIVE" {
		t.Fatalf("unexpected tool event: %+v ok=%v", ev, ok)
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
