// Live trace viewer for axo. Receives spans over Server-Sent Events and draws
// a waterfall. All DOM is built with createElement/textContent: span names,
// attributes, and events come from agy and the AX cluster and are untrusted.
'use strict';

const spans = new Map(); // id -> span
const children = new Map(); // parent id -> [child ids]
const collapsed = new Set();
let selected = null;
let serverSkewMs = 0; // server now - client now
let dirty = true;
let detailsDirty = false; // re-render details only when the selected span changes

const KINDS = ['run', 'phase', 'task', 'attempt', 'ax', 'agy', 'tool'];
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

function now() { return Date.now() + serverSkewMs; }
function t(s) { return Date.parse(s); }
function endOf(s) { return s.end ? t(s.end) : now(); }

function fmtDur(ms) {
  if (ms < 1000) return Math.max(0, Math.round(ms)) + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + 's';
  const m = Math.floor(s / 60);
  return m + 'm' + Math.round(s - m * 60) + 's';
}

function fmtClock(s) {
  const d = new Date(s);
  return d.toLocaleTimeString([], { hour12: false });
}

function upsert(s) {
  const prev = spans.get(s.id);
  if (s.id === selected) detailsDirty = true;
  spans.set(s.id, s);
  if (!prev) {
    const p = s.parent || '';
    if (!children.has(p)) children.set(p, []);
    children.get(p).push(s.id);
  }
}

function reset() {
  spans.clear();
  children.clear();
}

// ---------- rendering ----------

function visibleRows() {
  const out = [];
  const showTools = $('show-tools').checked;
  const walk = (id, depth) => {
    for (const cid of children.get(id) || []) {
      const s = spans.get(cid);
      if (!s) continue;
      if (s.kind === 'tool' && !showTools) continue;
      out.push({ s, depth });
      if (!collapsed.has(cid)) walk(cid, depth + 1);
    }
  };
  walk('', 0);
  return out;
}

function bounds() {
  let lo = Infinity;
  let hi = -Infinity;
  for (const s of spans.values()) {
    lo = Math.min(lo, t(s.start));
    hi = Math.max(hi, endOf(s));
  }
  if (!isFinite(lo)) { lo = now(); hi = lo + 1000; }
  if (hi - lo < 1000) hi = lo + 1000;
  return { lo, hi };
}

function niceStep(totalMs) {
  const steps = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600].map((x) => x * 1000);
  for (const st of steps) if (totalMs / st <= 8) return st;
  return 7200 * 1000;
}

function renderAxis(b) {
  const axis = $('axis');
  axis.replaceChildren();
  const total = b.hi - b.lo;
  const step = niceStep(total);
  for (let x = 0; x <= total; x += step) {
    const tick = el('div', 'tick', fmtDur(x));
    tick.style.left = (100 * x / total) + '%';
    axis.appendChild(tick);
  }
}

function renderHeader() {
  const roots = children.get('') || [];
  const run = roots.length ? spans.get(roots[roots.length - 1]) : null;
  if (!run) return;
  $('run-name').textContent = (run.attrs && run.attrs.name) || run.name;
  const st = $('run-status');
  st.textContent = run.status;
  st.className = 'pill ' + run.status;
  $('run-elapsed').textContent = fmtDur(endOf(run) - t(run.start));
  let done = 0;
  let total = 0;
  for (const s of spans.values()) {
    if (s.kind !== 'task') continue;
    total++;
    if (s.status !== 'running') done++;
  }
  $('run-tasks').textContent = total ? done + '/' + total + ' tasks finished' : '';
  document.title = 'axo trace · ' + $('run-name').textContent;
}

