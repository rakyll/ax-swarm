package store

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakyll/ax-swarm/internal/model"
)

type tokenEntry struct {
	SwarmID    string `json:"swarm_id"`
	MemberName string `json:"member_name"`
}

type subscriber struct {
	ch      chan model.WatchSnapshot
	lastSeq int64
}

type swarmData struct {
	swarm    model.Swarm
	messages []model.Message
	subs     map[*subscriber]struct{}
}

// FileStore persists each swarm under <dir>/<swarm_id>/{state.json,board.jsonl}
// and tokens in <dir>/tokens.json. It is safe for concurrent use.
type FileStore struct {
	dir    string
	mu     sync.RWMutex
	swarms map[string]*swarmData
	tokens map[string]tokenEntry
}

// NewFileStore opens or initializes a FileStore rooted at dir.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	fs := &FileStore{
		dir:    dir,
		swarms: make(map[string]*swarmData),
		tokens: make(map[string]tokenEntry),
	}
	if err := fs.loadAll(); err != nil {
		return nil, err
	}
	return fs, nil
}

func (s *FileStore) loadAll() error {
	tokensPath := filepath.Join(s.dir, "tokens.json")
	if b, err := os.ReadFile(tokensPath); err == nil {
		_ = json.Unmarshal(b, &s.tokens)
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		statePath := filepath.Join(s.dir, e.Name(), "state.json")
		b, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var sw model.Swarm
		if err := json.Unmarshal(b, &sw); err != nil {
			return fmt.Errorf("parse %s: %w", statePath, err)
		}
		if sw.Members == nil {
			sw.Members = make(map[string]model.Member)
		}
		msgs, err := readMessagesJSONL(filepath.Join(s.dir, e.Name(), "board.jsonl"))
		if err != nil {
			return err
		}
		if len(msgs) > 0 && msgs[len(msgs)-1].Seq > sw.HeadSeq {
			sw.HeadSeq = msgs[len(msgs)-1].Seq
		}
		s.swarms[sw.SwarmID] = &swarmData{
			swarm:    sw,
			messages: msgs,
			subs:     make(map[*subscriber]struct{}),
		}
	}
	return nil
}

func readMessagesJSONL(path string) ([]model.Message, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []model.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m model.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		msgs = append(msgs, m)
	}
	return msgs, sc.Err()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *FileStore) saveSwarmLocked(sw model.Swarm) error {
	dir := filepath.Join(s.dir, sw.SwarmID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sw, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "state.json"), b, 0o644)
}

func (s *FileStore) appendMessageLocked(swarmID string, msg model.Message) error {
	dir := filepath.Join(s.dir, swarmID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "board.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *FileStore) saveTokensLocked() error {
	b, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "tokens.json"), b, 0o600)
}

func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func cloneSwarm(sw model.Swarm) model.Swarm {
	out := sw
	out.Members = make(map[string]model.Member, len(sw.Members))
	for k, v := range sw.Members {
		v.Token = ""
		out.Members[k] = v
	}
	if len(sw.Channels) > 0 {
		out.Channels = append([]model.Channel(nil), sw.Channels...)
	}
	return out
}

func (s *FileStore) notifyLocked(sd *swarmData) {
	for sub := range sd.subs {
		var delta []model.Message
		for _, m := range sd.messages {
			if m.Seq > sub.lastSeq {
				delta = append(delta, m)
			}
		}
		snap := model.WatchSnapshot{
			Version:  1,
			SwarmID:  sd.swarm.SwarmID,
			Title:    sd.swarm.Title,
			Head:     sd.swarm.HeadSeq,
			Since:    sub.lastSeq,
			Channels: append([]model.Channel(nil), sd.swarm.Channels...),
			Members:  cloneSwarm(sd.swarm).Members,
			Messages: delta,
		}
		select {
		case sub.ch <- snap:
			sub.lastSeq = sd.swarm.HeadSeq
		default:
			// Slow consumer: drop and let next tick/reconnect resync.
		}
	}
}

