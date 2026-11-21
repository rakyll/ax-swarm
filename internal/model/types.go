package model

import (
	"regexp"
	"strings"
	"time"
)

const (
	PhaseQueued  = "QUEUED"
	PhaseRunning = "RUNNING"
	PhaseWaiting = "WAITING"
	PhaseStopped = "STOPPED"
	PhaseKilled  = "KILLED"

	DesiredRunning = "RUNNING"
	DesiredWaiting = "WAITING"
	DesiredKilled  = "KILLED"

	WakeMentions = "mentions"
	WakeAny      = "any"

	OwnerHuman = "HUMAN"
	OwnerAgent = "AGENT"

	DefaultChannel = "main"
)

// Swarm represents a swarm coordinated over a shared message board.
type Swarm struct {
	SwarmID      string            `json:"swarm_id"`
	Atespace     string            `json:"atespace"`
	Title        string            `json:"title"`
	WorkspaceRef string            `json:"workspace_ref,omitempty"`
	HeadSeq      int64             `json:"head_seq"`
	Channels     []Channel         `json:"channels,omitempty"`
	Members      map[string]Member `json:"members"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Channel is an optional topic stream inside a swarm.
type Channel struct {
	Name      string `json:"name"`
	Topic     string `json:"topic,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
}

// Member represents a worker or coordinator on the swarm roster.
// JSON tags match Antigravity's swarm_board remote_board contract.
type Member struct {
	Name           string    `json:"name"`
	TaskName       string    `json:"task_name,omitempty"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Model          string    `json:"model,omitempty"`
	Runtime        string    `json:"runtime,omitempty"`
	Image          string    `json:"image,omitempty"`
	Workspace      string    `json:"workspace,omitempty"`
	Brief          string    `json:"brief,omitempty"`
	Channels       []string  `json:"channels,omitempty"`
	WakeMode       string    `json:"wake_mode"`
	WaitSince      int64     `json:"wait_since"`
	LastReadSeq    int64     `json:"last_read_seq"`
	Phase          string    `json:"phase"`
	DesiredState   string    `json:"desired_state"`
	NodeID         string    `json:"node_id,omitempty"`
	OwnerKind      string    `json:"owner_kind,omitempty"`
	OwnerLabel     string    `json:"owner_label,omitempty"`
	Killed         bool      `json:"killed"`
	Token          string    `json:"-"` // per-member bearer token (never serialized to board snapshots)
	ParkedAt       time.Time `json:"parked_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Message is an immutable post on the swarm board.
type Message struct {
	Seq      int64     `json:"seq"`
	Channel  string    `json:"channel,omitempty"`
	Agent    string    `json:"agent"`
	Text     string    `json:"text"`
	Mentions []string  `json:"mentions,omitempty"`
	Time     time.Time `json:"time"`
}

// WatchSnapshot is the NDJSON document emitted by `ax-swarm swarm --json --watch`
// and WatchSwarm streams, matching Antigravity's swarm_board sidecar parser.
type WatchSnapshot struct {
	Version  int               `json:"version"`
	SwarmID  string            `json:"swarm_id"`
	Title    string            `json:"title"`
	Head     int64             `json:"head"`
	Since    int64             `json:"since"`
	Channels []Channel         `json:"channels,omitempty"`
	Members  map[string]Member `json:"members"`
	Messages []Message         `json:"messages"`
}

var mentionRe = regexp.MustCompile(`(^|[^a-zA-Z0-9_-])@(#?[a-zA-Z0-9][a-zA-Z0-9_-]*)`)

// ParseMentions extracts unique lowercase @mentions (including @all and @#channel)
// at word boundaries, matching swarm board addressing semantics.
func ParseMentions(text string) []string {
	matches := mentionRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		target := strings.ToLower(m[2])
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}

// MemberMatchesMessage reports whether a message should wake the given member.
func MemberMatchesMessage(m Member, msg Message) bool {
	if m.Killed || m.DesiredState == DesiredKilled {
		return false
	}
	if strings.EqualFold(m.Name, msg.Agent) {
		return false
	}
	name := strings.ToLower(m.Name)
	for _, target := range msg.Mentions {
		if target == "all" {
			return true
		}
		if target == name {
			return true
		}
		// Numbered group prefix match: @parser matches parser-1, parser-2, ...
		if strings.HasPrefix(name, target+"-") {
			return true
		}
		// Channel mention: @#parser wakes subscribers to channel "parser".
		if strings.HasPrefix(target, "#") {
			ch := strings.TrimPrefix(target, "#")
			if memberInChannel(m, ch) {
				return true
			}
		}
	}
	if m.WakeMode == WakeAny {
		ch := msg.Channel
		if ch == "" || ch == DefaultChannel {
			return true
		}
		return memberInChannel(m, ch)
	}
	return false
}

func memberInChannel(m Member, ch string) bool {
	if ch == "" || ch == DefaultChannel {
		return true
	}
	for _, c := range m.Channels {
		if strings.EqualFold(c, ch) {
			return true
		}
	}
	return false
}
