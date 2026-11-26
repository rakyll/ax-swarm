// Live trace viewer for axo. Receives spans over Server-Sent Events and draws
// them as an overview graph of the run. All DOM is built with
// createElement/createElementNS/textContent: span names, attributes, and
// events come from agy and the AX cluster and are untrusted.
'use strict';

const SVG_NS = 'http://www.w3.org/2000/svg';

const spans = new Map(); // id -> span
const children = new Map(); // parent id -> [child ids]
const openDetails = new Set(); // inspector <details> the user expanded

let selectedNode = null; // overview selection (node key)
let watched = new Set(); // span ids shown in the inspector
let serverSkewMs = 0; // server now - client now
let dirty = true;
let detailsDirty = false;

const KIND_LABELS = {
  run: 'run', phase: 'phase', task: 'task', attempt: 'attempt',
  ax: 'ax api', agy: 'agy', tool: 'tool call',
};

const $ = (id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function svg(tag, attrs, text) {
  const e = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

function now() { return Date.now() + serverSkewMs; }
function t(s) { return Date.parse(s); }
function endOf(s) { return s.end ? t(s.end) : now(); }
function attr(s, k) { return (s && s.attrs && s.attrs[k]) || ''; }

function fmtDur(ms) {
  if (ms < 1000) return Math.max(0, Math.round(ms)) + 'ms';
  const sec = ms / 1000;
  if (sec < 60) return sec.toFixed(1) + 's';
  const m = Math.floor(sec / 60);
  return m + 'm' + Math.round(sec - m * 60) + 's';
}

function fmtClock(ms) {
  return new Date(ms).toLocaleTimeString([], { hour12: false });
}

function clipText(s, n) {
  s = String(s || '');
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

function upsert(s) {
  const prev = spans.get(s.id);
  spans.set(s.id, s);
  if (!prev) {
    const p = s.parent || '';
    if (!children.has(p)) children.set(p, []);
    children.get(p).push(s.id);
  }
  if (watched.has(s.id) || watched.has(s.parent) ||
      (s.kind === 'run' && selectedNode)) {
    detailsDirty = true;
  }
}

function reset() {
  spans.clear();
  children.clear();
}

function runSpan() {
  const roots = children.get('') || [];
  return roots.length ? spans.get(roots[roots.length - 1]) : null;
}

function descendants(id, out) {
  for (const c of children.get(id) || []) {
    out.add(c);
    descendants(c, out);
  }
  return out;
}

// ---------- header & legend ----------

function renderHeader() {
  const run = runSpan();
  if (!run) return;
  $('run-name').textContent = attr(run, 'name') || run.name;
  const st = $('run-status');
  st.textContent = run.status;
  st.className = 'pill ' + run.status;
  $('run-elapsed').textContent = fmtDur(endOf(run) - t(run.start));
  const g = buildGraph();
  const tasks = g.nodes.filter((n) => n.kind === 'task');
  const done = tasks.filter((n) => n.status !== 'pending' && n.status !== 'running').length;
  $('run-tasks').textContent = tasks.length ? done + '/' + tasks.length + ' tasks finished' : '';
  document.title = 'axo trace · ' + $('run-name').textContent;
}

function renderLegend() {
  const legend = $('legend');
  legend.replaceChildren();
  const items = [['pending', 'pending'], ['running', 'running'], ['ok', 'done'], ['warn', 'blocked'], ['error', 'failed'], ['skipped', 'skipped']];
  for (const [cls, label] of items) {
    const item = el('span');
    const sw = el('span', 'swatch sw-' + cls);
    item.appendChild(sw);
    item.appendChild(document.createTextNode(label));
    legend.appendChild(item);
  }
}

// ---------- overview model ----------

// buildGraph derives the overview graph: plan -> tasks (by dependency) -> review ->
// follow-up tasks -> ... -> synthesis. Planned tasks come from the run span's
// "graph" attribute (so tasks that have not started yet appear as pending);
// task spans supply live status.
function buildGraph() {
  const run = runSpan();
  let planned = [];
  try { planned = JSON.parse(attr(run, 'graph') || '[]'); } catch (_) { planned = []; }
  if (!Array.isArray(planned)) planned = [];

  const taskSpans = new Map();
  let plan = null;
  let synth = null;
  const reviews = new Map();
  for (const s of spans.values()) {
    if (s.kind === 'task' && attr(s, 'id')) taskSpans.set(attr(s, 'id'), s);
    if (s.kind === 'agy') {
      const role = attr(s, 'role');
      if (role === 'planner') plan = s;
      if (role === 'synthesizer') synth = s;
      if (role === 'reviewer') {
        const k = parseInt(String(s.name).replace(/\D+/g, ''), 10) || reviews.size + 1;
        reviews.set(k, s);
      }
    }
  }

  const tasks = new Map();
  for (const p of planned) {
    if (!p || typeof p.id !== 'string') continue;
    tasks.set(p.id, {
      id: p.id, title: String(p.title || ''), goal: String(p.goal || ''),
      deps: Array.isArray(p.depends_on) ? p.depends_on.map(String) : [],
      round: Number(p.round) || 1,
    });
  }
  for (const [id, s] of taskSpans) {
    if (tasks.has(id)) continue; // traces without a graph attribute
    tasks.set(id, {
      id, title: attr(s, 'title'), goal: attr(s, 'goal'),
      deps: attr(s, 'depends_on').split(',').map((x) => x.trim()).filter(Boolean),
      round: parseInt(attr(s, 'round'), 10) || 1,
    });
  }

  const nodes = [];
  const byKey = new Map();
  const add = (n) => { nodes.push(n); byKey.set(n.key, n); };
  const statusOf = (s) => (s ? s.status : 'pending');

  // Setup (CreateGateway, CreateWorkspace, wait for Ready) is one node whose
  // rows are its steps; it leads into plan.
  const setupKeys = [];
  for (const s of spans.values()) {
    if (s.kind !== 'phase' || s.name !== 'setup') continue;
    add({ key: 'setup', kind: 'ax', label: 'setup', sub: 'one Gateway + one Workspace', span: s, status: statusOf(s) });
    setupKeys.push('setup');
  }
  if (plan) add({ key: 'plan', kind: 'agy', label: 'plan', sub: attr(plan, 'title') || 'planner', span: plan, status: statusOf(plan) });
  const rounds = [...new Set([...tasks.values()].map((x) => x.round))].sort((a, b) => a - b);
  for (const r of rounds) {
    for (const task of tasks.values()) {
      if (task.round !== r) continue;
      const s = taskSpans.get(task.id);
      add({ key: 'task:' + task.id, kind: 'task', label: task.id, sub: task.title, span: s, task, status: statusOf(s) });
    }
    const rv = reviews.get(r);
    if (rv) add({ key: 'review:' + r, kind: 'agy', label: 'review ' + r, sub: reviewSummary(rv), span: rv, status: statusOf(rv) });
  }
  if (synth) add({ key: 'synthesis', kind: 'agy', label: 'synthesis', sub: 'final answer', span: synth, status: statusOf(synth) });

  const edges = [];
  const link = (from, to) => { if (byKey.has(from) && byKey.has(to)) edges.push({ from, to }); };
  for (let i = 1; i < setupKeys.length; i++) link(setupKeys[i - 1], setupKeys[i]);
  if (setupKeys.length) link(setupKeys[setupKeys.length - 1], 'plan');
  const hasDependent = new Set();
  for (const task of tasks.values()) {
    for (const d of task.deps) {
      if (tasks.has(d) && tasks.get(d).round === task.round) hasDependent.add(d);
    }
  }
  const sinks = (r) => [...tasks.values()].filter((x) => x.round === r && !hasDependent.has(x.id));
  const sourceFor = (r) => (r <= 1 ? 'plan' : (byKey.has('review:' + (r - 1)) ? 'review:' + (r - 1) : 'plan'));
  for (const task of tasks.values()) {
    const deps = task.deps.filter((d) => tasks.has(d));
    for (const d of deps) link('task:' + d, 'task:' + task.id);
    // Follow-up tasks always hang off the review that created them.
    const sameRoundDeps = deps.filter((d) => tasks.get(d).round === task.round);
    if (!sameRoundDeps.length) link(sourceFor(task.round), 'task:' + task.id);
  }
  for (const [r] of reviews) {
    const ss = sinks(r);
    if (ss.length) for (const x of ss) link('task:' + x.id, 'review:' + r);
    else link(sourceFor(r), 'review:' + r);
  }
  if (synth) {
    const last = rounds.length ? rounds[rounds.length - 1] : 0;
    if (byKey.has('review:' + last)) link('review:' + last, 'synthesis');
    else if (last && sinks(last).length) for (const x of sinks(last)) link('task:' + x.id, 'synthesis');
    else link('plan', 'synthesis');
  }
  return { nodes, edges, byKey, tasks };
}

function reviewSummary(s) {
  if (!s.end) return 'reviewing…';
  if (attr(s, 'complete') === 'true') return 'goal complete';
  const n = attr(s, 'follow_up_tasks');
  return n && n !== '0' ? n + ' follow-up task(s)' : 'review';
}

// ---------- overview rendering ----------

const NODE_W = 230;
const NODE_H = 66;
const TOOL_ROW_H = 16;
const MAX_TOOL_ROWS = 6;
const COL_GAP = 56;
const ROW_GAP = 18;
const MIN_SCALE = 0.7; // shrink wide graphs to fit, but keep text readable

// toolSpans returns the tool-call spans under a task span, in start order.
function toolSpans(taskSpan) {
  if (!taskSpan) return [];
  const out = [];
  for (const id of descendants(taskSpan.id, new Set())) {
    const s = spans.get(id);
    if (s && s.kind === 'tool') out.push(s);
  }
  return out.sort((a, b) => t(a.start) - t(b.start));
}

// rowSpans are the spans listed inside a node: a task's tool calls, or the
// setup steps.
function rowSpans(n) {
  if (n.kind === 'task') return toolSpans(n.span);
  if (n.key === 'setup' && n.span) return (children.get(n.span.id) || []).map((id) => spans.get(id)).filter(Boolean);
  return [];
}

function rowLabel(rs) {
  if (rs.kind !== 'tool') {
    if (rs.name.startsWith('CreateGateway')) return 'Gateway ' + rs.name.split(' ').slice(1).join(' ');
    if (rs.name.startsWith('CreateWorkspace')) return 'Workspace ' + rs.name.split(' ').slice(1).join(' ');
    if (rs.name.startsWith('wait')) return 'Workspace ready' + (attr(rs, 'phase') ? ' (' + attr(rs, 'phase') + ')' : '');
    return rs.name;
  }
  const cmd = attr(rs, 'command') || attr(rs, 'path');
  const preview = cmd
    .replace(/^\S*ax\s+(-a\s+\S+\s+)?(--context\s+\S+\s+)?(--server\s+\S+\s+)?ssh\s+\S+\s+--\s+/, '')
    .replace(/^sh\s+-l?c\s+['"]?/, '')
    .replace(/^cd\s+\/workspace\S*\s*&&\s*/, '');
  return rs.name + '  ' + preview;
}

function nodeHeight(n) {
  const k = rowSpans(n).length;
  if (!k) return NODE_H;
  return NODE_H + 8 + Math.min(k, MAX_TOOL_ROWS) * TOOL_ROW_H + (k > MAX_TOOL_ROWS ? TOOL_ROW_H : 0);
}

function layout(g) {
  const layer = new Map(g.nodes.map((n) => [n.key, 0]));
  for (let i = 0; i < g.nodes.length; i++) {
    let changed = false;
    for (const e of g.edges) {
      const want = layer.get(e.from) + 1;
      if (want > layer.get(e.to)) { layer.set(e.to, want); changed = true; }
    }
    if (!changed) break;
  }
  const cols = [];
  for (const n of g.nodes) {
    const l = layer.get(n.key);
    (cols[l] = cols[l] || []).push(n);
  }
  const colHeight = (col) => col.reduce((h, n) => h + nodeHeight(n), 0) + Math.max(0, col.length - 1) * ROW_GAP;
  const height = Math.max(NODE_H, ...cols.map((c) => (c ? colHeight(c) : 0)));
  const pos = new Map();
  cols.forEach((col, ci) => {
    if (!col) return;
    let y = (height - colHeight(col)) / 2;
    for (const n of col) {
      const h = nodeHeight(n);
      pos.set(n.key, { x: ci * (NODE_W + COL_GAP), y, h });
      y += h + ROW_GAP;
    }
  });
  return { pos, width: cols.length * NODE_W + Math.max(0, cols.length - 1) * COL_GAP, height };
}

function nodeMeta(n) {
  const s = n.span;
  if (!s) return 'pending';
  const parts = [n.status === 'ok' ? 'done' : n.status === 'warn' ? 'blocked' : n.status === 'error' ? 'failed' : n.status];
  parts.push(fmtDur(endOf(s) - t(s.start)));
  if (n.kind === 'task') {
    const tries = (children.get(s.id) || []).filter((c) => spans.get(c) && spans.get(c).kind === 'attempt').length;
    if (tries > 1) parts.push('try ' + tries);
  }
  return parts.join(' · ');
}

function renderOverview() {
  const g = buildGraph();
  const host = $('graph');
  if (!g.nodes.length) {
    host.replaceChildren(el('p', 'muted', 'Waiting for the plan…'));
    return;
  }
  const { pos, width, height } = layout(g);
  const avail = Math.max(200, $('overview-view').clientWidth - 48);
  const scale = Math.min(1, Math.max(MIN_SCALE, avail / (width + 4)));
  const root = svg('svg', {
    width: Math.round((width + 4) * scale), height: Math.round((height + 4) * scale),
    viewBox: `-2 -2 ${width + 4} ${height + 4}`,
  });
  const defs = svg('defs');
  const marker = svg('marker', { id: 'arrow', viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse' });
  marker.appendChild(svg('path', { d: 'M 0 0 L 10 5 L 0 10 z', class: 'arrow' }));
  defs.appendChild(marker);
  root.appendChild(defs);

  for (const e of g.edges) {
    const a = pos.get(e.from);
    const b = pos.get(e.to);
    const x1 = a.x + NODE_W;
    const y1 = a.y + NODE_H / 2;
    const x2 = b.x - 2;
    const y2 = b.y + NODE_H / 2; // edges attach to the header, above tool rows
    const mid = (x1 + x2) / 2;
    const target = g.byKey.get(e.to);
    const source = g.byKey.get(e.from);
    let cls = 'edge';
    if (target.status === 'running') cls += ' active';
    else if (source.status !== 'pending' && source.status !== 'running') cls += ' done';
    root.appendChild(svg('path', { d: `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`, class: cls, 'marker-end': 'url(#arrow)' }));
  }

  for (const n of g.nodes) {
    const p = pos.get(n.key);
    const grp = svg('g', {
      class: `node st-${n.status}${n.key === selectedNode ? ' selected' : ''}`,
      transform: `translate(${p.x},${p.y})`,
    });
    grp.appendChild(svg('rect', { class: 'box', width: NODE_W, height: p.h, rx: 8 }));
    grp.appendChild(svg('rect', { class: 'stripe fill-' + n.kind, x: 0, y: 0, width: 5, height: p.h, rx: 2 }));
    grp.appendChild(svg('text', { class: 'n-label', x: 16, y: 21 }, clipText(n.label, 26)));
    grp.appendChild(svg('text', { class: 'n-sub', x: 16, y: 39 }, clipText(n.sub, 30)));
    grp.appendChild(svg('text', { class: 'n-meta st-' + n.status, x: 16, y: 56 }, nodeMeta(n)));
    const title = svg('title', {}, n.label + (n.sub ? ' — ' + n.sub : ''));
    grp.appendChild(title);
    drawRows(grp, n);
    grp.addEventListener('click', () => selectNode(n.key));
    root.appendChild(grp);
  }
  host.replaceChildren(root);
}

const TOOL_ICONS = { ok: '✓', error: '✗', running: '▸', warn: '!' };

// drawRows lists a node's row spans inside it: a task's tool calls (ssh,
// read_file, write_file), most recent last, or the setup steps. Clicking a
// row opens it in the inspector.
function drawRows(grp, n) {
  const tools = rowSpans(n);
  if (!tools.length) return;
  grp.appendChild(svg('line', { class: 'sep', x1: 12, y1: NODE_H, x2: NODE_W - 10, y2: NODE_H }));
  const shown = tools.slice(-MAX_TOOL_ROWS);
  let y = NODE_H + 6;
  if (tools.length > MAX_TOOL_ROWS) {
    grp.appendChild(svg('text', { class: 't-more', x: 16, y: y + 11 }, '+' + (tools.length - MAX_TOOL_ROWS) + ' earlier tool calls'));
    y += TOOL_ROW_H;
  }
  for (const ts of shown) {
    const cmd = attr(ts, 'command') || attr(ts, 'path') || ts.name;
    const dur = ts.end ? fmtDur(endOf(ts) - t(ts.start)) : '';
    const label = `${TOOL_ICONS[ts.status] || '·'} ${clipText(rowLabel(ts), 27)}`;
    const hit = svg('rect', { class: 't-hit', x: 6, y: y, width: NODE_W - 12, height: TOOL_ROW_H, rx: 3 });
    const text = svg('text', { class: 't-row st-' + ts.status, x: 16, y: y + 12 }, label);
    const durText = svg('text', { class: 't-more', x: NODE_W - 12, y: y + 12, 'text-anchor': 'end' }, dur);
    const tip = svg('title', {}, cmd);
    hit.appendChild(tip);
    hit.addEventListener('click', (ev) => { ev.stopPropagation(); openToolInInspector(n.key, ts.id); });
    grp.appendChild(hit);
    grp.appendChild(text);
    grp.appendChild(durText);
    y += TOOL_ROW_H;
  }
}

// openToolInInspector selects the task and expands the path to a tool call.
function openToolInInspector(nodeKey, spanId) {
  let s = spans.get(spanId);
  while (s) {
    openDetails.add('span:' + s.id);
    if (s.kind === 'task' || s.kind === 'phase') break;
    s = spans.get(s.parent);
  }
  scrollToSpan = spanId;
  selectNode(nodeKey);
}

let scrollToSpan = null;

function selectNode(key) {
  selectedNode = key;
  dirty = true;
  detailsDirty = true;
}

// ---------- axo events ----------

// taskOfEvent finds the task an axo progress line is about, e.g.
// "[fib-30] ax ssh …", "task fib-30 done …", "ax task <name> created for task fib-30".
function taskOfEvent(text, tasks) {
  const m = /^\[([a-z][a-z0-9-]*)\]/.exec(text) || /\btask ([a-z][a-z0-9-]*)\b/.exec(text.replace(/^ax task \S+/, ''));
  if (m && tasks.has(m[1])) return m[1];
  const m2 = /for task ([a-z][a-z0-9-]*)/.exec(text);
  if (m2 && tasks.has(m2[1])) return m2[1];
  return null;
}

function eventLine(e) {
  const line = el('div', 'event s-' + (e.level || 'info'));
  line.appendChild(el('span', 't', fmtClock(t(e.time))));
  line.appendChild(document.createTextNode(e.text));
  return line;
}

// ---------- inspector ----------

function kvRow(grid, k, v) {
  grid.appendChild(el('div', 'k', k));
  const val = el('div', 'v');
  if (v instanceof Node) val.appendChild(v); else val.textContent = v;
  grid.appendChild(val);
}

function longBlock(box, label, text) {
  box.appendChild(el('h3', null, label));
  box.appendChild(el('pre', 'block', text));
}

function attrsBlock(box, s, skip) {
  const entries = Object.entries(s.attrs || {})
    .filter(([k, v]) => v && !(skip && skip.has(k)))
    .sort(([a], [b]) => a.localeCompare(b));
  const grid = el('div', 'kv');
  for (const [k, v] of entries) {
    if (v.length > 80 || v.includes('\n')) {
      if (grid.childNodes.length) { box.appendChild(grid.cloneNode(true)); grid.replaceChildren(); }
      const d = detailsFor('attr:' + s.id + ':' + k, k);
      d.appendChild(el('pre', 'block', v));
      box.appendChild(d);
    } else {
      kvRow(grid, k, v);
    }
  }
  if (grid.childNodes.length) box.appendChild(grid);
}

// detailsFor returns a <details> that remembers whether the user opened it.
function detailsFor(key, summaryText, summaryNode) {
  const d = el('details');
  if (openDetails.has(key)) d.open = true;
  d.addEventListener('toggle', () => { if (d.open) openDetails.add(key); else openDetails.delete(key); });
  const sum = el('summary');
  if (summaryNode) sum.appendChild(summaryNode); else sum.textContent = summaryText;
  d.appendChild(sum);
  return d;
}

function spanLine(s) {
  const line = el('span');
  line.appendChild(el('span', 'kind', s.kind === 'tool' ? 'tool' : (KIND_LABELS[s.kind] || s.kind)));
  line.appendChild(el('span', 's-' + s.status, s.name));
  line.appendChild(el('span', 'dur', s.end ? fmtDur(endOf(s) - t(s.start)) : 'running'));
  return line;
}

// execTree renders a span's descendants as nested, expandable entries.
function execTree(parentId, host) {
  for (const cid of children.get(parentId) || []) {
    const s = spans.get(cid);
    if (!s) continue;
    const d = detailsFor('span:' + s.id, null, spanLine(s));
    d.dataset.span = s.id;
    const body = el('div', 'attrs');
    attrsBlock(body, s);
    if (s.events && s.events.length) {
      for (const e of s.events) {
        const ev = el('div', 'event s-' + (e.level || 'ok'));
        ev.appendChild(el('span', 't', fmtClock(t(e.time))));
        ev.appendChild(document.createTextNode(e.text));
        body.appendChild(ev);
      }
    }
    d.appendChild(body);
    execTree(s.id, d);
    host.appendChild(d);
  }
}

function renderInspector() {
  const box = $('details');
  const g = buildGraph();
  const n = selectedNode && g.byKey.get(selectedNode);
  if (!n) {
    watched = new Set();
    box.replaceChildren(el('p', 'muted', 'Select a task in the overview to inspect it.'));
    return;
  }
  const s = n.span;
  watched = s ? descendants(s.id, new Set([s.id])) : new Set();
  box.replaceChildren();

  const title = el('h2', 's-' + n.status, n.kind === 'task' ? 'task ' + n.label : n.label);
  box.appendChild(title);
  const grid = el('div', 'kv');
  kvRow(grid, 'status', n.status === 'ok' ? 'done' : n.status === 'warn' ? 'blocked' : n.status === 'error' ? 'failed' : n.status);
  if (s) {
    kvRow(grid, 'started', fmtClock(t(s.start)));
    kvRow(grid, 'duration', fmtDur(endOf(s) - t(s.start)) + (s.end ? '' : ' (running)'));
  }

  if (n.kind === 'task') {
    const task = n.task;
    if (task.title) kvRow(grid, 'title', task.title);
    kvRow(grid, 'round', String(task.round));
    if (task.deps.length) {
      const chips = el('span', 'chips');
      for (const d of task.deps) {
        const chip = el('button', 'chip', d);
        chip.type = 'button';
        chip.addEventListener('click', () => selectNode('task:' + d));
        chips.appendChild(chip);
      }
      kvRow(grid, 'depends on', chips);
    }
    const dependents = [...g.tasks.values()].filter((x) => x.deps.includes(task.id));
    if (dependents.length) {
      const chips = el('span', 'chips');
      for (const x of dependents) {
        const chip = el('button', 'chip', x.id);
        chip.type = 'button';
        chip.addEventListener('click', () => selectNode('task:' + x.id));
        chips.appendChild(chip);
      }
      kvRow(grid, 'needed by', chips);
    }
    if (attr(s, 'status')) kvRow(grid, 'reported', attr(s, 'status'));
    box.appendChild(grid);
    if (task.goal) longBlock(box, 'goal', task.goal);
    if (attr(s, 'summary')) longBlock(box, 'summary', attr(s, 'summary'));
    if (attr(s, 'error')) longBlock(box, 'error', attr(s, 'error'));
    if (attr(s, 'details')) longBlock(box, 'details', attr(s, 'details'));
    if (attr(s, 'patch')) longBlock(box, 'patch', attr(s, 'patch'));
    const run = runSpan();
    const mine = ((run && run.events) || []).filter((e) => taskOfEvent(e.text, g.tasks) === task.id);
    if (mine.length) {
      const d = detailsFor('events:' + task.id, 'events (' + mine.length + ')');
      for (const e of mine) d.appendChild(eventLine(e));
      box.appendChild(el('h3', null, 'axo events'));
      box.appendChild(d);
    }
    box.appendChild(el('h3', null, 'execution'));
    if (n.status === 'skipped') {
      box.appendChild(el('p', 'muted', 'Not run: a task it depends on did not finish.'));
    } else if (!s) {
      box.appendChild(el('p', 'muted', 'Not started yet.'));
    } else {
      const exec = el('div', 'exec');
      execTree(s.id, exec);
      if (!exec.childNodes.length) exec.appendChild(el('p', 'muted', 'Starting…'));
      box.appendChild(exec);
    }
    return;
  }

  box.appendChild(grid);
  if (s && (children.get(s.id) || []).length) {
    box.appendChild(el('h3', null, 'steps'));
    const exec = el('div', 'exec');
    execTree(s.id, exec);
    box.appendChild(exec);
  }
  if (s && Object.keys(s.attrs || {}).length) {
    box.appendChild(el('h3', null, 'attributes'));
    attrsBlock(box, s);
    if (s.events && s.events.length) {
      box.appendChild(el('h3', null, 'events'));
      for (const e of s.events) {
        const ev = el('div', 'event s-' + (e.level || 'ok'));
        ev.appendChild(el('span', 't', fmtClock(t(e.time))));
        ev.appendChild(document.createTextNode(e.text));
        box.appendChild(ev);
      }
    }
  }
}

// ---------- main loop ----------

function render() {
  renderHeader();
  renderOverview();
  if (detailsDirty) {
    renderInspector();
    detailsDirty = false;
    if (scrollToSpan) {
      const target = [...document.querySelectorAll('#details details')].find((d) => d.dataset.span === scrollToSpan);
      if (target) target.scrollIntoView({ block: 'center' });
      scrollToSpan = null;
    }
  }
}

// Timers rather than requestAnimationFrame: rAF is paused in background tabs
// and headless browsers, and the viewer should keep up regardless.
function loop() {
  const running = [...spans.values()].some((s) => s.status === 'running');
  if (dirty || running || detailsDirty) {
    render();
    dirty = false;
  }
  setTimeout(loop, running ? 500 : 750);
}

function connect() {
  const conn = $('conn');
  const es = new EventSource('events');
  es.onopen = () => { conn.className = 'conn live'; conn.title = 'live'; };
  es.onerror = () => { conn.className = 'conn down'; conn.title = 'disconnected, retrying'; };
  es.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch (_) { return; }
    if (!msg || !Array.isArray(msg.spans)) return;
    if (msg.now) serverSkewMs = Date.parse(msg.now) - Date.now();
    if (msg.type === 'snapshot') { reset(); detailsDirty = true; }
    for (const s of msg.spans) {
      if (s && typeof s.id === 'string') upsert(s);
    }
    dirty = true;
  };
}

renderLegend();
connect();
loop();
