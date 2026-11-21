package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rakyll/ax-swarm/internal/client"
	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/server"
	"github.com/rakyll/ax-swarm/internal/store"
)

// pipeTransport runs an http.Handler in-process over io.Pipe without opening a
// TCP socket, supporting both regular JSON endpoints and streaming NDJSON Watch.
type pipeTransport struct {
	handler http.Handler
}

type pipeResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	pw          *io.PipeWriter
	headerReady chan struct{}
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }
func (w *pipeResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	close(w.headerReady)
}
func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.pw.Write(p)
}
func (w *pipeResponseWriter) Flush() {}

func (t *pipeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	rw := &pipeResponseWriter{
		header:      make(http.Header),
		status:      http.StatusOK,
		pw:          pw,
		headerReady: make(chan struct{}),
	}
	go func() {
		defer pw.Close()
		t.handler.ServeHTTP(rw, req)
		if !rw.wroteHeader {
			rw.WriteHeader(http.StatusOK)
		}
	}()
	select {
	case <-rw.headerReady:
	case <-req.Context().Done():
		_ = pr.Close()
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: rw.status,
		Header:     rw.header.Clone(),
		Body:       pr,
		Request:    req,
	}, nil
}

func newTestClient(h http.Handler, token string) *client.Client {
	c := client.New("http://in-memory", token)
	c.HTTP = &http.Client{Transport: &pipeTransport{handler: h}}
	return c
}

func TestServerEndToEndAndTokenIdentity(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := server.New(st)

	// Quick sanity check on /healthz.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", rec.Code)
	}

	admin := newTestClient(srv, "")
	if _, err := admin.CreateSwarm(ctx, store.CreateSwarmParams{
		SwarmID: "s1",
		Title:   "e2e test",
	}); err != nil {
		t.Fatalf("CreateSwarm: %v", err)
	}

	added, err := admin.AddMember(ctx, store.AddMemberParams{
		SwarmID: "s1",
		Name:    "parser",
		Brief:   "do work",
	})
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Worker authenticates with its token; even if it tries to spoof agent="coordinator",
	// the server enforces agent="parser".
	worker := newTestClient(srv, added.Token)
	posted, err := worker.PostMessage(ctx, store.PostMessageParams{
		SwarmID: "s1",
		Agent:   "coordinator", // spoof attempt
		Text:    "@coordinator [done] finished",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if posted.Message.Agent != "parser" {
		t.Fatalf("expected server-enforced agent=parser, got %q", posted.Message.Agent)
	}

	// Watch stream delivers initial snapshot + live delta.
	watchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var snaps []model.WatchSnapshot
	done := make(chan error, 1)
	go func() {
		done <- admin.Watch(watchCtx, "s1", func(s model.WatchSnapshot) error {
			snaps = append(snaps, s)
			if len(snaps) == 2 {
				cancel()
			}
			return nil
		})
	}()

	time.Sleep(50 * time.Millisecond)
	_, err = admin.PostMessage(ctx, store.PostMessageParams{
		SwarmID: "s1",
		Agent:   "coordinator",
		Text:    "@parser nice work",
	})
	if err != nil {
		t.Fatalf("PostMessage #2: %v", err)
	}

	<-done
	if len(snaps) < 2 {
		t.Fatalf("expected at least 2 watch snapshots, got %d", len(snaps))
	}
	if snaps[0].Since != 0 || snaps[1].Head != 3 {
		t.Fatalf("unexpected snapshots: first=%+v second=%+v", snaps[0], snaps[1])
	}
}
