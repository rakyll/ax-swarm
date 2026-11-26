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

// agyCall describes one plain (non-/swarm) agy invocation.
type agyCall struct {
	// Schema, when set, is passed as --json-schema to force structured output.
	Schema string
	// AllowAX grants the ax binary in --settings so a headless worker can drive
	// its AX Task without an approval prompt (which headless mode would deny).
	AllowAX bool
}

// BuildAgyArgs constructs the argument list for one background, non-interactive
// `agy -p` call with NDJSON streaming output. Slash commands are disabled so the
// goal text can never trigger /swarm or any other agy command.
func BuildAgyArgs(cfg Config, prompt string, call agyCall) []string {
	args := []string{"--output-format", "stream-json", "--disable-slash-commands"}
	if settings := agySessionSettings(cfg, call.AllowAX); settings != "" {
		args = append(args, "--settings", settings)
	}
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
	if cfg.DangerouslySkipPermissions && call.AllowAX {
		args = append(args, "--dangerously-skip-permissions")
	}
	if call.Schema != "" {
		args = append(args, "--json-schema", call.Schema)
	}
	args = append(args, "-p", prompt)
	return args
}

// agyPermissionRules returns the allow rules for one agy session.
func agyPermissionRules(cfg Config, allowAX bool) []string {
	var rules []string
	seen := make(map[string]bool)
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			rules = append(rules, r)
		}
	}
	if allowAX && cfg.AXBin != "" {
		add("command(" + cfg.AXBin + ")")
	}
	if allowAX {
		for _, r := range cfg.AllowRules {
			add(r)
		}
	}
	return rules
}

// agySessionSettings returns the JSON passed to `agy --settings`. agy applies
// it to this session only and never saves it.
func agySessionSettings(cfg Config, allowAX bool) string {
	rules := agyPermissionRules(cfg, allowAX)
	if len(rules) == 0 {
		return ""
	}
	data, err := json.Marshal(map[string]any{"permissions": map[string]any{"allow": rules}})
	if err != nil {
		return ""
	}
	return string(data)
}

// AgyEventKind classifies normalized events streamed from a background agy run.
type AgyEventKind int

const (
	AgyEventInit AgyEventKind = iota
	AgyEventText
	AgyEventTool
	AgyEventResult
	AgyEventStderr
	AgyEventDone
)

// AgyEvent is a single parsed event from `agy --output-format stream-json`.
type AgyEvent struct {
	Kind AgyEventKind
	// StepIndex identifies the agy step (tool calls report ACTIVE then DONE
	// under the same index).
	StepIndex   int
	Model       string
	Text        string
	ToolName    string
	ToolState   string
	ToolSummary string
	// ToolCommand and ToolOutputFull are the untruncated command line and
	// output of a run_command call (recorded in traces).
	ToolCommand    string
	ToolOutputFull string
	// ToolPath is the file a file tool (view_file, write_to_file, ...) acts on.
	ToolPath     string
	ToolOutput   string
	ToolExitCode int
	HasExitCode  bool
	Status       string
	Response     string
	// Structured is result.structured_output: the schema-validated JSON
	// object agy returns when --json-schema is set.
	Structured string
	ErrorText  string
	Err        error
}

// agyError holds an error field that agy sends as a string or an object.
type agyError string

func (e *agyError) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*e = agyError(s)
		return nil
	}
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && (obj.Message != "" || obj.Type != "") {
		if obj.Message == "" {
			*e = agyError(obj.Type)
		} else {
			*e = agyError(obj.Message)
		}
		return nil
	}
	if string(data) != "null" {
		*e = agyError(strings.TrimSpace(string(data)))
	}
	return nil
}

type rawAgyEnvelope struct {
	Event string `json:"event"`
	Init  *struct {
		Model string `json:"model"`
	} `json:"init"`
	StepUpdate *rawAgyStep `json:"step_update"`
	Result     *struct {
		Status     string          `json:"status"`
		Response   string          `json:"response"`
		Structured json.RawMessage `json:"structured_output"`
		Error      agyError        `json:"error"`
	} `json:"result"`
	Error agyError `json:"error"`
}

type rawAgyStep struct {
	StepIndex int      `json:"step_index"`
	State     string   `json:"state"`
	StepType  string   `json:"step_type"`
	TextDelta string   `json:"text_delta"`
	ToolName  string   `json:"tool_name"`
	Error     agyError `json:"error"`
	ToolInfo  *struct {
		Name       string         `json:"name"`
		Parameters map[string]any `json:"parameters"`
		Output     string         `json:"output"`
		Error      agyError       `json:"error"`
	} `json:"tool_info"`
}

var agyNoticePrefixes = []string{"jetski:", "agy:", "warning:", "error:"}

func isAgyNotice(line string) bool {
	lower := strings.ToLower(line)
	for _, p := range agyNoticePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// parseAgyStreamLine decodes one line of `agy --output-format stream-json`.
func parseAgyStreamLine(line string) (AgyEvent, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return AgyEvent{}, false
	}
	if !strings.HasPrefix(trimmed, "{") {
		if isAgyNotice(trimmed) {
			return AgyEvent{Kind: AgyEventStderr, ErrorText: trimmed}, true
		}
		return AgyEvent{Kind: AgyEventText, Text: trimmed + "\n"}, true
	}
	var env rawAgyEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil || env.Event == "" {
		return AgyEvent{Kind: AgyEventText, Text: trimmed + "\n"}, true
	}
	switch env.Event {
	case "init":
		ev := AgyEvent{Kind: AgyEventInit}
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
		ev := AgyEvent{
			Kind:      AgyEventResult,
			Status:    env.Result.Status,
			Response:  env.Result.Response,
			ErrorText: string(env.Result.Error),
		}
		if s := strings.TrimSpace(string(env.Result.Structured)); s != "" && s != "null" {
			ev.Structured = s
		}
		return ev, true
	default:
		if env.Error != "" {
			return AgyEvent{Kind: AgyEventStderr, ErrorText: string(env.Error)}, true
		}
		return AgyEvent{}, false
	}
}

