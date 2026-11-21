package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/store"
)

// Server exposes the SwarmAgent API over HTTP/JSON and NDJSON streaming.
type Server struct {
	store store.Store
	mux   *http.ServeMux
}

func New(st store.Store) *Server {
	s := &Server{
		store: st,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("POST /v1/swarms", s.handleCreateSwarm)
	s.mux.HandleFunc("GET /v1/swarms", s.handleListSwarms)
	s.mux.HandleFunc("GET /v1/swarms/{id}", s.handleGetSwarm)
	s.mux.HandleFunc("PUT /v1/swarms/{id}/title", s.handleSetTitle)

	s.mux.HandleFunc("POST /v1/swarms/{id}/members", s.handleAddMember)
	s.mux.HandleFunc("DELETE /v1/swarms/{id}/members/{name}", s.handleKillMember)
	s.mux.HandleFunc("POST /v1/swarms/{id}/members/{name}/park", s.handleParkMember)

	s.mux.HandleFunc("POST /v1/swarms/{id}/messages", s.handlePostMessage)
	s.mux.HandleFunc("GET /v1/swarms/{id}/messages", s.handleReadBoard)
	s.mux.HandleFunc("GET /v1/swarms/{id}/watch", s.handleWatch)
}

func (s *Server) bearerIdentity(r *http.Request) (swarmID, member string, hasToken, valid bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", "", false, false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if tok == "" {
		return "", "", true, false
	}
	sid, mem, ok := s.store.AuthenticateToken(r.Context(), tok)
	return sid, mem, true, ok
}

func (s *Server) handleCreateSwarm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SwarmID      string `json:"swarm_id"`
		Atespace     string `json:"atespace"`
		Title        string `json:"title"`
		WorkspaceRef string `json:"workspace_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sw, err := s.store.CreateSwarm(r.Context(), store.CreateSwarmParams{
		SwarmID:      req.SwarmID,
		Atespace:     req.Atespace,
		Title:        req.Title,
		WorkspaceRef: req.WorkspaceRef,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sw)
}

func (s *Server) handleListSwarms(w http.ResponseWriter, r *http.Request) {
	swarms, err := s.store.ListSwarms(r.Context())
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"swarms": swarms})
}

func (s *Server) handleGetSwarm(w http.ResponseWriter, r *http.Request) {
	sw, err := s.store.GetSwarm(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sw)
}

func (s *Server) handleSetTitle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sw, err := s.store.SetTitle(r.Context(), r.PathValue("id"), req.Title)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sw)
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string   `json:"name"`
		Role       string   `json:"role"`
		Model      string   `json:"model"`
		Runtime    string   `json:"runtime"`
		Image      string   `json:"image"`
		Workspace  string   `json:"workspace"`
		Brief      string   `json:"brief"`
		Channels   []string `json:"channels"`
		NodeID     string   `json:"node_id"`
		OwnerKind  string   `json:"owner_kind"`
		OwnerLabel string   `json:"owner_label"`
		PostedBy   string   `json:"posted_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	m, briefMsg, err := s.store.AddMember(r.Context(), store.AddMemberParams{
		SwarmID:    r.PathValue("id"),
		Name:       req.Name,
		Role:       req.Role,
		Model:      req.Model,
		Runtime:    req.Runtime,
		Image:      req.Image,
		Workspace:  req.Workspace,
		Brief:      req.Brief,
		Channels:   req.Channels,
		NodeID:     req.NodeID,
		OwnerKind:  req.OwnerKind,
		OwnerLabel: req.OwnerLabel,
		PostedBy:   req.PostedBy,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"member":        m,
		"token":         m.Token,
		"brief_message": briefMsg,
	})
}

func (s *Server) handleKillMember(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.KillMember(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	swarmID := r.PathValue("id")
	var req struct {
		Channel string `json:"channel"`
		Agent   string `json:"agent"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// If a member Bearer token is provided, enforce identity from the token
	// so sandboxed workers cannot spoof other members or post across swarms.
	if tokSwarm, tokMember, hasToken, valid := s.bearerIdentity(r); hasToken {
		if !valid || tokSwarm != swarmID {
			writeErr(w, http.StatusUnauthorized, errors.New("invalid member token"))
			return
		}
		req.Agent = tokMember
	}

	msg, woken, err := s.store.PostMessage(r.Context(), store.PostMessageParams{
		SwarmID: swarmID,
		Channel: req.Channel,
		Agent:   req.Agent,
		Text:    req.Text,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message": msg,
		"woken":   woken,
	})
}

func (s *Server) handleReadBoard(w http.ResponseWriter, r *http.Request) {
	swarmID := r.PathValue("id")
	q := r.URL.Query()
	var since int64
	if raw := q.Get("since"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid since: %w", err))
			return
		}
		since = v
	}
	member := q.Get("member")
	if tokSwarm, tokMember, hasToken, valid := s.bearerIdentity(r); hasToken {
		if !valid || tokSwarm != swarmID {
			writeErr(w, http.StatusUnauthorized, errors.New("invalid member token"))
			return
		}
		member = tokMember
	}

	msgs, head, err := s.store.ReadBoard(r.Context(), swarmID, member, since, q.Get("channel"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []model.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"head":     head,
		"messages": msgs,
	})
}

func (s *Server) handleParkMember(w http.ResponseWriter, r *http.Request) {
	swarmID := r.PathValue("id")
	name := r.PathValue("name")
	if tokSwarm, tokMember, hasToken, valid := s.bearerIdentity(r); hasToken {
		if !valid || tokSwarm != swarmID || !strings.EqualFold(tokMember, name) {
			writeErr(w, http.StatusUnauthorized, errors.New("invalid member token"))
			return
		}
	}

	var req struct {
		Since    int64  `json:"since"`
		WakeMode string `json:"wake_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.store.ParkMember(r.Context(), store.ParkMemberParams{
		SwarmID:  swarmID,
		Name:     name,
		Since:    req.Since,
		WakeMode: req.WakeMode,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	ch, err := s.store.Watch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for snap := range ch {
		if err := enc.Encode(snap); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrAlreadyExists):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrInvalidArg):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
