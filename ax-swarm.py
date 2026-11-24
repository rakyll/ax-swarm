#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""ax-swarm: works on a goal with a swarm of AX tasks planned by Gemini.

A Gemini model plans. Each round it either answers the goal or fans the work
out into steps that run in parallel, one AX task per step on copies of the
same Workspace. A step can name earlier steps as inputs: it starts once they
finish, with their outputs appended to its goal (fan-in). Every output goes
back to the planner, which keeps going until it can answer.

The script shells out to the ax CLI. It creates, all named after the swarm:

  - a Secret holding GEMINI_API_KEY (with kubectl), which never enters a
    sandbox;
  - a Gateway that injects the key into the tasks' Gemini API requests and lets
    them reach PyPI, npm and GitHub;
  - a Model naming the Gemini model the workers use;
  - a Workspace that clones --repo and installs a small Gemini worker agent
    that can run shell commands in its sandbox;
  - one Task per step,

and deletes them when it exits, unless --keep is given.

Usage:
  GEMINI_API_KEY=... ./ax-swarm.py --repo https://github.com/chalk/chalk \\
      "Why would I use this library, and what are its weak spots?"

  Every model input and output is logged to stderr with UTC timestamps: the
  planner's prompts and plans, each worker's goal and report, and a transcript
  of the worker's tool calls and their results. -q logs only progress.

Environment:
  GEMINI_API_KEY  Gemini API key for the planner and, through the Gateway, the
                  workers
  AX_ROOT         AX checkout whose bin/ the defaults below read (default: ~/ax)
  AX_BIN          ax CLI to use (default: $AX_ROOT/bin/ax, else ax on PATH)
  AX_TASK_IMAGE   Digest-pinned runner image with python3 for the Workspace
                  (default: the ax-runner that `make build-local-cluster`
                  pushed to localhost:5001, read from
                  $AX_ROOT/bin/ax-runner.digest)
