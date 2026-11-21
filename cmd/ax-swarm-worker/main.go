package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rakyll/ax-swarm/internal/client"
	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/store"
)

// ax-swarm-worker is the in-sandbox turn loop executed inside an AX Task.
// On boot/resume it fetches unread board messages, invokes the configured
// headless agent harness, and calls ParkMember so ax-swarm-server suspends the Task.
func main() {
	apiURL := flag.String("api-url", os.Getenv("SWARM_API_URL"), "ax-swarm-server URL")
	swarmID := flag.String("swarm", os.Getenv("SWARM_ID"), "swarm ID")
	member := flag.String("member", os.Getenv("SWARM_MEMBER"), "member name")
	token := flag.String("token", os.Getenv("SWARM_TOKEN"), "member bearer token (optional if Gateway injects it)")
	harness := flag.String("harness", os.Getenv("SWARM_HARNESS_CMD"), "shell command to run for each turn (receives prompt on stdin)")
	stateDir := flag.String("state-dir", "/workspace/.ax-swarm", "durable state directory inside the AX workspace")
	flag.Parse()

	if *apiURL == "" || *swarmID == "" || *member == "" {
		log.Fatalf("SWARM_API_URL, SWARM_ID, and SWARM_MEMBER are required")
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Fatalf("create state dir: %v", err)
	}

	c := client.New(*apiURL, *token)
	ctx := context.Background()
	cursorPath := filepath.Join(*stateDir, "last_seq")
	since := readCursor(cursorPath)

	for {
		resp, err := c.ReadBoard(ctx, *swarmID, *member, since, "")
		if err != nil {
			log.Fatalf("read board: %v", err)
		}

		if len(resp.Messages) > 0 {
			prompt := formatPrompt(*member, resp.Messages)
			if err := runHarness(ctx, *harness, prompt); err != nil {
				log.Printf("harness exited with error: %v", err)
				_, _ = c.PostMessage(ctx, store.PostMessageParams{
					SwarmID: *swarmID,
					Agent:   *member,
					Text:    fmt.Sprintf("@coordinator [error] worker %s failed: %v", *member, err),
				})
				os.Exit(1)
			}
			since = resp.Head
			writeCursor(cursorPath, since)
		}

		park, err := c.ParkMember(ctx, store.ParkMemberParams{
			SwarmID:  *swarmID,
			Name:     *member,
			Since:    since,
			WakeMode: model.WakeMentions,
		})
		if err != nil {
			log.Fatalf("park member: %v", err)
		}
		if park.ParkBlocked {
			// New matching messages arrived while we were working; run another turn immediately.
			continue
		}
		log.Printf("worker %s parked at #%d; exiting cleanly for AX SuspendTask", *member, since)
		return
	}
}

func formatPrompt(member string, msgs []model.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are swarm member @%s. New board messages:\n\n", member)
	for _, m := range msgs {
		fmt.Fprintf(&b, "#%d [%s]: %s\n", m.Seq, m.Agent, m.Text)
	}
	b.WriteString("\nPost updates with `ax-swarm post` and tag the next peer or @coordinator when done.\n")
	return b.String()
}

func runHarness(ctx context.Context, harnessCmd, prompt string) error {
	if strings.TrimSpace(harnessCmd) == "" {
		log.Printf("no SWARM_HARNESS_CMD configured; received prompt:\n%s", prompt)
		return nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", harnessCmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func readCursor(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func writeCursor(path string, seq int64) {
	_ = os.WriteFile(path, []byte(strconv.FormatInt(seq, 10)), 0o644)
}
