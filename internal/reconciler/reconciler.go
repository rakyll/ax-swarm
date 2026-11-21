package reconciler

import (
	"context"
	"log"
	"time"

	"github.com/rakyll/ax-swarm/internal/axclient"
	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/store"
)

// Config tunes the wake-on-@mention reconciler.
type Config struct {
	PollInterval time.Duration // defaults to 2s
	SuspendGrace time.Duration // wait before calling SuspendTask on waiting workers (defaults to 15s)
	SwarmAPIURL  string        // URL passed to AX Tasks as SWARM_API_URL
	DefaultImage string        // fallback container image when Member.Image is empty
	Logger       *log.Logger
}

// Reconciler drives AX Task lifecycle (Create/Resume/Suspend/Delete) from
// swarm_members desired state.
type Reconciler struct {
	store store.Store
	ax    axclient.Client
	cfg   Config
}

func New(st store.Store, ax axclient.Client, cfg Config) *Reconciler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.SuspendGrace < 0 {
		cfg.SuspendGrace = 15 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Reconciler{store: st, ax: ax, cfg: cfg}
}

// Run reconciles all swarms on PollInterval until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := r.ReconcileOnce(ctx); err != nil {
			r.cfg.Logger.Printf("reconcile error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReconcileOnce runs a single reconciliation pass across all swarms.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	swarms, err := r.store.ListSwarms(ctx)
	if err != nil {
		return err
	}
	for _, sw := range swarms {
		if err := r.reconcileSwarm(ctx, sw); err != nil {
			r.cfg.Logger.Printf("reconcile swarm %s: %v", sw.SwarmID, err)
		}
	}
	return nil
}

func (r *Reconciler) reconcileSwarm(ctx context.Context, sw model.Swarm) error {
	tasks, err := r.ax.ListTasks(ctx, sw.Atespace)
	if err != nil {
		return err
	}
	byName := make(map[string]axclient.TaskState, len(tasks))
	for _, t := range tasks {
		byName[t.Name] = t.State
	}

	now := time.Now().UTC()
	for _, m := range sw.Members {
		// Only reconcile agent-backed workers; human/local coordinators have no AX Task.
		if m.Role == "coordinator" && m.OwnerKind == model.OwnerHuman {
			continue
		}
		taskName := m.TaskName
		if taskName == "" {
			taskName = m.Name
		}
		state, exists := byName[taskName]

		switch m.DesiredState {
		case model.DesiredKilled:
			if exists {
				if err := r.ax.DeleteTask(ctx, sw.Atespace, taskName); err != nil {
					r.cfg.Logger.Printf("delete task %s/%s: %v", sw.Atespace, taskName, err)
				}
			}
			_, _ = r.store.UpdateMemberPhase(ctx, sw.SwarmID, m.Name, model.PhaseKilled)

		case model.DesiredRunning:
			if !exists {
				img := m.Image
				if img == "" {
					img = r.cfg.DefaultImage
				}
				spec := axclient.TaskSpec{
					Name:      taskName,
					Atespace:  sw.Atespace,
					Workspace: m.Workspace,
					Image:     img,
					Goal:      m.Brief,
					Commands:  []string{"ax-swarm-worker"},
					Env: map[string]string{
						"SWARM_API_URL": r.cfg.SwarmAPIURL,
						"SWARM_ID":      sw.SwarmID,
						"SWARM_MEMBER":  m.Name,
						"SWARM_RUNTIME": m.Runtime,
					},
					Debug: true,
				}
				if err := r.ax.CreateTask(ctx, spec); err != nil {
					r.cfg.Logger.Printf("create task %s/%s: %v", sw.Atespace, taskName, err)
					continue
				}
				state = axclient.TaskStateSuspended
			}
			if state != axclient.TaskStateWorking {
				if err := r.ax.ResumeTask(ctx, sw.Atespace, taskName); err != nil {
					r.cfg.Logger.Printf("resume task %s/%s: %v", sw.Atespace, taskName, err)
					continue
				}
			}
			_, _ = r.store.UpdateMemberPhase(ctx, sw.SwarmID, m.Name, model.PhaseRunning)

		case model.DesiredWaiting:
			if exists && state == axclient.TaskStateWorking {
				if m.ParkedAt.IsZero() || now.Sub(m.ParkedAt) >= r.cfg.SuspendGrace {
					if err := r.ax.SuspendTask(ctx, sw.Atespace, taskName); err != nil {
						r.cfg.Logger.Printf("suspend task %s/%s: %v", sw.Atespace, taskName, err)
						continue
					}
				}
			}
			_, _ = r.store.UpdateMemberPhase(ctx, sw.SwarmID, m.Name, model.PhaseWaiting)
		}
	}
	return nil
}