"""

import argparse
import concurrent.futures
import json
import os
import pathlib
import re
import secrets
import shutil
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

AX_ROOT = pathlib.Path(os.environ.get("AX_ROOT") or pathlib.Path.home() / "ax").expanduser()
GEMINI_API = "https://generativelanguage.googleapis.com/v1beta"
DEFAULT_MODEL = "gemini-3.6-flash"
OUTPUT_FILE = "/workspace/.ax/output"
# A worker's goal with its inputs stays well within the 31 KiB the control
# plane allows for a task's launch configuration.
MAX_GOAL_BYTES = 20 << 10
MAX_HISTORY_OUTPUT_BYTES = 16 << 10
STEP_NAME_RE = re.compile(r"^[a-z]([-a-z0-9]{0,22}[a-z0-9])?$")

# The worker agent installed in the Workspace. It gives Gemini the task's goal
# and two tools on the sandbox, runs until the model answers, and writes the
# answer, or the error that stopped it, to AX_OUTPUT_FILE. Before that, it
# writes its tool calls, their results and the model's errors next to it, to
# TRANSCRIPT_FILE, which the swarm logs unless --quiet is given.
TRANSCRIPT_FILE = "/workspace/.ax/swarm-transcript"
WORKER_PY = r'''import json, os, pathlib, subprocess, time
from google import genai
from google.genai import types

transcript = []
# The (start, end) times of the tool calls in the order they ran, which is the
# order of the calls in the model's function calling history.
call_times = []

def now():
    return time.strftime("%H:%M:%SZ", time.gmtime())

def record(history):
    calls = results = 0
    for content in history:
        for part in content.parts or []:
            if part.text and content.role != "user":  # the user's text is the goal
                transcript.append(f"[{content.role}] {part.text}")
            if part.function_call:
                start = call_times[calls][0] if calls < len(call_times) else "?"
                calls += 1
                transcript.append(f"[{start} call] {part.function_call.name}({json.dumps(part.function_call.args, default=str)})")
            if part.function_response:
                end = call_times[results][1] if results < len(call_times) else "?"
                results += 1
                r = part.function_response.response or {}
                r = r.get("result", r) if isinstance(r, dict) else r
                transcript.append(f"[{end} result] {part.function_response.name}: {str(r)[:4000]}")

def run_shell(command: str) -> str:
    """Runs a shell command in /workspace and returns its exit code and output."""
    start = now()
    try:
        p = subprocess.run(command, shell=True, cwd="/workspace", capture_output=True, text=True, timeout=300)
        out = (p.stdout + p.stderr)[-20000:]
        return f"exit code {p.returncode}\n{out}"
    finally:
        call_times.append((start, now()))

def read_file(path: str) -> str:
    """Returns the content of a file, relative to /workspace."""
    start = now()
    try:
        return pathlib.Path("/workspace", path).read_text(errors="replace")[:50000]
    finally:
        call_times.append((start, now()))

def generate(client, model, goal):
    call_times.clear()
    transcript.append(f"[{now()} model] {model}")
    resp = client.models.generate_content(
        model=model,
        contents=goal,
        config=types.GenerateContentConfig(
            system_instruction=(
                "You are a worker agent in a sandbox. The workspace is /workspace. "
                "Use the tools to investigate and do the work; do not guess. "
                "Finish with a concise report of what you found or did, with evidence."
            ),
            tools=[run_shell, read_file],
            automatic_function_calling=types.AutomaticFunctionCallingConfig(maximum_remote_calls=40),
        ),
    )
    record(resp.automatic_function_calling_history or [])
    transcript.append(f"[{now()} answer]")
    return resp

output = pathlib.Path(os.environ["AX_OUTPUT_FILE"])
try:
    goal = pathlib.Path(os.environ["AX_GOAL_FILE"]).read_text()
    config = json.loads(pathlib.Path(os.environ["AX_CONFIG_FILE"]).read_text())
    # GEMINI_API_KEY is a placeholder; the Gateway sets the real key. Without
    # a timeout, a request to an overloaded model can hang for many minutes.
    client = genai.Client(http_options=types.HttpOptions(
        timeout=120_000, retry_options=types.HttpRetryOptions(attempts=4)))
    for model in [config["model"]] + config.get("fallbackModels", []):
        try:
            answer = generate(client, model, goal).text or "(the model returned no text)"
            break
        except Exception as e:
            print(f"{model}: {type(e).__name__}: {e}", flush=True)
            transcript.append(f"[{now()} error] {model}: {type(e).__name__}: {e}")
            error = e
    else:
        raise error
except Exception as e:  # report every failure so the swarm does not wait for nothing
    answer = f"WORKER ERROR: {type(e).__name__}: {e}"
output.parent.mkdir(parents=True, exist_ok=True)
# The transcript goes first: once the output exists, the swarm may read both.
output.with_name("swarm-transcript").write_text("\n".join(transcript) + "\n")
output.write_text(answer.rstrip() + "\n")
print(answer)
'''

PLANNER_INSTRUCTION = """You are the planner of ax-swarm. You reach a goal by delegating work to worker agents.

Each worker is an AI agent with a shell, running in its own sandbox on its own copy of the same workspace (at /workspace; see the goal for what it holds). A worker receives only the goal text you write for its step, followed by the outputs of the steps you list as its inputs, and returns a text report. Workers cannot see each other, this conversation, or the overall goal unless you put it in their step goal. Changes a worker makes to its copy of the workspace are not seen by other workers.

Each turn, either:
- set done to true and write the final answer for the user in answer, once the results are enough to answer the goal or to show that it cannot be reached; or
- set done to false and list the steps of the next round.

The steps of a round run concurrently, each in its own sandbox, so use them: split the work into independent steps that run at the same time (different files, components, hypotheses, sources or approaches), and cover several angles at once rather than one after another. A round with a single step leaves the swarm idle; use one only when the work cannot be split. To fan in, list steps in inputs: a step starts after its inputs finish, and their outputs are appended to its goal. Inputs may name steps from earlier rounds or steps listed before it in the same round.

Step names use lowercase letters, digits and '-', start with a letter, have at most 24 characters, and are unique across the whole run.

