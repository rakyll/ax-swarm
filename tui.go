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
type taskCountsMsg struct {
	running int
	total   int
	err     error
}
type cleanupDoneMsg struct{ res CleanupResult }

type tuiStyles struct {
	badge     lipgloss.Style
	swarmName lipgloss.Style
	metaKey   lipgloss.Style
	metaVal   lipgloss.Style
	prompt    lipgloss.Style
	okIcon    lipgloss.Style
	infoIcon  lipgloss.Style
	warnIcon  lipgloss.Style
	errIcon   lipgloss.Style
	active    lipgloss.Style
	dim       lipgloss.Style
	errBox    lipgloss.Style
	spinner   lipgloss.Style
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

	return tuiStyles{
		badge:     r.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("14")).Padding(0, 1),
		swarmName: r.NewStyle().Bold(true).Foreground(lipgloss.Color("14")),
		metaKey:   r.NewStyle().Foreground(lipgloss.Color("8")).Width(10),
		metaVal:   r.NewStyle().Foreground(lipgloss.Color("7")),
		prompt:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("15")),
		okIcon:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("10")),
		infoIcon:  r.NewStyle().Foreground(lipgloss.Color("8")),
		warnIcon:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("11")),
		errIcon:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("9")),
		active:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("15")),
		dim:       r.NewStyle().Foreground(lipgloss.Color("8")),
		errBox:    r.NewStyle().Foreground(lipgloss.Color("9")),
		spinner:   r.NewStyle().Foreground(lipgloss.Color("14")),
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

	setupCtx      context.Context
	cancelSetup   context.CancelFunc
	setupDeadline time.Time
	inFlightSetup bool

	workspaceWaitStart time.Time
	workspacePhase     string

	startAgy      agyStarter
	agyCtx        context.Context
	cancelAgy     context.CancelFunc
	agyCh         <-chan AgyEvent
	agyStart      time.Time
	agyModel      string
	agyActiveTool string
	agyRan        bool
	runningTasks  int
	totalTasks    int

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
		cfg:           cfg,
		env:           env,
		styles:        styles,
		stage:         stage,
		spinner:       sp,
		input:         ti,
		setupCtx:      setupCtx,
		cancelSetup:   cancelSetup,
		setupDeadline: time.Now().Add(cfg.SetupTimeout),
		inFlightSetup: inFlightSetup,
		startAgy:      starter,
		agyCtx:        agyCtx,
		cancelAgy:     cancelAgy,
		agyModel:      cfg.Model,
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
					// Second Ctrl+C forces immediate exit; runTUI still runs cleanup.
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
		m.stage = stageRunningAgy
		return m, tea.Batch(waitAgyEventCmd(m.agyCh), m.scheduleTaskPollCmd())

	case agyStreamMsg:
		if m.stage != stageRunningAgy {
			return m, nil
		}
		if !msg.ok || msg.ev.Kind == AgyEventDone {
			var flushCmds []tea.Cmd
			if m.lineBuf != "" {
				flushCmds = append(flushCmds, tea.Println(m.lineBuf))
				m.lineBuf = ""
			}
			if m.finalResponse != "" && strings.TrimSpace(m.streamedOutput) == "" {
				trimmed := strings.TrimRight(m.finalResponse, "\n")
				m.streamedOutput = m.finalResponse
				if trimmed != "" {
					flushCmds = append(flushCmds, tea.Println(trimmed))
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
		switch msg.ev.Kind {
		case AgyEventInit:
			if msg.ev.Model != "" {
				m.agyModel = msg.ev.Model
			}
		case AgyEventTool:
			if msg.ev.ToolState == "ACTIVE" {
				m.agyActiveTool = msg.ev.ToolName
			} else {
				m.agyActiveTool = ""
			}
		case AgyEventText:
			m.agyActiveTool = ""
			m.streamedOutput += msg.ev.Text
			m.lineBuf += msg.ev.Text
			if idx := strings.LastIndex(m.lineBuf, "\n"); idx >= 0 {
				ready := m.lineBuf[:idx]
				m.lineBuf = m.lineBuf[idx+1:]
				cmds = append(cmds, tea.Println(ready))
			}
		case AgyEventResult:
			if msg.ev.Response != "" {
				m.finalResponse = msg.ev.Response
			}
			if msg.ev.Status == "ERROR" || msg.ev.ErrorText != "" {
				errMsg := msg.ev.ErrorText
				if errMsg == "" {
					errMsg = "agy returned status " + msg.ev.Status
				}
				m.agyResultErr = errors.New(errMsg)
			}
		case AgyEventStderr:
			if len(m.agyStderr) < 20 {
				m.agyStderr = append(m.agyStderr, msg.ev.ErrorText)
			}
		}

		cmds = append(cmds, waitAgyEventCmd(m.agyCh))
		return m, tea.Batch(cmds...)

	case taskPollTickMsg:
		if m.stage != stageRunningAgy || m.cancelled {
			return m, nil
		}
		return m, m.pollTaskCountsCmd()

	case taskCountsMsg:
		if m.stage != stageRunningAgy {
			return m, nil
		}
		if msg.err == nil {
			m.runningTasks = msg.running
			m.totalTasks = msg.total
		}
		return m, m.scheduleTaskPollCmd()

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
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return taskPollTickMsg{}
	})
}

