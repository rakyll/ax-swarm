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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BuildAgyArgs constructs the argument list for running `agy` in the background
// in non-interactive print mode with NDJSON streaming output.
func BuildAgyArgs(cfg Config, prompt string) []string {
	args := []string{"--output-format", "stream-json"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if cfg.Mode != "" {
		args = append(args, "--mode", cfg.Mode)
	}
	if cfg.Project != "" {
		args = append(args, "--project", cfg.Project)
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "-p", prompt)
	return args
}

// AgyEventKind classifies normalized events streamed from a background `agy` run.
type AgyEventKind int

const (
	AgyEventInit AgyEventKind = iota
	AgyEventText
	AgyEventTool
	AgyEventSwarmTitle
	AgyEventSwarmWorker
	AgyEventSwarmRoster
	AgyEventBoardMessages
	AgyEventActivity
	AgyEventResult
	AgyEventStderr
	AgyEventDone
)

// SwarmMemberInfo describes a member reported by `agy /swarm`.
type SwarmMemberInfo struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Model  string `json:"model"`
	Killed bool   `json:"killed"`
}

// SwarmBoardMessage describes a board message returned by `read_board` or `post_message`.
type SwarmBoardMessage struct {
	Seq   int    `json:"seq"`
	Agent string `json:"agent"`
	Text  string `json:"text"`
	Time  string `json:"time"`
}

// AgyEvent represents a single parsed event from `agy --output-format stream-json`.
type AgyEvent struct {
	Kind            AgyEventKind
	ConversationID  string
	Model           string
	Text            string
	ToolName        string
	ToolState       string
	ToolSummary     string
	SwarmTitle      string
	WorkerName      string
	WorkerBrief     string
	WorkerState     string
	Members         []SwarmMemberInfo
	BoardMessages   []SwarmBoardMessage
	Status          string
	Response        string
	ErrorText       string
	DurationSeconds float64
	NumTurns        int
	Err             error
}

type rawAgyEnvelope struct {
	Event          string        `json:"event"`
	ConversationID string        `json:"conversation_id"`
	Init           *rawAgyInit   `json:"init"`
	StepUpdate     *rawAgyStep   `json:"step_update"`
	Result         *rawAgyResult `json:"result"`
	Error          string        `json:"error"`
}

type rawAgyInit struct {
	Model          string `json:"model"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
}

type rawAgyStep struct {
	ConversationID  string      `json:"conversation_id"`
	StepIndex       int         `json:"step_index"`
	State           string      `json:"state"`
	StepType        string      `json:"step_type"`
	TextDelta       string      `json:"text_delta"`
	ToolName        string      `json:"tool_name"`
	ToolInfo        *rawAgyTool `json:"tool_info"`
	DurationSeconds float64     `json:"duration_seconds"`
	Error           string      `json:"error"`
}

type rawAgyTool struct {
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters"`
	Output     string         `json:"output"`
	Error      string         `json:"error"`
}

type rawAgyResult struct {
	ConversationID  string  `json:"conversation_id"`
	Status          string  `json:"status"`
	Response        string  `json:"response"`
	Error           string  `json:"error"`
	DurationSeconds float64 `json:"duration_seconds"`
	NumTurns        int     `json:"num_turns"`
}

// parseAgyStreamLine decodes one line from `agy --output-format stream-json`.
// Non-JSON stdout lines fall back to plain AgyEventText so plain-text stubs and
// non-structured output still render cleanly.
func parseAgyStreamLine(line string) (AgyEvent, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return AgyEvent{}, false
	}

	if !strings.HasPrefix(trimmed, "{") {
		return AgyEvent{Kind: AgyEventText, Text: trimmed + "\n"}, true
	}

	var env rawAgyEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil || env.Event == "" {
		return AgyEvent{Kind: AgyEventText, Text: trimmed + "\n"}, true
	}

	switch env.Event {
	case "init":
		ev := AgyEvent{
			Kind:           AgyEventInit,
			ConversationID: env.ConversationID,
		}
		if env.Init != nil {
			ev.Model = env.Init.Model
		}
		return ev, true

	case "step_update":
		if env.StepUpdate == nil {
			return AgyEvent{}, false
		}
		return parseStepUpdate(env.StepUpdate)

	case "result":
		if env.Result == nil {
			return AgyEvent{}, false
		}
		r := env.Result
		return AgyEvent{
			Kind:            AgyEventResult,
			ConversationID:  r.ConversationID,
			Status:          r.Status,
			Response:        r.Response,
			ErrorText:       r.Error,
			DurationSeconds: r.DurationSeconds,
			NumTurns:        r.NumTurns,
		}, true

	default:
		if env.Error != "" {
			return AgyEvent{Kind: AgyEventStderr, ErrorText: env.Error}, true
		}
		return AgyEvent{}, false
	}
}