Write every step goal so that it is self-contained and specific, and asks for a concise report with evidence. Prefer well-scoped steps over vague ones. When a step fails or its result is inconclusive, change the approach instead of repeating the same step. Use thought to explain your reasoning in one or two sentences."""

PLAN_SCHEMA = {
    "type": "OBJECT",
    "properties": {
        "thought": {"type": "STRING"},
        "done": {"type": "BOOLEAN"},
        "answer": {"type": "STRING"},
        "steps": {
            "type": "ARRAY",
            "items": {
                "type": "OBJECT",
                "properties": {
                    "name": {"type": "STRING"},
                    "goal": {"type": "STRING"},
                    "inputs": {"type": "ARRAY", "items": {"type": "STRING"}},
                },
                "required": ["name", "goal"],
                "propertyOrdering": ["name", "goal", "inputs"],
            },
        },
    },
    "required": ["thought", "done"],
    "propertyOrdering": ["thought", "done", "answer", "steps"],
}

log_lock = threading.Lock()


def log(msg):
    """Logs msg with a UTC timestamp, indenting its continuation lines under
    the first."""
    ts = time.strftime("%H:%M:%SZ", time.gmtime())
    msg = msg.replace("\n", "\n" + " " * (len(ts) + 1))
    with log_lock:
        print(f"{ts} {msg}", file=sys.stderr, flush=True)


def log_block(title, text):
    """Logs a multi-line model input or output in one piece, so the blocks of
    concurrent steps do not interleave."""
    lines = (text or "").rstrip().splitlines() or ["(empty)"]
    log(f"┌ {title}\n" + "\n".join(f"│ {line}" for line in lines) + "\n└")


def truncate(s, n):
    marker = "\n[truncated]"
    data = s.encode()
    if len(data) <= n:
        return s
    if n <= len(marker):
        return ""
    return data[: n - len(marker)].decode(errors="ignore") + marker


class AX:
    """Runs the ax CLI against one atespace."""

    def __init__(self, bin_, atespace, context):
        self.base = [bin_, "-a", atespace] + (["--context", context] if context else [])

    def run(self, *args, stdin=None, check=True):
        p = subprocess.run(self.base + list(args), input=stdin, capture_output=True, text=True)
        if check and p.returncode != 0:
            raise RuntimeError(f"ax {' '.join(args)}: {(p.stderr or p.stdout).strip()}")
        return p

    def apply(self, manifest):
        self.run("apply", "-f", "-", stdin=json.dumps(manifest))

    def column(self, kind, name, col):
        """Returns column col of resource name in `ax get <kind>s`, or None."""
        out = self.run("get", kind + "s").stdout.splitlines()
        if not out:
            return None
        header = out[0].split()
        for line in out[1:]:
            fields = line.split()
            if fields and fields[0] == name and len(fields) > header.index(col):
                return fields[header.index(col)]
        return None

    def delete(self, kind, name):
        self.run("delete", kind, name, check=False)


class Gemini:
    """Calls generateContent on the Gemini API, falling back to the next model
    when one keeps failing."""

    def __init__(self, api_key, models):
        self.api_key, self.models = api_key, models

    def generate_json(self, system, prompt, schema):
        body = json.dumps({
            "systemInstruction": {"parts": [{"text": system}]},
            "contents": [{"role": "user", "parts": [{"text": prompt}]}],
            "generationConfig": {"responseMimeType": "application/json", "responseSchema": schema},
        }).encode()
        # An overloaded model answers 503, an empty 404, or nothing at all, so
        # every request has a timeout and transient failures are retried.
        err = None
        for model in self.models:
            url = f"{GEMINI_API}/models/{model}:generateContent"
            for attempt in range(3):
                if attempt:
                    time.sleep(2 ** attempt)
                req = urllib.request.Request(url, data=body, method="POST", headers={
                    "Content-Type": "application/json", "x-goog-api-key": self.api_key})
                try:
                    with urllib.request.urlopen(req, timeout=120) as resp:
                        data = json.load(resp)
                    break
                except urllib.error.HTTPError as e:
                    detail = e.read().decode(errors="replace").strip()
                    err = f"Gemini API error {e.code} for {model}: {detail or e.reason}"
                    if e.code not in (429, 500, 502, 503, 504) and (e.code != 404 or detail):
                        raise RuntimeError(err)
                except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
                    err = f"Gemini API request to {model} failed: {e}"
                log(f"planner: {err}; retrying")
            else:
                continue
            break
        else:
            raise RuntimeError(err)
        candidates = data.get("candidates") or []
        if not candidates:
            raise RuntimeError(f"Gemini API returned no candidates: {data.get('promptFeedback')}")
        text = "".join(p.get("text", "") for p in candidates[0].get("content", {}).get("parts", []))
        try:
            return json.loads(text)
        except json.JSONDecodeError as e:
            raise RuntimeError(f"Gemini API returned invalid JSON ({e}): {truncate(text, 2000)}")


def planner_prompt(goal, history, rounds_left, max_steps, parallel):
    parts = [f"# Goal\n\n{goal}\n"]
    for i, rnd in enumerate(history, 1):
        parts.append(f"\n# Round {i}\n\nYour thought: {rnd['plan'].get('thought', '')}\n")
        if rnd.get("rejected"):
            parts.append(f"\nThe plan was rejected and nothing ran: {rnd['rejected']}\n")
            continue
        for res in rnd["results"]:
            step = res["step"]
            parts.append(f"\n## Step {step['name']}\n\nGoal: {step['goal']}\n")
            if step.get("inputs"):
                parts.append(f"Inputs: {', '.join(step['inputs'])}\n")
            if res.get("error"):
                parts.append(f"\nFailed: {res['error']}\n")
            if res.get("output"):
                parts.append(f"\nOutput:\n{truncate(res['output'], MAX_HISTORY_OUTPUT_BYTES)}\n")
    if rounds_left > 0:
        parts.append(f"\n# Next\n\nYou may run {rounds_left} more round(s), each with at most {max_steps} step(s); {parallel} workers run at once. Answer as soon as you can.\n")
    else:
        parts.append("\n# Next\n\nNo rounds are left: set done to true and answer with what you know, saying what remains uncertain.\n")
    return "".join(parts)


def validate(plan, finished, max_steps):
    steps = plan.get("steps") or []
    if not steps:
        return "done is false but there are no steps"
    if len(steps) > max_steps:
        return f"{len(steps)} steps, at most {max_steps} are allowed in a round"
    seen = set()
    for st in steps:
        name = st.get("name", "")
        if not STEP_NAME_RE.match(name):
            return f"step name {name!r}: use at most 24 lowercase letters, digits and '-', starting with a letter"
        if name in seen:
            return f"step name {name!r} is used twice in this round"
        if name in finished:
            return f"step name {name!r} was used in an earlier round; names must be unique"
        if not st.get("goal", "").strip():
            return f"step {name!r} has no goal"
        for inp in st.get("inputs") or []:
            if inp not in seen and inp not in finished:
                return f"step {name!r}: input {inp!r} is not an earlier step"
        seen.add(name)
    return None


def worker_goal(step, inputs):
    if not inputs:
        return step["goal"]
    budget = (MAX_GOAL_BYTES - len(step["goal"].encode())) // len(inputs)
    parts = [step["goal"]]
    for res in inputs:
        header = f"\n\n--- Output of step {res['step']['name']!r} ---\n"
        parts.append(header + truncate(res.get("output", ""), budget - len(header.encode())))
    return "".join(parts)


class Swarm:
    def __init__(self, args, ax, planner):
        self.args, self.ax, self.planner = args, ax, planner
        self.name = args.name
        self.created = []  # (kind, name), deleted in reverse order
        self.secret = None
        self.slots = threading.BoundedSemaphore(args.parallel)

    # --- Environment -----------------------------------------------------

    def setup(self, api_key):
        a = self.args
        self.secret = f"{self.name}-gemini"
        kubectl = [shutil.which("kubectl") or str(pathlib.Path.home() / ".ax/bin/kubectl")]
        if a.context:
            kubectl += ["--context", a.context]
        kubectl += ["-n", a.atespace]
        # The key goes in on stdin, not on the command line.
        subprocess.run(kubectl + ["create", "secret", "generic", self.secret, "--from-file=api-key=/dev/stdin"],
                       input=api_key, text=True, check=True, capture_output=True)
        self.kubectl = kubectl
        subprocess.run(kubectl + ["label", "secret", self.secret, "ax.io/credential=true"], check=True, capture_output=True)
        log(f"created Secret {a.atespace}/{self.secret}")

        self.ax.apply({
            "apiVersion": "ax.io/v1alpha1", "kind": "Gateway", "metadata": {"name": self.name},
            "spec": {"egress": [
                {"https": {"hostnames": ["generativelanguage.googleapis.com"], "effects": {"replaceHeaders": [{
                    "header": "x-goog-api-key",
                    "credentialUri": f"ate-secret://k8s.io/default/{a.atespace}/{self.secret}/api-key"}]}}},
                {"https": {"hostnames": ["pypi.org", "files.pythonhosted.org", "registry.npmjs.org"]}},
                {"https": {"hostnames": ["github.com", "*.github.com", "*.githubusercontent.com"]}},
            ]},
        })
        self.created.append(("gateway", self.name))
        log(f"created Gateway {self.name}")

        self.ax.apply({
            "apiVersion": "ax.io/v1alpha1", "kind": "Model", "metadata": {"name": self.name},
            "spec": {"provider": "google", "model": a.worker_model,
                     "secretKey": {"name": self.secret, "key": "api-key"}},
        })
        self.created.append(("model", self.name))
        # Workers use the model the Model resource names.
        self.worker_model = self.ax.column("model", self.name, "MODEL") or a.worker_model
        log(f"created Model {self.name} ({self.worker_model})")

        self.ax.apply({
            "apiVersion": "ax.io/v1alpha1", "kind": "Workspace", "metadata": {"name": self.name},
            "spec": {
                "profile": {"image": a.image},
                "setup": {
                    "gateway": {"name": self.name},
                    "git": [{"repo": r, "depth": 1} for r in a.repo],
                    "files": [{"path": "swarm_worker.py", "content": WORKER_PY}],
                    "commands": ["python3 -m venv .venv && .venv/bin/pip install --quiet --disable-pip-version-check google-genai"],
                },
            },
        })
        self.created.append(("workspace", self.name))
        log(f"created Workspace {self.name}; waiting for its setup")
        deadline = time.time() + a.task_timeout
        while (phase := self.ax.column("workspace", self.name, "PHASE")) != "Ready":
            if phase == "Failed":
                raise RuntimeError(f"workspace {self.name} failed setup; see `ax describe workspace {self.name}`")
            if time.time() > deadline:
                raise RuntimeError(f"workspace {self.name} is {phase} after {a.task_timeout}s")
            time.sleep(2)

    def cleanup(self):
        if self.args.keep:
            log(f"keeping everything named {self.name}*")
            return
        for kind, name in reversed(self.created):
            self.ax.delete(kind, name)
        if self.secret and getattr(self, "kubectl", None):
            subprocess.run(self.kubectl + ["delete", "secret", self.secret, "--ignore-not-found"], capture_output=True)
        log(f"deleted everything named {self.name}*")

    # --- Workers -----------------------------------------------------------

    def run_task(self, step, goal):
        task = f"{self.name}-{step}"
        self.ax.apply({
            "apiVersion": "ax.io/v1alpha1", "kind": "Task", "metadata": {"name": task},
            "spec": {
                "workspace": {"name": self.name},
                "gateway": {"name": self.name},
                "goal": {"text": goal},
                "config": {"model": self.worker_model, "fallbackModels": self.args.fallback_model, "swarm": self.name, "step": step},
                "env": [{"name": "GEMINI_API_KEY", "value": "placeholder"}],  # the Gateway sets the real key
                "commands": [".venv/bin/python", "swarm_worker.py"],
                "debug": True,  # the output is read with ax ssh
            },
        })
        self.created.append(("task", task))
        try:
            self.wait_state(task, {"Suspended", "Running"})
            if self.ax.column("task", task, "STATE") == "Suspended":
                self.ax.run("resume", "task", task)
                self.wait_state(task, {"Running"})
            deadline = time.time() + self.args.task_timeout
            while self.ax.run("ssh", task, "--", "test", "-s", OUTPUT_FILE, check=False).returncode != 0:
                state = self.ax.column("task", task, "STATE")
                if state in ("Failed", "Crashed"):
                    raise RuntimeError(f"task {task} is {state}; see `ax describe task {task}`")
                if time.time() > deadline:
                    raise RuntimeError(f"no output from task {task} after {self.args.task_timeout}s")
                time.sleep(3)
            out = self.ax.run("ssh", task, "--", "cat", OUTPUT_FILE).stdout
            if not self.args.quiet:
                p = self.ax.run("ssh", task, "--", "cat", TRANSCRIPT_FILE, check=False)
                log_block(f"{step}: worker transcript", p.stdout if p.returncode == 0 else f"(unavailable: {p.stderr.strip()})")
            if out.startswith("WORKER ERROR:"):
                raise RuntimeError(out.strip())
            return out
        finally:
            if not self.args.keep:
                self.ax.delete("task", task)

    def wait_state(self, task, want):
        deadline = time.time() + self.args.task_timeout
        while (state := self.ax.column("task", task, "STATE")) not in want:
            if state in ("Failed", "Crashed"):
                raise RuntimeError(f"task {task} is {state}; see `ax describe task {task}`")
            if time.time() > deadline:
                raise RuntimeError(f"task {task} is {state} after {self.args.task_timeout}s")
            time.sleep(1)

    def run_step(self, step, input_futures, finished):
        inputs = []
        for inp in step.get("inputs") or []:
            res = input_futures[inp].result() if inp in input_futures else finished[inp]
            if res.get("error"):
                log(f"  {step['name']}: skipped, input {inp} failed")
                return {"step": step, "error": f"not run: input {inp!r} failed"}
            inputs.append(res)
        start = time.time()
        try:
            with self.slots:
                log(f"  {step['name']}: started")
                start = time.time()
                goal = worker_goal(step, inputs)
                if not self.args.quiet:
                    log_block(f"{step['name']}: worker input", goal)
                out = self.run_task(step["name"], goal)
        except Exception as e:
            log(f"  {step['name']}: failed after {time.time() - start:.0f}s: {e}")
            return {"step": step, "error": str(e)}
        log(f"  {step['name']}: done in {time.time() - start:.0f}s")
        if not self.args.quiet:
            log_block(f"{step['name']}: worker output", out)
        return {"step": step, "output": out}

    def run_round(self, steps, finished):
        # Every step gets a thread, so a step waiting for its inputs holds no
        # slot; self.slots caps how many tasks run at once. Steps wait only on
        # steps listed before them, so they cannot deadlock.
        futures = {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=len(steps)) as pool:
            for st in steps:
                futures[st["name"]] = pool.submit(self.run_step, st, dict(futures), finished)
            return [futures[st["name"]].result() for st in steps]

    # --- Planning ----------------------------------------------------------

    def run(self, goal):
        a = self.args
        history, finished = [], {}
        rnd = 0
        while True:
            rnd += 1
            left = a.max_rounds - len(history)
            prompt = planner_prompt(goal, history, left, a.max_steps, a.parallel)
            if not a.quiet:
                log_block(f"round {rnd}: planner input", prompt)
            plan = self.planner.generate_json(PLANNER_INSTRUCTION, prompt, PLAN_SCHEMA)
            if not a.quiet:
                log_block(f"round {rnd}: planner output", json.dumps(plan, indent=2, ensure_ascii=False))
            if plan.get("thought"):
                log(f"round {rnd}: {plan['thought']}")
            if plan.get("done"):
                return plan.get("answer", "")
            if left <= 0:
                raise RuntimeError(f"the planner did not answer after {a.max_rounds} rounds")
            if err := validate(plan, finished, a.max_steps):
                log(f"round {rnd}: plan rejected: {err}")
                history.append({"plan": plan, "rejected": err})
                continue
            results = self.run_round(plan["steps"], finished)
            for res in results:
                finished[res["step"]["name"]] = res
            history.append({"plan": plan, "results": results})


def default_image():
    image = os.environ.get("AX_TASK_IMAGE", "")
    digest = AX_ROOT / "bin/ax-runner.digest"
    if not image and digest.exists():
        image = f"localhost:5001/ax-runner@{digest.read_text().strip()}"
    return image


def main():
    p = argparse.ArgumentParser(description="Work on a goal with a swarm of AX tasks planned by Gemini.")
    p.add_argument("goal", nargs="+", help="what to achieve")
    p.add_argument("--repo", action="append", default=[], help="git repository the Workspace clones (repeatable)")
    p.add_argument("--model", default=DEFAULT_MODEL, help="Gemini model that plans (default %(default)s)")
    p.add_argument("--fallback-model", action="append", default=[],
                   help="Gemini model the planner and workers fall back to when the others fail (repeatable)")
    p.add_argument("--worker-model", default=DEFAULT_MODEL, help="Gemini model the workers use (default %(default)s)")
    p.add_argument("--image", default=default_image(), help="digest-pinned runner image with python3 (default $AX_TASK_IMAGE)")
    p.add_argument("--atespace", default="default")
    p.add_argument("--context", default="", help="kubeconfig context (default: the current one)")
    p.add_argument("--max-rounds", type=int, default=6, help="rounds of steps before the planner has to answer")
    p.add_argument("--max-steps", type=int, default=8, help="steps per round at most")
    p.add_argument("--parallel", type=int, default=8, help="tasks running at once at most")
    p.add_argument("--task-timeout", type=int, default=1800, help="seconds a task or the workspace setup may take")
    p.add_argument("--name", default="", help="name of the swarm, prefixed to everything it creates (default swarm-<random>)")
    p.add_argument("--keep", action="store_true", help="keep what the swarm created")
    p.add_argument("-q", "--quiet", action="store_true",
                   help="do not log the planner's and the workers' model inputs and outputs and the workers' tool calls")
    args = p.parse_args()

    api_key = os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY")
    if not api_key:
        sys.exit("ax-swarm: set GEMINI_API_KEY")
    if "@sha256:" not in args.image:
        sys.exit(f"ax-swarm: set AX_TASK_IMAGE or --image to a digest-pinned runner image, or run make build-local-cluster "
                 f"in the AX checkout at {AX_ROOT} (set AX_ROOT if it is elsewhere)")
    if args.parallel < 1 or args.max_steps < 1:
        sys.exit("ax-swarm: --parallel and --max-steps must be at least 1")
    args.name = args.name or f"swarm-{secrets.token_hex(3)}"
    if not re.match(r"^[a-z]([-a-z0-9]*[a-z0-9])?$", args.name) or len(args.name) + 1 + 24 > 63:
        sys.exit(f"ax-swarm: --name {args.name!r} must be a lowercase name of at most 38 characters")

    ax_bin = os.environ.get("AX_BIN") or (str(AX_ROOT / "bin/ax") if (AX_ROOT / "bin/ax").exists() else "ax")
    swarm = Swarm(args, AX(ax_bin, args.atespace, args.context), Gemini(api_key, [args.model] + args.fallback_model))
    goal = " ".join(args.goal)
    log(f"swarm {args.name}: planning with {args.model}, workers on {args.worker_model}")
    try:
        swarm.setup(api_key)
        answer = swarm.run(goal)
    except subprocess.CalledProcessError as e:
        sys.exit(f"ax-swarm: {' '.join(e.cmd[:6])}...: {(e.stderr or b'').strip()}")
    except (RuntimeError, KeyboardInterrupt) as e:
        sys.exit(f"ax-swarm: {e or 'interrupted'}")
    finally:
        swarm.cleanup()
    print(answer.strip())


if __name__ == "__main__":
    main()
