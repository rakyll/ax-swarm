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
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

type tuiStage int

const (
	stageGoalInput tuiStage = iota
	stageCreateGateway
	stageCreateWorkspace
	stageWaitWorkspace
	stageStartingAgy
	stageRunningAgy
	stageCleanup
	stageDone
)

type stepKind int

const (
	stepOK stepKind = iota
	stepInfo
	stepWarn
	stepErr
)

type stepEntry struct {
	kind stepKind
	text string
}

type gatewayCreatedMsg struct{ err error }
type workspaceCreatedMsg struct{ err error }
type workspacePhaseMsg struct {
	phase string
	err   error
}
type pollTickMsg struct{}
type agyStartedMsg struct {
	ch  <-chan AgyEvent
	err error
}
type agyStreamMsg struct {
	ev AgyEvent
	ok bool
}
type taskPollTickMsg struct{}
type taskListMsg struct {
	tasks []AXTaskInfo
	err   error
}
type taskResumedMsg struct {
	name string
	err  error
}
type cleanupDoneMsg struct{ res CleanupResult }

type tuiStyles struct {
	badge      lipgloss.Style
	swarmName  lipgloss.Style
	workerName lipgloss.Style
	metaKey    lipgloss.Style
	metaVal    lipgloss.Style
	prompt     lipgloss.Style
	okIcon     lipgloss.Style
	infoIcon   lipgloss.Style
	warnIcon   lipgloss.Style
	errIcon    lipgloss.Style
	active     lipgloss.Style
	subtle     lipgloss.Style
	errBox     lipgloss.Style
	spinner    lipgloss.Style
}

func newTUIStyles(out io.Writer, colorMode string, colorEnabled bool) tuiStyles {
	r := lipgloss.NewRenderer(out)
	switch colorMode {
	case "never":
		r.SetColorProfile(termenv.Ascii)
	case "always":
		r.SetColorProfile(termenv.ANSI256)
	default:
		if !colorEnabled {
			r.SetColorProfile(termenv.Ascii)
		}
	}

	// Use standard ANSI colors 1-6 + Faint/Bold so text remains legible on both
	// dark and light terminal themes (avoiding ANSI 8 / ANSI 15 low-contrast traps).
	return tuiStyles{
		badge:      r.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1),
		swarmName:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		workerName: r.NewStyle().Bold(true).Foreground(lipgloss.Color("5")),
		metaKey:    r.NewStyle().Faint(true).Width(10),
		metaVal:    r.NewStyle(),
		prompt:     r.NewStyle().Bold(true),
		okIcon:     r.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		infoIcon:   r.NewStyle().Faint(true),
		warnIcon:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("3")),
		errIcon:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
		active:     r.NewStyle().Bold(true),
		subtle:     r.NewStyle().Faint(true),
		errBox:     r.NewStyle().Foreground(lipgloss.Color("1")),
		spinner:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
	}
}

type agyStarter func(ctx context.Context, agyBin string, agyArgs []string) (<-chan AgyEvent, error)

type tuiModel struct {
	cfg    Config
	env    *SwarmEnv
	styles tuiStyles

	stage    tuiStage
	steps    []stepEntry
	spinner  spinner.Model
	input    textinput.Model
	inputErr string
	width    int

	setupCtx      context.Context
	cancelSetup   context.CancelFunc
	setupDeadline time.Time
	inFlightSetup bool

	workspaceWaitStart time.Time
	workspacePhase     string

	startAgy    agyStarter
	agyCtx      context.Context
	cancelAgy   context.CancelFunc
	agyCh       <-chan AgyEvent
	agyStart    time.Time
	agyModel    string
	agyStatus   string
	agyRan      bool
	swarmTitle  string
	workers     map[string]string
	workerOrder []string
	workerSince map[string]time.Time
	seenSeq     map[int]bool

	tasks          []AXTaskInfo
	prevTaskStates map[string]string
	taskFirstSeen  map[string]time.Time
	taskStateSince map[string]time.Time
	resumer        *autoResumer
	resumeErrShown map[string]bool
	taskPollCount  int
	lastPollErr    string

	lastActivity time.Time // last agy event or AX task state change
	waitingSince time.Time // coordinator is blocked in swarm_wait since then
	rosterAt     time.Time // when worker states were last reported by `swarm status`
	idleWarned   bool

	lineBuf        string
	streamedOutput string
	finalResponse  string
	agyStderr      []string
	agyResultErr   error

	err        error
	agyErr     error
	cancelled  bool
	cleanupRan bool
	finalStep  *stepEntry
}

func newTUIModel(cfg Config, env *SwarmEnv, styles tuiStyles, starter agyStarter) tuiModel {
	if starter == nil {
		starter = StartAgyStream
	}

	sp := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(styles.spinner),
	)

	ti := textinput.New()
	ti.Placeholder = "Describe the goal for the AX swarm..."
	ti.Prompt = "❯ "
	ti.PromptStyle = styles.swarmName
	ti.CharLimit = 4096
	ti.Width = 64

	setupCtx, cancelSetup := context.WithCancel(context.Background())
	agyCtx, cancelAgy := context.WithCancel(context.Background())

	stage := stageCreateGateway
	inFlightSetup := true
	if cfg.Goal == "" {
		stage = stageGoalInput
		inFlightSetup = false
		ti.Focus()
	}

	return tuiModel{
		cfg:            cfg,
		env:            env,
		styles:         styles,
		stage:          stage,
		spinner:        sp,
		input:          ti,
		setupCtx:       setupCtx,
		cancelSetup:    cancelSetup,
		setupDeadline:  time.Now().Add(cfg.SetupTimeout),
		inFlightSetup:  inFlightSetup,
		startAgy:       starter,
		agyCtx:         agyCtx,
		cancelAgy:      cancelAgy,
		agyModel:       cfg.Model,
		agyStatus:      "starting agy /swarm",
		workers:        make(map[string]string),
		workerSince:    make(map[string]time.Time),
		seenSeq:        make(map[int]bool),
		prevTaskStates: make(map[string]string),
		taskFirstSeen:  make(map[string]time.Time),
		taskStateSince: make(map[string]time.Time),
		resumer:        newAutoResumer(3 * time.Second),
		resumeErrShown: make(map[string]bool),
	}
}

