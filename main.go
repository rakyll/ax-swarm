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

// Command ax-swarm sets up a single AX Workspace and Gateway per swarm and runs
// `agy --output-format stream-json -p "/swarm ..."` in the background, showing
// only outputs from agy while the swarm controls all AX Task execution via the
// ax CLI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	swarmNameRe   = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	atespaceRe    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	imageDigestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	hostnameRe    = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)
)

// Config holds all validated runtime configuration for an ax-swarm invocation.
type Config struct {
	Goal                       string
	Repos                      []string
	SetupCmds                  []string
	AllowHosts                 []string
	Image                      string
	Atespace                   string
	KubeContext                string
	Server                     string
	Name                       string
	Parallel                   int
	SetupTimeout               time.Duration
	Keep                       bool
	Color                      string
	AXBin                      string
	AgyBin                     string
	Model                      string
	Effort                     string
	Mode                       string
	Project                    string
	DangerouslySkipPermissions bool
	DryRun                     bool
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// timeoutFlag parses either a Go duration (e.g. "30m", "600s") or a positive
// integer number of seconds (e.g. "1800") for compatibility with ax-swarm.py.
type timeoutFlag time.Duration

func (t *timeoutFlag) String() string {
	return time.Duration(*t).String()
}

func (t *timeoutFlag) Set(v string) error {
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return fmt.Errorf("must be positive, got %d", secs)
		}
		*t = timeoutFlag(time.Duration(secs) * time.Second)
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	if d <= 0 {
		return fmt.Errorf("must be positive, got %s", d)
	}
	*t = timeoutFlag(d)
	return nil
}

func defaultAXRoot() string {
	if env := os.Getenv("AX_ROOT"); env != "" {
		return expandHome(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "ax"
	}
	return filepath.Join(home, "ax")
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Clean(home)
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Clean(filepath.Join(home, p[2:]))
		}
	}
	return filepath.Clean(p)
}

func defaultImage(axRoot string) string {
	if img := strings.TrimSpace(os.Getenv("AX_TASK_IMAGE")); img != "" {
		return img
	}
	digestPath := filepath.Join(axRoot, "bin", "ax-runner.digest")
	data, err := os.ReadFile(digestPath)
	if err != nil {
		return ""
	}
	digest := strings.TrimSpace(string(data))
	if digest == "" {
		return ""
	}
	return "localhost:5001/ax-runner@" + digest
}

func defaultAXBin(axRoot string) string {
	if env := strings.TrimSpace(os.Getenv("AX_BIN")); env != "" {
		return env
	}
	candidate := filepath.Join(axRoot, "bin", "ax")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return "ax"
}

func defaultAgyBin() string {
	if env := strings.TrimSpace(os.Getenv("AGY_BIN")); env != "" {
		return env
	}
	return "agy"
}

func randomSwarmName() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate swarm name: %w", err)
	}
	return "swarm-" + hex.EncodeToString(b[:]), nil
}

