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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	axclient "github.com/google/ax/client"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxAXStepLen is the longest task suffix appended to the run name
// (`<name>-<step>`), keeping AX resource names within 63 characters.
const maxAXStepLen = 24

// AX drives the AX control plane (the ax-server gRPC API) for one atespace.
// axo never shells out to the ax CLI for lifecycle operations: Gateways,
// Workspaces, and Tasks are created, watched, resumed, and deleted with RPCs.
type AX struct {
	Client   v1alpha1.AXClient
	Atespace string
	// KubeContext and Server are only used to render the command line agy
	// workers use to run commands inside their AX Task (see ExecCommandLine).
	KubeContext string
	Server      string
	// Bin is the ax CLI used by agy workers for `ax ssh`. The control plane
	// has no exec RPC; running a process inside a Task goes through the
	// Task's guest daemon, which only the ax CLI can reach.
	Bin          string
	PollInterval time.Duration
	// RPCTimeout bounds short RPCs (get, list, resume, delete).
	RPCTimeout time.Duration
}

// dialAX connects to the ax-server the same way the ax CLI does: --server /
// $AX_SERVER when set, otherwise a (shared, reused) port-forward to
// svc/ax-server in the kubeconfig context.
func dialAX(ctx context.Context, cfg Config) (*AX, *grpc.ClientConn, error) {
	c, conn, err := axclient.Dial(ctx, axclient.Options{
		ServerURL: cfg.Server,
		Context:   cfg.KubeContext,
		FromEnv:   true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the AX control plane: %w", err)
	}
	return newAX(c, cfg), conn, nil
}

func newAX(c v1alpha1.AXClient, cfg Config) *AX {
	return &AX{
		Client:       c,
		Atespace:     cfg.Atespace,
		KubeContext:  cfg.KubeContext,
		Server:       cfg.Server,
		Bin:          cfg.AXBin,
		PollInterval: 500 * time.Millisecond,
		RPCTimeout:   30 * time.Second,
	}
}

// ExecCommandLine renders the shell-safe `ax` prefix agy workers use for
// `ax ssh`, scoped to this run's atespace and cluster.
func (a *AX) ExecCommandLine() string {
	parts := []string{shellQuote(a.Bin), "-a", shellQuote(a.Atespace)}
	if a.KubeContext != "" {
		parts = append(parts, "--context", shellQuote(a.KubeContext))
	}
	if a.Server != "" {
		parts = append(parts, "--server", shellQuote(a.Server))
	}
	return strings.Join(parts, " ")
}

func (a *AX) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := a.RPCTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	return context.WithTimeout(ctx, d)
}

func (a *AX) meta(name string) *v1alpha1.ObjectMeta {
	return &v1alpha1.ObjectMeta{Name: name, Atespace: a.Atespace}
}

// rpcError adds the operation to a gRPC error, keeping the status message.
func rpcError(op string, err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return fmt.Errorf("%s: %s: %s", op, st.Code(), st.Message())
	}
	return fmt.Errorf("%s: %w", op, err)
}

// errAlreadyExists reports a create that hit an existing resource. axo
// never adopts (or later deletes) a Gateway or Workspace it did not create.
var errAlreadyExists = errors.New("already exists")

// CreateGateway creates the run's Gateway via the CreateGateway RPC.
func (a *AX) CreateGateway(ctx context.Context, name string, extraHosts []string) error {
	gw, err := GatewayProto(name, extraHosts)
	if err != nil {
		return err
	}
	gw.Metadata.Atespace = a.Atespace
	rctx, cancel := a.rpcCtx(ctx)
	defer cancel()
	if _, err := a.Client.CreateGateway(rctx, &v1alpha1.CreateGatewayRequest{Gateway: gw}); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("gateway %s %w in atespace %s; pick another --name", name, errAlreadyExists, a.Atespace)
		}
		return rpcError("CreateGateway "+name, err)
	}
	return nil
}

// CreateWorkspace creates the run's Workspace via the CreateWorkspace RPC.
// The server returns once the Workspace is pre-warmed (or fails), so the call
// is bounded by timeout rather than the short RPC timeout.
func (a *AX) CreateWorkspace(ctx context.Context, name, image string, spec map[string]any, timeout time.Duration) error {
	ws, err := WorkspaceProto(name, image, spec)
	if err != nil {
		return err
	}
	ws.Metadata.Atespace = a.Atespace
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := a.Client.CreateWorkspace(cctx, &v1alpha1.CreateWorkspaceRequest{Workspace: ws}); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("workspace %s %w in atespace %s; pick another --name", name, errAlreadyExists, a.Atespace)
		}
		return rpcError("CreateWorkspace "+name, err)
	}
	return nil
}

