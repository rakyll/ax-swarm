package store

import (
	"context"
	"testing"
	"time"

	"github.com/rakyll/ax-swarm/internal/model"
)

func TestFileStoreWakeAndParkProtection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	sw, err := st.CreateSwarm(ctx, CreateSwarmParams{
		SwarmID:      "sw1",
		Title:        "parser rewrite",
		WorkspaceRef: "golang",
	})
	if err != nil {
		t.Fatalf("CreateSwarm: %v", err)
	}
	if sw.Atespace != "swarm-sw1" {
		t.Fatalf("expected default atespace swarm-sw1, got %q", sw.Atespace)
	}

	m, brief, err := st.AddMember(ctx, AddMemberParams{
		SwarmID:  "sw1",
		Name:     "parser-1",
		Brief:    "Fix parser tests",
		Channels: []string{"parser"},
	})
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if m.Token == "" {
		t.Fatalf("expected non-empty token")
	}
	if brief.Seq != 1 || m.LastReadSeq != 0 {
		t.Fatalf("expected brief seq=1 and start LastReadSeq=0, got seq=%d read=%d", brief.Seq, m.LastReadSeq)
	}

	// Reading advances LastReadSeq.
	msgs, head, err := st.ReadBoard(ctx, "sw1", "parser-1", 0, "")
	if err != nil || head != 1 || len(msgs) != 1 {
		t.Fatalf("ReadBoard: err=%v head=%d len=%d", err, head, len(msgs))
	}

	// Park at #1 succeeds.
	park, err := st.ParkMember(ctx, ParkMemberParams{
		SwarmID: "sw1",
		Name:    "parser-1",
		Since:   1,
	})
	if err != nil || park.ParkBlocked || park.Member.DesiredState != model.DesiredWaiting {
		t.Fatalf("ParkMember: err=%v blocked=%v desired=%s", err, park.ParkBlocked, park.Member.DesiredState)
	}

	// Prefix mention (@parser) wakes parser-1.
	msg2, woken, err := st.PostMessage(ctx, PostMessageParams{
		SwarmID: "sw1",
		Agent:   "verifier",
		Text:    "@parser please check edge case",
	})
	if err != nil || msg2.Seq != 2 {
		t.Fatalf("PostMessage: err=%v seq=%d", err, msg2.Seq)
	}
	if len(woken) != 1 || woken[0] != "parser-1" {
		t.Fatalf("expected woken=[parser-1], got %v", woken)
	}

	// Trying to park with stale since=1 is blocked and returns unread #2.
	park2, err := st.ParkMember(ctx, ParkMemberParams{
		SwarmID: "sw1",
		Name:    "parser-1",
		Since:   1,
	})
	if err != nil || !park2.ParkBlocked || len(park2.Unread) != 1 || park2.Unread[0].Seq != 2 {
		t.Fatalf("expected park blocked with unread #2, got %+v err=%v", park2, err)
	}

	// Reopening from disk preserves state and tokens.
	st2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	sid, mem, ok := st2.AuthenticateToken(ctx, m.Token)
	if !ok || sid != "sw1" || mem != "parser-1" {
		t.Fatalf("AuthenticateToken after reload: ok=%v sid=%q mem=%q", ok, sid, mem)
	}
}

func TestFileStoreWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	_, _ = st.CreateSwarm(ctx, CreateSwarmParams{SwarmID: "w1", Title: "watch test"})

	ch, err := st.Watch(ctx, "w1")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	initSnap := <-ch
	if initSnap.Since != 0 || initSnap.SwarmID != "w1" {
		t.Fatalf("unexpected initial snapshot: %+v", initSnap)
	}

	_, _, err = st.PostMessage(ctx, PostMessageParams{
		SwarmID: "w1",
		Agent:   "coordinator",
		Text:    "@all hello",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	select {
	case delta := <-ch:
		if delta.Head != 1 || len(delta.Messages) != 1 || delta.Messages[0].Seq != 1 {
			t.Fatalf("unexpected delta snapshot: %+v", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch delta")
	}
}