func parseStepUpdate(st *rawAgyStep) (AgyEvent, bool) {
	switch st.StepType {
	case "agent_response":
		if st.TextDelta != "" {
			return AgyEvent{Kind: AgyEventText, Text: st.TextDelta}, true
		}
		return AgyEvent{}, false
	case "tool":
		name := st.ToolName
		var params map[string]any
		var output, toolErr string
		if st.ToolInfo != nil {
			if name == "" {
				name = st.ToolInfo.Name
			}
			params = st.ToolInfo.Parameters
			output = st.ToolInfo.Output
			toolErr = string(st.ToolInfo.Error)
		}
		if toolErr == "" {
			toolErr = string(st.Error)
		}
		if toolErr == "" && st.State == "ERROR" {
			toolErr = "tool call failed"
		}
		ev := AgyEvent{Kind: AgyEventTool, StepIndex: st.StepIndex, ToolName: name, ToolState: st.State, ErrorText: toolErr}
		for _, key := range []string{"AbsolutePath", "TargetFile", "Path", "path"} {
			if p := paramString(params, key); p != "" {
				ev.ToolPath = p
				break
			}
		}
		if name == "run_command" {
			ev.ToolCommand = paramString(params, "CommandLine")
			ev.ToolSummary = summarizeCommand(ev.ToolCommand)
			if st.State == "DONE" {
				ev.ToolOutputFull = output
				ev.ToolExitCode, ev.HasExitCode, ev.ToolOutput = parseCommandOutputSummary(output)
			}
		} else {
			ev.ToolSummary = name
		}
		return ev, true
	default:
		return AgyEvent{}, false
	}
}

func paramString(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	s, _ := params[key].(string)
	return strings.TrimSpace(s)
}

func summarizeCommand(cmdLine string) string {
	first := strings.TrimSpace(cmdLine)
	if first == "" {
		return "run_command"
	}
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = strings.TrimSpace(first[:idx])
	}
	if len(first) > 96 {
		return first[:95] + "…"
	}
	return first
}

func parseCommandOutputSummary(output string) (exitCode int, hasExit bool, preview string) {
	inOutput := false
	for _, raw := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if !inOutput {
			if strings.HasPrefix(line, "Command exited with code ") {
				rest := strings.TrimSuffix(strings.TrimPrefix(line, "Command exited with code "), ".")
				var code int
				if _, err := fmt.Sscanf(rest, "%d", &code); err == nil {
					exitCode, hasExit = code, true
				}
			}
			if line == "Command output:" {
				inOutput = true
			}
			continue
		}
		if line != "" {
			if len(line) > 96 {
				line = line[:95] + "…"
			}
			return exitCode, hasExit, line
		}
	}
	return exitCode, hasExit, preview
}

// StartAgyStream launches agy in the background (stdin detached) and streams
// parsed events. The final event before the channel closes is AgyEventDone.
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

// stopAgy cancels a background agy run and waits (bounded) for it to exit.
func stopAgy(cancel context.CancelFunc, events <-chan AgyEvent, timeout time.Duration) {
	if cancel != nil {
		cancel()
	}
	if events == nil {
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

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

// AgyOutcome is the collected result of one agy call.
type AgyOutcome struct {
	// Text is agy's final response (result.response), or the streamed answer
	// text when the result carried none.
	Text string
	// Structured is the schema-validated JSON object (--json-schema calls).
	Structured string
	Model      string
}

// AgyRunner runs one agy call to completion, forwarding each event to onEvent.
type AgyRunner interface {
	Run(ctx context.Context, args []string, onEvent func(AgyEvent)) (AgyOutcome, error)
}

// streamAgyRunner runs the real agy binary via StartAgyStream.
type streamAgyRunner struct{ bin string }

func (r streamAgyRunner) Run(ctx context.Context, args []string, onEvent func(AgyEvent)) (AgyOutcome, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := StartAgyStream(cctx, r.bin, args)
	if err != nil {
		return AgyOutcome{}, err
	}
	var out AgyOutcome
	var streamed strings.Builder
	var response, lastStderr string
	var resultErr, waitErr error
	gotResult := false
	for ev := range ch {
		if onEvent != nil {
			onEvent(ev)
		}
		switch ev.Kind {
		case AgyEventInit:
			out.Model = ev.Model
		case AgyEventText:
			streamed.WriteString(ev.Text)
		case AgyEventStderr:
			lastStderr = ev.ErrorText
		case AgyEventResult:
			gotResult = true
			response = ev.Response
			out.Structured = ev.Structured
			if ev.Status == "ERROR" || ev.ErrorText != "" {
				msg := ev.ErrorText
				if msg == "" {
					msg = "agy returned status " + ev.Status
				}
				resultErr = errors.New(msg)
			}
		case AgyEventDone:
			waitErr = ev.Err
		}
		if gotResult {
			// `result` is agy's completion signal; don't wait for the process.
			break
		}
	}
	stopAgy(cancel, ch, 10*time.Second)

	out.Text = response
	if strings.TrimSpace(out.Text) == "" {
		out.Text = streamed.String()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if resultErr != nil {
		return out, resultErr
	}
	if !gotResult {
		msg := "agy exited without a result"
		if waitErr != nil {
			msg = fmt.Sprintf("agy exited without a result: %v", waitErr)
		}
		if lastStderr != "" {
			msg += " (" + lastStderr + ")"
		}
		return out, errors.New(msg)
	}
	return out, nil
}