// WorkspacePhase returns the Workspace's phase and last setup error.
func (a *AX) WorkspacePhase(ctx context.Context, name string) (phase, setupErr string, err error) {
	rctx, cancel := a.rpcCtx(ctx)
	defer cancel()
	ws, err := a.Client.GetWorkspace(rctx, &v1alpha1.GetWorkspaceRequest{Atespace: a.Atespace, Name: name})
	if err != nil {
		return "", "", rpcError("GetWorkspace "+name, err)
	}
	return ws.GetStatus().GetPhase(), ws.GetStatus().GetLastSetupError(), nil
}

// CreateTask creates an AX Task that reuses the run's single shared
// Workspace and Gateway (both named shared).
func (a *AX) CreateTask(ctx context.Context, taskName, shared, step, goal string) error {
	cfg, err := structpb.NewStruct(map[string]any{"swarm": shared, "step": step})
	if err != nil {
		return err
	}
	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   a.meta(taskName),
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: shared},
			Gateway:   &v1alpha1.GatewayRef{Name: shared},
			Goal:      &v1alpha1.Goal{Text: goal},
			Config:    cfg,
			// Debug exposes the Task's guest services, which `ax ssh` needs.
			Debug: true,
		},
	}
	// CreateTask may build a snapshot when profiles differ; allow longer.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := a.Client.CreateTask(cctx, &v1alpha1.CreateTaskRequest{Task: task}); err != nil {
		return rpcError("CreateTask "+taskName, err)
	}
	return nil
}

// TaskState returns the Task's display state (Running, Suspended, ...).
func (a *AX) TaskState(ctx context.Context, name string) (string, error) {
	rctx, cancel := a.rpcCtx(ctx)
	defer cancel()
	t, err := a.Client.GetTask(rctx, &v1alpha1.GetTaskRequest{Atespace: a.Atespace, Name: name})
	if err != nil {
		return "", rpcError("GetTask "+name, err)
	}
	return v1alpha1.FormatTaskState(t), nil
}

// AXTaskInfo summarizes one AX Task.
type AXTaskInfo struct {
	Name  string
	Step  string
	State string
}