func (m tuiModel) Init() tea.Cmd {
	if m.stage == stageGoalInput {
		return textinput.Blink
	}
	return tea.Batch(m.spinner.Tick, m.createGatewayCmd())
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		w := msg.Width - 8
		if w < 20 {
			w = 20
		}
		if w > 88 {
			w = 88
		}
		m.input.Width = w
		return m, nil

	case spinner.TickMsg:
		switch m.stage {
		case stageCreateGateway, stageCreateWorkspace, stageWaitWorkspace, stageStartingAgy, stageRunningAgy, stageCleanup:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.stage {
		case stageGoalInput:
			switch msg.Type {
			case tea.KeyCtrlC, tea.KeyEsc:
				m.cancelled = true
				m.cancelSetup()
				m.cancelAgy()
				m.stage = stageDone
				return m, tea.Quit
			case tea.KeyEnter:
				goal := strings.TrimSpace(m.input.Value())
				if goal == "" {
					m.inputErr = "Enter a goal to start the swarm."
					return m, nil
				}
				m.cfg.Goal = goal
				m.inputErr = ""
				m.input.Blur()
				m.setupDeadline = time.Now().Add(m.cfg.SetupTimeout)
				m.stage = stageCreateGateway
				m.inFlightSetup = true
				return m, tea.Batch(m.spinner.Tick, m.createGatewayCmd())
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			if m.inputErr != "" && strings.TrimSpace(m.input.Value()) != "" {
				m.inputErr = ""
			}
			return m, cmd

		case stageCreateGateway, stageCreateWorkspace, stageWaitWorkspace:
			if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
				if !m.cancelled {
					m.cancelled = true
					m.cancelSetup()
					if !m.inFlightSetup {
						return m.startCleanupOrQuit()
					}
				}
				return m, nil
			}

		case stageStartingAgy, stageRunningAgy:
			if msg.Type == tea.KeyCtrlC {
				if m.cancelled {
					return m, tea.Quit
				}
				m.cancelled = true
				m.cancelAgy()
				return m, nil
			}
		}
		return m, nil

	case gatewayCreatedMsg:
		m.inFlightSetup = false
		if m.cancelled {
			return m.startCleanupOrQuit()
		}
		if msg.err != nil {
			m.err = msg.err
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Failed to create Gateway " + m.cfg.Name})
			return m.startCleanupOrQuit()
		}
		m.steps = append(m.steps, stepEntry{kind: stepOK, text: "Created Gateway " + m.cfg.Name})
		m.stage = stageCreateWorkspace
		m.inFlightSetup = true
		return m, m.createWorkspaceCmd()

	case workspaceCreatedMsg:
		m.inFlightSetup = false
		if m.cancelled {
			return m.startCleanupOrQuit()
		}
		if msg.err != nil {
			m.err = msg.err
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Failed to create Workspace " + m.cfg.Name})
			return m.startCleanupOrQuit()
		}
		m.steps = append(m.steps, stepEntry{kind: stepOK, text: "Created Workspace " + m.cfg.Name})
		m.workspaceWaitStart = time.Now()
		m.workspacePhase = "pending"
		m.stage = stageWaitWorkspace
		m.inFlightSetup = true
		return m, m.pollWorkspaceCmd()

	case workspacePhaseMsg:
		m.inFlightSetup = false
		if m.cancelled {
			return m.startCleanupOrQuit()
		}
		if msg.err != nil {
			m.err = msg.err
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Failed while checking Workspace " + m.cfg.Name})
			return m.startCleanupOrQuit()
		}
		if msg.phase == "Ready" {
			elapsed := formatElapsed(time.Since(m.workspaceWaitStart))
			m.steps = append(m.steps, stepEntry{kind: stepOK, text: fmt.Sprintf("Workspace %s ready (%s)", m.cfg.Name, elapsed)})
			m.stage = stageStartingAgy
			m.agyStart = time.Now()
			return m, m.startAgyCmd()
		}
		label := msg.phase
		if label == "" {
			label = "pending"
		}
		m.workspacePhase = label
		if msg.phase == "Failed" {
			m.err = fmt.Errorf("workspace %s failed setup; see `%s describe workspace %s`",
				m.cfg.Name, m.env.AX.BaseCommandLine(), m.cfg.Name)
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Workspace " + m.cfg.Name + " failed setup"})
			return m.startCleanupOrQuit()
		}
		if time.Now().After(m.setupDeadline) {
			m.err = fmt.Errorf("workspace %s is %s after %s", m.cfg.Name, label, m.cfg.SetupTimeout)
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Workspace " + m.cfg.Name + " setup timed out"})
			return m.startCleanupOrQuit()
		}
		pollInterval := m.env.AX.PollInterval
		if pollInterval <= 0 {
			pollInterval = 500 * time.Millisecond
		}
		return m, tea.Tick(pollInterval, func(time.Time) tea.Msg {
			return pollTickMsg{}
		})

	case pollTickMsg:
		if m.stage != stageWaitWorkspace {
			return m, nil
		}
		if m.cancelled {
			return m.startCleanupOrQuit()
		}
		m.inFlightSetup = true
		return m, m.pollWorkspaceCmd()

	case agyStartedMsg:
		if msg.err != nil {
			m.agyErr = msg.err
			return m.startCleanupOrQuit()
		}
		m.agyCh = msg.ch
		m.agyRan = true
		m.lastActivity = time.Now()
		m.agyStatus = "initializing swarm"
		m.stage = stageRunningAgy
		return m, tea.Batch(
			m.println(m.renderSetupBanner()),
			waitAgyEventCmd(m.agyCh),
			m.pollTasksCmd(),
		)

	case agyStreamMsg:
		if m.stage != stageRunningAgy {
			return m, nil
		}
		if !msg.ok || msg.ev.Kind == AgyEventDone {
			var flushCmds []tea.Cmd
			if m.lineBuf != "" {
				flushCmds = append(flushCmds, m.println(m.lineBuf))
				m.lineBuf = ""
			}
			if m.finalResponse != "" && strings.TrimSpace(m.streamedOutput) == "" {
				trimmed := strings.TrimRight(m.finalResponse, "\n")
				m.streamedOutput = m.finalResponse
				if trimmed != "" {
					flushCmds = append(flushCmds, m.println(trimmed))
				}
			}
			if msg.ok && msg.ev.Err != nil {
				m.agyErr = msg.ev.Err
			} else if m.agyResultErr != nil {
				m.agyErr = m.agyResultErr
			}
			nextModel, nextCmd := m.startCleanupOrQuit()
			if len(flushCmds) > 0 {
				cmds := append(flushCmds, nextCmd)
				return nextModel, tea.Sequence(cmds...)
			}
			return nextModel, nextCmd
		}

		var cmds []tea.Cmd
		ev := msg.ev
		m.lastActivity = time.Now()
		m.idleWarned = false
		if !(ev.Kind == AgyEventTool && ev.ToolName == "swarm_wait" && ev.ToolState == "ACTIVE") {
			m.waitingSince = time.Time{}
		}
		switch ev.Kind {
		case AgyEventInit:
			if ev.Model != "" {
				m.agyModel = ev.Model
			}
			m.agyStatus = "planning swarm"
			if m.agyModel != "" {
				cmds = append(cmds, m.println(fmt.Sprintf("%s started agy /swarm %s",
					m.styles.swarmName.Render("▸"),
					m.styles.subtle.Render("("+m.agyModel+")"),
				)))
			}

		case AgyEventActivity:
			if ev.Text != "" {
				m.agyStatus = ev.Text
			}

		case AgyEventSwarmTitle:
			m.swarmTitle = ev.SwarmTitle
			m.agyStatus = "swarm: " + ev.SwarmTitle
			cmds = append(cmds, m.println(fmt.Sprintf("%s %s", m.styles.swarmName.Render("◆"), m.styles.active.Render(ev.SwarmTitle))))

		case AgyEventSwarmWorker:
			m.setWorkerState(ev.WorkerName, ev.WorkerState)
			if ev.WorkerState == "killed" {
				m.agyStatus = "stopped worker " + ev.WorkerName
				cmds = append(cmds, m.println(fmt.Sprintf("%s worker %s stopped", m.styles.warnIcon.Render("×"), m.styles.workerName.Render(ev.WorkerName))))
			} else {
				m.agyStatus = "waiting on workers"
				line := fmt.Sprintf("%s worker %s", m.styles.okIcon.Render("+"), m.styles.workerName.Render(ev.WorkerName))
				if brief := oneLinePreview(ev.WorkerBrief, 90); brief != "" {
					line += "  " + m.styles.subtle.Render(brief)
				}
				cmds = append(cmds, m.println(line))
			}

		case AgyEventSwarmRoster:
			m.rosterAt = time.Now()
			if ev.SwarmTitle != "" {
				m.swarmTitle = ev.SwarmTitle
			}
			var summaryParts []string
			for _, mem := range ev.Members {
				if mem.Role == "coordinator" {
					continue
				}
				st := mem.State
				if mem.Killed {
					st = "killed"
				}
				m.setWorkerState(mem.Name, st)
				summaryParts = append(summaryParts, fmt.Sprintf("%s=%s", mem.Name, st))
			}
			detail := "no workers yet"
			if len(summaryParts) > 0 {
				detail = strings.Join(summaryParts, ", ")
			}
			cmds = append(cmds, m.println(fmt.Sprintf("%s swarm status: %s",
				m.styles.infoIcon.Render("◌"),
				m.styles.subtle.Render(detail),
			)))
			m.agyStatus = "waiting on workers"

		case AgyEventBoardMessages:
			printed := 0
			for _, bm := range ev.BoardMessages {
				if bm.Seq > 0 {
					if m.seenSeq[bm.Seq] {
						continue
					}
					m.seenSeq[bm.Seq] = true
				}
				prefix := m.styles.workerName.Render("[" + bm.Agent + "]")
				if bm.Seq > 0 {
					prefix = fmt.Sprintf("%s %s", m.styles.subtle.Render(fmt.Sprintf("#%d", bm.Seq)), prefix)
				}
				preview := oneLinePreview(bm.Text, 120)
				cmds = append(cmds, m.println(fmt.Sprintf("%s %s", prefix, preview)))
				printed++
			}
			if printed == 0 && ev.BoardHead > 0 {
				cmds = append(cmds, m.println(fmt.Sprintf("%s board check %s",
					m.styles.infoIcon.Render("◌"),
					m.styles.subtle.Render(fmt.Sprintf("(head #%d, no new messages)", ev.BoardHead)),
				)))
			}
			m.agyStatus = "coordinating swarm"

		case AgyEventTool:
			if ev.ToolName == "swarm_wait" && ev.ToolState == "ACTIVE" {
				m.waitingSince = time.Now()
				m.agyStatus = "waiting for a worker to report"
				cmds = append(cmds, m.println(fmt.Sprintf("%s coordinator is waiting for a worker to post %s",
					m.styles.infoIcon.Render("◌"),
					m.styles.subtle.Render("@coordinator"),
				)))
			} else if ev.ToolState == "ACTIVE" {
				m.agyStatus = ev.ToolSummary
				if ev.ToolName == "run_command" && ev.ToolSummary != "" {
					cmds = append(cmds, m.println(fmt.Sprintf("%s %s", m.styles.swarmName.Render("▸"), ev.ToolSummary)))
				} else if ev.ToolName != "swarm" && ev.ToolName != "read_board" && ev.ToolName != "post_message" && ev.ToolSummary != "" {
					cmds = append(cmds, m.println(fmt.Sprintf("%s %s", m.styles.infoIcon.Render("◌"), m.styles.subtle.Render(ev.ToolSummary))))
				}
			} else {
				if ev.ErrorText != "" {
					cmds = append(cmds, m.println(fmt.Sprintf("%s %s: %s", m.styles.errIcon.Render("!"), ev.ToolName, oneLinePreview(ev.ErrorText, 120))))
				} else if ev.ToolName == "run_command" {
					dur := ""
					if ev.DurationSeconds > 0 {
						dur = fmt.Sprintf(" (%.1fs)", ev.DurationSeconds)
					}
					if ev.HasExitCode && ev.ToolExitCode != 0 {
						msgText := fmt.Sprintf("exit %d", ev.ToolExitCode)
						if ev.ToolOutput != "" {
							msgText += ": " + ev.ToolOutput
						}
						cmds = append(cmds, m.println(fmt.Sprintf("  %s %s%s", m.styles.errIcon.Render("✗"), m.styles.errBox.Render(msgText), m.styles.subtle.Render(dur))))
					} else if ev.ToolOutput != "" {
						cmds = append(cmds, m.println(fmt.Sprintf("  %s %s%s", m.styles.okIcon.Render("✓"), m.styles.subtle.Render(ev.ToolOutput), m.styles.subtle.Render(dur))))
					}
				}
				if len(m.workerOrder) > 0 {
					m.agyStatus = "waiting on workers"
				} else {
					m.agyStatus = "thinking"
				}
			}

		case AgyEventText:
			m.agyStatus = "streaming output"
			m.streamedOutput += ev.Text
			m.lineBuf += ev.Text
			if idx := strings.LastIndex(m.lineBuf, "\n"); idx >= 0 {
				ready := m.lineBuf[:idx]
				m.lineBuf = m.lineBuf[idx+1:]
				cmds = append(cmds, m.println(ready))
			}

		case AgyEventResult:
			if ev.Response != "" {
				m.finalResponse = ev.Response
			}
			if ev.Status == "ERROR" || ev.ErrorText != "" {
				errMsg := ev.ErrorText
				if errMsg == "" {
					errMsg = "agy returned status " + ev.Status
				}
				m.agyResultErr = errors.New(errMsg)
			}

		case AgyEventStderr:
			if len(m.agyStderr) < 20 {
				m.agyStderr = append(m.agyStderr, ev.ErrorText)
			}
		}

		cmds = append(cmds, waitAgyEventCmd(m.agyCh))
		return m, tea.Batch(cmds...)

	case taskPollTickMsg:
		if m.stage != stageRunningAgy || m.cancelled {
			return m, nil
		}
		return m, m.pollTasksCmd()

	case taskListMsg:
		if m.stage != stageRunningAgy {
			return m, nil
		}
		var cmds []tea.Cmd
		m.taskPollCount++
		if msg.err != nil {
			errStr := oneLinePreview(msg.err.Error(), 96)
			if errStr != m.lastPollErr {
				m.lastPollErr = errStr
				cmds = append(cmds, m.println(fmt.Sprintf("%s ax status check failed: %s",
					m.styles.warnIcon.Render("!"),
					m.styles.subtle.Render(errStr),
				)))
			}
		} else {
			m.lastPollErr = ""
			now := time.Now()
			seenNow := make(map[string]string, len(msg.tasks))
			for _, t := range msg.tasks {
				seenNow[t.Step] = t.State
				prev, existed := m.prevTaskStates[t.Step]
				if !existed {
					m.taskFirstSeen[t.Step] = now
					m.taskStateSince[t.Step] = now
					cmds = append(cmds, m.println(m.formatTaskCreated(t.Name, t.State)))
				} else if prev != t.State {
					m.taskStateSince[t.Step] = now
					cmds = append(cmds, m.println(m.formatTaskTransition(t.Name, prev, t.State)))
				}
			}
			for step := range m.prevTaskStates {
				if _, stillPresent := seenNow[step]; !stillPresent {
					ran := ""
					if started, ok := m.taskFirstSeen[step]; ok {
						ran = fmt.Sprintf(" (ran %s)", formatElapsed(now.Sub(started)))
					}
					cmds = append(cmds, m.println(fmt.Sprintf("%s ax task %s deleted%s",
						m.styles.okIcon.Render("✓"),
						m.styles.swarmName.Render(m.cfg.Name+"-"+step),
						m.styles.subtle.Render(ran),
					)))
					delete(m.taskFirstSeen, step)
					delete(m.taskStateSince, step)
				}
			}
			changed := len(seenNow) != len(m.prevTaskStates)
			for step, st := range seenNow {
				if m.prevTaskStates[step] != st {
					changed = true
				}
			}
			if changed {
				m.lastActivity = now
				m.idleWarned = false
			}
			if idle := now.Sub(m.lastActivity); !m.lastActivity.IsZero() && idle >= idleWarnAfter && !m.idleWarned {
				m.idleWarned = true
				cmds = append(cmds, m.println(m.styles.warnIcon.Render("!")+" "+m.styles.subtle.Render(m.idleMessage(idle, now))))
			}
			m.prevTaskStates = seenNow
			m.tasks = msg.tasks
			if m.cfg.AutoResume && m.resumer != nil {
				for _, t := range m.resumer.due(msg.tasks, now) {
					cmds = append(cmds, m.resumeTaskCmd(t.Name))
				}
			}
		}
		cmds = append(cmds, m.scheduleTaskPollCmd())
		return m, tea.Batch(cmds...)

	case taskResumedMsg:
		if m.stage != stageRunningAgy {
			return m, nil
		}
		if msg.err == nil {
			return m, m.println(fmt.Sprintf("%s ax-swarm resumed ax task %s",
				m.styles.swarmName.Render("↻"),
				m.styles.swarmName.Render(msg.name),
			))
		}
		// A worker may have resumed or deleted the task first; report other
		// failures once per task.
		if isNotFound(msg.err) || strings.Contains(strings.ToLower(msg.err.Error()), "running") || m.resumeErrShown[msg.name] {
			return m, nil
		}
		m.resumeErrShown[msg.name] = true
		return m, m.println(fmt.Sprintf("%s could not resume ax task %s: %s",
			m.styles.warnIcon.Render("!"),
			m.styles.swarmName.Render(msg.name),
			m.styles.subtle.Render(oneLinePreview(msg.err.Error(), 100)),
		))

	case cleanupDoneMsg:
		m.cleanupRan = true
		var entry stepEntry
		if msg.res.Err != nil {
			entry = stepEntry{
				kind: stepWarn,
				text: fmt.Sprintf("Cleanup warnings for %s*: %v", m.cfg.Name, msg.res.Err),
			}
		} else {
			entry = stepEntry{
				kind: stepOK,
				text: capitalizeFirst(msg.res.Summary(m.cfg.Name)),
			}
		}
		m.finalStep = &entry
		m.steps = append(m.steps, entry)
		m.stage = stageDone
		return m, tea.Quit
	}

	return m, nil
}

