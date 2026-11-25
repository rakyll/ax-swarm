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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CommandRunner executes external commands without a shell.
type CommandRunner interface {
	Run(ctx context.Context, bin string, args []string, stdin []byte) (stdout, stderr string, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, bin string, args []string, stdin []byte) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// AX runs the ax CLI against one atespace.
type AX struct {
	Bin          string
	Atespace     string
	KubeContext  string
	Server       string
	Runner       CommandRunner
	PollInterval time.Duration
}

func newAX(bin, atespace, kubeContext, server string) *AX {
	return &AX{
		Bin:          bin,
		Atespace:     atespace,
		KubeContext:  kubeContext,
		Server:       server,
		Runner:       execRunner{},
		PollInterval: 500 * time.Millisecond,
	}
}

// BaseArgs returns the common ax CLI flags for this swarm's scope.
func (a *AX) BaseArgs() []string {
	args := []string{"-a", a.Atespace}
	if a.KubeContext != "" {
		args = append(args, "--context", a.KubeContext)
	}
	if a.Server != "" {
		args = append(args, "--server", a.Server)
	}
	return args
}

// BaseCommandLine renders a shell-safe base command line for instructions given to agy /swarm.
func (a *AX) BaseCommandLine() string {
	parts := make([]string, 0, 1+len(a.BaseArgs()))
	parts = append(parts, shellQuote(a.Bin))
	for _, arg := range a.BaseArgs() {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func (a *AX) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	fullArgs := append(a.BaseArgs(), args...)
	stdout, stderr, err := a.Runner.Run(ctx, a.Bin, fullArgs, stdin)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			detail = err.Error()
		}
		return stdout, fmt.Errorf("ax %s: %s", strings.Join(args, " "), detail)
	}
	return stdout, nil
}

// Apply submits a JSON-encoded AX resource manifest via `ax apply -f -`.
func (a *AX) Apply(ctx context.Context, manifest any) error {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	_, err = a.run(ctx, payload, "apply", "-f", "-")
	return err
}

// Column returns column col of resource name in `ax get <kind>s`, or "" if absent.
func (a *AX) Column(ctx context.Context, kind, name, col string) (string, error) {
	out, err := a.run(ctx, nil, "get", kind+"s")
	if err != nil {
		return "", err
	}
	return parseTableColumn(out, name, col), nil
}

// ListTasksWithPrefix returns all task names in the atespace starting with prefix.
func (a *AX) ListTasksWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	out, err := a.run(ctx, nil, "get", "tasks")
	if err != nil {
		return nil, err
	}
	return parseTableNamesWithPrefix(out, prefix), nil
}

// AXTaskInfo represents one AX Task row from `ax get tasks`.
type AXTaskInfo struct {
	Name  string
	Step  string
	State string
	Age   string
}

// ListTasksInfoWithPrefix returns detailed task rows whose names start with prefix.
func (a *AX) ListTasksInfoWithPrefix(ctx context.Context, prefix string) ([]AXTaskInfo, error) {
	out, err := a.run(ctx, nil, "get", "tasks")
	if err != nil {
		return nil, err
	}
	return parseTableTasksInfoWithPrefix(out, prefix), nil
}

// ListTaskCountsWithPrefix returns the number of Running tasks and total tasks
// whose names start with prefix.
func (a *AX) ListTaskCountsWithPrefix(ctx context.Context, prefix string) (running, total int, err error) {
	out, err := a.run(ctx, nil, "get", "tasks")
	if err != nil {
		return 0, 0, err
	}
	running, total = parseTableTaskCountsWithPrefix(out, prefix)
	return running, total, nil
}

// Delete deletes a resource by kind and name.
func (a *AX) Delete(ctx context.Context, kind, name string) error {
	_, err := a.run(ctx, nil, "delete", kind, name)
	return err
}

func parseTableColumn(out, name, col string) string {
	lines := nonEmptyLines(out)
	if len(lines) < 2 {
		return ""
	}
	header := strings.Fields(lines[0])
	colIdx := -1
	for i, h := range header {
		if h == col {
			colIdx = i
			break
		}
	}
	if colIdx < 0 {
		return ""
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) > colIdx && fields[0] == name {
			return fields[colIdx]
		}
	}
	return ""
}

func parseTableNamesWithPrefix(out, prefix string) []string {
	lines := nonEmptyLines(out)
	if len(lines) < 2 {
		return nil
	}
	header := strings.Fields(lines[0])
	if len(header) == 0 || header[0] != "NAME" {
		return nil
	}
	var names []string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], prefix) {
			names = append(names, fields[0])
		}
	}
	return names
}

