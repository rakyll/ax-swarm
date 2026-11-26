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
	"fmt"
	"io"
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
	stageSetup
	stageRunning
	stageCleanup
	stageDone
)

type stepEntry struct {
	level LogLevel
	text  string
}

type orchEventMsg struct {
	ev Event
	ok bool
}
type taskPollTickMsg struct{}
type taskListMsg struct {
	tasks []AXTaskInfo
	err   error
}
type cleanupDoneMsg struct{ res CleanupResult }

type tuiStyles struct {
	badge      lipgloss.Style
	runName    lipgloss.Style
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
	// Same palette as ax-swarm: ANSI 0-6 + Faint/Bold stay legible on dark and
	// light themes.
	return tuiStyles{
		badge:      r.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1),
		runName:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
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

type tuiModel struct {
	cfg     Config
	env     *Env
	styles  tuiStyles
	starter orchStarter

	stage    tuiStage
	steps    []stepEntry
	spinner  spinner.Model
	input    textinput.Model
	inputErr string
	width    int

	ctx    context.Context
	cancel context.CancelFunc
	events <-chan Event

	start     time.Time
	runStart  time.Time
	status    string
	phase     string
	tasks     []TaskSnapshot
	ran       bool
	lineBuf   string
	output    string
	gotDone   bool
	runErr    error
	cancelled bool

	axTasks     map[string]AXTaskInfo
	pollCount   int
	lastPollErr string

	cleanupRan bool
	finalStep  *stepEntry
}

func newTUIModel(cfg Config, env *Env, styles tuiStyles, starter orchStarter) tuiModel {
	if starter == nil {
		starter = defaultOrchStarter(env)
	}
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(styles.spinner))
	ti := textinput.New()
	ti.Placeholder = "Describe the goal for axo..."
	ti.Prompt = "❯ "
	ti.PromptStyle = styles.runName
	ti.CharLimit = 4096
	ti.Width = 64

	ctx, cancel := context.WithCancel(context.Background())
	stage := stageSetup
	if cfg.Goal == "" {
		stage = stageGoalInput
		ti.Focus()
	}
	return tuiModel{
		cfg:     cfg,
		env:     env,
		styles:  styles,
		starter: starter,
		stage:   stage,
		spinner: sp,
		input:   ti,
		ctx:     ctx,
		cancel:  cancel,
		status:  fmt.Sprintf("Creating Gateway %s...", cfg.Name),
		axTasks: make(map[string]AXTaskInfo),
	}
}

func (m tuiModel) Init() tea.Cmd {
	if m.stage == stageGoalInput {
		return textinput.Blink
	}
	return nil
}

// startRun launches the orchestrator. It must run from Update so the event
// channel is stored on the model Bubble Tea keeps.
func (m *tuiModel) startRun() tea.Cmd {
	m.start = time.Now()
	m.events = m.starter(m.ctx, m.cfg)
	return tea.Batch(m.spinner.Tick, waitEventCmd(m.events))
}

func waitEventCmd(ch <-chan Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return orchEventMsg{ev: ev, ok: ok}
	}
}

