import { execFile } from "node:child_process";

export type TaskState = "Unknown" | "Suspended" | "Working" | "Completed" | "Failed";

export interface TaskSpec {
  name: string;
  atespace: string;
  workspace?: string;
  image?: string;
  goal?: string;
  commands?: string[];
  env?: Record<string, string>;
  debug?: boolean;
}

export interface TaskInfo {
  name: string;
  atespace: string;
  state: TaskState;
}

export interface AXClient {
  createTask(spec: TaskSpec): Promise<void>;
  resumeTask(atespace: string, name: string): Promise<void>;
  suspendTask(atespace: string, name: string): Promise<void>;
  deleteTask(atespace: string, name: string): Promise<void>;
  listTasks(atespace: string): Promise<TaskInfo[]>;
}

export interface CLIAXClientOptions {
  binary?: string;
  context?: string;
  server?: string;
}

export class CLIAXClient implements AXClient {
  private readonly binary: string;
  private readonly context?: string;
  private readonly server?: string;

  constructor(opts: CLIAXClientOptions = {}) {
    this.binary = opts.binary || "ax";
    this.context = opts.context;
    this.server = opts.server;
  }

  private baseArgs(atespace: string): string[] {
    const args: string[] = [];
    if (atespace) args.push("-a", atespace);
    if (this.context) args.push("--context", this.context);
    if (this.server) args.push("--server", this.server);
    return args;
  }

  private run(args: string[], stdin?: string): Promise<string> {
    return new Promise((resolve, reject) => {
      const child = execFile(this.binary, args, { maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
        if (err) {
          reject(new Error(`ax ${args.join(" ")}: ${err.message}: ${stderr.trim()}`));
          return;
        }
        resolve(stdout);
      });
      if (stdin !== undefined) {
        child.stdin?.end(stdin);
      }
    });
  }

  async createTask(spec: TaskSpec): Promise<void> {
    await this.run([...this.baseArgs(spec.atespace), "apply", "-f", "-"], renderTaskYAML(spec));
  }

  async resumeTask(atespace: string, name: string): Promise<void> {
    await this.run([...this.baseArgs(atespace), "resume", "task", name]);
  }

  async suspendTask(atespace: string, name: string): Promise<void> {
    await this.run([...this.baseArgs(atespace), "suspend", "task", name]);
  }

  async deleteTask(atespace: string, name: string): Promise<void> {
    await this.run([...this.baseArgs(atespace), "delete", "task", name]);
  }

  async listTasks(atespace: string): Promise<TaskInfo[]> {
    const out = await this.run([...this.baseArgs(atespace), "get", "tasks"]);
    const tasks: TaskInfo[] = [];
    const lines = out.split("\n");
    for (let i = 1; i < lines.length; i++) {
      const fields = lines[i].trim().split(/\s+/);
      if (fields.length < 3) continue;
      tasks.push({
        name: fields[0],
        atespace: fields[1],
        state: fields[2] as TaskState,
      });
    }
    return tasks;
  }
}

export class MockAXClient implements AXClient {
  readonly tasks = new Map<string, TaskState>();
  readonly specs = new Map<string, TaskSpec>();
  readonly calls: string[] = [];

  private key(atespace: string, name: string): string {
    return `${atespace}/${name}`;
  }

  async createTask(spec: TaskSpec): Promise<void> {
    const k = this.key(spec.atespace, spec.name);
    this.calls.push(`create:${k}`);
    if (!this.tasks.has(k)) this.tasks.set(k, "Suspended");
    this.specs.set(k, spec);
  }

  async resumeTask(atespace: string, name: string): Promise<void> {
    const k = this.key(atespace, name);
    this.calls.push(`resume:${k}`);
    if (!this.tasks.has(k)) throw new Error(`task ${k} not found`);
    this.tasks.set(k, "Working");
  }

  async suspendTask(atespace: string, name: string): Promise<void> {
    const k = this.key(atespace, name);
    this.calls.push(`suspend:${k}`);
    if (!this.tasks.has(k)) throw new Error(`task ${k} not found`);
    this.tasks.set(k, "Suspended");
  }

  async deleteTask(atespace: string, name: string): Promise<void> {
    const k = this.key(atespace, name);
    this.calls.push(`delete:${k}`);
    this.tasks.delete(k);
    this.specs.delete(k);
  }

  async listTasks(atespace: string): Promise<TaskInfo[]> {
    const prefix = `${atespace}/`;
    const out: TaskInfo[] = [];
    for (const [k, state] of this.tasks.entries()) {
      if (k.startsWith(prefix)) {
        out.push({ name: k.slice(prefix.length), atespace, state });
      }
    }
    return out;
  }

  state(atespace: string, name: string): TaskState | undefined {
    return this.tasks.get(this.key(atespace, name));
  }
}

function renderTaskYAML(spec: TaskSpec): string {
  const lines = [
    "apiVersion: ax.io/v1alpha1",
    "kind: Task",
    "metadata:",
    `  name: ${JSON.stringify(spec.name)}`,
    "spec:",
  ];
  if (spec.image) {
    lines.push("  profile:", `    image: ${JSON.stringify(spec.image)}`);
  }
  if (spec.workspace) {
    lines.push("  workspace:", `    name: ${JSON.stringify(spec.workspace)}`);
  }
  if (spec.goal) {
    lines.push("  goal:", `    text: ${JSON.stringify(spec.goal)}`);
  }
  if (spec.commands?.length) {
    lines.push("  commands:");
    for (const c of spec.commands) lines.push(`    - ${JSON.stringify(c)}`);
  }
  if (spec.env && Object.keys(spec.env).length > 0) {
    lines.push("  env:");
    for (const k of Object.keys(spec.env).sort()) {
      lines.push(`    - name: ${JSON.stringify(k)}`);
      lines.push(`      value: ${JSON.stringify(spec.env[k])}`);
    }
  }
  if (spec.debug) lines.push("  debug: true");
  return `${lines.join("\n")}\n`;
}
