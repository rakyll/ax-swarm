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
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// BuildAgyArgs constructs the argument list for launching an interactive `agy`
// session with the initial `/swarm` prompt.
func BuildAgyArgs(cfg Config, prompt string) []string {
	var args []string
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if cfg.Mode != "" {
		args = append(args, "--mode", cfg.Mode)
	}
	if cfg.Project != "" {
		args = append(args, "--project", cfg.Project)
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "-i", prompt)
	return args
}

// RunInteractiveAgy hands the terminal over to an interactive `agy` session and
// waits until it exits. While `agy` is attached to the terminal, SIGINT is
// ignored by the parent so Ctrl-C works inside agy's TUI without aborting
// ax-swarm prematurely, while SIGTERM is forwarded so cleanup still runs.
func RunInteractiveAgy(agyBin string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(agyBin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", agyBin, err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigCh:
				if sig == syscall.SIGTERM && cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
				}
				// Ignore os.Interrupt in the parent: the foreground terminal
				// delivers SIGINT directly to agy's TUI.
			}
		}
	}()

	return cmd.Wait()
}