func parseArgs(args []string, axRoot string, stderr io.Writer) (Config, error) {
	var cfg Config
	var repos, setupCmds, allowHosts stringSliceFlag
	timeout := timeoutFlag(1800 * time.Second)

	fs := flag.NewFlagSet("ax-swarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: ax-swarm [flags] [goal...]

Sets up one AX Workspace and Gateway per swarm, runs agy /swarm in the
background, and streams outputs from agy while the swarm controls all AX Task
execution via the ax CLI.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Environment:
  AX_ROOT        AX checkout used for default ax binary and runner digest (default: ~/ax)
  AX_BIN         ax CLI binary to use (default: $AX_ROOT/bin/ax, else ax on PATH)
  AX_TASK_IMAGE  Digest-pinned runner image (default: localhost:5001/ax-runner@<digest>)
  AX_SERVER      Optional AX API server address
  AGY_BIN        agy CLI binary to use (default: agy on PATH)
`)
	}

	fs.Var(&repos, "repo", "git repository URL the AX Workspace clones into /workspace (repeatable)")
	fs.Var(&setupCmds, "setup-cmd", "shell command run during AX Workspace setup (repeatable)")
	fs.Var(&allowHosts, "allow-host", "extra HTTPS hostname or *.domain allowed through the AX Gateway (repeatable)")
	fs.StringVar(&cfg.Image, "image", defaultImage(axRoot), "digest-pinned runner image (default $AX_TASK_IMAGE or $AX_ROOT/bin/ax-runner.digest)")
	fs.StringVar(&cfg.Atespace, "atespace", "default", "AX atespace name")
	fs.StringVar(&cfg.KubeContext, "context", "", "kubeconfig context (default: active context)")
	fs.StringVar(&cfg.Server, "server", strings.TrimSpace(os.Getenv("AX_SERVER")), "AX API server address (default: $AX_SERVER or auto-detected)")
	fs.StringVar(&cfg.Name, "name", "", "swarm name prefix for AX resources (default: swarm-<random>)")
	fs.IntVar(&cfg.Parallel, "parallel", 8, "target max concurrent AX Tasks hint for the swarm")
	fs.Var(&timeout, "task-timeout", "workspace setup timeout as duration or seconds")
	fs.BoolVar(&cfg.Keep, "keep", false, "keep AX Tasks, Workspace, and Gateway on exit")
	fs.StringVar(&cfg.Color, "color", "auto", "color logs: auto, always, never")
	fs.StringVar(&cfg.AXBin, "ax-bin", defaultAXBin(axRoot), "path to ax CLI binary")
	fs.StringVar(&cfg.AgyBin, "agy-bin", defaultAgyBin(), "path to agy CLI binary")
	fs.StringVar(&cfg.Model, "model", "", "model passed to agy --model")
	fs.StringVar(&cfg.Effort, "effort", "", "reasoning effort passed to agy --effort (low|medium|high|xhigh|max)")
	fs.StringVar(&cfg.Mode, "mode", "", "agent mode passed to agy --mode (accept-edits|auto|plan)")
	fs.StringVar(&cfg.Project, "project", "", "project ID or name passed to agy --project")
	fs.BoolVar(&cfg.DangerouslySkipPermissions, "dangerously-skip-permissions", false, "pass --dangerously-skip-permissions to agy")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print the agy command and /swarm prompt without creating AX resources or launching agy")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.Repos = []string(repos)
	cfg.SetupCmds = []string(setupCmds)
	cfg.AllowHosts = []string(allowHosts)
	cfg.SetupTimeout = time.Duration(timeout)
	if fs.NArg() > 0 {
		cfg.Goal = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	return cfg, nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateConfig(cfg *Config, axRoot string) error {
	if cfg.Name == "" {
		name, err := randomSwarmName()
		if err != nil {
			return err
		}
		cfg.Name = name
	}
	if !swarmNameRe.MatchString(cfg.Name) || len(cfg.Name)+1+24 > 63 {
		return fmt.Errorf("--name %q must be a lowercase alphanumeric/hyphen name starting with a letter and at most 38 characters", cfg.Name)
	}
	if !atespaceRe.MatchString(cfg.Atespace) {
		return fmt.Errorf("--atespace %q must be a lowercase alphanumeric/hyphen name of at most 63 characters", cfg.Atespace)
	}
	if cfg.KubeContext != "" && (hasControlChars(cfg.KubeContext) || strings.HasPrefix(cfg.KubeContext, "-")) {
		return fmt.Errorf("--context %q is invalid", cfg.KubeContext)
	}
	if cfg.Server != "" {
		if hasControlChars(cfg.Server) {
			return fmt.Errorf("--server %q contains invalid characters", cfg.Server)
		}
		u, err := url.Parse(cfg.Server)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("--server %q must be a valid http(s) URL", cfg.Server)
		}
	}
	if cfg.Image == "" || hasControlChars(cfg.Image) || strings.ContainsAny(cfg.Image, " \t") || !imageDigestRe.MatchString(cfg.Image) {
		return fmt.Errorf("set AX_TASK_IMAGE or --image to a digest-pinned runner image (...@sha256:<64 hex>), or run make build-local-cluster in the AX checkout at %s (set AX_ROOT if it is elsewhere)", axRoot)
	}
	for _, repo := range cfg.Repos {
		if repo == "" || hasControlChars(repo) || strings.ContainsAny(repo, " \t") {
			return fmt.Errorf("--repo %q is invalid", repo)
		}
		if !strings.HasPrefix(repo, "https://") &&
			!strings.HasPrefix(repo, "http://") &&
			!strings.HasPrefix(repo, "ssh://") &&
			!strings.HasPrefix(repo, "git@") {
			return fmt.Errorf("--repo %q must use https://, http://, ssh://, or git@ scheme", repo)
		}
	}
	for _, host := range cfg.AllowHosts {
		if !hostnameRe.MatchString(host) {
			return fmt.Errorf("--allow-host %q is not a valid hostname pattern", host)
		}
	}
	if cfg.Parallel < 1 {
		return fmt.Errorf("--parallel must be at least 1")
	}
	if cfg.SetupTimeout <= 0 {
		return fmt.Errorf("--task-timeout must be positive")
	}
	switch cfg.Color {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("--color must be auto, always, or never")
	}
	switch cfg.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("--effort must be one of low, medium, high, xhigh, max")
	}
	switch cfg.Mode {
	case "", "accept-edits", "auto", "plan":
	default:
		return fmt.Errorf("--mode must be one of accept-edits, auto, plan")
	}
	if cfg.Model != "" && (hasControlChars(cfg.Model) || strings.HasPrefix(cfg.Model, "-")) {
		return fmt.Errorf("--model %q is invalid", cfg.Model)
	}
	if cfg.Project != "" && (hasControlChars(cfg.Project) || strings.HasPrefix(cfg.Project, "-")) {
		return fmt.Errorf("--project %q is invalid", cfg.Project)
	}
	if cfg.AXBin == "" || hasControlChars(cfg.AXBin) {
		return fmt.Errorf("--ax-bin %q is invalid", cfg.AXBin)
	}
	if cfg.AgyBin == "" || hasControlChars(cfg.AgyBin) {
		return fmt.Errorf("--agy-bin %q is invalid", cfg.AgyBin)
	}
	return nil
}

func run(args []string, stdin *os.File, stdout, stderr *os.File) int {
	axRoot := defaultAXRoot()
	cfg, err := parseArgs(args, axRoot, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	stderrTTY := isTerminal(stderr)
	stdinTTY := isTerminal(stdin)
	color := resolveColor(cfg.Color, stderrTTY)
	ui := newUI(stderr, stdin, color, false)

	if cfg.Goal == "" && !stdinTTY {
		fmt.Fprintln(stderr, ui.Paint("ax-swarm: a goal is required when stdin is not a terminal", ansiRed))
		return 2
	}

	if err := validateConfig(&cfg, axRoot); err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+err.Error(), ansiRed))
		return 1
	}

	if cfg.DryRun {
		if cfg.Goal == "" {
			prompt := fmt.Sprintf("%s %s ", ui.Paint("?", ansiBold, ansiCyan), ui.Paint("What should the swarm do?", ansiBold))
			answer, ok, askErr := ui.Ask(prompt)
			if askErr != nil {
				fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+askErr.Error(), ansiRed))
				return 1
			}
			if !ok || answer == "" {
				fmt.Fprintln(stderr, ui.Paint("ax-swarm: no goal", ansiRed))
				return 1
			}
			cfg.Goal = answer
		}
		axClient := newAX(cfg.AXBin, cfg.Atespace, cfg.KubeContext, cfg.Server)
		swarmPrompt := BuildSwarmPrompt(cfg, axClient.BaseCommandLine())
		agyArgs := BuildAgyArgs(cfg, swarmPrompt)
		fmt.Fprintf(stderr, "dry-run: would create Gateway %s and Workspace %s (atespace=%s, image=%s)\n",
			cfg.Name, cfg.Name, cfg.Atespace, cfg.Image)
		fmt.Fprintf(stderr, "dry-run: would run: %s", shellQuote(cfg.AgyBin))
		for _, a := range agyArgs[:len(agyArgs)-1] {
			fmt.Fprintf(stderr, " %s", shellQuote(a))
		}
		fmt.Fprintf(stderr, " <prompt>\n")
		fmt.Fprint(stdout, swarmPrompt)
		return 0
	}

	resolvedAX, err := exec.LookPath(cfg.AXBin)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(fmt.Sprintf("ax-swarm: ax binary %q not found: %v", cfg.AXBin, err), ansiRed))
		return 1
	}
	cfg.AXBin = resolvedAX

	resolvedAgy, err := exec.LookPath(cfg.AgyBin)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(fmt.Sprintf("ax-swarm: agy binary %q not found: %v", cfg.AgyBin, err), ansiRed))
		return 1
	}
	cfg.AgyBin = resolvedAgy

	axClient := newAX(cfg.AXBin, cfg.Atespace, cfg.KubeContext, cfg.Server)
	env := &SwarmEnv{
		AX:   axClient,
		Name: cfg.Name,
	}

	stdoutTTY := isTerminal(stdout)
	if stdinTTY && stderrTTY {
		return runTUI(cfg, env, stdin, stdout, stderr, stdoutTTY, color)
	}

	return runHeadless(cfg, env, stdin, stdout, stderr, color)
}

func runHeadless(cfg Config, env *SwarmEnv, stdin io.Reader, stdout, stderr io.Writer, color bool) int {
	ui := newUI(stderr, stdin, color, false)
	if cfg.Goal == "" {
		prompt := fmt.Sprintf("%s %s ", ui.Paint("?", ansiBold, ansiCyan), ui.Paint("What should the swarm do?", ansiBold))
		answer, ok, askErr := ui.Ask(prompt)
		if askErr != nil {
			fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+askErr.Error(), ansiRed))
			return 1
		}
		if !ok || answer == "" {
			fmt.Fprintln(stderr, ui.Paint("ax-swarm: no goal", ansiRed))
			return 1
		}
		cfg.Goal = answer
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		env.Cleanup(cleanupCtx, cfg.Keep, ui)
	}()

	ui.Log(fmt.Sprintf("%s: setting up AX Workspace in atespace %s",
		ui.Paint("swarm "+cfg.Name, ansiBold, ansiCyan),
		ui.Paint(cfg.Atespace, ansiBold),
	))

	runCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if err := env.Setup(runCtx, cfg, ui); err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+err.Error(), ansiRed))
		return 1
	}

	swarmPrompt := BuildSwarmPrompt(cfg, env.AX.BaseCommandLine())
	agyArgs := BuildAgyArgs(cfg, swarmPrompt)

	events, err := StartAgyStream(runCtx, cfg.AgyBin, agyArgs)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+err.Error(), ansiRed))
		return 1
	}

	// Poll AX tasks in the background so headless stderr shows live AX task activity.
	pollCtx, stopPoll := context.WithCancel(runCtx)
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		prev := make(map[string]string)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				tasks, pollErr := env.AX.ListTasksInfoWithPrefix(pollCtx, cfg.Name+"-")
				if pollErr != nil {
					continue
				}
				curr := make(map[string]string, len(tasks))
				for _, t := range tasks {
					curr[t.Step] = t.State
					if old, ok := prev[t.Step]; !ok || old != t.State {
						ui.Log(fmt.Sprintf("ax task %s (%s)", t.Step, t.State))
					}
				}
				for step := range prev {
					if _, ok := curr[step]; !ok {
						ui.Log(fmt.Sprintf("ax task %s (finished)", step))
					}
				}
				prev = curr
			}
		}
	}()
	defer func() {
		stopPoll()
		pollWG.Wait()
	}()

	var streamed strings.Builder
	var finalResponse string
	var agyStderr []string
	var resultErr error
	var waitErr error
	seenSeq := make(map[int]bool)

	for ev := range events {
		switch ev.Kind {
		case AgyEventSwarmTitle:
			ui.Log("swarm: " + ev.SwarmTitle)
		case AgyEventSwarmWorker:
			if ev.WorkerState == "killed" {
				ui.Log("worker stopped: " + ev.WorkerName)
			} else {
				ui.Log("worker added: " + ev.WorkerName)
			}
		case AgyEventBoardMessages:
			for _, bm := range ev.BoardMessages {
				if bm.Seq > 0 {
					if seenSeq[bm.Seq] {
						continue
					}
					seenSeq[bm.Seq] = true
				}
				ui.Log(fmt.Sprintf("#%d [%s] %s", bm.Seq, bm.Agent, oneLinePreview(bm.Text, 120)))
			}
		case AgyEventTool:
			if ev.ToolState == "ACTIVE" && ev.ToolName == "run_command" && ev.ToolSummary != "" {
				ui.Log("▸ " + ev.ToolSummary)
			} else if ev.ToolState != "ACTIVE" && ev.ErrorText != "" {
				ui.Log(fmt.Sprintf("! %s: %s", ev.ToolName, oneLinePreview(ev.ErrorText, 120)))
			}
		case AgyEventText:
			streamed.WriteString(ev.Text)
			_, _ = io.WriteString(stdout, ev.Text)
		case AgyEventResult:
			if ev.Response != "" {
				finalResponse = ev.Response
			}
			if ev.Status == "ERROR" || ev.ErrorText != "" {
				errMsg := ev.ErrorText
				if errMsg == "" {
					errMsg = "agy returned status " + ev.Status
				}
				resultErr = errors.New(errMsg)
			}
		case AgyEventStderr:
			if len(agyStderr) < 20 {
				agyStderr = append(agyStderr, ev.ErrorText)
			}
		case AgyEventDone:
			waitErr = ev.Err
		}
	}
	stopPoll()
	pollWG.Wait()

	if finalResponse != "" && strings.TrimSpace(streamed.String()) == "" {
		if !strings.HasSuffix(finalResponse, "\n") {
			finalResponse += "\n"
		}
		_, _ = io.WriteString(stdout, finalResponse)
	} else if streamed.Len() > 0 && !strings.HasSuffix(streamed.String(), "\n") {
		_, _ = io.WriteString(stdout, "\n")
	}

	if runCtx.Err() != nil {
		return 1
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if len(agyStderr) > 0 {
				fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+agyStderr[len(agyStderr)-1], ansiRed))
			}
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+waitErr.Error(), ansiRed))
		return 1
	}
	if resultErr != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint("ax-swarm: "+resultErr.Error(), ansiRed))
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