// ListTasksInfoWithPrefix lists the atespace's Tasks whose names start with prefix.
func (a *AX) ListTasksInfoWithPrefix(ctx context.Context, prefix string) ([]AXTaskInfo, error) {
	var out []AXTaskInfo
	token := ""
	for page := 0; page < 100; page++ {
		rctx, cancel := a.rpcCtx(ctx)
		resp, err := a.Client.ListTasks(rctx, &v1alpha1.ListTasksRequest{Atespace: a.Atespace, PageSize: 200, PageToken: token})
		cancel()
		if err != nil {
			return nil, rpcError("ListTasks", err)
		}
		for _, t := range resp.GetTasks() {
			name := t.GetMetadata().GetName()
			if strings.HasPrefix(name, prefix) {
				out = append(out, AXTaskInfo{Name: name, Step: strings.TrimPrefix(name, prefix), State: v1alpha1.FormatTaskState(t)})
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
	return out, nil
}

// Resume resumes (starts) a Task.
func (a *AX) Resume(ctx context.Context, name string) error {
	rctx, cancel := a.rpcCtx(ctx)
	defer cancel()
	_, err := a.Client.ResumeTask(rctx, &v1alpha1.ResumeTaskRequest{Atespace: a.Atespace, Name: name})
	return rpcError("ResumeTask "+name, err)
}

// Delete deletes a resource; kind is "task", "workspace", or "gateway".
func (a *AX) Delete(ctx context.Context, kind, name string) error {
	rctx, cancel := a.rpcCtx(ctx)
	defer cancel()
	var err error
	switch kind {
	case "task":
		_, err = a.Client.DeleteTask(rctx, &v1alpha1.DeleteTaskRequest{Atespace: a.Atespace, Name: name})
	case "workspace":
		_, err = a.Client.DeleteWorkspace(rctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: a.Atespace, Name: name})
	case "gateway":
		_, err = a.Client.DeleteGateway(rctx, &v1alpha1.DeleteGatewayRequest{Atespace: a.Atespace, Name: name})
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return rpcError("Delete"+capitalizeFirst(kind)+" "+name, err)
}

// WaitTaskRunning waits until the Task is Running, resuming it while it is
// Suspended (new Tasks start Suspended). It fails fast on Crashed.
func (a *AX) WaitTaskRunning(ctx context.Context, name string, timeout time.Duration) error {
	poll := a.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var lastResume time.Time
	state := ""
	for {
		st, err := a.TaskState(ctx, name)
		if err != nil {
			return err
		}
		state = st
		switch st {
		case "Running":
			return nil
		case "Crashed", "Terminating", "Deleting":
			return fmt.Errorf("ax task %s is %s", name, st)
		case "Suspended":
			if time.Since(lastResume) >= 3*time.Second {
				lastResume = time.Now()
				_ = a.Resume(ctx, name)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ax task %s is %s after %s", name, state, timeout)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// GatewayManifest returns the Gateway manifest (as shown by --dry-run).
func GatewayManifest(name string, extraHosts []string) map[string]any {
	egress := []any{
		map[string]any{"https": map[string]any{"hostnames": []any{
			"pypi.org",
			"files.pythonhosted.org",
			"registry.npmjs.org",
			"proxy.golang.org",
			"sum.golang.org",
			"storage.googleapis.com",
		}}},
		map[string]any{"https": map[string]any{"hostnames": []any{
			"github.com",
			"*.github.com",
			"*.githubusercontent.com",
		}}},
	}
	if len(extraHosts) > 0 {
		hosts := make([]any, len(extraHosts))
		for i, h := range extraHosts {
			hosts[i] = h
		}
		egress = append(egress, map[string]any{"https": map[string]any{"hostnames": hosts}})
	}
	return map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       v1alpha1.KindGateway,
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"egress": egress},
	}
}

// WorkspaceManifest returns the Workspace manifest (as shown by --dry-run).
// base is deep copied and never modified; image (when non-empty) is set as
// spec.profile.image, and a setup section's gateway is pointed at the run's
// single Gateway.
func WorkspaceManifest(name, image string, base map[string]any) map[string]any {
	spec := deepCopyMap(base)
	if spec == nil {
		spec = map[string]any{}
	}
	if image != "" {
		profile, _ := spec["profile"].(map[string]any)
		if profile == nil {
			profile = map[string]any{}
		}
		profile["image"] = image
		spec["profile"] = profile
	}
	if setup, ok := spec["setup"].(map[string]any); ok {
		setup["gateway"] = map[string]any{"name": name}
	}
	// Accept the legacy bare-list form of spec.mcp, as `ax apply` does.
	if list, ok := spec["mcp"].([]any); ok {
		spec["mcp"] = map[string]any{"servers": list}
	}
	return map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       v1alpha1.KindWorkspace,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}
}

var strictProto = protojson.UnmarshalOptions{} // unknown fields are errors

// GatewayProto returns the Gateway API message for the run.
func GatewayProto(name string, extraHosts []string) (*v1alpha1.Gateway, error) {
	data, err := json.Marshal(GatewayManifest(name, extraHosts))
	if err != nil {
		return nil, err
	}
	var gw v1alpha1.Gateway
	if err := strictProto.Unmarshal(data, &gw); err != nil {
		return nil, fmt.Errorf("gateway manifest: %w", err)
	}
	return &gw, nil
}

// WorkspaceProto returns the Workspace API message for the run.
func WorkspaceProto(name, image string, spec map[string]any) (*v1alpha1.Workspace, error) {
	data, err := json.Marshal(WorkspaceManifest(name, image, spec))
	if err != nil {
		return nil, err
	}
	var ws v1alpha1.Workspace
	if err := strictProto.Unmarshal(data, &ws); err != nil {
		return nil, fmt.Errorf("workspace spec: %w", err)
	}
	return &ws, nil
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}

// workspaceRepos returns the git repositories a Workspace manifest clones.
func workspaceRepos(manifest map[string]any) []string {
	var out []string
	for _, e := range workspaceSetupList(manifest, "git") {
		if m, ok := e.(map[string]any); ok {
			if r, ok := m["repo"].(string); ok && r != "" {
				out = append(out, r)
			}
		}
	}
	return out
}

// workspaceCommands returns the setup commands of a Workspace manifest.
func workspaceCommands(manifest map[string]any) []string {
	var out []string
	for _, e := range workspaceSetupList(manifest, "commands") {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func workspaceSetupList(manifest map[string]any, key string) []any {
	spec, _ := manifest["spec"].(map[string]any)
	setup, _ := spec["setup"].(map[string]any)
	list, _ := setup[key].([]any)
	return list
}

// Env tracks the AX resources created for one run: exactly one Gateway and
// one Workspace, plus the per-task AX Tasks named <name>-*.
type Env struct {
	AX   *AX
	Name string

	mu               sync.Mutex
	GatewayCreated   bool
	WorkspaceCreated bool
}

// CreateGateway creates the single shared Gateway for this run, at most once.
// A failed create still marks the Gateway for cleanup (the server may have
// persisted it), unless it failed because the name was already taken.
func (e *Env) CreateGateway(ctx context.Context, allowHosts []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.GatewayCreated {
		return nil
	}
	err := e.AX.CreateGateway(ctx, e.Name, allowHosts)
	e.GatewayCreated = !errors.Is(err, errAlreadyExists)
	return err
}

// CreateWorkspace creates the single shared Workspace for this run, at most once.
func (e *Env) CreateWorkspace(ctx context.Context, cfg Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.WorkspaceCreated {
		return nil
	}
	err := e.AX.CreateWorkspace(ctx, e.Name, cfg.Image, cfg.WorkspaceSpec, cfg.SetupTimeout)
	e.WorkspaceCreated = !errors.Is(err, errAlreadyExists)
	return err
}

func (e *Env) created() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.GatewayCreated || e.WorkspaceCreated
}

// CleanupResult summarizes what CleanupWithResult deleted or failed to delete.
type CleanupResult struct {
	TasksDeleted     []string
	WorkspaceDeleted bool
	GatewayDeleted   bool
	Err              error
}

// Summary returns a concise human-readable description of deleted resources.
func (r CleanupResult) Summary(name string) string {
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
		return fmt.Sprintf("no resources to delete for %s*", name)
	}
	return fmt.Sprintf("deleted %s (%s*)", joinHuman(parts), name)
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

// CleanupWithResult deletes all Tasks named <name>-*, then the Workspace and
// Gateway, via the control plane.
func (e *Env) CleanupWithResult(ctx context.Context) CleanupResult {
	var res CleanupResult
	e.mu.Lock()
	gw, ws := e.GatewayCreated, e.WorkspaceCreated
	e.mu.Unlock()
	if !gw && !ws {
		return res
	}
	var errs []error
	tasks, err := e.AX.ListTasksInfoWithPrefix(ctx, e.Name+"-")
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, task := range tasks {
			if delErr := e.AX.Delete(ctx, "task", task.Name); delErr != nil {
				if !isNotFound(delErr) {
					errs = append(errs, delErr)
				}
			} else {
				res.TasksDeleted = append(res.TasksDeleted, task.Name)
			}
		}
	}
	if ws {
		if delErr := e.AX.Delete(ctx, "workspace", e.Name); delErr != nil {
			if !isNotFound(delErr) {
				errs = append(errs, delErr)
			}
		} else {
			res.WorkspaceDeleted = true
		}
	}
	if gw {
		if delErr := e.AX.Delete(ctx, "gateway", e.Name); delErr != nil {
			if !isNotFound(delErr) {
				errs = append(errs, delErr)
			}
		} else {
			res.GatewayDeleted = true
		}
	}
	if len(errs) > 0 {
		res.Err = errors.Join(errs...)
	}
	return res
}

// Cleanup deletes all run resources unless keep is true, logging to ui.
func (e *Env) Cleanup(ctx context.Context, keep bool, ui *UI) {
	if !e.created() {
		return
	}
	if keep {
		ui.Log(fmt.Sprintf("keeping everything named %s*", e.Name))
		return
	}
	res := e.CleanupWithResult(ctx)
	if res.Err != nil {
		ui.Log(ui.Paint(fmt.Sprintf("cleanup warnings for %s*: %v", e.Name, res.Err), ansiYellow))
		return
	}
	ui.Log(res.Summary(e.Name))
}

// isNotFound reports whether an error means the resource does not exist.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "notfound") || strings.Contains(msg, "not found")
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