type startMsg struct{}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.input.Width = max(20, min(88, msg.Width-8))
		if m.stage == stageSetup && m.events == nil {
			return m, m.startRun()
		}
		return m, nil

	case startMsg:
		if m.stage == stageSetup && m.events == nil {
			return m, m.startRun()
		}
		return m, nil

	case spinner.TickMsg:
		switch m.stage {
		case stageSetup, stageRunning, stageCleanup:
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
				m.cancel()
				m.stage = stageDone
				return m, tea.Quit
			case tea.KeyEnter:
				goal := strings.TrimSpace(m.input.Value())
				if goal == "" {
					m.inputErr = "Enter a goal to start axo."
					return m, nil
				}
				m.cfg.Goal = goal
				m.inputErr = ""
				m.input.Blur()
				m.stage = stageSetup
				return m, m.startRun()
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			if m.inputErr != "" && strings.TrimSpace(m.input.Value()) != "" {
				m.inputErr = ""
			}
			return m, cmd
		case stageSetup, stageRunning:
			if msg.Type == tea.KeyCtrlC || (m.stage == stageSetup && msg.String() == "q") {
				if m.cancelled {
					return m, tea.Quit
				}
				m.cancelled = true
				m.cancel()
			}
		}
		return m, nil

	case orchEventMsg:
		return m.handleEvent(msg)

	case taskPollTickMsg:
		if m.stage != stageRunning || m.cancelled {
			return m, nil
		}
		return m, m.pollTasksCmd()

	case taskListMsg:
		if m.stage != stageRunning {
			return m, nil
		}
		m.pollCount++
		if msg.err != nil {
			m.lastPollErr = oneLinePreview(msg.err.Error(), 96)
		} else {
			m.lastPollErr = ""
			m.axTasks = make(map[string]AXTaskInfo, len(msg.tasks))
			for _, t := range msg.tasks {
				m.axTasks[t.Name] = t
			}
		}
		return m, m.scheduleTaskPollCmd()

	case cleanupDoneMsg:
		m.cleanupRan = true
		var entry stepEntry
		if msg.res.Err != nil {
			entry = stepEntry{level: LevelWarn, text: fmt.Sprintf("Cleanup warnings for %s*: %v", m.cfg.Name, msg.res.Err)}
		} else {
			entry = stepEntry{level: LevelOK, text: capitalizeFirst(msg.res.Summary(m.cfg.Name))}
		}
		m.finalStep = &entry
		m.steps = append(m.steps, entry)
		m.stage = stageDone
		return m, tea.Quit
	}
	return m, nil
}

func (m tuiModel) handleEvent(msg orchEventMsg) (tea.Model, tea.Cmd) {
	if !msg.ok || msg.ev.Kind == EvDone {
		m.gotDone = true
		if msg.ok {
			m.runErr = msg.ev.Err
		}
		var cmds []tea.Cmd
		if m.stage == stageRunning {
			if m.lineBuf != "" {
				cmds = append(cmds, m.println(m.lineBuf))
				m.lineBuf = ""
			}
			cmds = append(cmds, m.println(m.completionLine()))
		}
		next, cleanup := m.startCleanupOrQuit()
		return next, tea.Sequence(append(cmds, cleanup)...)
	}

	ev := msg.ev
	var cmds []tea.Cmd
	switch ev.Kind {
	case EvStep:
		st := stepEntry{level: ev.Level, text: ev.Text}
		m.steps = append(m.steps, st)
		if m.stage == stageRunning {
			cmds = append(cmds, m.println(m.renderStep(st)))
		}
	case EvStatus:
		m.status = ev.Text
	case EvSetupDone:
		m.stage = stageRunning
		m.ran = true
		m.runStart = time.Now()
		cmds = append(cmds, m.println(m.renderSetupBanner()), m.pollTasksCmd())
	case EvPhase:
		m.phase = ev.Text
	case EvLog:
		cmds = append(cmds, m.println(m.renderLog(ev)))
	case EvTasks:
		m.tasks = ev.Tasks
	case EvText:
		m.output += ev.Text
		m.lineBuf += ev.Text
		if idx := strings.LastIndex(m.lineBuf, "\n"); idx >= 0 {
			ready := m.lineBuf[:idx]
			m.lineBuf = m.lineBuf[idx+1:]
			cmds = append(cmds, m.println(ready))
		}
	}
	cmds = append(cmds, waitEventCmd(m.events))
	return m, tea.Batch(cmds...)
}

func (m tuiModel) startCleanupOrQuit() (tea.Model, tea.Cmd) {
	if !m.env.created() {
		m.cleanupRan = true
		m.stage = stageDone
		return m, tea.Quit
	}
	if m.cfg.Keep {
		m.cleanupRan = true
		entry := stepEntry{level: LevelInfo, text: fmt.Sprintf("Kept AX resources named %s*", m.cfg.Name)}
		m.finalStep = &entry
		m.steps = append(m.steps, entry)
		m.stage = stageDone
		return m, tea.Quit
	}
	m.stage = stageCleanup
	return m, tea.Batch(m.spinner.Tick, m.cleanupCmd())
}

