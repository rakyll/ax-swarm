package axclient

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// CLIClient talks to AX by invoking the `ax` binary, automatically inheriting
// the active kubectx and AX's background cluster tunneling.
type CLIClient struct {
	Binary  string // defaults to "ax"
	Context string // optional --context flag
	Server  string // optional --server flag
}

func (c *CLIClient) bin() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "ax"
}

func (c *CLIClient) baseArgs(atespace string) []string {
	var args []string
	if atespace != "" {
		args = append(args, "-a", atespace)
	}
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	if c.Server != "" {
		args = append(args, "--server", c.Server)
	}
	return args
}

func (c *CLIClient) run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ax %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (c *CLIClient) CreateTask(ctx context.Context, spec TaskSpec) error {
	manifest := renderTaskYAML(spec)
	args := append(c.baseArgs(spec.Atespace), "apply", "-f", "-")
	_, err := c.run(ctx, manifest, args...)
	return err
}

func (c *CLIClient) ResumeTask(ctx context.Context, atespace, name string) error {
	args := append(c.baseArgs(atespace), "resume", "task", name)
	_, err := c.run(ctx, "", args...)
	return err
}

func (c *CLIClient) SuspendTask(ctx context.Context, atespace, name string) error {
	args := append(c.baseArgs(atespace), "suspend", "task", name)
	_, err := c.run(ctx, "", args...)
	return err
}

func (c *CLIClient) DeleteTask(ctx context.Context, atespace, name string) error {
	args := append(c.baseArgs(atespace), "delete", "task", name)
	_, err := c.run(ctx, "", args...)
	return err
}

func (c *CLIClient) ListTasks(ctx context.Context, atespace string) ([]TaskInfo, error) {
	args := append(c.baseArgs(atespace), "get", "tasks")
	out, err := c.run(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var tasks []TaskInfo
	for i, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) < 3 {
			continue
		}
		tasks = append(tasks, TaskInfo{
			Name:     fields[0],
			Atespace: fields[1],
			State:    TaskState(fields[2]),
		})
	}
	return tasks, nil
}

func renderTaskYAML(spec TaskSpec) string {
	var b strings.Builder
	b.WriteString("apiVersion: ax.io/v1alpha1\n")
	b.WriteString("kind: Task\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %q\n", spec.Name)
	b.WriteString("spec:\n")
	if spec.Image != "" {
		b.WriteString("  profile:\n")
		fmt.Fprintf(&b, "    image: %q\n", spec.Image)
	}
	if spec.Workspace != "" {
		b.WriteString("  workspace:\n")
		fmt.Fprintf(&b, "    name: %q\n", spec.Workspace)
	}
	if spec.Goal != "" {
		b.WriteString("  goal:\n")
		fmt.Fprintf(&b, "    text: %q\n", spec.Goal)
	}
	if len(spec.Commands) > 0 {
		b.WriteString("  commands:\n")
		for _, c := range spec.Commands {
			fmt.Fprintf(&b, "    - %q\n", c)
		}
	}
	if len(spec.Env) > 0 {
		keys := make([]string, 0, len(spec.Env))
		for k := range spec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  env:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    - name: %q\n", k)
			fmt.Fprintf(&b, "      value: %q\n", spec.Env[k])
		}
	}
	if spec.Debug {
		b.WriteString("  debug: true\n")
	}
	return b.String()
}