func parseTableTasksInfoWithPrefix(out, prefix string) []AXTaskInfo {
	lines := nonEmptyLines(out)
	if len(lines) < 2 {
		return nil
	}
	header := strings.Fields(lines[0])
	if len(header) == 0 || header[0] != "NAME" {
		return nil
	}
	stateIdx, ageIdx := -1, -1
	for i, h := range header {
		switch h {
		case "STATE":
			stateIdx = i
		case "AGE":
			ageIdx = i
		}
	}
	var tasks []AXTaskInfo
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		info := AXTaskInfo{
			Name: fields[0],
			Step: strings.TrimPrefix(fields[0], prefix),
		}
		if stateIdx >= 0 && len(fields) > stateIdx {
			info.State = fields[stateIdx]
		}
		if ageIdx >= 0 && len(fields) > ageIdx {
			info.Age = fields[ageIdx]
		}
		tasks = append(tasks, info)
	}
	return tasks
}

func parseTableTaskCountsWithPrefix(out, prefix string) (running, total int) {
	tasks := parseTableTasksInfoWithPrefix(out, prefix)
	for _, t := range tasks {
		total++
		if t.State == "Running" {
			running++
		}
	}
	return running, total
}

func nonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// GatewayManifest returns the ax.io/v1alpha1 Gateway manifest for the swarm.
func GatewayManifest(name string, extraHosts []string) map[string]any {
	egress := []map[string]any{
		{"https": map[string]any{"hostnames": []string{
			"pypi.org",
			"files.pythonhosted.org",
			"registry.npmjs.org",
			"proxy.golang.org",
			"sum.golang.org",
			"storage.googleapis.com",
		}}},
		{"https": map[string]any{"hostnames": []string{
			"github.com",
			"*.github.com",
			"*.githubusercontent.com",
		}}},
	}
	if len(extraHosts) > 0 {
		egress = append(egress, map[string]any{
			"https": map[string]any{"hostnames": extraHosts},
		})
	}
	return map[string]any{
		"apiVersion": "ax.io/v1alpha1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"egress": egress},
	}
}

// WorkspaceManifest returns the ax.io/v1alpha1 Workspace manifest for the swarm.
func WorkspaceManifest(name, image string, repos, setupCmds []string) map[string]any {
	spec := map[string]any{
		"profile": map[string]any{"image": image},
	}
	if len(repos) > 0 || len(setupCmds) > 0 {
		setup := map[string]any{
			"gateway": map[string]any{"name": name},
		}
		if len(repos) > 0 {
			gitSpecs := make([]map[string]any, 0, len(repos))
			for _, r := range repos {
				gitSpecs = append(gitSpecs, map[string]any{"repo": r, "depth": 1})
			}
			setup["git"] = gitSpecs
		}
		if len(setupCmds) > 0 {
			setup["commands"] = setupCmds
		}
		spec["setup"] = setup
	}
	return map[string]any{
		"apiVersion": "ax.io/v1alpha1",
		"kind":       "Workspace",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}
}

// SwarmEnv tracks the AX resources created for one swarm run.
type SwarmEnv struct {
	AX               *AX
	Name             string
	GatewayCreated   bool
	WorkspaceCreated bool
}

// CreateGateway creates the single shared Gateway for this swarm if it has not
// already been created.
func (e *SwarmEnv) CreateGateway(ctx context.Context, allowHosts []string) error {
	if e.GatewayCreated {
		return nil
	}
	if err := e.AX.Apply(ctx, GatewayManifest(e.Name, allowHosts)); err != nil {
		return err
	}
	e.GatewayCreated = true
	return nil
}

// CreateWorkspace creates the single shared Workspace for this swarm if it has
// not already been created.
func (e *SwarmEnv) CreateWorkspace(ctx context.Context, image string, repos, setupCmds []string) error {
	if e.WorkspaceCreated {
		return nil
	}
	if err := e.AX.Apply(ctx, WorkspaceManifest(e.Name, image, repos, setupCmds)); err != nil {
		return err
	}
	e.WorkspaceCreated = true
	return nil
}

