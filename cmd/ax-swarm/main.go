package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/rakyll/ax-swarm/internal/axclient"
	"github.com/rakyll/ax-swarm/internal/client"
	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/reconciler"
	"github.com/rakyll/ax-swarm/internal/server"
	"github.com/rakyll/ax-swarm/internal/store"
)

func defaultServerURL() string {
	if v := os.Getenv("SWARM_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8787"
}

func defaultDataDir() string {
	if v := os.Getenv("AX_SWARM_DATA_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ax-swarm", "data")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "create":
		err = cmdCreate(args)
	case "title":
		err = cmdTitle(args)
	case "add":
		err = cmdAdd(args)
	case "kill":
		err = cmdKill(args)
	case "status":
		err = cmdStatus(args)
	case "post":
		err = cmdPost(args)
	case "read":
		err = cmdRead(args)
	case "wait":
		err = cmdWait(args)
	case "swarm":
		err = cmdSwarmViewer(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-swarm %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ax-swarm — external swarm coordinator & wake-on-@mention control plane for AX

Usage:
  ax-swarm serve   [--addr :8787] [--data-dir ~/.ax-swarm/data] [--ax cli|mock] [--suspend-grace 15s]
  ax-swarm create  <swarm-id> [--title "..."] [--atespace swarm-<id>] [--workspace <ws>]
  ax-swarm title   <swarm-id> <title>
  ax-swarm add     <swarm-id> <name> --brief "..." [--workspace <ws>] [--runtime <rt>] [--model <m>] [--image <img>]
  ax-swarm kill    <swarm-id> <name>
  ax-swarm status  <swarm-id>
  ax-swarm post    <swarm-id> [--as coordinator] [--channel main] <message...>
  ax-swarm read    <swarm-id> [--since 0] [--as <member>] [--channel <ch>]
  ax-swarm wait    <swarm-id> --as <member> [--since 0] [--wake-mode mentions|any]
  ax-swarm swarm   --json [--watch | --roster-only] [--db <db>] <swarm-id...>
`)
}

func newClient(serverURL string) *client.Client {
	return client.New(serverURL, os.Getenv("SWARM_TOKEN"))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for swarm state")
	axMode := fs.String("ax", "cli", "AX backend: cli or mock")
	axBin := fs.String("ax-bin", "ax", "path to ax binary")
	axCtx := fs.String("ax-context", "", "optional kubectx passed to ax")
	axServer := fs.String("ax-server", "", "optional --server passed to ax")
	publicURL := fs.String("public-url", "", "SWARM_API_URL injected into AX Tasks (defaults to http://<addr>)")
	defaultImg := fs.String("default-image", "", "default container image for worker Tasks")
	poll := fs.Duration("poll-interval", 2*time.Second, "reconciler poll interval")
	grace := fs.Duration("suspend-grace", 15*time.Second, "grace period before suspending waiting tasks")
	_ = fs.Parse(args)

	st, err := store.NewFileStore(*dataDir)
	if err != nil {
		return err
	}

	var ax axclient.Client
	switch *axMode {
	case "cli":
		ax = &axclient.CLIClient{Binary: *axBin, Context: *axCtx, Server: *axServer}
	case "mock":
		ax = axclient.NewMockClient()
	default:
		return fmt.Errorf("invalid --ax mode %q (want cli or mock)", *axMode)
	}

	swarmURL := *publicURL
	if swarmURL == "" {
		swarmURL = "http://" + *addr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rec := reconciler.New(st, ax, reconciler.Config{
		PollInterval: *poll,
		SuspendGrace: *grace,
		SwarmAPIURL:  swarmURL,
		DefaultImage: *defaultImg,
	})
	go rec.Run(ctx)

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(st),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("ax-swarm listening on %s (data=%s, ax=%s)", *addr, *dataDir, *axMode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	title := fs.String("title", "", "swarm title")
	atespace := fs.String("atespace", "", "AX atespace (default: swarm-<id>)")
	ws := fs.String("workspace", "", "default AX Workspace name for workers")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ax-swarm create <swarm-id>")
	}
	sw, err := newClient(*srv).CreateSwarm(context.Background(), store.CreateSwarmParams{
		SwarmID:      fs.Arg(0),
		Atespace:     *atespace,
		Title:        *title,
		WorkspaceRef: *ws,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created swarm %s (atespace=%s)\n", sw.SwarmID, sw.Atespace)
	return nil
}

func cmdTitle(args []string) error {
	fs := flag.NewFlagSet("title", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: ax-swarm title <swarm-id> <title>")
	}
	sw, err := newClient(*srv).SetTitle(context.Background(), fs.Arg(0), strings.Join(fs.Args()[1:], " "))
	if err != nil {
		return err
	}
	fmt.Printf("swarm %s title: %s\n", sw.SwarmID, sw.Title)
	return nil
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	brief := fs.String("brief", "", "worker assignment posted to the board")
	role := fs.String("role", "worker", "member role")
	modelName := fs.String("model", "", "model label")
	runtime := fs.String("runtime", "", "worker harness runtime (e.g. claude-code, gemini-cli)")
	image := fs.String("image", "", "AX Task container image")
	ws := fs.String("workspace", "", "AX Workspace name")
	nodeID := fs.String("node-id", "ax", "node label for UI badge")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: ax-swarm add <swarm-id> <name> --brief '...'")
	}
	resp, err := newClient(*srv).AddMember(context.Background(), store.AddMemberParams{
		SwarmID:   fs.Arg(0),
		Name:      fs.Arg(1),
		Role:      *role,
		Model:     *modelName,
		Runtime:   *runtime,
		Image:     *image,
		Workspace: *ws,
		Brief:     *brief,
		NodeID:    *nodeID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("added %s (task=%s, phase=%s, seq=#%d)\n", resp.Member.Name, resp.Member.TaskName, resp.Member.Phase, resp.BriefMessage.Seq)
	return nil
}

func cmdKill(args []string) error {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: ax-swarm kill <swarm-id> <name>")
	}
	m, err := newClient(*srv).KillMember(context.Background(), fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	fmt.Printf("killed %s (phase=%s)\n", m.Name, m.Phase)
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ax-swarm status <swarm-id>")
	}
	sw, err := newClient(*srv).GetSwarm(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("Swarm: %s  Title: %q  Atespace: %s  Head: #%d\n\n", sw.SwarmID, sw.Title, sw.Atespace, sw.HeadSeq)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tPHASE\tDESIRED\tWAKE\tREAD\tRUNTIME")
	names := make([]string, 0, len(sw.Members))
	for n := range sw.Members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := sw.Members[n]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t#%d\t%s\n", m.Name, m.Role, m.Phase, m.DesiredState, m.WakeMode, m.LastReadSeq, m.Runtime)
	}
	return tw.Flush()
}

func cmdPost(args []string) error {
	fs := flag.NewFlagSet("post", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	as := fs.String("as", "coordinator", "posting agent name")
	ch := fs.String("channel", model.DefaultChannel, "channel name")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: ax-swarm post <swarm-id> <text...>")
	}
	resp, err := newClient(*srv).PostMessage(context.Background(), store.PostMessageParams{
		SwarmID: fs.Arg(0),
		Agent:   *as,
		Channel: *ch,
		Text:    strings.Join(fs.Args()[1:], " "),
	})
	if err != nil {
		return err
	}
	fmt.Printf("#%d [%s] %s (woken: %v)\n", resp.Message.Seq, resp.Message.Agent, resp.Message.Text, resp.Woken)
	return nil
}

func cmdRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	since := fs.Int64("since", 0, "return messages with seq > since")
	as := fs.String("as", "", "optional member name (advances last_read_seq)")
	ch := fs.String("channel", "", "optional channel filter")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ax-swarm read <swarm-id>")
	}
	resp, err := newClient(*srv).ReadBoard(context.Background(), fs.Arg(0), *as, *since, *ch)
	if err != nil {
		return err
	}
	for _, m := range resp.Messages {
		fmt.Printf("#%d (%s) [%s] %s\n", m.Seq, m.Time.Format(time.RFC3339), m.Agent, m.Text)
	}
	return nil
}

func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	as := fs.String("as", os.Getenv("SWARM_MEMBER"), "member name")
	since := fs.Int64("since", 0, "last seen sequence number")
	wake := fs.String("wake-mode", model.WakeMentions, "mentions or any")
	_ = fs.Parse(args)
	if fs.NArg() < 1 || *as == "" {
		return fmt.Errorf("usage: ax-swarm wait <swarm-id> --as <member> [--since N]")
	}
	res, err := newClient(*srv).ParkMember(context.Background(), store.ParkMemberParams{
		SwarmID:  fs.Arg(0),
		Name:     *as,
		Since:    *since,
		WakeMode: *wake,
	})
	if err != nil {
		return err
	}
	if res.ParkBlocked {
		fmt.Printf("park blocked (%d unread matching messages)\n", len(res.Unread))
		for _, m := range res.Unread {
			fmt.Printf("#%d [%s] %s\n", m.Seq, m.Agent, m.Text)
		}
		return nil
	}
	fmt.Printf("parked %s (waiting since #%d, wake=%s)\n", res.Member.Name, res.Member.WaitSince, res.Member.WakeMode)
	return nil
}

// cmdSwarmViewer implements Antigravity swarm_board's remote_board CLI protocol:
//   ax-swarm swarm --json --watch [--db <db>] <swarm-id>
//   ax-swarm swarm --json --roster-only [--db <db>] <swarm-id...>
func cmdSwarmViewer(args []string) error {
	fs := flag.NewFlagSet("swarm", flag.ExitOnError)
	srv := fs.String("server", defaultServerURL(), "ax-swarm server URL")
	asJSON := fs.Bool("json", false, "output JSON / NDJSON")
	watch := fs.Bool("watch", false, "stream NDJSON snapshots until stdin EOF")
	rosterOnly := fs.Bool("roster-only", false, "print rosters for listed swarm IDs and exit")
	_ = fs.String("db", "", "optional db/atespace hint from swarm.json anchor")
	_ = fs.Parse(args)

	if !*asJSON || fs.NArg() < 1 {
		return fmt.Errorf("usage: ax-swarm swarm --json [--watch|--roster-only] [--db <db>] <swarm-id...>")
	}

	c := newClient(*srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Exit cleanly when stdin closes (EOF), matching swarm_board sidecar lifecycle.
	if *watch {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
		enc := json.NewEncoder(os.Stdout)
		err := c.Watch(ctx, fs.Arg(0), func(snap model.WatchSnapshot) error {
			return enc.Encode(snap)
		})
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	if *rosterOnly {
		enc := json.NewEncoder(os.Stdout)
		for _, id := range fs.Args() {
			sw, err := c.GetSwarm(ctx, id)
			if err != nil {
				continue
			}
			snap := model.WatchSnapshot{
				Version:  1,
				SwarmID:  sw.SwarmID,
				Title:    sw.Title,
				Head:     sw.HeadSeq,
				Since:    0,
				Channels: sw.Channels,
				Members:  sw.Members,
			}
			if err := enc.Encode(snap); err != nil {
				return err
			}
		}
		return nil
	}

	// One-shot full snapshot.
	sw, err := c.GetSwarm(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	readResp, err := c.ReadBoard(ctx, fs.Arg(0), "", 0, "")
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(model.WatchSnapshot{
		Version:  1,
		SwarmID:  sw.SwarmID,
		Title:    sw.Title,
		Head:     readResp.Head,
		Since:    0,
		Channels: sw.Channels,
		Members:  sw.Members,
		Messages: readResp.Messages,
	})
}