func (m *tuiModel) setWorkerState(name, state string) {
	if name == "" {
		return
	}
	if _, exists := m.workers[name]; !exists {
		m.workerOrder = append(m.workerOrder, name)
		if m.workerSince != nil {
			m.workerSince[name] = time.Now()
		}
	}
	if state == "" {
		state = "working"
	}
	m.workers[name] = state
}

func (m tuiModel) renderSetupBanner() string {
	var lines []string
	for _, st := range m.steps {
		lines = append(lines, m.renderStep(st))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s %s %s",
			m.styles.okIcon.Render("✓"),
			m.styles.swarmName.Render(m.cfg.Name),
			m.styles.subtle.Render("Gateway & Workspace ready"),
		)
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) formatTaskCreated(taskName, state string) string {
	if state == "" {
		state = "Creating"
	}
	return fmt.Sprintf("%s ax task %s created %s",
		m.styles.okIcon.Render("+"),
		m.styles.swarmName.Render(taskName),
		m.styles.subtle.Render("("+state+")"),
	)
}

func (m tuiModel) formatTaskTransition(taskName, prevState, state string) string {
	arrow := state
	if prevState != "" {
		arrow = prevState + " → " + state
	}
	switch state {
	case "Running":
		return fmt.Sprintf("%s ax task %s: %s", m.styles.okIcon.Render("▶"), m.styles.swarmName.Render(taskName), m.styles.subtle.Render(arrow))
	case "Suspended":
		return fmt.Sprintf("%s ax task %s: %s", m.styles.warnIcon.Render("⏸"), m.styles.swarmName.Render(taskName), m.styles.subtle.Render(arrow))
	case "Failed", "Crashed":
		return fmt.Sprintf("%s ax task %s: %s", m.styles.errIcon.Render("✗"), m.styles.swarmName.Render(taskName), m.styles.errBox.Render(arrow))
	default:
		if arrow == "" {
			arrow = "Starting"
		}
		return fmt.Sprintf("%s ax task %s: %s", m.styles.infoIcon.Render("…"), m.styles.swarmName.Render(taskName), m.styles.subtle.Render(arrow))
	}
}

