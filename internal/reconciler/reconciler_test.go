package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/rakyll/ax-swarm/internal/axclient"
	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/store"
)

func TestReconcilerScaleToZeroLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	mockAX := axclient.NewMockClient()
	rec := New(st, mockAX, Config{
		SuspendGrace: 0, // immediate suspend in tests
		SwarmAPIURL:  "http://127.0.0.1:8787",
		DefaultImage: "ghcr.io/example/worker:latest",
	})

	_, err = st.CreateSwarm(ctx, store.CreateSwarmParams{
		SwarmID:      "sw1",
		WorkspaceRef: "golang",
	})
	if err != nil {
		t.Fatalf("CreateSwarm: %v", err)
	}
	_, _, err = st.AddMember(ctx, store.AddMemberParams{
		SwarmID: "sw1",
		Name:    "parser",
		Brief:   "Fix bugs",
		Runtime: "claude-code",
	})
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// 1. Reconcile creates and resumes the AX Task.
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce #1: %v", err)
	}
	if state, ok := mockAX.State("swarm-sw1", "parser"); !ok || state != axclient.TaskStateWorking {
		t.Fatalf("expected Working task, got ok=%v state=%s", ok, state)
	}

	// 2. Worker parks -> Reconcile suspends the AX Task.
	if _, err := st.ParkMember(ctx, store.ParkMemberParams{SwarmID: "sw1", Name: "parser", Since: 1}); err != nil {
		t.Fatalf("ParkMember: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce #2: %v", err)
	}
	if state, _ := mockAX.State("swarm-sw1", "parser"); state != axclient.TaskStateSuspended {
		t.Fatalf("expected Suspended task after park, got %s", state)
	}

	// 3. @mention wakes worker -> Reconcile resumes the AX Task.
	if _, _, err := st.PostMessage(ctx, store.PostMessageParams{
		SwarmID: "sw1",
		Agent:   "verifier",
		Text:    "@parser fix lint",
	}); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce #3: %v", err)
	}
	if state, _ := mockAX.State("swarm-sw1", "parser"); state != axclient.TaskStateWorking {
		t.Fatalf("expected Working task after @mention, got %s", state)
	}

	// 4. Kill member -> Reconcile deletes the AX Task.
	if _, err := st.KillMember(ctx, "sw1", "parser"); err != nil {
		t.Fatalf("KillMember: %v", err)
	}
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce #4: %v", err)
	}
	if _, ok := mockAX.State("swarm-sw1", "parser"); ok {
		t.Fatalf("expected task deleted after kill")
	}
	sw, _ := st.GetSwarm(ctx, "sw1")
	if sw.Members["parser"].Phase != model.PhaseKilled {
		t.Fatalf("expected member phase KILLED, got %s", sw.Members["parser"].Phase)
	}
}
