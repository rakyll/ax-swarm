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
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ansiBold    = "1"
	ansiDim     = "2"
	ansiRed     = "31"
	ansiGreen   = "32"
	ansiYellow  = "33"
	ansiCyan    = "36"
	ansiMagenta = "35"
)

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// UI writes UTC-timestamped logs to stderr, with optional ANSI color and a live
// spinner status line when attached to a terminal. It stops and clears the
// status line before handing the terminal to interactive agy.
type UI struct {
	out   io.Writer
	in    io.Reader
	color bool
	live  bool

	mu      sync.Mutex
	shown   bool
	paused  bool
	text    string
	since   time.Time
	frame   int
	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool
}

func newUI(out io.Writer, in io.Reader, color, live bool) *UI {
	return &UI{
		out:   out,
		in:    in,
		color: color,
		live:  live,
	}
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func resolveColor(mode string, stderrTTY bool) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default:
		return stderrTTY && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == ""
	}
}

func (u *UI) Start() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.live || u.started {
		return
	}
	u.started = true
	u.stopCh = make(chan struct{})
	u.doneCh = make(chan struct{})
	go u.tick(u.stopCh, u.doneCh)
}

func (u *UI) Stop() {
	u.mu.Lock()
	if !u.started {
		u.clearLocked()
		u.live = false
		u.mu.Unlock()
		return
	}
	stopCh := u.stopCh
	doneCh := u.doneCh
	u.started = false
	u.mu.Unlock()

	close(stopCh)
	<-doneCh

	u.mu.Lock()
	u.clearLocked()
	u.text = ""
	u.live = false
	u.mu.Unlock()
}

func (u *UI) Paint(text string, codes ...string) string {
	if !u.color || len(codes) == 0 {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

func (u *UI) Log(msg string) {
	ts := time.Now().UTC().Format("15:04:05Z")
	indent := "\n" + strings.Repeat(" ", len(ts)+1)
	formatted := strings.ReplaceAll(msg, "\n", indent)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
	fmt.Fprintf(u.out, "%s %s\n", u.Paint(ts, ansiDim), formatted)
	u.drawLocked()
}

func (u *UI) Status(text string, restart bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text = text
	if restart || u.since.IsZero() {
		u.since = time.Now()
	}
	u.drawLocked()
}

func (u *UI) ClearStatus() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text = ""
	u.clearLocked()
}

func (u *UI) Ask(prompt string) (string, bool, error) {
	u.mu.Lock()
	u.clearLocked()
	u.paused = true
	fmt.Fprint(u.out, prompt)
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.paused = false
		u.mu.Unlock()
	}()

	reader := bufio.NewReader(u.in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", false, err
	}
	if err == io.EOF && line == "" {
		return "", false, nil
	}
	return strings.TrimSpace(line), true, nil
}

func (u *UI) tick(stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			u.mu.Lock()
			u.frame++
			u.drawLocked()
			u.mu.Unlock()
		}
	}
}

func (u *UI) clearLocked() {
	if u.shown {
		fmt.Fprint(u.out, "\r\x1b[2K")
		u.shown = false
	}
}

func (u *UI) drawLocked() {
	if !u.live || u.paused || u.text == "" {
		return
	}
	elapsed := int(time.Since(u.since).Seconds())
	frame := string(spinnerFrames[u.frame%len(spinnerFrames)])
	line := u.Paint(frame+" ", ansiCyan) +
		u.Paint(u.text, ansiBold) +
		u.Paint(fmt.Sprintf(" %ds", elapsed), ansiDim)
	fmt.Fprint(u.out, "\r\x1b[2K"+line)
	u.shown = true
}
