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
	"sort"
	"sync"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakeAX is an in-memory AX control plane implementing the gRPC AXClient.
// Embedding the interface makes any RPC the orchestrator should not call
// panic.
type fakeAX struct {
	v1alpha1.AXClient

	mu sync.Mutex
	// applied records every created resource as protojson-shaped maps.
	applied    []map[string]any
	gateways   map[string]bool
	workspaces map[string]bool
	tasks      map[string]v1alpha1.TaskState
	// workspacePhase is reported by GetWorkspace (default Ready).
	workspacePhase string
	deleted        []string
}

func newFakeAX() *fakeAX {
	return &fakeAX{
		gateways:   map[string]bool{},
		workspaces: map[string]bool{},
		tasks:      map[string]v1alpha1.TaskState{},
	}
}

func (f *fakeAX) record(m proto.Message) {
	data, _ := protojson.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	f.applied = append(f.applied, out)
}

func notFound(kind, name string) error {
	return status.Errorf(codes.NotFound, "%s %q not found in atespace \"default\"", kind, name)
}

func (f *fakeAX) CreateGateway(_ context.Context, in *v1alpha1.CreateGatewayRequest, _ ...grpc.CallOption) (*v1alpha1.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetGateway().GetMetadata().GetName()
	if f.gateways[name] {
		return nil, status.Errorf(codes.AlreadyExists, "gateway %q already exists", name)
	}
	f.gateways[name] = true
	f.record(in.GetGateway())
	return in.GetGateway(), nil
}

func (f *fakeAX) CreateWorkspace(_ context.Context, in *v1alpha1.CreateWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetWorkspace().GetMetadata().GetName()
	if f.workspaces[name] {
		return nil, status.Errorf(codes.AlreadyExists, "workspace %q already exists", name)
	}
	f.workspaces[name] = true
	f.record(in.GetWorkspace())
	return in.GetWorkspace(), nil
}

func (f *fakeAX) GetWorkspace(_ context.Context, in *v1alpha1.GetWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.workspaces[in.GetName()] {
		return nil, notFound("workspace", in.GetName())
	}
	phase := f.workspacePhase
	if phase == "" {
		phase = v1alpha1.WorkspacePhaseReady
	}
	return &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: in.GetName(), Atespace: in.GetAtespace()},
		Status:   &v1alpha1.WorkspaceStatus{Phase: phase, LastSetupError: "clone failed"},
	}, nil
}

func (f *fakeAX) CreateTask(_ context.Context, in *v1alpha1.CreateTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := in.GetTask()
	ws := t.GetSpec().GetWorkspace().GetName()
	if !f.workspaces[ws] {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace %q not found", ws)
	}
	f.tasks[t.GetMetadata().GetName()] = v1alpha1.TaskState_TASK_STATE_SUSPENDED
	f.record(t)
	return t, nil
}

func (f *fakeAX) taskLocked(name string) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Status:   &v1alpha1.TaskStatus{State: f.tasks[name]},
	}
}

func (f *fakeAX) GetTask(_ context.Context, in *v1alpha1.GetTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[in.GetName()]; !ok {
		return nil, notFound("task", in.GetName())
	}
	return f.taskLocked(in.GetName()), nil
}

func (f *fakeAX) ListTasks(_ context.Context, in *v1alpha1.ListTasksRequest, _ ...grpc.CallOption) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.tasks))
	for n := range f.tasks {
		names = append(names, n)
	}
	sort.Strings(names)
	// Page by one to exercise pagination.
	start := 0
	if in.GetPageToken() != "" {
		for i, n := range names {
			if n == in.GetPageToken() {
				start = i
			}
		}
	}
	resp := &v1alpha1.ListTasksResponse{}
	if start < len(names) {
		resp.Tasks = []*v1alpha1.Task{f.taskLocked(names[start])}
		if start+1 < len(names) {
			resp.NextPageToken = names[start+1]
		}
	}
	return resp, nil
}

func (f *fakeAX) ResumeTask(_ context.Context, in *v1alpha1.ResumeTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[in.GetName()]; !ok {
		return nil, notFound("task", in.GetName())
	}
	f.tasks[in.GetName()] = v1alpha1.TaskState_TASK_STATE_RUNNING
	return f.taskLocked(in.GetName()), nil
}

func (f *fakeAX) DeleteTask(_ context.Context, in *v1alpha1.DeleteTaskRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[in.GetName()]; !ok {
		return nil, notFound("task", in.GetName())
	}
	delete(f.tasks, in.GetName())
	f.deleted = append(f.deleted, "task/"+in.GetName())
	return &emptypb.Empty{}, nil
}

func (f *fakeAX) DeleteWorkspace(_ context.Context, in *v1alpha1.DeleteWorkspaceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.workspaces[in.GetName()] {
		return nil, notFound("workspace", in.GetName())
	}
	delete(f.workspaces, in.GetName())
	f.deleted = append(f.deleted, "workspace/"+in.GetName())
	return &emptypb.Empty{}, nil
}

func (f *fakeAX) DeleteGateway(_ context.Context, in *v1alpha1.DeleteGatewayRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gateways[in.GetName()] {
		return nil, notFound("gateway", in.GetName())
	}
	delete(f.gateways, in.GetName())
	f.deleted = append(f.deleted, "gateway/"+in.GetName())
	return &emptypb.Empty{}, nil
}

func (f *fakeAX) appliedKinds() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, m := range f.applied {
		k, _ := m["kind"].(string)
		out[k]++
	}
	return out
}