func (s *FileStore) CreateSwarm(_ context.Context, p CreateSwarmParams) (model.Swarm, error) {
	if p.SwarmID == "" {
		return model.Swarm{}, fmt.Errorf("%w: swarm_id is required", ErrInvalidArg)
	}
	if p.Atespace == "" {
		p.Atespace = "swarm-" + p.SwarmID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.swarms[p.SwarmID]; exists {
		return model.Swarm{}, fmt.Errorf("%w: swarm %q", ErrAlreadyExists, p.SwarmID)
	}
	now := time.Now().UTC()
	sw := model.Swarm{
		SwarmID:      p.SwarmID,
		Atespace:     p.Atespace,
		Title:        p.Title,
		WorkspaceRef: p.WorkspaceRef,
		Members:      make(map[string]model.Member),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.saveSwarmLocked(sw); err != nil {
		return model.Swarm{}, err
	}
	s.swarms[p.SwarmID] = &swarmData{
		swarm: sw,
		subs:  make(map[*subscriber]struct{}),
	}
	return cloneSwarm(sw), nil
}

func (s *FileStore) GetSwarm(_ context.Context, swarmID string) (model.Swarm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sd, ok := s.swarms[swarmID]
	if !ok {
		return model.Swarm{}, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}
	return cloneSwarm(sd.swarm), nil
}

func (s *FileStore) ListSwarms(_ context.Context) ([]model.Swarm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Swarm, 0, len(s.swarms))
	for _, sd := range s.swarms {
		out = append(out, cloneSwarm(sd.swarm))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *FileStore) SetTitle(_ context.Context, swarmID, title string) (model.Swarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sd, ok := s.swarms[swarmID]
	if !ok {
		return model.Swarm{}, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}
	sd.swarm.Title = title
	sd.swarm.UpdatedAt = time.Now().UTC()
	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return model.Swarm{}, err
	}
	s.notifyLocked(sd)
	return cloneSwarm(sd.swarm), nil
}

func (s *FileStore) AddMember(_ context.Context, p AddMemberParams) (model.Member, model.Message, error) {
	if p.SwarmID == "" || p.Name == "" {
		return model.Member{}, model.Message{}, fmt.Errorf("%w: swarm_id and name are required", ErrInvalidArg)
	}
	name := strings.ToLower(p.Name)
	if name == "all" {
		return model.Member{}, model.Message{}, fmt.Errorf("%w: %q is reserved", ErrInvalidArg, name)
	}
	if p.Role == "" {
		p.Role = "worker"
	}
	if p.OwnerKind == "" {
		p.OwnerKind = model.OwnerAgent
	}
	if p.OwnerLabel == "" {
		p.OwnerLabel = name
	}
	if p.PostedBy == "" {
		p.PostedBy = "coordinator"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sd, ok := s.swarms[p.SwarmID]
	if !ok {
		return model.Member{}, model.Message{}, fmt.Errorf("%w: swarm %q", ErrNotFound, p.SwarmID)
	}
	if _, exists := sd.swarm.Members[name]; exists {
		return model.Member{}, model.Message{}, fmt.Errorf("%w: member %q", ErrAlreadyExists, name)
	}

	tok, err := generateToken()
	if err != nil {
		return model.Member{}, model.Message{}, err
	}

	now := time.Now().UTC()
	ws := p.Workspace
	if ws == "" {
		ws = sd.swarm.WorkspaceRef
	}

	// Post the public brief onto the board if provided.
	var briefMsg model.Message
	if strings.TrimSpace(p.Brief) != "" {
		sd.swarm.HeadSeq++
		ch := model.DefaultChannel
		if len(p.Channels) > 0 {
			ch = p.Channels[0]
		}
		text := fmt.Sprintf("@%s %s", name, strings.TrimSpace(p.Brief))
		briefMsg = model.Message{
			Seq:      sd.swarm.HeadSeq,
			Channel:  ch,
			Agent:    p.PostedBy,
			Text:     text,
			Mentions: model.ParseMentions(text),
			Time:     now,
		}
		sd.messages = append(sd.messages, briefMsg)
		if err := s.appendMessageLocked(p.SwarmID, briefMsg); err != nil {
			return model.Member{}, model.Message{}, err
		}
	}

	// New worker's LastReadSeq starts right before its brief so its first
	// ReadBoard call delivers the brief immediately.
	startSeq := sd.swarm.HeadSeq
	if briefMsg.Seq > 0 {
		startSeq = briefMsg.Seq - 1
	}

	m := model.Member{
		Name:           name,
		TaskName:       name,
		ConversationID: fmt.Sprintf("ax-swarm:%s:%s", sd.swarm.Atespace, name),
		Role:           p.Role,
		Model:          p.Model,
		Runtime:        p.Runtime,
		Image:          p.Image,
		Workspace:      ws,
		Brief:          p.Brief,
		Channels:       append([]string(nil), p.Channels...),
		WakeMode:       model.WakeMentions,
		WaitSince:      startSeq,
		LastReadSeq:    startSeq,
		Phase:          model.PhaseQueued,
		DesiredState:   model.DesiredRunning,
		NodeID:         p.NodeID,
		OwnerKind:      p.OwnerKind,
		OwnerLabel:     p.OwnerLabel,
		Token:          tok,
		UpdatedAt:      now,
	}

	sd.swarm.Members[name] = m
	sd.swarm.UpdatedAt = now
	s.tokens[tok] = tokenEntry{SwarmID: p.SwarmID, MemberName: name}

	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return model.Member{}, model.Message{}, err
	}
	if err := s.saveTokensLocked(); err != nil {
		return model.Member{}, model.Message{}, err
	}
	s.notifyLocked(sd)
	return m, briefMsg, nil
}

func (s *FileStore) KillMember(_ context.Context, swarmID, name string) (model.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sd, ok := s.swarms[swarmID]
	if !ok {
		return model.Member{}, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}
	name = strings.ToLower(name)
	m, ok := sd.swarm.Members[name]
	if !ok {
		return model.Member{}, fmt.Errorf("%w: member %q", ErrNotFound, name)
	}
	now := time.Now().UTC()
	m.Killed = true
	m.DesiredState = model.DesiredKilled
	m.Phase = model.PhaseKilled
	m.UpdatedAt = now
	sd.swarm.Members[name] = m
	sd.swarm.UpdatedAt = now
	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return model.Member{}, err
	}
	s.notifyLocked(sd)
	m.Token = ""
	return m, nil
}

