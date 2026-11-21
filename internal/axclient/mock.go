package axclient

import (
	"context"
	"fmt"
	"sync"
)

// MockClient is an in-memory AX client for unit tests and local dry-runs.
type MockClient struct {
	mu    sync.Mutex
	tasks map[string]TaskState // key: atespace/name
	Specs map[string]TaskSpec
	Calls []string
}

func NewMockClient() *MockClient {
	return &MockClient{
		tasks: make(map[string]TaskState),
		Specs: make(map[string]TaskSpec),
	}
}

func key(atespace, name string) string { return atespace + "/" + name }

func (m *MockClient) CreateTask(_ context.Context, spec TaskSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(spec.Atespace, spec.Name)
	m.Calls = append(m.Calls, "create:"+k)
	if _, exists := m.tasks[k]; !exists {
		m.tasks[k] = TaskStateSuspended
	}
	m.Specs[k] = spec
	return nil
}

func (m *MockClient) ResumeTask(_ context.Context, atespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(atespace, name)
	m.Calls = append(m.Calls, "resume:"+k)
	if _, exists := m.tasks[k]; !exists {
		return fmt.Errorf("task %s not found", k)
	}
	m.tasks[k] = TaskStateWorking
	return nil
}

func (m *MockClient) SuspendTask(_ context.Context, atespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(atespace, name)
	m.Calls = append(m.Calls, "suspend:"+k)
	if _, exists := m.tasks[k]; !exists {
		return fmt.Errorf("task %s not found", k)
	}
	m.tasks[k] = TaskStateSuspended
	return nil
}

func (m *MockClient) DeleteTask(_ context.Context, atespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(atespace, name)
	m.Calls = append(m.Calls, "delete:"+k)
	delete(m.tasks, k)
	delete(m.Specs, k)
	return nil
}

func (m *MockClient) ListTasks(_ context.Context, atespace string) ([]TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TaskInfo
	prefix := atespace + "/"
	for k, st := range m.tasks {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, TaskInfo{
				Name:     k[len(prefix):],
				Atespace: atespace,
				State:    st,
			})
		}
	}
	return out, nil
}

func (m *MockClient) State(atespace, name string) (TaskState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.tasks[key(atespace, name)]
	return st, ok
}