func (m tuiModel) cleanupCmd() tea.Cmd {
	env := m.env
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return cleanupDoneMsg{res: env.CleanupWithResult(ctx)}
	}
}

func (m tuiModel) scheduleTaskPollCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return taskPollTickMsg{} })
}

func (m tuiModel) pollTasksCmd() tea.Cmd {
	ctx := m.ctx
	env := m.env
	return func() tea.Msg {
		tasks, err := env.AX.ListTasksInfoWithPrefix(ctx, env.Name+"-")
		return taskListMsg{tasks: tasks, err: err}
	}
}

func (m tuiModel) View() string {
	if m.stage == stageRunning {
		var b strings.Builder
		if m.lineBuf != "" {
			b.WriteString(m.lineBuf + "\n")
		}
		if tbl := m.renderTaskTable(); tbl != "" {
			b.WriteString(tbl + "\n")
		}
		if m.pollCount > 0 {
			b.WriteString(m.styles.metaKey.Render("ax check") + m.renderAXCheckSummary() + "\n")
		}
		b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.renderRunningStatusLine()))
		return b.String()
	}

	if m.ran && (m.stage == stageCleanup || m.stage == stageDone) {
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
		} else if m.runErr != nil {
			b.WriteString(m.renderError(m.runErr.Error(), 0) + "\n")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString("\n")
	header := fmt.Sprintf("  %s  %s  %s  %s  %s",
		m.styles.badge.Render(progName),
		m.styles.runName.Render(m.cfg.Name),
		m.styles.subtle.Render("atespace:"+m.cfg.Atespace),
		m.styles.subtle.Render(fmt.Sprintf("parallel:%d", m.cfg.Parallel)),
		m.styles.subtle.Render(fmt.Sprintf("rounds:%d", m.cfg.MaxRounds)),
	)
	if m.cfg.Model != "" {
		header += "  " + m.styles.subtle.Render("model:"+m.cfg.Model)
	}
	if m.cfg.Keep {
		header += "  " + m.styles.warnIcon.Render("keep")
	}
	b.WriteString(header + "\n")
	for _, repo := range workspaceRepos(map[string]any{"spec": m.cfg.WorkspaceSpec}) {
		b.WriteString("  " + m.styles.metaKey.Render("repo") + m.styles.metaVal.Render(repo) + "\n")
	}
	if m.cfg.Goal != "" {
		b.WriteString("  " + m.styles.metaKey.Render("goal") + m.styles.metaVal.Render(oneLine(m.cfg.Goal)) + "\n")
	}
	b.WriteString("\n")

	if m.stage == stageGoalInput {
		b.WriteString("  " + m.styles.prompt.Render("What should axo do?") + "\n")
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
	case stageSetup:
		label := m.status
		if m.cancelled {
			label = "Canceling setup..."
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(label)))
		b.WriteString("\n  " + m.styles.subtle.Render("ctrl+c cancel") + "\n")
	case stageCleanup:
		b.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), m.styles.active.Render(fmt.Sprintf("Cleaning up AX resources (%s*)...", m.cfg.Name))))
	case stageDone:
		if m.cancelled {
			b.WriteString("\n  " + m.styles.warnIcon.Render("Canceled.") + "\n")
		} else if m.runErr != nil {
			b.WriteString("\n  " + m.renderError(m.runErr.Error(), 2) + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

func (m tuiModel) iconStyle(l LogLevel) lipgloss.Style {
	switch l {
	case LevelOK:
		return m.styles.okIcon
	case LevelWarn:
		return m.styles.warnIcon
	case LevelErr:
		return m.styles.errIcon
	case LevelAction:
		return m.styles.runName
	default:
		return m.styles.infoIcon
	}
}

func (m tuiModel) renderStep(st stepEntry) string {
	icon := defaultIcon(st.level)
	if st.level == LevelInfo {
		icon = "○"
	}
	return fmt.Sprintf("%s %s", m.iconStyle(st.level).Render(icon), st.text)
}

func (m tuiModel) renderLog(ev Event) string {
	icon := ev.Icon
	if icon == "" {
		icon = defaultIcon(ev.Level)
	}
	text := ev.Text
	if ev.Level == LevelInfo {
		text = m.styles.subtle.Render(text)
	}
	return m.iconStyle(ev.Level).Render(icon) + " " + text
}

func (m tuiModel) renderSetupBanner() string {
	var lines []string
	for _, st := range m.steps {
		lines = append(lines, m.renderStep(st))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s %s %s", m.styles.okIcon.Render("✓"), m.styles.runName.Render(m.cfg.Name), m.styles.subtle.Render("Gateway & Workspace ready"))
	}
	return strings.Join(lines, "\n")
}

// renderTaskTable renders orchestrator tasks and their AX Tasks as one table.
func (m tuiModel) renderTaskTable() string {
	if len(m.tasks) == 0 {
		return ""
	}
	now := time.Now()
	data := make([][]string, len(m.tasks))
	for i, t := range m.tasks {
		axName, axState := "", ""
		if t.AXName != "" {
			axName = t.AXName
			if info, ok := m.axTasks[t.AXName]; ok {
				axState = info.State
			} else if t.Status.terminal() && !m.cfg.Keep {
				axState = "deleted"
			}
		}
		status := string(t.Status)
		if t.Attempt > 1 && !t.Status.terminal() {
			status += fmt.Sprintf(" (try %d)", t.Attempt)
		}
		data[i] = []string{t.ID, status, axName, axState, formatElapsed(now.Sub(t.Since))}
	}
	cell := m.styles.metaVal.Padding(0, 1)
	header := m.styles.subtle.Bold(true).Padding(0, 1)
	tasks := m.tasks
	return table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(m.styles.subtle).
		Headers("TASK", "STATE", "AX TASK", "AX STATE", "FOR").
		Rows(data...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return header
			}
			if row < 0 || row >= len(tasks) {
				return cell
			}
			switch col {
			case 0:
				return m.styles.workerName.Padding(0, 1)
			case 1:
				return m.statusStyle(tasks[row].Status).Padding(0, 1)
			case 2:
				return m.styles.runName.Padding(0, 1)
			case 3:
				return m.axStateStyle(data[row][3]).Padding(0, 1)
			default:
				return m.styles.subtle.Padding(0, 1)
			}
		}).
		Render()
}