func (m tuiModel) pollTaskCountsCmd() tea.Cmd {
	ctx := m.agyCtx
	env := m.env
	return func() tea.Msg {
		running, total, err := env.AX.ListTaskCountsWithPrefix(ctx, env.Name+"-")
		return taskCountsMsg{running: running, total: total, err: err}
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
	// Once agy is running in the background, keep the view focused cleanly on
	// agy's streamed output with only a minimal status line underneath.
	if m.stage == stageRunningAgy {
		var b strings.Builder
		if m.lineBuf != "" {
			b.WriteString(m.lineBuf + "\n")
		}
		status := m.runningStatusText()
		b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.styles.dim.Render(status)))
		return b.String()
	}

	if m.agyRan && (m.stage == stageCleanup || m.stage == stageDone) {
		var b strings.Builder
		if m.stage == stageCleanup {
			b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.styles.dim.Render(fmt.Sprintf("cleaning up %s*...", m.cfg.Name))))
			return b.String()
		}
		if m.finalStep != nil {
			b.WriteString(m.renderStep(*m.finalStep) + "\n")
		}
		if m.cancelled {
			b.WriteString(m.styles.warnIcon.Render("Canceled.") + "\n")
		} else if m.agyErr != nil {
			b.WriteString(m.styles.errBox.Render("Error: "+m.formatAgyError()) + "\n")
		}
		return b.String()
	}

	var b strings.Builder

	// Header bar (shown during goal input and AX setup)
	b.WriteString("\n")
	header := fmt.Sprintf("  %s  %s  %s  %s",
		m.styles.badge.Render("ax-swarm"),
		m.styles.swarmName.Render(m.cfg.Name),
		m.styles.dim.Render("atespace:"+m.cfg.Atespace),
		m.styles.dim.Render(fmt.Sprintf("parallel:%d", m.cfg.Parallel)),
	)
	if m.cfg.Model != "" {
		header += "  " + m.styles.dim.Render("model:"+m.cfg.Model)
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
		b.WriteString("\n  " + m.styles.dim.Render("enter submit · esc / ctrl+c quit") + "\n")
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
		b.WriteString("\n  " + m.styles.dim.Render("ctrl+c cancel") + "\n")

	case stageCreateWorkspace:
		label := fmt.Sprintf("Creating Workspace %s...", m.cfg.Name)
		if m.cancelled {
			label = "Canceling setup..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))
		b.WriteString("\n  " + m.styles.dim.Render("ctrl+c cancel") + "\n")

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
		b.WriteString("\n  " + m.styles.dim.Render("ctrl+c cancel") + "\n")

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
			b.WriteString("\n  " + m.styles.errBox.Render("Error: "+m.err.Error()) + "\n")
		} else if m.agyErr != nil {
			b.WriteString("\n  " + m.styles.errBox.Render("Error: "+m.formatAgyError()) + "\n")
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

func (m tuiModel) runningStatusText() string {
	if m.cancelled {
		return "canceling agy /swarm..."
	}
	parts := []string{m.cfg.Name}
	if m.agyModel != "" {
		parts = append(parts, m.agyModel)
	}
	if m.totalTasks > 0 {
		if m.runningTasks == m.totalTasks {
			parts = append(parts, fmt.Sprintf("%d AX tasks running", m.runningTasks))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%d AX tasks running", m.runningTasks, m.totalTasks))
		}
	}
	if m.agyActiveTool != "" {
		parts = append(parts, m.agyActiveTool)
	}
	parts = append(parts, formatElapsed(time.Since(m.agyStart)))
	return strings.Join(parts, " · ") + "  (ctrl+c cancel)"
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
		// Ensure AX resources are still cleaned up if Bubble Tea itself failed.
		if !cfg.Keep && (env.GatewayCreated || env.WorkspaceCreated) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			env.CleanupWithResult(ctx)
		}
		fmt.Fprintf(stderr, "ax-swarm: tui error: %v\n", err)
		return 1
	}

	final, ok := finalModel.(tuiModel)
	if !ok {
		return 0
	}
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
