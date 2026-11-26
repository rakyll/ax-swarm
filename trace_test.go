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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTracerNilSafe(t *testing.T) {
	var tr *Tracer
	sp := tr.Start(nil, "x", SpanRun, nil)
	sp.SetAttr("k", "v")
	sp.Event("info", "e")
	sp.EndErr(errors.New("boom"))
	tr.CloseOpen(StatusSpanWarn, "n")
	if got := tr.Snapshot(); got != nil {
		t.Fatalf("nil tracer snapshot = %v", got)
	}
}

func TestTracerSpansAndSubscribe(t *testing.T) {
	tr := NewTracer()
	root := tr.Start(nil, "run", SpanRun, map[string]string{"name": "r"})
	snap, updates, cancel := tr.Subscribe()
	defer cancel()
	var first traceMessage
	if err := json.Unmarshal(snap, &first); err != nil || first.Type != "snapshot" || len(first.Spans) != 1 {
		t.Fatalf("snapshot = %s (%v)", snap, err)
	}
	child := tr.Start(root, "child", SpanTask, nil)
	child.Event("warn", "careful")
	for i := 0; i < maxSpanEvents+10; i++ {
		child.Event("info", "spam")
	}
	child.End(StatusSpanOK)
	child.End(StatusSpanError) // ending twice is a no-op

	select {
	case msg := <-updates:
		var m traceMessage
		if err := json.Unmarshal(msg, &m); err != nil || m.Type != "span" || m.Spans[0].Name != "child" {
			t.Fatalf("update = %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no update")
	}
	spans := tr.Snapshot()
	c := spans[1]
	if c.Parent != spans[0].ID || c.Status != StatusSpanOK || c.End == nil {
		t.Errorf("child span = %+v", c)
	}
	if len(c.Events) != maxSpanEvents {
		t.Errorf("events = %d, want capped at %d", len(c.Events), maxSpanEvents)
	}

	open := tr.Start(root, "open", SpanTool, nil)
	_ = open
	tr.CloseOpen(StatusSpanWarn, "canceled")
	for _, s := range tr.Snapshot() {
		if s.Parent != "" && s.End == nil {
			t.Errorf("span %s still open after CloseOpen", s.Name)
		}
	}
	if tr.Snapshot()[0].End != nil {
		t.Errorf("CloseOpen ended the root span")
	}
}

func TestTracerWriteFile(t *testing.T) {
	tr := NewTracer()
	tr.Start(nil, "run", SpanRun, nil).End(StatusSpanOK)
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := tr.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var out struct{ Spans []Span }
	if err := json.Unmarshal(data, &out); err != nil || len(out.Spans) != 1 {
		t.Fatalf("trace file = %s (%v)", data, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("trace file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func spanByName(spans []Span, name string) (Span, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return Span{}, false
}

func TestOrchestratorRecordsTrace(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRounds = 1
	fax, fagy := newFakeAX(), newFakeAgy()
	fagy.failFirst["a"] = true
	o, _ := newTestOrchestrator(cfg, fax, fagy)
	o.Trace = NewTracer()
	if _, err := collect(t, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spans := o.Trace.Snapshot()
	byID := map[string]Span{}
	for _, s := range spans {
		byID[s.ID] = s
		if s.End == nil {
			t.Errorf("span %q (%s) never ended", s.Name, s.Kind)
		}
	}
	parentName := func(s Span) string { return byID[s.Parent].Name }

	root := spans[0]
	if root.Kind != SpanRun || root.Status != StatusSpanOK || root.Attrs["goal"] != cfg.Goal {
		t.Fatalf("root = %+v", root)
	}
	for _, name := range []string{"setup", "CreateGateway axo-test", "CreateWorkspace axo-test", "wait for Workspace Ready", "plan", "round 1", "synthesis"} {
		if _, ok := spanByName(spans, name); !ok {
			t.Errorf("missing span %q", name)
		}
	}
	if s, _ := spanByName(spans, "CreateGateway axo-test"); parentName(s) != "setup" || s.Kind != SpanAX {
		t.Errorf("CreateGateway span = %+v", s)
	}
	var graph []graphNode
	if err := json.Unmarshal([]byte(root.Attrs["graph"]), &graph); err != nil || len(graph) != 3 {
		t.Fatalf("graph attr = %q (%v)", root.Attrs["graph"], err)
	}
	if g := graph[2]; g.ID != "c" || strings.Join(g.DependsOn, ",") != "a,b" || g.Round != 1 || g.Goal != "combine" {
		t.Errorf("graph node c = %+v", g)
	}
	if s, _ := spanByName(spans, "plan"); s.Attrs["tasks"] != "3" || s.Attrs["role"] != "planner" || s.Attrs["prompt"] == "" {
		t.Errorf("plan span attrs = %v", s.Attrs)
	}
	taskA, ok := spanByName(spans, "task a")
	if !ok || parentName(taskA) != "round 1" || taskA.Status != StatusSpanOK {
		t.Fatalf("task a span = %+v", taskA)
	}
	// Task a failed once, then succeeded: two attempts with AX calls and a worker.
	var attempts []Span
	for _, s := range spans {
		if s.Parent == taskA.ID && s.Kind == SpanTry {
			attempts = append(attempts, s)
		}
	}
	if len(attempts) != 2 || attempts[0].Status != StatusSpanError || attempts[1].Status != StatusSpanOK {
		t.Fatalf("task a attempts = %+v", attempts)
	}
	kids := map[string]Span{}
	for _, s := range spans {
		if s.Parent == attempts[1].ID {
			kids[s.Name] = s
		}
	}
	for _, name := range []string{"CreateTask axo-test-a-r2", "start axo-test-a-r2", "worker a", "DeleteTask axo-test-a-r2"} {
		if _, ok := kids[name]; !ok {
			t.Errorf("attempt 2 missing child %q (have %v)", name, kids)
		}
	}
	worker := kids["worker a"]
	var tool Span
	for _, s := range spans {
		if s.Parent == worker.ID && s.Kind == SpanTool {
			tool = s
		}
	}
	if tool.Name != "ssh" || tool.Status != StatusSpanOK || tool.Attrs["exit_code"] != "0" || tool.Attrs["command"] != "ax ssh x -- ls" {
		t.Errorf("tool span = %+v", tool)
	}
}

func TestToolLabel(t *testing.T) {
	for tool, want := range map[string]string{
		"run_command":                "ssh",
		"view_file":                  "read_file",
		"write_to_file":              "write_file",
		"replace_file_content":       "write_file",
		"multi_replace_file_content": "write_file",
		"":                           "tool",
	} {
		if got := toolLabel(tool); got != want {
			t.Errorf("toolLabel(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestParseToolPath(t *testing.T) {
	ev, ok := parseAgyStreamLine(`{"event":"step_update","step_update":{"step_index":7,"step_type":"tool","state":"ACTIVE","tool_name":"view_file","tool_info":{"parameters":{"AbsolutePath":"/tmp/x.go"}}}}`)
	if !ok || ev.StepIndex != 7 || ev.ToolPath != "/tmp/x.go" || toolLabel(ev.ToolName) != "read_file" {
		t.Fatalf("event = %+v", ev)
	}
}

// newTestTraceServer starts a server and returns it with a cookie-aware
// client that does not follow redirects.
func newTestTraceServer(t *testing.T) (*TraceServer, *http.Client) {
	t.Helper()
	tr := NewTracer()
	tr.Start(nil, "axo test", SpanRun, map[string]string{"name": "test"})
	srv, err := StartTraceServer(tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar:           jar,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, c
}

func get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTraceServerAccessControl(t *testing.T) {
	srv, c := newTestTraceServer(t)
	base := "http://" + srv.host

	if !strings.HasPrefix(srv.host, "127.0.0.1:") {
		t.Fatalf("server listens on %s, want loopback", srv.host)
	}
	if r := get(t, c, base+"/"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated / = %d", r.StatusCode)
	}
	if r := get(t, c, base+"/events"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /events = %d", r.StatusCode)
	}
	if r := get(t, c, base+"/login?token=nope"); r.StatusCode != http.StatusForbidden {
		t.Errorf("bad token = %d", r.StatusCode)
	}

	login, err := srv.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	r := get(t, c, login)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("login = %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	setCookie := r.Header.Get("Set-Cookie")
	for _, want := range []string{sessionCookie + "=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(setCookie, want) {
			t.Errorf("Set-Cookie %q missing %q", setCookie, want)
		}
	}
	// The login token is single use.
	if r := get(t, c, login); r.StatusCode != http.StatusForbidden {
		t.Errorf("reused token = %d", r.StatusCode)
	}

	r = get(t, c, base+"/")
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), "app.js") {
		t.Fatalf("/ = %d %s", r.StatusCode, body)
	}
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP allows unsafe sources: %q", csp)
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := r.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS header set")
	}
	if r := get(t, c, base+"/app.js"); r.StatusCode != http.StatusOK {
		t.Errorf("/app.js = %d", r.StatusCode)
	}

	// DNS rebinding: a foreign Host header is refused even with a session.
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Host = "evil.example:" + strings.Split(srv.host, ":")[1]
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host = %d", resp.StatusCode)
	}

	// Only GET/HEAD.
	resp, err = c.Post(base+"/", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", resp.StatusCode)
	}
}

func TestTraceServerStreamsEvents(t *testing.T) {
	srv, c := newTestTraceServer(t)
	login, _ := srv.LoginURL()
	get(t, c, login)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.host+"/events", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	next := func() traceMessage {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("reading stream: %v", err)
			}
			if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				var m traceMessage
				if err := json.Unmarshal([]byte(data), &m); err != nil {
					t.Fatalf("bad event %q: %v", data, err)
				}
				return m
			}
		}
	}
	if m := next(); m.Type != "snapshot" || len(m.Spans) != 1 || m.Spans[0].Name != "axo test" {
		t.Fatalf("first event = %+v", m)
	}
	root := &SpanRef{t: srv.tracer, id: "1"}
	srv.tracer.Start(root, "live span", SpanTask, nil)
	if m := next(); m.Type != "span" || m.Spans[0].Name != "live span" {
		t.Fatalf("update event = %+v", m)
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !l.allow() {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	if l.allow() {
		t.Fatalf("request beyond burst allowed")
	}
}
