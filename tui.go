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
type agyExitedMsg struct{ err error }
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
	agyStart           time.Time

	execAgy func(agyBin string, agyArgs []string) tea.Cmd

	err        error
	agyErr     error
	cancelled  bool
	cleanupRan bool
}

func defaultExecAgy(stdin io.Reader, stdout, stderr io.Writer) func(string, []string) tea.Cmd {
	return func(agyBin string, agyArgs []string) tea.Cmd {
		cmd := exec.Command(agyBin, agyArgs...)
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return agyExitedMsg{err: err}
		})
	}
}

func newTUIModel(cfg Config, env *SwarmEnv, styles tuiStyles, execAgy func(string, []string) tea.Cmd) tuiModel {
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
		execAgy:       execAgy,
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
		if m.stage == stageCreateGateway || m.stage == stageCreateWorkspace || m.stage == stageWaitWorkspace || m.stage == stageCleanup {
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
		}
		return m, nil

	case gatewayCreatedMsg:
		m.inFlightSetup = false
		if m.cancelled {
			if msg.err == nil {
				m.env.GatewayCreated = true
			}
			return m.startCleanupOrQuit()
		}
		if msg.err != nil {
			m.err = msg.err
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Failed to create Gateway " + m.cfg.Name})
			return m.startCleanupOrQuit()
		}
		m.env.GatewayCreated = true
		m.steps = append(m.steps, stepEntry{kind: stepOK, text: "Created Gateway " + m.cfg.Name})
		m.stage = stageCreateWorkspace
		m.inFlightSetup = true
		return m, m.createWorkspaceCmd()

	case workspaceCreatedMsg:
		m.inFlightSetup = false
		if m.cancelled {
			if msg.err == nil {
				m.env.WorkspaceCreated = true
			}
			return m.startCleanupOrQuit()
		}
		if msg.err != nil {
			m.err = msg.err
			m.steps = append(m.steps, stepEntry{kind: stepErr, text: "Failed to create Workspace " + m.cfg.Name})
			return m.startCleanupOrQuit()
		}
		m.env.WorkspaceCreated = true
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
			m.stage = stageRunningAgy
			m.agyStart = time.Now()
			prompt := BuildSwarmPrompt(m.cfg, m.env.AX.BaseCommandLine())
			agyArgs := BuildAgyArgs(m.cfg, prompt)
			return m, m.execAgy(m.cfg.AgyBin, agyArgs)
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

	case agyExitedMsg:
		m.agyErr = msg.err
		elapsed := formatElapsed(time.Since(m.agyStart))
		if msg.err != nil {
			var exitErr *exec.ExitError
			if errors.As(msg.err, &exitErr) {
				m.steps = append(m.steps, stepEntry{
					kind: stepWarn,
					text: fmt.Sprintf("agy /swarm exited with status %d (%s)", exitErr.ExitCode(), elapsed),
				})
			} else {
				m.steps = append(m.steps, stepEntry{
					kind: stepErr,
					text: fmt.Sprintf("agy /swarm failed (%s)", elapsed),
				})
			}
		} else {
			m.steps = append(m.steps, stepEntry{
				kind: stepOK,
				text: fmt.Sprintf("agy /swarm session finished (%s)", elapsed),
			})
		}
		return m.startCleanupOrQuit()

	case cleanupDoneMsg:
		m.cleanupRan = true
		if msg.res.Err != nil {
			m.steps = append(m.steps, stepEntry{
				kind: stepWarn,
				text: fmt.Sprintf("Cleanup warnings for %s*: %v", m.cfg.Name, msg.res.Err),
			})
		} else {
			m.steps = append(m.steps, stepEntry{
				kind: stepOK,
				text: capitalizeFirst(msg.res.Summary(m.cfg.Name)),
			})
		}
		m.stage = stageDone
		return m, tea.Quit
	}

	return m, nil
}

func (m tuiModel) startCleanupOrQuit() (tea.Model, tea.Cmd) {
	if m.cfg.Keep {
		m.cleanupRan = true
		if m.env.GatewayCreated || m.env.WorkspaceCreated {
			m.steps = append(m.steps, stepEntry{
				kind: stepInfo,
				text: fmt.Sprintf("Kept AX resources named %s*", m.cfg.Name),
			})
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

func (m tuiModel) cleanupCmd() tea.Cmd {
	env := m.env
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return cleanupDoneMsg{res: env.CleanupWithResult(ctx)}
	}
}

func (m tuiModel) View() string {
	var b strings.Builder

	// Header bar
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
		b.WriteString(fmt.Sprintf("  %s %s\n", icon, st.text))
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

	case stageRunningAgy:
		b.WriteString(fmt.Sprintf("  %s %s\n", m.styles.swarmName.Render("▶"), m.styles.active.Render("Launching interactive agy /swarm...")))

	case stageCleanup:
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(fmt.Sprintf("Cleaning up AX resources (%s*)...", m.cfg.Name))))

	case stageDone:
		if m.cancelled {
			b.WriteString("\n  " + m.styles.warnIcon.Render("Canceled.") + "\n")
		} else if m.err != nil {
			b.WriteString("\n  " + m.styles.errBox.Render("Error: "+m.err.Error()) + "\n")
		} else if m.agyErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(m.agyErr, &exitErr) {
				b.WriteString("\n  " + m.styles.errBox.Render("Error: "+m.agyErr.Error()) + "\n")
			}
		}
	}

	b.WriteString("\n")
	return b.String()
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

func runTUI(cfg Config, env *SwarmEnv, stdin io.Reader, stdout, stderr io.Writer, colorEnabled bool) int {
	styles := newTUIStyles(stderr, cfg.Color, colorEnabled)
	model := newTUIModel(cfg, env, styles, defaultExecAgy(stdin, stdout, stderr))
	defer model.cancelSetup()

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