func (m tuiModel) startCleanupOrQuit() (tea.Model, tea.Cmd) {
	if m.cfg.Keep {
		m.cleanupRan = true
		if m.env.GatewayCreated || m.env.WorkspaceCreated {
			entry := stepEntry{
				kind: stepInfo,
				text: fmt.Sprintf("Kept AX resources named %s*", m.cfg.Name),
			}
			m.finalStep = &entry
			m.steps = append(m.steps, entry)
		}
		m.stage = stageDone
		return m, tea.Quit
	}
	if !m.env.GatewayCreated && !m.env.WorkspaceCreated {
		m.cleanupRan = true
		m.stage = stageDone
		return m, tea.Quit
	}
	m.stage = stageCleanup
	return m, tea.Batch(m.spinner.Tick, m.cleanupCmd())
}

func (m tuiModel) createGatewayCmd() tea.Cmd {
	ctx := m.setupCtx
	env := m.env
	allowHosts := m.cfg.AllowHosts
	return func() tea.Msg {
		err := env.CreateGateway(ctx, allowHosts)
		return gatewayCreatedMsg{err: err}
	}
}

func (m tuiModel) createWorkspaceCmd() tea.Cmd {
	ctx := m.setupCtx
	env := m.env
	cfg := m.cfg
	return func() tea.Msg {
		err := env.CreateWorkspace(ctx, cfg.Image, cfg.Repos, cfg.SetupCmds)
		return workspaceCreatedMsg{err: err}
	}
}