func (s *FileStore) UpdateMemberPhase(_ context.Context, swarmID, name, phase string) (model.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sd, ok := s.swarms[swarmID]
	if !ok {
		return model.Member{}, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}
	name = strings.ToLower(name)
	m, ok := sd.swarm.Members[name]
	if !ok {
		return model.Member{}, fmt.Errorf("%w: member %q", ErrNotFound, name)
	}
	if m.Phase == phase {
		m.Token = ""
		return m, nil
	}
	now := time.Now().UTC()
	m.Phase = phase
	m.UpdatedAt = now
	sd.swarm.Members[name] = m
	sd.swarm.UpdatedAt = now
	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return model.Member{}, err
	}
	s.notifyLocked(sd)
	m.Token = ""
	return m, nil
}

func (s *FileStore) AuthenticateToken(_ context.Context, token string) (string, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tokens[token]
	if !ok {
		return "", "", false
	}
	return e.SwarmID, e.MemberName, true
}

func (s *FileStore) PostMessage(_ context.Context, p PostMessageParams) (model.Message, []string, error) {
	if p.SwarmID == "" || p.Agent == "" || strings.TrimSpace(p.Text) == "" {
		return model.Message{}, nil, fmt.Errorf("%w: swarm_id, agent, and text are required", ErrInvalidArg)
	}
	if p.Channel == "" {
		p.Channel = model.DefaultChannel
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sd, ok := s.swarms[p.SwarmID]
	if !ok {
		return model.Message{}, nil, fmt.Errorf("%w: swarm %q", ErrNotFound, p.SwarmID)
	}
	now := time.Now().UTC()
	sd.swarm.HeadSeq++
	msg := model.Message{
		Seq:      sd.swarm.HeadSeq,
		Channel:  p.Channel,
		Agent:    strings.ToLower(p.Agent),
		Text:     p.Text,
		Mentions: model.ParseMentions(p.Text),
		Time:     now,
	}
	sd.messages = append(sd.messages, msg)

	// Wake-on-@mention: flip matching waiting/stopped members to DesiredRunning.
	var woken []string
	for name, m := range sd.swarm.Members {
		if model.MemberMatchesMessage(m, msg) {
			if m.DesiredState != model.DesiredRunning {
				m.DesiredState = model.DesiredRunning
				m.ParkedAt = time.Time{}
				m.UpdatedAt = now
				sd.swarm.Members[name] = m
			}
			woken = append(woken, name)
		}
	}
	sort.Strings(woken)
	sd.swarm.UpdatedAt = now

	if err := s.appendMessageLocked(p.SwarmID, msg); err != nil {
		return model.Message{}, nil, err
	}
	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return model.Message{}, nil, err
	}
	s.notifyLocked(sd)
	return msg, woken, nil
}

