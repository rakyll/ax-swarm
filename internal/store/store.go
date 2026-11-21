package store

import (
	"context"
	"errors"

	"github.com/rakyll/ax-swarm/internal/model"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInvalidArg    = errors.New("invalid argument")
)

type CreateSwarmParams struct {
	SwarmID      string
	Atespace     string
	Title        string
	WorkspaceRef string
}

type AddMemberParams struct {
	SwarmID    string
	Name       string
	Role       string
	Model      string
	Runtime    string
	Image      string
	Workspace  string
	Brief      string
	Channels   []string
	NodeID     string
	OwnerKind  string
	OwnerLabel string
	PostedBy   string // defaults to "coordinator"
}

type PostMessageParams struct {
	SwarmID string
	Channel string
	Agent   string
	Text    string
}

type ParkMemberParams struct {
	SwarmID  string
	Name     string
	Since    int64
	WakeMode string // "mentions" (default) or "any"
}

type ParkResult struct {
	Member      model.Member    `json:"member"`
	ParkBlocked bool            `json:"park_blocked"`
	Unread      []model.Message `json:"unread,omitempty"`
}

// Store is the persistence and pub/sub interface for ax-swarm.
// FileStore is the zero-dependency default; SQLite or PostgreSQL can implement
// the same contract without changing the server or reconciler.
type Store interface {
	CreateSwarm(ctx context.Context, p CreateSwarmParams) (model.Swarm, error)
	GetSwarm(ctx context.Context, swarmID string) (model.Swarm, error)
	ListSwarms(ctx context.Context) ([]model.Swarm, error)
	SetTitle(ctx context.Context, swarmID, title string) (model.Swarm, error)

	AddMember(ctx context.Context, p AddMemberParams) (model.Member, model.Message, error)
	KillMember(ctx context.Context, swarmID, name string) (model.Member, error)
	UpdateMemberPhase(ctx context.Context, swarmID, name, phase string) (model.Member, error)
	AuthenticateToken(ctx context.Context, token string) (swarmID string, memberName string, ok bool)

	PostMessage(ctx context.Context, p PostMessageParams) (model.Message, []string, error)
	ReadBoard(ctx context.Context, swarmID, memberName string, since int64, channel string) ([]model.Message, int64, error)
	ParkMember(ctx context.Context, p ParkMemberParams) (ParkResult, error)

	// Watch streams a full initial snapshot (Since=0) followed by incremental
	// snapshots whenever the swarm changes, until ctx is cancelled.
	Watch(ctx context.Context, swarmID string) (<-chan model.WatchSnapshot, error)
}