func (m tuiModel) statusStyle(s TaskStatus) lipgloss.Style {
	switch s {
	case StatusRunning, StatusDone:
		return m.styles.okIcon
	case StatusStarting, StatusBlocked:
		return m.styles.warnIcon
	case StatusFailed:
		return m.styles.errIcon
	default:
		return m.styles.subtle
	}
}

func (m tuiModel) axStateStyle(state string) lipgloss.Style {
	switch state {
	case "Running":
		return m.styles.okIcon
	case "Suspended":
		return m.styles.warnIcon
	case "Failed", "Crashed":
		return m.styles.errIcon
	default:
		return m.styles.subtle
	}
}

func (m tuiModel) renderAXCheckSummary() string {
	if m.lastPollErr != "" {
		return m.styles.warnIcon.Render(fmt.Sprintf("#%d error: %s", m.pollCount, m.lastPollErr))
	}
	var running, suspended, failed int
	for _, t := range m.axTasks {
		switch t.State {
		case "Running":
			running++
		case "Suspended":
			suspended++
		case "Failed", "Crashed":
			failed++
		}
	}
	detail := fmt.Sprintf("#%d · %d ax task", m.pollCount, len(m.axTasks))
	if len(m.axTasks) != 1 {
		detail += "s"
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
	if len(counts) > 0 {
		detail += " (" + strings.Join(counts, ", ") + ")"
	}
	var done, total int
	for _, t := range m.tasks {
		total++
		if t.Status.terminal() {
			done++
		}
	}
	detail += fmt.Sprintf(" · %d/%d tasks finished", done, total)
	return m.styles.subtle.Render(detail)
}

func (m tuiModel) renderRunningStatusLine() string {
	if m.cancelled {
		return m.styles.warnIcon.Render("canceling axo...")
	}
	parts := []string{m.styles.runName.Render(m.cfg.Name)}
	if m.cfg.Model != "" {
		parts = append(parts, m.styles.active.Render(m.cfg.Model))
	}
	if m.status != "" {
		parts = append(parts, m.status)
	}
	parts = append(parts, m.styles.subtle.Render(formatElapsed(time.Since(m.runStart))))
	return strings.Join(parts, m.styles.subtle.Render(" · ")) + m.styles.subtle.Render("  (ctrl+c cancel)")
}

func (m tuiModel) completionLine() string {
	elapsed := formatElapsed(time.Since(m.runStart))
	switch {
	case m.cancelled:
		return m.styles.warnIcon.Render("×") + " axo canceled after " + elapsed
	case m.runErr != nil:
		return m.styles.errIcon.Render("✗") + " axo finished with an error after " + elapsed
	default:
		var done, total int
		for _, t := range m.tasks {
			total++
			if t.Status == StatusDone {
				done++
			}
		}
		return m.styles.okIcon.Render("✓") + " axo finished in " + elapsed +
			m.styles.subtle.Render(fmt.Sprintf(" (%d/%d tasks done)", done, total))
	}
}

// println prints a line above the live view, wrapped to the terminal width so
// Bubble Tea's row accounting stays correct.
func (m tuiModel) println(s string) tea.Cmd {
	if m.width > 1 {
		s = ansi.Wrap(s, m.width-1, " ")
	}
	return tea.Println(s)
}

func (m tuiModel) renderError(msg string, indent int) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	wrapAt := max(20, width-indent-1)
	pad := strings.Repeat(" ", indent)
	body := m.styles.errBox.Width(wrapAt).Render("Error: " + msg)
	body = strings.ReplaceAll(body, "\n", "\n"+pad)
	if hint := axErrorHint(msg, m.cfg.Name); hint != "" {
		hintText := m.styles.subtle.Width(wrapAt).Render(hint)
		body += "\n" + pad + strings.ReplaceAll(hintText, "\n", "\n"+pad)
	}
	return body
}