// Setup creates the single Gateway and single Workspace for the swarm and waits
// until the Workspace is Ready.
func (e *SwarmEnv) Setup(ctx context.Context, cfg Config, ui *UI) error {
	if err := e.CreateGateway(ctx, cfg.AllowHosts); err != nil {
		return err
	}
	ui.Log(fmt.Sprintf("%s created Gateway %s", ui.Paint("✓", ansiGreen), e.Name))

	if err := e.CreateWorkspace(ctx, cfg.Image, cfg.Repos, cfg.SetupCmds); err != nil {
		return err
	}
	ui.Log(fmt.Sprintf("%s created Workspace %s; waiting for its setup", ui.Paint("✓", ansiGreen), e.Name))
	ui.Status(fmt.Sprintf("setting up Workspace %s", e.Name), true)

	pollInterval := e.AX.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}

	deadline := time.Now().Add(cfg.SetupTimeout)
	for {
		phase, err := e.AX.Column(ctx, "workspace", e.Name, "PHASE")
		if err != nil {
			return err
		}
		if phase == "Ready" {
			ui.ClearStatus()
			ui.Log(fmt.Sprintf("%s Workspace %s is Ready", ui.Paint("✓", ansiGreen), e.Name))
			return nil
		}
		label := phase
		if label == "" {
			label = "pending"
		}
		ui.Status(fmt.Sprintf("setting up Workspace %s: %s", e.Name, label), false)
		if phase == "Failed" {
			return fmt.Errorf("workspace %s failed setup; see `%s describe workspace %s`", e.Name, e.AX.BaseCommandLine(), e.Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workspace %s is %s after %s", e.Name, label, cfg.SetupTimeout)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// CleanupResult summarizes what CleanupWithResult deleted or failed to delete.
type CleanupResult struct {
	TasksDeleted     []string
	WorkspaceDeleted bool
	GatewayDeleted   bool
	Err              error
}

// Summary returns a concise human-readable description of deleted resources.
func (r CleanupResult) Summary(swarmName string) string {
	var parts []string
	switch len(r.TasksDeleted) {
	case 0:
	case 1:
		parts = append(parts, "1 task")
	default:
		parts = append(parts, fmt.Sprintf("%d tasks", len(r.TasksDeleted)))
	}
	if r.WorkspaceDeleted {
		parts = append(parts, "Workspace")
	}
	if r.GatewayDeleted {
		parts = append(parts, "Gateway")
	}
	if len(parts) == 0 {
		return fmt.Sprintf("no resources to delete for %s*", swarmName)
	}
	return fmt.Sprintf("deleted %s (%s*)", joinHuman(parts), swarmName)
}

func joinHuman(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// CleanupWithResult deletes all AX Tasks created under <name>-*, then the Workspace
// and Gateway, returning a structured summary.
func (e *SwarmEnv) CleanupWithResult(ctx context.Context) CleanupResult {
	var res CleanupResult
	if !e.GatewayCreated && !e.WorkspaceCreated {
		return res
	}

	var errs []error
	tasks, err := e.AX.ListTasksWithPrefix(ctx, e.Name+"-")
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, task := range tasks {
			if delErr := e.AX.Delete(ctx, "task", task); delErr != nil {
				errs = append(errs, delErr)
			} else {
				res.TasksDeleted = append(res.TasksDeleted, task)
			}
		}
	}

	if e.WorkspaceCreated {
		if delErr := e.AX.Delete(ctx, "workspace", e.Name); delErr != nil {
			errs = append(errs, delErr)
		} else {
			res.WorkspaceDeleted = true
		}
	}
	if e.GatewayCreated {
		if delErr := e.AX.Delete(ctx, "gateway", e.Name); delErr != nil {
			errs = append(errs, delErr)
		} else {
			res.GatewayDeleted = true
		}
	}

	if len(errs) > 0 {
		res.Err = errors.Join(errs...)
	}
	return res
}

// Cleanup deletes all AX Tasks created under <name>-*, then the Workspace and Gateway,
// unless keep is true.
func (e *SwarmEnv) Cleanup(ctx context.Context, keep bool, ui *UI) {
	if keep {
		ui.Log(fmt.Sprintf("keeping everything named %s*", e.Name))
		return
	}
	if !e.GatewayCreated && !e.WorkspaceCreated {
		return
	}

	ui.Status("cleaning up AX resources", true)
	defer ui.ClearStatus()

	res := e.CleanupWithResult(ctx)
	if res.Err != nil {
		ui.Log(ui.Paint(fmt.Sprintf("cleanup warnings for %s*: %v", e.Name, res.Err), ansiYellow))
		return
	}
	ui.Log(res.Summary(e.Name))
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		isSafe := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == '/' || c == ':' || c == '@' || c == '=' || c == '+'
		if !isSafe {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
