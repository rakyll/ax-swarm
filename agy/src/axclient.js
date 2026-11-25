// @ts-check
import { execFile } from "node:child_process";

export class CLIAXClient {
  /** @param {{ binary?: string, context?: string, server?: string }} [opts] */
  constructor(opts = {}) {
    this.binary = opts.binary || "ax";
    this.context = opts.context;
    this.server = opts.server;
  }

  /** @param {string} atespace */
  baseArgs(atespace) {
    /** @type {string[]} */
    const args = [];
    if (atespace) args.push("-a", atespace);
    if (this.context) args.push("--context", this.context);
    if (this.server) args.push("--server", this.server);
    return args;
  }

  /**
   * @param {string[]} args
   * @param {string} [stdin]
   * @returns {Promise<string>}
   */
  run(args, stdin) {
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

  /** @param {any} spec */
  async createTask(spec) {
    await this.run([...this.baseArgs(spec.atespace), "apply", "-f", "-"], renderTaskYAML(spec));
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async resumeTask(atespace, name) {
    await this.run([...this.baseArgs(atespace), "resume", "task", name]);
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async suspendTask(atespace, name) {
    await this.run([...this.baseArgs(atespace), "suspend", "task", name]);
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async deleteTask(atespace, name) {
    await this.run([...this.baseArgs(atespace), "delete", "task", name]);
  }

  /** @param {string} atespace */
  async listTasks(atespace) {
    const out = await this.run([...this.baseArgs(atespace), "get", "tasks"]);
    const tasks = [];
    const lines = out.split("\n");
    for (let i = 1; i < lines.length; i++) {
      const fields = lines[i].trim().split(/\s+/);
      if (fields.length < 3) continue;
      tasks.push({ name: fields[0], atespace: fields[1], state: fields[2] });
    }
    return tasks;
  }
}

export class MockAXClient {
  constructor() {
    /** @type {Map<string, string>} */
    this.tasks = new Map();
    /** @type {Map<string, any>} */
    this.specs = new Map();
    /** @type {string[]} */
    this.calls = [];
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  key(atespace, name) {
    return `${atespace}/${name}`;
  }

  /** @param {any} spec */
  async createTask(spec) {
    const k = this.key(spec.atespace, spec.name);
    this.calls.push(`create:${k}`);
    if (!this.tasks.has(k)) this.tasks.set(k, "Suspended");
    this.specs.set(k, spec);
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async resumeTask(atespace, name) {
    const k = this.key(atespace, name);
    this.calls.push(`resume:${k}`);
    if (!this.tasks.has(k)) throw new Error(`task ${k} not found`);
    this.tasks.set(k, "Working");
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async suspendTask(atespace, name) {
    const k = this.key(atespace, name);
    this.calls.push(`suspend:${k}`);
    if (!this.tasks.has(k)) throw new Error(`task ${k} not found`);
    this.tasks.set(k, "Suspended");
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  async deleteTask(atespace, name) {
    const k = this.key(atespace, name);
    this.calls.push(`delete:${k}`);
    this.tasks.delete(k);
    this.specs.delete(k);
  }

  /** @param {string} atespace */
  async listTasks(atespace) {
    const prefix = `${atespace}/`;
    const out = [];
    for (const [k, state] of this.tasks.entries()) {
      if (k.startsWith(prefix)) {
        out.push({ name: k.slice(prefix.length), atespace, state });
      }
    }
    return out;
  }

  /**
   * @param {string} atespace
   * @param {string} name
   */
  state(atespace, name) {
    return this.tasks.get(this.key(atespace, name));
  }
}

/** @param {any} spec */
function renderTaskYAML(spec) {
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