func (m tuiModel) pollWorkspaceCmd() tea.Cmd {
	ctx := m.setupCtx
	env := m.env
	return func() tea.Msg {
		phase, err := env.AX.Column(ctx, "workspace", env.Name, "PHASE")
		return workspacePhaseMsg{phase: phase, err: err}
	}
}

func (m tuiModel) startAgyCmd() tea.Cmd {
	ctx := m.agyCtx
	starter := m.startAgy
	cfg := m.cfg
	axBase := m.env.AX.BaseCommandLine()
	return func() tea.Msg {
		prompt := BuildSwarmPrompt(cfg, axBase)
		args := BuildAgyArgs(cfg, prompt)
		ch, err := starter(ctx, cfg.AgyBin, args)
		return agyStartedMsg{ch: ch, err: err}
	}
}

func waitAgyEventCmd(ch <-chan AgyEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return agyStreamMsg{ev: ev, ok: ok}
	}
}

func (m tuiModel) scheduleTaskPollCmd() tea.Cmd {
	interval := m.env.AX.PollInterval
	if interval <= 0 || interval < time.Second {
		interval = time.Second
	}
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return taskPollTickMsg{}
	})
}

func (m tuiModel) pollTasksCmd() tea.Cmd {
	ctx := m.agyCtx
	env := m.env
	return func() tea.Msg {
		tasks, err := env.AX.ListTasksInfoWithPrefix(ctx, env.Name+"-")
		return taskListMsg{tasks: tasks, err: err}
	}
}

