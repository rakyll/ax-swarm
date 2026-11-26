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

// Command axo is a custom AX task orchestrator built on plain
// `agy -p` calls (it never uses agy's /swarm mode). It provisions exactly one
// AX Gateway and one AX Workspace, asks agy to plan the goal as a dependency
// graph of tasks, runs every task inside its own AX Task (all sharing the one
// Workspace and Gateway) with a dedicated agy worker, reviews the results and
// schedules follow-up tasks until the goal is complete, and finally asks agy
// to synthesize the answer.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"syscall"
	"time"
)

const progName = "axo"

var (
	runNameRe     = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	atespaceRe    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	imageDigestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	hostnameRe    = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)
	agyRuleRe     = regexp.MustCompile(`^(command|mcp|read_file|write_file|read_url|execute_url|download_url)\([^\x00-\x1f]+\)$`)
)

// Config holds all validated runtime configuration for one orchestrator run.
type Config struct {
	Goal                       string
	AllowHosts                 []string
	Image                      string
	Atespace                   string
	KubeContext                string
	Server                     string
	Name                       string
	Parallel                   int
	MaxRounds                  int
	MaxRetries                 int
	MaxTasks                   int
	SetupTimeout               time.Duration
	WorkerTimeout              time.Duration
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
	AllowRules                 []string
	// Workspace is the --workspace value: an inline JSON AX Workspace spec.
	Workspace string
	// WorkspaceConfig is the --workspace-config value: a path to a JSON file
	// holding an AX Workspace spec.
	WorkspaceConfig string
	// WorkspaceSpec is the parsed spec from Workspace or WorkspaceConfig.
	WorkspaceSpec map[string]any
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// durationFlag parses either a Go duration (e.g. "30m") or a positive integer
// number of seconds (e.g. "1800").
type durationFlag time.Duration

func (t *durationFlag) String() string { return time.Duration(*t).String() }

func (t *durationFlag) Set(v string) error {
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return fmt.Errorf("must be positive, got %d", secs)
		}
		*t = durationFlag(time.Duration(secs) * time.Second)
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	if d <= 0 {
		return fmt.Errorf("must be positive, got %s", d)
	}
	*t = durationFlag(d)
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
	data, err := os.ReadFile(filepath.Join(axRoot, "bin", "ax-runner.digest"))
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

// agyInstallCommand is the official Antigravity CLI installer for macOS/Linux.
const agyInstallCommand = "curl -fsSL https://antigravity.google/cli/install.sh | bash"

// defaultModel is the agy model used when --model is not set.
const defaultModel = "hydrogen-l-tf-medium" // Gemini 4 Hydrogen (Medium)

// resolveAgy finds the agy binary, also checking ~/.local/bin/agy where the
// installer puts it.
func resolveAgy(bin string) (string, error) {
	path, err := exec.LookPath(bin)
	if err == nil {
		return path, nil
	}
	if bin == "agy" {
		if home, herr := os.UserHomeDir(); herr == nil {
			if p, lerr := exec.LookPath(filepath.Join(home, ".local", "bin", "agy")); lerr == nil {
				return p, nil
			}
		}
	}
	return "", err
}

func agyMissingMessage(bin string) string {
	where := "the Antigravity CLI (agy) is not installed or not on your PATH."
	if bin != "agy" {
		where = fmt.Sprintf("agy was not found at %q (set by --agy-bin or AGY_BIN).", bin)
	}
	return where + "\n\nInstall it with:\n\n  " + agyInstallCommand +
		"\n\nthen make sure ~/.local/bin is on your PATH (or pass --agy-bin <path>).\n" +
		"Docs: https://antigravity.google/docs/getting-started"
}

// resourcePrefix starts the name of every AX resource axo creates: the
// Gateway, the Workspace, and all Tasks.
const resourcePrefix = "axo-"

// withResourcePrefix prefixes a run name with "axo-" unless it already has it.
func withResourcePrefix(name string) string {
	if strings.HasPrefix(name, resourcePrefix) {
		return name
	}
	return resourcePrefix + name
}

func randomRunName() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func parseArgs(args []string, axRoot string, stderr io.Writer) (Config, error) {
	var cfg Config
	var allowHosts, allowRules stringSliceFlag
	setupTimeout := durationFlag(30 * time.Minute)
	workerTimeout := durationFlag(20 * time.Minute)

	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: %s [flags] [goal...]

Sets up one AX Workspace and one AX Gateway, then orchestrates the goal toward
completion with plain agy calls (no /swarm): agy plans a task graph, each task
runs in its own AX Task (sharing the single Workspace and Gateway) driven by an
agy worker, agy reviews the results and schedules follow-ups, and finally agy
synthesizes the answer.

Flags:
`, progName)
		fs.PrintDefaults()
		fmt.Fprint(stderr, `
Environment:
  AX_ROOT        AX checkout used for default ax binary and runner digest (default: ~/ax)
  AX_BIN         ax CLI binary to use (default: $AX_ROOT/bin/ax, else ax on PATH)
  AX_TASK_IMAGE  Digest-pinned runner image (default: localhost:5001/ax-runner@<digest>)
  AX_SERVER      Optional AX API server address
  AGY_BIN        agy CLI binary to use (default: agy on PATH)
`)
	}

	fs.Var(&allowHosts, "allow-host", "extra HTTPS hostname or *.domain allowed through the AX Gateway (repeatable)")
	fs.StringVar(&cfg.Workspace, "workspace", "", "inline AX Workspace spec as JSON, e.g. '{\"setup\":{\"git\":[{\"repo\":\"https://github.com/chalk/chalk\"}]}}'")
	fs.StringVar(&cfg.WorkspaceConfig, "workspace-config", "", "path to a JSON file with the AX Workspace spec")
	fs.StringVar(&cfg.Atespace, "atespace", "default", "AX atespace name")
	fs.StringVar(&cfg.KubeContext, "context", "", "kubeconfig context (default: active context)")
	fs.StringVar(&cfg.Server, "server", strings.TrimSpace(os.Getenv("AX_SERVER")), "AX control plane (ax-server) address (default: $AX_SERVER, else a port-forward in the kubeconfig context)")
	fs.StringVar(&cfg.Name, "name", "", "run name; AX resources are named axo-<name>, axo-<name>-<task> (default: random)")
	fs.IntVar(&cfg.Parallel, "parallel", 4, "max concurrent AX Tasks (and agy workers)")
	fs.IntVar(&cfg.MaxRounds, "max-rounds", 3, "max plan/execute/review rounds before synthesizing the answer")
	fs.IntVar(&cfg.MaxRetries, "max-retries", 1, "retries for a task whose worker fails (0-5)")
	fs.IntVar(&cfg.MaxTasks, "max-tasks", 16, "max total tasks across all rounds")
	fs.Var(&setupTimeout, "task-timeout", "workspace setup timeout as duration or seconds")
	fs.Var(&workerTimeout, "worker-timeout", "time limit for each agy worker as duration or seconds")
	fs.BoolVar(&cfg.Keep, "keep", false, "keep AX Tasks, Workspace, and Gateway on exit")
	fs.StringVar(&cfg.Color, "color", "auto", "color logs: auto, always, never")
	fs.StringVar(&cfg.AXBin, "ax-bin", defaultAXBin(axRoot), "ax CLI binary agy workers use for ax ssh into their AX Task (lifecycle uses the AX control plane API)")
	fs.StringVar(&cfg.AgyBin, "agy-bin", defaultAgyBin(), "path to agy CLI binary")
	fs.StringVar(&cfg.Model, "model", defaultModel, "model passed to agy --model (empty uses agy's default)")
	fs.StringVar(&cfg.Effort, "effort", "", "reasoning effort passed to agy --effort (low|medium|high|xhigh|max)")
	fs.StringVar(&cfg.Mode, "mode", "", "agent mode passed to agy --mode (accept-edits|auto|plan)")
	fs.StringVar(&cfg.Project, "project", "", "project ID or name passed to agy --project")
	fs.BoolVar(&cfg.DangerouslySkipPermissions, "dangerously-skip-permissions", false, "pass --dangerously-skip-permissions to agy workers")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print the Gateway/Workspace manifests, agy command, and planner prompt without creating AX resources or launching agy")
	fs.Var(&allowRules, "allow", "extra agy permission rule allowed for this run only, e.g. 'mcp(server/*)' (repeatable)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.Image = defaultImage(axRoot)
	cfg.AllowHosts = []string(allowHosts)
	cfg.AllowRules = []string(allowRules)
	cfg.SetupTimeout = time.Duration(setupTimeout)
	cfg.WorkerTimeout = time.Duration(workerTimeout)
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
		name, err := randomRunName()
		if err != nil {
			return err
		}
		cfg.Name = name
	}
	cfg.Name = withResourcePrefix(cfg.Name)
	if !runNameRe.MatchString(cfg.Name) || len(cfg.Name)+1+maxAXStepLen > 63 {
		return fmt.Errorf("--name %q must be lowercase letters, digits, and hyphens, at most %d characters (%d including the %q prefix)",
			strings.TrimPrefix(cfg.Name, resourcePrefix), 63-1-maxAXStepLen-len(resourcePrefix), 63-1-maxAXStepLen, resourcePrefix)
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
	if cfg.Workspace != "" && cfg.WorkspaceConfig != "" {
		return fmt.Errorf("use either --workspace or --workspace-config, not both")
	}
	if cfg.Workspace != "" || cfg.WorkspaceConfig != "" {
		flagName, data := "--workspace", []byte(cfg.Workspace)
		if cfg.WorkspaceConfig != "" {
			flagName = "--workspace-config"
			var err error
			if data, err = readWorkspaceConfigFile(cfg.WorkspaceConfig); err != nil {
				return fmt.Errorf("%s: %w", flagName, err)
			}
		}
		spec, err := parseWorkspaceSpec(data)
		if err != nil {
			return fmt.Errorf("%s: %w", flagName, err)
		}
		cfg.WorkspaceSpec = spec
		if profile, ok := spec["profile"].(map[string]any); ok {
			if img, ok := profile["image"].(string); ok && img != "" {
				cfg.Image = img
			}
		}
		for _, repo := range workspaceRepos(map[string]any{"spec": spec}) {
			if err := validateRepo(repo); err != nil {
				return fmt.Errorf("%s: %w", flagName, err)
			}
		}
	}
	if cfg.Image == "" || hasControlChars(cfg.Image) || strings.ContainsAny(cfg.Image, " \t") || !imageDigestRe.MatchString(cfg.Image) {
		return fmt.Errorf("set profile.image in the Workspace spec or AX_TASK_IMAGE to a digest-pinned runner image (...@sha256:<64 hex>), or run make build-local-cluster in the AX checkout at %s (set AX_ROOT if it is elsewhere)", axRoot)
	}
	for _, host := range cfg.AllowHosts {
		if !hostnameRe.MatchString(host) {
			return fmt.Errorf("--allow-host %q is not a valid hostname pattern", host)
		}
	}
	if _, err := WorkspaceProto(cfg.Name, cfg.Image, cfg.WorkspaceSpec); err != nil {
		return err
	}
	if _, err := GatewayProto(cfg.Name, cfg.AllowHosts); err != nil {
		return err
	}
	for _, rule := range cfg.AllowRules {
		if !agyRuleRe.MatchString(rule) {
			return fmt.Errorf("--allow %q is not an agy permission rule; use action(target) with action one of command, mcp, read_file, write_file, read_url, execute_url, download_url (e.g. 'mcp(server/*)')", rule)
		}
	}
	if cfg.Parallel < 1 {
		return fmt.Errorf("--parallel must be at least 1")
	}
	if cfg.MaxRounds < 1 {
		return fmt.Errorf("--max-rounds must be at least 1")
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > 5 {
		return fmt.Errorf("--max-retries must be between 0 and 5")
	}
	if cfg.MaxTasks < 1 {
		return fmt.Errorf("--max-tasks must be at least 1")
	}
	if cfg.SetupTimeout <= 0 {
		return fmt.Errorf("--task-timeout must be positive")
	}
	if cfg.WorkerTimeout <= 0 {
		return fmt.Errorf("--worker-timeout must be positive")
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

func validateRepo(repo string) error {
	if repo == "" || hasControlChars(repo) || strings.ContainsAny(repo, " \t") {
		return fmt.Errorf("repo %q is invalid", repo)
	}
	if !strings.HasPrefix(repo, "https://") &&
		!strings.HasPrefix(repo, "http://") &&
		!strings.HasPrefix(repo, "ssh://") &&
		!strings.HasPrefix(repo, "git@") {
		return fmt.Errorf("repo %q must use https://, http://, ssh://, or git@ scheme", repo)
	}
	return nil
}

// maxWorkspaceConfigBytes bounds the size of a Workspace spec.
const maxWorkspaceConfigBytes = 1 << 20

func readWorkspaceConfigFile(path string) ([]byte, error) {
	f, err := os.Open(expandHome(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWorkspaceConfigBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxWorkspaceConfigBytes)
	}
	return data, nil
}

// parseWorkspaceSpec parses an AX Workspace spec from JSON. Either the bare
// spec or a full Workspace manifest is accepted; for a manifest only the spec
// is used, since the orchestrator owns the name.
func parseWorkspaceSpec(data []byte) (map[string]any, error) {
	if len(data) > maxWorkspaceConfigBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxWorkspaceConfigBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON: unexpected data after the top-level object")
	}
	if spec == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	if kind, ok := spec["kind"]; ok {
		if kind != "Workspace" {
			return nil, fmt.Errorf("kind is %v, want Workspace", kind)
		}
		inner, ok := spec["spec"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("workspace manifest has no spec object")
		}
		spec = inner
	}
	for _, key := range []string{"profile", "setup"} {
		if v, ok := spec[key]; ok {
			if _, isObj := v.(map[string]any); !isObj {
				return nil, fmt.Errorf("%s must be an object", key)
			}
		}
	}
	return spec, nil
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
		fmt.Fprintln(stderr, ui.Paint(progName+": a goal is required when stdin is not a terminal", ansiRed))
		return 2
	}
	if err := validateConfig(&cfg, axRoot); err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(progName+": "+err.Error(), ansiRed))
		return 1
	}

	if cfg.DryRun {
		return runDryRun(cfg, ui, stdout, stderr)
	}

	// agy workers still run commands inside their AX Task with `ax ssh`: the
	// control plane API has no exec RPC.
	resolvedAX, err := exec.LookPath(cfg.AXBin)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(fmt.Sprintf("%s: ax binary %q (used by workers for `ax ssh`) not found: %v", progName, cfg.AXBin, err), ansiRed))
		return 1
	}
	cfg.AXBin = resolvedAX

	resolvedAgy, err := resolveAgy(cfg.AgyBin)
	if err != nil {
		fmt.Fprintln(stderr, ui.Paint(progName+": "+agyMissingMessage(cfg.AgyBin), ansiRed))
		return 1
	}
	cfg.AgyBin = resolvedAgy

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 60*time.Second)
	ax, conn, err := dialAX(dialCtx, cfg)
	cancelDial()
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(progName+": "+err.Error(), ansiRed))
		return 1
	}
	defer conn.Close()
	env := &Env{AX: ax, Name: cfg.Name}

	if stdinTTY && stderrTTY {
		return runTUI(cfg, env, stdin, stdout, stderr, isTerminal(stdout), color, nil)
	}
	return runHeadless(cfg, env, stdin, stdout, stderr, color, nil)
}

func runDryRun(cfg Config, ui *UI, stdout, stderr io.Writer) int {
	if cfg.Goal == "" {
		prompt := fmt.Sprintf("%s %s ", ui.Paint("?", ansiBold, ansiCyan), ui.Paint("What should axo do?", ansiBold))
		answer, ok, err := ui.Ask(prompt)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", ui.Paint(progName+": "+err.Error(), ansiRed))
			return 1
		}
		if !ok || answer == "" {
			fmt.Fprintln(stderr, ui.Paint(progName+": no goal", ansiRed))
			return 1
		}
		cfg.Goal = answer
	}
	ax := newAX(nil, cfg)
	fmt.Fprintf(stderr, "dry-run: would create exactly one Gateway %s and one Workspace %s via the AX control plane API (atespace=%s, image=%s)\n",
		cfg.Name, cfg.Name, cfg.Atespace, cfg.Image)
	if gwJSON, err := json.MarshalIndent(GatewayManifest(cfg.Name, cfg.AllowHosts), "", "  "); err == nil {
		fmt.Fprintf(stderr, "dry-run: Gateway manifest:\n%s\n", gwJSON)
	}
	if wsJSON, err := json.MarshalIndent(WorkspaceManifest(cfg.Name, cfg.Image, cfg.WorkspaceSpec), "", "  "); err == nil {
		fmt.Fprintf(stderr, "dry-run: Workspace manifest:\n%s\n", wsJSON)
	}
	prompt := BuildPlannerPrompt(cfg)
	args := BuildAgyArgs(cfg, prompt, agyCall{Schema: plannerSchema})
	fmt.Fprintf(stderr, "dry-run: planner would run: %s", shellQuote(cfg.AgyBin))
	for _, a := range args[:len(args)-1] {
		fmt.Fprintf(stderr, " %s", shellQuote(a))
	}
	fmt.Fprintf(stderr, " <prompt>\n")
	fmt.Fprintf(stderr, "dry-run: workers run commands in their AX Task with: %s ssh <task> -- ...\n", ax.ExecCommandLine())
	fmt.Fprint(stdout, prompt)
	return 0
}

func runHeadless(cfg Config, env *Env, stdin io.Reader, stdout, stderr io.Writer, color bool, starter orchStarter) int {
	if starter == nil {
		starter = defaultOrchStarter(env)
	}
	ui := newUI(stderr, stdin, color, false)
	if cfg.Goal == "" {
		prompt := fmt.Sprintf("%s %s ", ui.Paint("?", ansiBold, ansiCyan), ui.Paint("What should axo do?", ansiBold))
		answer, ok, err := ui.Ask(prompt)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", ui.Paint(progName+": "+err.Error(), ansiRed))
			return 1
		}
		if !ok || answer == "" {
			fmt.Fprintln(stderr, ui.Paint(progName+": no goal", ansiRed))
			return 1
		}
		cfg.Goal = answer
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		env.Cleanup(ctx, cfg.Keep, ui)
	}()

	ui.Log(fmt.Sprintf("%s: setting up one AX Gateway and one AX Workspace in atespace %s",
		ui.Paint("axo "+cfg.Name, ansiBold, ansiCyan), ui.Paint(cfg.Atespace, ansiBold)))

	runCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	events := starter(runCtx, cfg)
	var runErr error
	var streamed strings.Builder
	for ev := range events {
		switch ev.Kind {
		case EvStep, EvLog:
			ui.Log(headlessLine(ui, ev))
		case EvText:
			streamed.WriteString(ev.Text)
			_, _ = io.WriteString(stdout, ev.Text)
		case EvDone:
			runErr = ev.Err
		}
	}
	if s := streamed.String(); s != "" && !strings.HasSuffix(s, "\n") {
		_, _ = io.WriteString(stdout, "\n")
	}
	if runCtx.Err() != nil {
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "%s\n", ui.Paint(progName+": "+runErr.Error(), ansiRed))
		if hint := axErrorHint(runErr.Error(), cfg.Name); hint != "" {
			fmt.Fprintln(stderr, ui.Paint(hint, ansiDim))
		}
		return 1
	}
	return 0
}

func headlessLine(ui *UI, ev Event) string {
	icon := ev.Icon
	if icon == "" {
		icon = defaultIcon(ev.Level)
	}
	var code string
	switch ev.Level {
	case LevelOK:
		code = ansiGreen
	case LevelWarn:
		code = ansiYellow
	case LevelErr:
		code = ansiRed
	case LevelAction:
		code = ansiCyan
	default:
		code = ansiDim
	}
	return ui.Paint(icon, ansiBold, code) + " " + ev.Text
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