func parseStepUpdate(st *rawAgyStep) (AgyEvent, bool) {
	switch st.StepType {
	case "agent_response":
		if st.TextDelta != "" {
			return AgyEvent{
				Kind:           AgyEventText,
				ConversationID: st.ConversationID,
				Text:           st.TextDelta,
			}, true
		}
		if st.State == "DONE" {
			return AgyEvent{
				Kind:           AgyEventActivity,
				ConversationID: st.ConversationID,
				Text:           "planning",
			}, true
		}
		return AgyEvent{}, false

	case "system_message":
		return AgyEvent{
			Kind:           AgyEventActivity,
			ConversationID: st.ConversationID,
			Text:           "swarm update received",
		}, true

	case "tool":
		return parseToolStepUpdate(st)

	default:
		return AgyEvent{}, false
	}
}

func parseToolStepUpdate(st *rawAgyStep) (AgyEvent, bool) {
	name := st.ToolName
	var params map[string]any
	var output, toolErr string
	if st.ToolInfo != nil {
		if name == "" {
			name = st.ToolInfo.Name
		}
		params = st.ToolInfo.Parameters
		output = st.ToolInfo.Output
		toolErr = st.ToolInfo.Error
	}
	if toolErr == "" {
		toolErr = st.Error
	}

	switch name {
	case "swarm":
		action := paramString(params, "action")
		if st.State == "DONE" {
			switch action {
			case "title":
				if title := paramString(params, "title"); title != "" {
					return AgyEvent{
						Kind:           AgyEventSwarmTitle,
						ConversationID: st.ConversationID,
						SwarmTitle:     title,
					}, true
				}
			case "add":
				if wName := paramString(params, "name"); wName != "" {
					return AgyEvent{
						Kind:           AgyEventSwarmWorker,
						ConversationID: st.ConversationID,
						WorkerName:     wName,
						WorkerBrief:    paramString(params, "brief"),
						WorkerState:    "working",
					}, true
				}
			case "kill":
				if wName := paramString(params, "name"); wName != "" {
					return AgyEvent{
						Kind:           AgyEventSwarmWorker,
						ConversationID: st.ConversationID,
						WorkerName:     wName,
						WorkerState:    "killed",
					}, true
				}
			case "status":
				if roster, title, ok := parseSwarmStatusOutput(output); ok {
					return AgyEvent{
						Kind:           AgyEventSwarmRoster,
						ConversationID: st.ConversationID,
						SwarmTitle:     title,
						Members:        roster,
					}, true
				}
			}
		}
		summary := "swarm"
		if action != "" {
			summary = "swarm:" + action
			if wName := paramString(params, "name"); wName != "" {
				summary += " " + wName
			}
		}
		return AgyEvent{
			Kind:            AgyEventTool,
			ConversationID:  st.ConversationID,
			ToolName:        name,
			ToolState:       st.State,
			ToolSummary:     summary,
			ErrorText:       toolErr,
			DurationSeconds: st.DurationSeconds,
		}, true

	case "read_board":
		if st.State == "DONE" {
			if msgs, ok := parseBoardMessagesOutput(output); ok && len(msgs) > 0 {
				return AgyEvent{
					Kind:           AgyEventBoardMessages,
					ConversationID: st.ConversationID,
					BoardMessages:  msgs,
				}, true
			}
		}
		return AgyEvent{
			Kind:            AgyEventTool,
			ConversationID:  st.ConversationID,
			ToolName:        name,
			ToolState:       st.State,
			ToolSummary:     "reading swarm board",
			ErrorText:       toolErr,
			DurationSeconds: st.DurationSeconds,
		}, true

	case "post_message":
		if st.State == "DONE" {
			if txt := paramString(params, "text"); txt != "" {
				seq := parsePostedSeq(output)
				return AgyEvent{
					Kind:           AgyEventBoardMessages,
					ConversationID: st.ConversationID,
					BoardMessages: []SwarmBoardMessage{
						{Seq: seq, Agent: "coordinator", Text: txt},
					},
				}, true
			}
		}
		return AgyEvent{
			Kind:            AgyEventTool,
			ConversationID:  st.ConversationID,
			ToolName:        name,
			ToolState:       st.State,
			ToolSummary:     "posting to board",
			ErrorText:       toolErr,
			DurationSeconds: st.DurationSeconds,
		}, true

	case "run_command":
		cmdLine := paramString(params, "CommandLine")
		summary := summarizeCommand(cmdLine)
		return AgyEvent{
			Kind:            AgyEventTool,
			ConversationID:  st.ConversationID,
			ToolName:        name,
			ToolState:       st.State,
			ToolSummary:     summary,
			ErrorText:       toolErr,
			DurationSeconds: st.DurationSeconds,
		}, true

	default:
		summary := name
		if summary == "" {
			summary = "tool"
		}
		return AgyEvent{
			Kind:            AgyEventTool,
			ConversationID:  st.ConversationID,
			ToolName:        name,
			ToolState:       st.State,
			ToolSummary:     summary,
			ErrorText:       toolErr,
			DurationSeconds: st.DurationSeconds,
		}, true
	}
}