func (m tuiModel) resumeTaskCmd(name string) tea.Cmd {
	ctx := m.agyCtx
	ax := m.env.AX
	return func() tea.Msg {
		return taskResumedMsg{name: name, err: ax.Resume(ctx, name)}
	}
}

func (m tuiModel) cleanupCmd() tea.Cmd {
	env := m.env
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return cleanupDoneMsg{res: env.CleanupWithResult(ctx)}
	}
}

func (m tuiModel) outputText() string {
	if strings.TrimSpace(m.streamedOutput) != "" {
		return m.streamedOutput
	}
	return m.finalResponse
}

func (m tuiModel) View() string {
	if m.stage == stageRunningAgy {
		var b strings.Builder
		if m.lineBuf != "" {
			b.WriteString(m.lineBuf + "\n")
		}
		if tbl := m.renderSwarmTable(); tbl != "" {
			b.WriteString(tbl + "\n")
			if note := m.rosterNote(time.Now()); note != "" {
				b.WriteString(m.styles.subtle.Render(note) + "\n")
			}
		}
		if m.taskPollCount > 0 {
			b.WriteString(m.styles.metaKey.Render("ax check") + m.renderAXCheckSummary() + "\n")
		}
		b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.renderRunningStatusLine()))
		return b.String()
	}

	if m.agyRan && (m.stage == stageCleanup || m.stage == stageDone) {
		var b strings.Builder
		if m.stage == stageCleanup {
			b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.styles.active.Render(fmt.Sprintf("Cleaning up %s*...", m.cfg.Name))))
			return b.String()
		}
		if m.finalStep != nil {
			b.WriteString(m.renderStep(*m.finalStep) + "\n")
		}
		if m.cancelled {
			b.WriteString(m.styles.warnIcon.Render("Canceled.") + "\n")
		} else if m.agyErr != nil {
			b.WriteString(m.renderError(m.formatAgyError(), 0) + "\n")
		}
		return b.String()
	}

	var b strings.Builder

	// Header bar (shown during goal input and AX setup)
	b.WriteString("\n")
	header := fmt.Sprintf("  %s  %s  %s  %s",
		m.styles.badge.Render("ax-swarm"),
		m.styles.swarmName.Render(m.cfg.Name),
		m.styles.subtle.Render("atespace:"+m.cfg.Atespace),
		m.styles.subtle.Render(fmt.Sprintf("parallel:%d", m.cfg.Parallel)),
	)
	if m.cfg.Model != "" {
		header += "  " + m.styles.subtle.Render("model:"+m.cfg.Model)
	}
	if m.cfg.Keep {
		header += "  " + m.styles.warnIcon.Render("keep")
	}
	b.WriteString(header + "\n")

	for _, repo := range m.cfg.Repos {
		b.WriteString("  " + m.styles.metaKey.Render("repo") + m.styles.metaVal.Render(repo) + "\n")
	}
	if m.cfg.Goal != "" {
		goalPreview := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(m.cfg.Goal, "\r\n", " "), "\n", " "))
		b.WriteString("  " + m.styles.metaKey.Render("goal") + m.styles.metaVal.Render(goalPreview) + "\n")
	}
	b.WriteString("\n")

	if m.stage == stageGoalInput {
		b.WriteString("  " + m.styles.prompt.Render("What should the swarm do?") + "\n")
		b.WriteString("  " + m.input.View() + "\n")
		if m.inputErr != "" {
			b.WriteString("  " + m.styles.errBox.Render(m.inputErr) + "\n")
		}
		b.WriteString("\n  " + m.styles.subtle.Render("enter submit · esc / ctrl+c quit") + "\n")
		return b.String()
	}

	for _, st := range m.steps {
		b.WriteString("  " + m.renderStep(st) + "\n")
	}

	switch m.stage {
	case stageCreateGateway:
		label := fmt.Sprintf("Creating Gateway %s...", m.cfg.Name)
		if m.cancelled {
			label = "Canceling setup..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))
		b.WriteString("\n  " + m.styles.subtle.Render("ctrl+c cancel") + "\n")

	case stageCreateWorkspace:
		label := fmt.Sprintf("Creating Workspace %s...", m.cfg.Name)
		if m.cancelled {
			label = "Canceling setup..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))
		b.WriteString("\n  " + m.styles.subtle.Render("ctrl+c cancel") + "\n")

	case stageWaitWorkspace:
		elapsed := formatElapsed(time.Since(m.workspaceWaitStart))
		phase := m.workspacePhase
		if phase == "" {
			phase = "pending"
		}
		label := fmt.Sprintf("Waiting for Workspace %s (%s · %s)", m.cfg.Name, phase, elapsed)
		if m.cancelled {
			label = "Canceling setup..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))
		b.WriteString("\n  " + m.styles.subtle.Render("ctrl+c cancel") + "\n")

	case stageStartingAgy:
		label := "Starting agy /swarm in background..."
		if m.cancelled {
			label = "Canceling..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))

	case stageCleanup:
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(fmt.Sprintf("Cleaning up AX resources (%s*)...", m.cfg.Name))))

	case stageDone:
		if m.cancelled {
			b.WriteString("\n  " + m.styles.warnIcon.Render("Canceled.") + "\n")
		} else if m.err != nil {
			b.WriteString("\n  " + m.renderError(m.err.Error(), 2) + "\n")
		} else if m.agyErr != nil {
			b.WriteString("\n  " + m.renderError(m.formatAgyError(), 2) + "\n")
		}
	}

	b.WriteString("\n")
	return b.String()
}

func (m tuiModel) renderStep(st stepEntry) string {
	var icon string
	switch st.kind {
	case stepOK:
		icon = m.styles.okIcon.Render("✓")
	case stepInfo:
		icon = m.styles.infoIcon.Render("○")
	case stepWarn:
		icon = m.styles.warnIcon.Render("!")
	case stepErr:
		icon = m.styles.errIcon.Render("✗")
	}
	return fmt.Sprintf("%s %s", icon, st.text)
}

type swarmTableRow struct {
	kind  string
	name  string
	state string
	age   string
}

// renderSwarmTable renders swarm workers and AX tasks as one aligned table.
func (m tuiModel) renderSwarmTable() string {
	now := time.Now()
	var rows []swarmTableRow
	for _, name := range m.workerOrder {
		age := ""
		if since, ok := m.workerSince[name]; ok {
			age = formatElapsed(now.Sub(since))
		}
		rows = append(rows, swarmTableRow{kind: "worker", name: name, state: m.workers[name], age: age})
	}
	for _, t := range m.tasks {
		age := ""
		if since, ok := m.taskStateSince[t.Step]; ok {
			age = formatElapsed(now.Sub(since))
		}
		state := t.State
		if state == "" {
			state = "Starting"
		}
		rows = append(rows, swarmTableRow{kind: "ax task", name: t.Name, state: state, age: age})
	}
	if len(rows) == 0 {
		return ""
	}

	data := make([][]string, len(rows))
	for i, r := range rows {
		data[i] = []string{r.kind, r.name, r.state, r.age}
	}

	cell := m.styles.metaVal.Padding(0, 1)
	header := m.styles.subtle.Bold(true).Padding(0, 1)
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(m.styles.subtle).
		Headers("TYPE", "NAME", "STATE", "FOR").
		Rows(data...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return header
			}
			if row < 0 || row >= len(rows) {
				return cell
			}
			r := rows[row]
			switch col {
			case 0:
				return m.styles.subtle.Padding(0, 1)
			case 1:
				if r.kind == "worker" {
					return m.styles.workerName.Padding(0, 1)
				}
				return m.styles.swarmName.Padding(0, 1)
			case 2:
				return m.stateStyle(r.state).Padding(0, 1)
			case 3:
				return m.styles.subtle.Padding(0, 1)
			}
			return cell
		})
	return t.Render()
}

