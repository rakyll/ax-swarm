package axclient

import "context"

// TaskState represents the observed lifecycle state of an AX Task.
type TaskState string

const (
	TaskStateUnknown   TaskState = "Unknown"
	TaskStateSuspended TaskState = "Suspended"
	TaskStateWorking   TaskState = "Working"
	TaskStateCompleted TaskState = "Completed"
	TaskStateFailed    TaskState = "Failed"
)

// TaskSpec describes the AX Task created for a swarm worker.
type TaskSpec struct {
	Name      string
	Atespace  string
	Workspace string
	Image     string
	Goal      string
	Commands  []string
	Env       map[string]string
	Debug     bool
}

// TaskInfo is a lightweight summary returned by ListTasks.
type TaskInfo struct {
	Name     string
	Atespace string
	State    TaskState
}

// Client is the minimal subset of AX control-plane operations required by
// ax-swarm's wake-on-@mention reconciler.
type Client interface {
	CreateTask(ctx context.Context, spec TaskSpec) error
	ResumeTask(ctx context.Context, atespace, name string) error
	SuspendTask(ctx context.Context, atespace, name string) error
	DeleteTask(ctx context.Context, atespace, name string) error
	ListTasks(ctx context.Context, atespace string) ([]TaskInfo, error)
}