func runTUI(cfg Config, env *Env, stdin io.Reader, stdout, stderr io.Writer, stdoutTTY, colorEnabled bool, starter orchStarter) int {
	styles := newTUIStyles(stderr, cfg.Color, colorEnabled)
	model := newTUIModel(cfg, env, styles, starter)
	defer model.cancel()

	p := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stderr))
	if cfg.Goal != "" {
		// Start right away; WindowSizeMsg may never arrive on some terminals.
		go p.Send(startMsg{})
	}
	finalModel, err := p.Run()
	final, ok := finalModel.(tuiModel)

	// Stop the orchestrator and wait (bounded) for it to wind down before
	// cleaning up, so no worker can create AX tasks after cleanup.
	if ok {
		final.cancel()
		drainEvents(final.events, 15*time.Second)
	}
	if (!ok || !final.cleanupRan) && !cfg.Keep && env.created() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		env.CleanupWithResult(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", progName, err)
		return 1
	}
	if !ok {
		return 0
	}
	// With stdout redirected, write the final answer there cleanly; the TUI
	// rendered progress on stderr.
	if !stdoutTTY && final.output != "" {
		out := final.output
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		_, _ = io.WriteString(stdout, out)
	}
	if final.cancelled || final.runErr != nil {
		return 1
	}
	return 0
}

func drainEvents(ch <-chan Event, timeout time.Duration) {
	if ch == nil {
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline.C:
			return
		}
	}
}