func (m tuiModel) stateStyle(state string) lipgloss.Style {
	switch state {
	case "Running", "working":
		return m.styles.okIcon
	case "Suspended", "waiting":
		return m.styles.warnIcon
	case "Failed", "Crashed":
		return m.styles.errIcon
	case "killed":
		return m.styles.infoIcon
	default:
		return m.styles.subtle
	}
}

func (m tuiModel) renderAXCheckSummary() string {
	if m.lastPollErr != "" {
		return m.styles.warnIcon.Render(fmt.Sprintf("#%d error: %s", m.taskPollCount, m.lastPollErr))
	}
	var running, suspended, failed int
	for _, t := range m.tasks {
		switch t.State {
		case "Running":
			running++
		case "Suspended":
			suspended++
		case "Failed", "Crashed":
			failed++
		}
	}
	var counts []string
	if running > 0 {
		counts = append(counts, fmt.Sprintf("%d running", running))
	}
	if suspended > 0 {
		counts = append(counts, fmt.Sprintf("%d suspended", suspended))
	}
	if failed > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", failed))
	}
	if len(m.tasks) == 0 {
		return m.styles.subtle.Render(fmt.Sprintf("#%d · 0 tasks", m.taskPollCount))
	}
	detail := fmt.Sprintf("#%d · %d task", m.taskPollCount, len(m.tasks))
	if len(m.tasks) != 1 {
		detail += "s"
	}
	if len(counts) > 0 {
		detail += " (" + strings.Join(counts, ", ") + ")"
	}
	return m.styles.subtle.Render(detail)
}

func (m tuiModel) renderRunningStatusLine() string {
	if m.cancelled {
		return m.styles.warnIcon.Render("canceling agy /swarm...")
	}
	parts := []string{m.styles.swarmName.Render(m.cfg.Name)}
	if m.agyModel != "" {
		parts = append(parts, m.styles.active.Render(m.agyModel))
	}
	if !m.waitingSince.IsZero() {
		parts = append(parts, m.styles.warnIcon.Render(fmt.Sprintf("waiting for a worker to report (%s)", formatElapsed(time.Since(m.waitingSince)))))
	} else if m.agyStatus != "" {
		parts = append(parts, m.agyStatus)
	}
	parts = append(parts, m.styles.subtle.Render(formatElapsed(time.Since(m.agyStart))))
	return strings.Join(parts, m.styles.subtle.Render(" · ")) + m.styles.subtle.Render("  (ctrl+c cancel)")
}