func paramString(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func extractJSONObject(s string) ([]byte, bool) {
	idx := strings.IndexByte(s, '{')
	if idx < 0 {
		return nil, false
	}
	return []byte(strings.TrimSpace(s[idx:])), true
}

func parseSwarmStatusOutput(output string) ([]SwarmMemberInfo, string, bool) {
	raw, ok := extractJSONObject(output)
	if !ok {
		return nil, "", false
	}
	var payload struct {
		Title   string            `json:"title"`
		Members []SwarmMemberInfo `json:"members"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, "", false
	}
	if len(payload.Members) == 0 && payload.Title == "" {
		return nil, "", false
	}
	return payload.Members, payload.Title, true
}

func parseBoardMessagesOutput(output string) ([]SwarmBoardMessage, bool) {
	raw, ok := extractJSONObject(output)
	if !ok {
		return nil, false
	}
	var payload struct {
		Messages []SwarmBoardMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	return payload.Messages, true
}

func parsePostedSeq(output string) int {
	raw, ok := extractJSONObject(output)
	if !ok {
		return 0
	}
	var payload struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	return payload.Seq
}

func summarizeCommand(cmdLine string) string {
	first := strings.TrimSpace(cmdLine)
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = strings.TrimSpace(first[:idx])
	}
	if first == "" {
		return "run_command"
	}
	if len(first) > 72 {
		return first[:71] + "…"
	}
	return first
}

// StartAgyStream launches `agy` in the background (with stdin detached so it
// never competes for terminal input) and streams parsed events on the returned
// channel. The final event before channel close is always AgyEventDone.
func StartAgyStream(ctx context.Context, agyBin string, args []string) (<-chan AgyEvent, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("agy stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("agy stderr pipe: %w", err)
	}

	cmd := exec.CommandContext(ctx, agyBin, args...)
	cmd.Stdin = nil
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Signal the entire process group so background agy and child helpers
		// terminate promptly on cancellation.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = time.Second

	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, fmt.Errorf("start %s: %w", agyBin, err)
	}
	_ = outW.Close()
	_ = errW.Close()

	events := make(chan AgyEvent, 64)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		readLines(outR, func(line string) {
			if ev, ok := parseAgyStreamLine(line); ok {
				events <- ev
			}
		})
	}()

	go func() {
		defer wg.Done()
		readLines(errR, func(line string) {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				events <- AgyEvent{Kind: AgyEventStderr, ErrorText: trimmed}
			}
		})
	}()

	go func() {
		waitErr := cmd.Wait()
		if ctx.Err() != nil && cmd.Process != nil {
			// Ensure no orphaned child in the process group keeps stdout/stderr open.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
		events <- AgyEvent{Kind: AgyEventDone, Err: waitErr}
		close(events)
	}()

	return events, nil
}

// readLines reads arbitrarily long lines without bufio.Scanner's token size cap.
func readLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fn(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}