func (s *FileStore) ReadBoard(_ context.Context, swarmID, memberName string, since int64, channel string) ([]model.Message, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sd, ok := s.swarms[swarmID]
	if !ok {
		return nil, 0, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}
	var out []model.Message
	for _, m := range sd.messages {
		if m.Seq <= since {
			continue
		}
		if channel != "" && channel != "*" && m.Channel != channel {
			continue
		}
		out = append(out, m)
	}
	if memberName != "" {
		name := strings.ToLower(memberName)
		if m, exists := sd.swarm.Members[name]; exists && sd.swarm.HeadSeq > m.LastReadSeq {
			m.LastReadSeq = sd.swarm.HeadSeq
			m.UpdatedAt = time.Now().UTC()
			sd.swarm.Members[name] = m
			_ = s.saveSwarmLocked(sd.swarm)
		}
	}
	return out, sd.swarm.HeadSeq, nil
}

func (s *FileStore) ParkMember(_ context.Context, p ParkMemberParams) (ParkResult, error) {
	if p.SwarmID == "" || p.Name == "" {
		return ParkResult{}, fmt.Errorf("%w: swarm_id and name are required", ErrInvalidArg)
	}
	if p.WakeMode == "" {
		p.WakeMode = model.WakeMentions
	}
	if p.WakeMode != model.WakeMentions && p.WakeMode != model.WakeAny {
		return ParkResult{}, fmt.Errorf("%w: invalid wake_mode %q", ErrInvalidArg, p.WakeMode)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sd, ok := s.swarms[p.SwarmID]
	if !ok {
		return ParkResult{}, fmt.Errorf("%w: swarm %q", ErrNotFound, p.SwarmID)
	}
	name := strings.ToLower(p.Name)
	m, ok := sd.swarm.Members[name]
	if !ok {
		return ParkResult{}, fmt.Errorf("%w: member %q", ErrNotFound, name)
	}
	if m.Killed {
		m.Token = ""
		return ParkResult{Member: m}, nil
	}

	// Race protection: if a matching message arrived after `since`, refuse to park
	// and return the unread matching messages so the worker processes them right away.
	candidate := m
	candidate.WakeMode = p.WakeMode
	var unread []model.Message
	for _, msg := range sd.messages {
		if msg.Seq <= p.Since {
			continue
		}
		if model.MemberMatchesMessage(candidate, msg) {
			unread = append(unread, msg)
		}
	}
	if len(unread) > 0 {
		m.Token = ""
		return ParkResult{
			Member:      m,
			ParkBlocked: true,
			Unread:      unread,
		}, nil
	}

	now := time.Now().UTC()
	m.WakeMode = p.WakeMode
	m.WaitSince = p.Since
	if p.Since > m.LastReadSeq {
		m.LastReadSeq = p.Since
	}
	m.DesiredState = model.DesiredWaiting
	m.Phase = model.PhaseWaiting
	m.ParkedAt = now
	m.UpdatedAt = now
	sd.swarm.Members[name] = m
	sd.swarm.UpdatedAt = now

	if err := s.saveSwarmLocked(sd.swarm); err != nil {
		return ParkResult{}, err
	}
	s.notifyLocked(sd)
	m.Token = ""
	return ParkResult{Member: m}, nil
}

func (s *FileStore) Watch(ctx context.Context, swarmID string) (<-chan model.WatchSnapshot, error) {
	s.mu.Lock()
	sd, ok := s.swarms[swarmID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
	}

	ch := make(chan model.WatchSnapshot, 16)
	sub := &subscriber{ch: ch, lastSeq: sd.swarm.HeadSeq}
	sd.subs[sub] = struct{}{}

	// Full initial snapshot (Since = 0).
	initSnap := model.WatchSnapshot{
		Version:  1,
		SwarmID:  sd.swarm.SwarmID,
		Title:    sd.swarm.Title,
		Head:     sd.swarm.HeadSeq,
		Since:    0,
		Channels: append([]model.Channel(nil), sd.swarm.Channels...),
		Members:  cloneSwarm(sd.swarm).Members,
		Messages: append([]model.Message(nil), sd.messages...),
	}
	ch <- initSnap
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(sd.subs, sub)
		close(ch)
		s.mu.Unlock()
	}()
	return ch, nil
}