function renderRows(b) {
  const rows = $('rows');
  const frag = document.createDocumentFragment();
  const total = b.hi - b.lo;
  for (const { s, depth } of visibleRows()) {
    const row = el('div', 'row' + (s.id === selected ? ' selected' : ''));
    row.addEventListener('click', () => select(s.id));

    const name = el('div', 'name-col');
    name.style.paddingLeft = (8 + depth * 14) + 'px';
    const hasKids = (children.get(s.id) || []).length > 0;
    const caret = el('span', 'caret', hasKids ? (collapsed.has(s.id) ? '▸' : '▾') : '');
    if (hasKids) {
      caret.addEventListener('click', (ev) => {
        ev.stopPropagation();
        if (collapsed.has(s.id)) collapsed.delete(s.id); else collapsed.add(s.id);
        dirty = true;
      });
    }
    name.appendChild(caret);
    name.appendChild(el('span', 'kind', KIND_LABELS[s.kind] || s.kind));
    const label = el('span', 's-' + s.status, s.name);
    label.title = s.name;
    name.appendChild(label);
    row.appendChild(name);

    const track = el('div', 'track');
    const start = t(s.start);
    const end = endOf(s);
    const bar = el('div', 'bar k-' + (KINDS.includes(s.kind) ? s.kind : 'run') +
      (s.status === 'running' ? ' running' : '') +
      (s.status === 'error' ? ' error' : '') +
      (s.status === 'warn' ? ' warn' : '') +
      (s.status === 'skipped' ? ' skipped' : ''));
    const left = 100 * (start - b.lo) / total;
    const width = Math.max(0.15, 100 * (end - start) / total);
    bar.style.left = left + '%';
    bar.style.width = width + '%';
    track.appendChild(bar);
    const dur = el('div', 'dur', fmtDur(end - start));
    dur.style.left = 'calc(' + Math.min(left + width, 92) + '% + 4px)';
    track.appendChild(dur);
    row.appendChild(track);

    frag.appendChild(row);
  }
  rows.replaceChildren(frag);
}

function kvRow(grid, k, v) {
  grid.appendChild(el('div', 'k', k));
  grid.appendChild(el('div', 'v', v));
}

function renderDetails() {
  const box = $('details');
  const s = selected && spans.get(selected);
  if (!s) return;
  box.replaceChildren();
  box.appendChild(el('h2', 's-' + s.status, s.name));
  const grid = el('div', 'kv');
  kvRow(grid, 'kind', KIND_LABELS[s.kind] || s.kind);
  kvRow(grid, 'status', s.status);
  kvRow(grid, 'started', fmtClock(t(s.start)));
  kvRow(grid, 'duration', fmtDur(endOf(s) - t(s.start)) + (s.end ? '' : ' (running)'));
  box.appendChild(grid);

  const attrs = Object.entries(s.attrs || {}).sort(([a], [b]) => a.localeCompare(b));
  if (attrs.length) {
    box.appendChild(el('h3', null, 'attributes'));
    for (const [k, v] of attrs) {
      const long = v.length > 80 || v.includes('\n');
      if (long) {
        box.appendChild(el('div', 'k muted', k));
        box.appendChild(el('pre', 'block', v));
      } else {
        const g = el('div', 'kv');
        kvRow(g, k, v);
        box.appendChild(g);
      }
    }
  }
  if (s.events && s.events.length) {
    box.appendChild(el('h3', null, 'events'));
    for (const e of s.events) {
      const line = el('div', 'event s-' + (e.level || 'ok'));
      line.appendChild(el('span', 't', fmtClock(t(e.time))));
      line.appendChild(document.createTextNode(e.text));
      box.appendChild(line);
    }
  }
}

function select(id) {
  selected = id;
  dirty = true;
  detailsDirty = true;
}

function renderLegend() {
  const legend = $('legend');
  for (const k of KINDS) {
    const item = el('span');
    item.appendChild(el('span', 'swatch k-' + k));
    item.appendChild(document.createTextNode(KIND_LABELS[k]));
    legend.appendChild(item);
  }
}

function render() {
  const b = bounds();
  renderHeader();
  renderAxis(b);
  renderRows(b);
  if (detailsDirty) {
    renderDetails();
    detailsDirty = false;
  }
  if ($('follow').checked) {
    const wf = document.querySelector('.waterfall');
    wf.scrollTop = wf.scrollHeight;
  }
}

// Timers rather than requestAnimationFrame: rAF is paused in background tabs
// and headless browsers, and the viewer should keep up regardless.
function loop() {
  const running = [...spans.values()].some((s) => s.status === 'running');
  if (dirty || running) {
    render();
    dirty = false;
  }
  setTimeout(loop, running ? 250 : 500);
}

// ---------- live stream ----------

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
    if (msg.type === 'snapshot') reset();
    for (const s of msg.spans) {
      if (s && typeof s.id === 'string') upsert(s);
    }
    dirty = true;
  };
}

$('show-tools').addEventListener('change', () => { dirty = true; });
$('follow').addEventListener('change', () => { dirty = true; });
renderLegend();
connect();
loop();
