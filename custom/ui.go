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
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
	ansiCyan   = "36"
)

// UI writes UTC-timestamped logs to stderr with optional ANSI color. It is
// used by headless mode, which matches ax-swarm's headless output.
type UI struct {
	out   io.Writer
	in    io.Reader
	color bool

	mu sync.Mutex
}

func newUI(out io.Writer, in io.Reader, color, _ bool) *UI {
	return &UI{out: out, in: in, color: color}
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

// Paint wraps text in ANSI codes when color is enabled.
func (u *UI) Paint(text string, codes ...string) string {
	if !u.color || len(codes) == 0 {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

// Log writes one timestamped line.
func (u *UI) Log(msg string) {
	ts := time.Now().UTC().Format("15:04:05Z")
	formatted := strings.ReplaceAll(msg, "\n", "\n"+strings.Repeat(" ", len(ts)+1))
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.out, "%s %s\n", u.Paint(ts, ansiDim), formatted)
}

// Ask prints prompt and reads one line of input.
func (u *UI) Ask(prompt string) (string, bool, error) {
	u.mu.Lock()
	fmt.Fprint(u.out, prompt)
	u.mu.Unlock()
	line, err := bufio.NewReader(u.in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", false, err
	}
	if err == io.EOF && line == "" {
		return "", false, nil
	}
	return strings.TrimSpace(line), true, nil
}