func (m tuiModel) formatAgyError() string {
	if m.agyErr == nil {
		return ""
	}
	if len(m.agyStderr) > 0 {
		return fmt.Sprintf("%v (%s)", m.agyErr, m.agyStderr[len(m.agyStderr)-1])
	}
	return m.agyErr.Error()
}

// idleWarnAfter is how long the swarm may show no activity before ax-swarm
// explains what it is waiting on.
const idleWarnAfter = 90 * time.Second

func (m tuiModel) idleMessage(idle time.Duration, now time.Time) string {
	msg := fmt.Sprintf("no swarm activity for %s.", formatElapsed(idle))
	if !m.waitingSince.IsZero() {
		msg += " The coordinator is waiting for a worker to post @coordinator; if workers finished without reporting, it will keep waiting."
	} else {
		msg += " Workers may still be busy in their own sessions."
	}
	if note := m.rosterNote(now); note != "" {
		msg += " " + capitalizeFirst(note) + "."
	}
	return msg + " Press ctrl+c to stop and clean up."
}

// rosterNote says how fresh the worker states in the table are: they are only
// updated when the coordinator runs `swarm status`.
func (m tuiModel) rosterNote(now time.Time) string {
	if len(m.workerOrder) == 0 {
		return ""
	}
	if m.rosterAt.IsZero() {
		return "worker states are as of when each worker was added"
	}
	return fmt.Sprintf("worker states are from the last swarm status, %s ago", formatElapsed(now.Sub(m.rosterAt)))
}

// println prints a line above the live view. Bubble Tea assumes each printed
// line takes one terminal row, so a line wider than the terminal would wrap,
// throw off its row accounting, and leave fragments of the live view behind.
// Wrapping to the terminal width ourselves avoids that.
func (m tuiModel) println(s string) tea.Cmd {
	if m.width > 1 {
		s = ansi.Wrap(s, m.width-1, " ")
	}
	return tea.Println(s)
}

// renderError formats an error so the whole message stays visible: it is
// word-wrapped to the terminal width (Bubble Tea truncates long lines) and
// continuation lines are indented to line up under the first.
func (m tuiModel) renderError(msg string, indent int) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	wrapAt := width - indent - 1
	if wrapAt < 20 {
		wrapAt = 20
	}
	pad := strings.Repeat(" ", indent)
	body := m.styles.errBox.Width(wrapAt).Render("Error: " + msg)
	body = strings.ReplaceAll(body, "\n", "\n"+pad)
	if hint := axErrorHint(msg, m.cfg.Name); hint != "" {
		hintText := m.styles.subtle.Width(wrapAt).Render(hint)
		body += "\n" + pad + strings.ReplaceAll(hintText, "\n", "\n"+pad)
	}
	return body
}

// axErrorHint explains errors that come from the AX cluster rather than ax-swarm.
func axErrorHint(msg, swarmName string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "runsc") || strings.Contains(lower, "callateletrestore"):
		return "The AX cluster could not start a sandbox (gVisor/runsc). This is an AX cluster problem, " +
			"not an ax-swarm one: check `ax local status` and the atelet logs " +
			"(`kubectl -n ate-system logs ds/atelet --all-containers`), or restart the local cluster."
	case strings.Contains(lower, "preparing workspace") || strings.Contains(lower, "failed setup"):
		return fmt.Sprintf("AX failed to set up the Workspace. Rerun with --keep to inspect it with "+
			"`ax describe workspace %s`.", swarmName)
	}
	return ""
}

func oneLinePreview(s string, maxLen int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
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

func runTUI(cfg Config, env *SwarmEnv, stdin io.Reader, stdout, stderr io.Writer, stdoutTTY, colorEnabled bool) int {
	styles := newTUIStyles(stderr, cfg.Color, colorEnabled)
	model := newTUIModel(cfg, env, styles, StartAgyStream)
	defer model.cancelSetup()
	defer model.cancelAgy()

	p := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stderr))
	finalModel, err := p.Run()
	if err != nil {
		// p.Run also returns an error when ax-swarm is killed (SIGTERM), with
		// the live model: stop agy first, then clean up AX resources.
		if killed, ok := finalModel.(tuiModel); ok {
			stopAgy(killed.cancelAgy, killed.agyCh, 10*time.Second)
		}
		if !cfg.Keep && (env.GatewayCreated || env.WorkspaceCreated) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			env.CleanupWithResult(ctx)
		}
		fmt.Fprintf(stderr, "ax-swarm: %v\n", err)
		return 1
	}

	final, ok := finalModel.(tuiModel)
	if !ok {
		return 0
	}

	// Stop the background agy and wait for it to exit before cleaning up, so it
	// cannot recreate AX tasks after cleanup and is never left orphaned when
	// ax-swarm returns (os.Exit would otherwise skip the asynchronous kill).
	stopAgy(final.cancelAgy, final.agyCh, 10*time.Second)

	if !final.cleanupRan && !cfg.Keep && (env.GatewayCreated || env.WorkspaceCreated) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		env.CleanupWithResult(ctx)
	}

	// If stdout is redirected/piped (not a TTY), write agy's output cleanly to
	// stdout while the TUI rendered live progress and output on stderr.
	if !stdoutTTY {
		if out := final.outputText(); out != "" {
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			_, _ = io.WriteString(stdout, out)
		}
	}

	if final.cancelled || final.err != nil {
		return 1
	}
	if final.agyErr != nil {
		var exitErr *exec.ExitError
		if errors.As(final.agyErr, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}
