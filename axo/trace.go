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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Span kinds shown in the trace viewer.
const (
	SpanRun   = "run"   // the whole axo run
	SpanPhase = "phase" // setup, a round, review, synthesis
	SpanTask  = "task"  // one planned task
	SpanTry   = "attempt"
	SpanAX    = "ax"   // an AX control plane call
	SpanAgy   = "agy"  // one agy -p call (planner, worker, reviewer, synthesizer)
	SpanTool  = "tool" // one tool call made by an agy worker
)

// Span statuses.
const (
	StatusSpanRunning = "running"
	StatusSpanOK      = "ok"
	StatusSpanWarn    = "warn"
	StatusSpanError   = "error"
	StatusSpanSkipped = "skipped"
)

// maxSpanEvents and maxAttrLen bound how much one span can hold.
const (
	maxSpanEvents = 200
	maxAttrLen    = 8000
	maxSpans      = 5000
)

// SpanEvent is a timestamped note on a span.
type SpanEvent struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Span is one timed unit of work in an axo run.
type Span struct {
	ID     string            `json:"id"`
	Parent string            `json:"parent,omitempty"`
	Name   string            `json:"name"`
	Kind   string            `json:"kind"`
	Status string            `json:"status"`
	Start  time.Time         `json:"start"`
	End    *time.Time        `json:"end,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
	Events []SpanEvent       `json:"events,omitempty"`
}

func (s *Span) clone() Span {
	c := *s
	if s.End != nil {
		e := *s.End
		c.End = &e
	}
	if s.Attrs != nil {
		c.Attrs = make(map[string]string, len(s.Attrs))
		for k, v := range s.Attrs {
			c.Attrs[k] = v
		}
	}
	c.Events = append([]SpanEvent(nil), s.Events...)
	return c
}

// Tracer records the spans of one axo run in memory and streams changes to
// subscribers (the live trace viewer). All methods are safe for concurrent
// use, and a nil *Tracer is a valid no-op tracer.
type Tracer struct {
	mu      sync.Mutex
	spans   map[string]*Span
	order   []string
	nextID  int
	dropped int
	subs    map[chan []byte]struct{}
	now     func() time.Time
}

// NewTracer returns an empty tracer.
func NewTracer() *Tracer {
	return &Tracer{
		spans: make(map[string]*Span),
		subs:  make(map[chan []byte]struct{}),
		now:   time.Now,
	}
}

// SpanRef is a handle to a started span. A nil *SpanRef is a valid no-op.
type SpanRef struct {
	t  *Tracer
	id string
}

// ID returns the span's id, or "" for a no-op span.
func (r *SpanRef) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

func clip(s string) string {
	if len(s) > maxAttrLen {
		return s[:maxAttrLen] + "…"
	}
	return s
}

// Start begins a span under parent (nil for a root span).
func (t *Tracer) Start(parent *SpanRef, name, kind string, attrs map[string]string) *SpanRef {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if len(t.order) >= maxSpans {
		t.dropped++
		t.mu.Unlock()
		return nil
	}
	t.nextID++
	s := &Span{
		ID:     strconv.Itoa(t.nextID),
		Parent: parent.ID(),
		Name:   name,
		Kind:   kind,
		Status: StatusSpanRunning,
		Start:  t.now(),
	}
	for k, v := range attrs {
		if s.Attrs == nil {
			s.Attrs = make(map[string]string, len(attrs))
		}
		s.Attrs[k] = clip(v)
	}
	t.spans[s.ID] = s
	t.order = append(t.order, s.ID)
	t.publishLocked(s)
	t.mu.Unlock()
	return &SpanRef{t: t, id: s.ID}
}

func (r *SpanRef) update(fn func(s *Span)) {
	if r == nil || r.t == nil {
		return
	}
	t := r.t
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.spans[r.id]
	if !ok {
		return
	}
	fn(s)
	t.publishLocked(s)
}

// SetAttr sets attributes on the span.
func (r *SpanRef) SetAttr(kv ...string) {
	r.update(func(s *Span) {
		if s.Attrs == nil {
			s.Attrs = make(map[string]string)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			s.Attrs[kv[i]] = clip(kv[i+1])
		}
	})
}

// Event appends a timestamped note to the span.
func (r *SpanRef) Event(level, text string) {
	r.update(func(s *Span) {
		if len(s.Events) >= maxSpanEvents {
			s.Events = s.Events[1:]
		}
		s.Events = append(s.Events, SpanEvent{Time: r.t.now(), Level: level, Text: clip(text)})
	})
}

// End finishes the span with status. Ending an ended span is a no-op.
func (r *SpanRef) End(status string) {
	r.update(func(s *Span) {
		if s.End != nil {
			return
		}
		now := r.t.now()
		s.End = &now
		s.Status = status
	})
}

// EndErr ends the span as ok, or as an error carrying err's message.
func (r *SpanRef) EndErr(err error) {
	if err != nil {
		r.SetAttr("error", err.Error())
		r.End(StatusSpanError)
		return
	}
	r.End(StatusSpanOK)
}

// CloseOpen ends every span still running with status and a note. It is
// used when a run ends (or is canceled) with work still in flight.
func (t *Tracer) CloseOpen(status, note string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, id := range t.order {
		s := t.spans[id]
		if s.End != nil || s.Parent == "" {
			continue
		}
		s.End = &now
		s.Status = status
		if s.Attrs == nil {
			s.Attrs = make(map[string]string)
		}
		s.Attrs["note"] = note
		t.publishLocked(s)
	}
}

// Snapshot returns copies of all spans in start order.
func (t *Tracer) Snapshot() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Span, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.spans[id].clone())
	}
	return out
}

// traceMessage is one server-sent event payload.
type traceMessage struct {
	Type  string    `json:"type"` // "snapshot" or "span"
	Now   time.Time `json:"now"`
	Spans []Span    `json:"spans"`
}

func (t *Tracer) publishLocked(s *Span) {
	if len(t.subs) == 0 {
		return
	}
	data, err := json.Marshal(traceMessage{Type: "span", Now: t.now(), Spans: []Span{s.clone()}})
	if err != nil {
		return
	}
	for ch := range t.subs {
		select {
		case ch <- data:
		default:
			// A slow viewer is disconnected; its EventSource reconnects and
			// receives a fresh snapshot.
			delete(t.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns the current snapshot (as an encoded message) and a channel
// of subsequent span updates. The channel is closed if the subscriber falls
// behind or after cancel is called.
func (t *Tracer) Subscribe() (snapshot []byte, updates <-chan []byte, cancel func()) {
	ch := make(chan []byte, 1024)
	t.mu.Lock()
	spans := make([]Span, 0, len(t.order))
	for _, id := range t.order {
		spans = append(spans, t.spans[id].clone())
	}
	snap, _ := json.Marshal(traceMessage{Type: "snapshot", Now: t.now(), Spans: spans})
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	var once sync.Once
	return snap, ch, func() {
		once.Do(func() {
			t.mu.Lock()
			if _, ok := t.subs[ch]; ok {
				delete(t.subs, ch)
				close(ch)
			}
			t.mu.Unlock()
		})
	}
}

// WriteFile writes all spans as JSON to path.
func (t *Tracer) WriteFile(path string) error {
	spans := t.Snapshot()
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	data, err := json.MarshalIndent(map[string]any{"spans": spans}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write trace: %w", err)
	}
	return nil
}
