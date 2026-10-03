// VPS probe UI: a small hash-routed single-page app over the read-only API.
//
// Strings reported by agents (hostnames, interfaces, mounts, peer names) are
// untrusted: they only ever reach the DOM as text via h(), reach URLs via
// encodeURIComponent, and reach charts as canvas text (richText tooltips).
// Nothing here builds HTML from strings.

'use strict';

const $app = document.getElementById('app');
const $status = document.getElementById('status');
const $foot = document.getElementById('foot');

// ---------- DOM helpers ----------

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'href' || k === 'title' || k.startsWith('data-')) el.setAttribute(k, v);
    else throw new Error('h: unsupported prop ' + k);
  }
  for (const c of kids.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

const enc = encodeURIComponent;
const fmtPPS = v => v == null ? '—' : v >= 1e4 ? (v / 1e4).toFixed(1) + ' 万包/秒' : Math.round(v) + ' 包/秒';
function dec(s) {
  try { return decodeURIComponent(s); } catch { return s; }
}

// A progress bar; width is set through CSSOM, which CSP allows.
function bar(pct) {
  const fill = h('span');
  const p = Math.max(0, Math.min(100, pct || 0));
  fill.style.width = p + '%';
  return h('div', { class: 'bar' + (p >= 90 ? ' bad' : p >= 80 ? ' warn' : '') }, fill);
}

// A sparkline: an SVG line per series over the same x range, each scaled to
// max. A gap in the data breaks the line. series: [values, class] pairs.
const SVG_NS = 'http://www.w3.org/2000/svg';
function sparkline(series, max, title) {
  const W = 120, H = 30;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', 'spark');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('preserveAspectRatio', 'none');
  const tip = document.createElementNS(SVG_NS, 'title');
  tip.textContent = title;
  svg.append(tip);
  for (const [vals, cls] of series) {
    let d = '', pen = false;
    vals.forEach((v, i) => {
      if (v == null) { pen = false; return; }
      const x = i * W / (vals.length - 1), y = H - 1.5 - Math.min(1, v / (max || 1)) * (H - 3);
      d += (pen ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
      pen = true;
    });
    const path = document.createElementNS(SVG_NS, 'path');
    path.setAttribute('d', d);
    path.setAttribute('class', cls);
    svg.append(path);
  }
  return svg;
}

// A <select> over [value, label] pairs. fill() only rebuilds the options when
// they change, so the periodic refresh doesn't close an open menu.
function picker(onPick) {
  const el = h('select', { onchange: () => onPick(el.value) });
  let key = '';
  el.fill = (options, current) => {
    const k = JSON.stringify([options, current]);
    if (k === key) return;
    key = k;
    el.replaceChildren(...options.map(([value, label]) => {
      const o = h('option', { text: label });
      o.value = value;
      return o;
    }));
    el.value = current;
  };
  return el;
}

function seg(options, current, onPick) {
  return h('div', { class: 'seg' }, options.map(([value, label]) =>
    h('button', { class: value === current ? 'active' : null, text: label, onclick: () => onPick(value) })));
}

// ---------- formatting ----------

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
function fmtBytes(n) {
  if (n == null || !isFinite(n)) return '—';
  let v = Math.abs(n), i = 0;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  const s = i === 0 ? v.toFixed(0) : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2);
  return (n < 0 ? '-' : '') + s + ' ' + UNITS[i];
}
const fmtRate = n => n == null ? '—' : fmtBytes(n) + '/s';
const fmtPct = (v, d = 1) => v == null || !isFinite(v) ? '—' : v.toFixed(d) + '%';
const fmtMs = v => v == null ? '—' : (v < 10 ? v.toFixed(2) : v < 100 ? v.toFixed(1) : v.toFixed(0)) + ' ms';
function fmtDur(sec) {
  if (sec == null || sec < 0) return '—';
  const d = Math.floor(sec / 86400), hr = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d > 0) return `${d} 天 ${hr} 小时`;
  if (hr > 0) return `${hr} 小时 ${m} 分`;
  return `${m} 分`;
}
const fmtTime = unix => new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false });
const nowSec = () => Math.floor(Date.now() / 1000);

const QUOTA_MODES = { sum: '收+发', max: '取大', tx: '仅上行', rx: '仅下行' };
function billable(rx, tx, mode) {
  switch (mode) {
    case 'max': return Math.max(rx, tx);
    case 'tx': return tx;
    case 'rx': return rx;
    default: return rx + tx;
  }
}
const quotaBytes = gb => gb * 1024 ** 3; // traffic_quota_gb counts 1024³ bytes

// expire_days is counted by the server in its timezone: 0 = today.
function fmtExpiry(days) {
  if (days > 0) return `剩 ${days} 天`;
  return days === 0 ? '今天到期' : `已过期 ${-days} 天`;
}
const expiryClass = days => days <= 0 ? 'bad' : days <= 7 ? 'warn' : null;

// Agent versions like "0.1.4"; anything else (e.g. "dev") is not compared.
function parseVer(v) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)/.exec(v || '');
  return m ? m.slice(1).map(Number) : null;
}
function cmpVer(a, b) {
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] - b[i];
  return 0;
}
// The newest agent version among the nodes. Server-only releases don't
// change agents, so agents are compared with each other, not the server.
function newestAgent(nodes) {
  let best = null;
  for (const n of nodes) {
    const v = parseVer(n.status && n.status.sys && n.status.sys.agent_version);
    if (v && (!best || cmpVer(v, best) > 0)) best = v;
  }
  return best;
}

// ---------- API ----------

async function api(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`${r.status} ${(await r.text()).trim()}`);
  return r.json();
}

// ---------- charts ----------

const darkQuery = matchMedia('(prefers-color-scheme: dark)');

class Chart {
  constructor(title, group) {
    this.box = h('div', { class: 'chart' });
    this.el = h('div', { class: 'panel' }, h('h2', { text: title }), this.box);
    this.c = null;
    this.group = group;
    this.tip = true; // show the tooltip box, not only the crosshair
    this.ro = new ResizeObserver(() => this.c && this.c.resize());
    this.ro.observe(this.box);
  }
  set(option) {
    if (!this.c) {
      this.c = echarts.init(this.box, darkQuery.matches ? 'dark' : null);
      if (this.group) this.group.add(this);
    }
    if (option.tooltip) option.tooltip.showContent = this.tip;
    this.c.setOption(option, { notMerge: true, lazyUpdate: true });
  }
  // note shows a line of text under the title, or hides it when empty.
  note(text) {
    if (!this.noteEl) {
      this.noteEl = h('div', { class: 'muted chart-note' });
      this.el.insertBefore(this.noteEl, this.box);
    }
    this.noteEl.textContent = text || '';
    this.noteEl.hidden = !text;
  }
  dispose() {
    this.ro.disconnect();
    if (this.c) this.c.dispose();
    this.c = null; // a ChartGroup frame may still be queued
  }
}

// Charts sharing a time axis: hovering one moves the crosshair on all of
// them to the same moment, so a CPU spike can be lined up with a latency
// one. Not echarts.connect: that syncs by data index, which lands on the
// wrong time (or nowhere) in charts sampled differently, like disks every
// 60s, or with gaps. Only the chart under the pointer shows its tooltip;
// the others' would cover the very lines being compared.
// hideTip alone leaves a crosshair placed by showTip; ECharts' own mouse-out
// sends this leave trigger too.
function hideTip(c) {
  c.c.dispatchAction({ type: 'hideTip' });
  c.c.dispatchAction({ type: 'updateAxisPointer', currTrigger: 'leave' });
}

class ChartGroup {
  constructor() {
    this.charts = [];
    this.active = null;
    this.frame = 0;
  }
  add(chart) {
    this.charts.push(chart);
    const zr = chart.c.getZr();
    zr.on('mousemove', e => {
      this.focus(chart);
      cancelAnimationFrame(this.frame);
      this.frame = requestAnimationFrame(() => this.follow(chart, e.offsetX, e.offsetY));
    });
    zr.on('globalout', () => {
      cancelAnimationFrame(this.frame);
      this.others(chart, hideTip);
    });
  }
  others(chart, fn) {
    for (const c of this.charts) if (c !== chart && c.c && !c.el.hidden) fn(c);
  }
  focus(chart) {
    if (this.active === chart) return;
    this.active = chart;
    for (const c of this.charts) {
      c.tip = c === chart;
      if (c.c) c.c.setOption({ tooltip: { showContent: c.tip } });
    }
  }
  // follow puts the other charts' crosshairs at the time under (x, y).
  follow(chart, x, y) {
    if (!chart.c) return; // page left
    if (!chart.c.containPixel('grid', [x, y])) {
      this.others(chart, hideTip);
      return;
    }
    const t = chart.c.convertFromPixel({ xAxisIndex: 0 }, x);
    this.others(chart, c => {
      // Just above the x axis: inside the plot whatever the y range.
      c.c.dispatchAction({ type: 'showTip', x: c.c.convertToPixel({ xAxisIndex: 0 }, t),
        y: c.c.convertToPixel({ yAxisIndex: 0 }, 0) - 2 });
    });
  }
}

// Series points; a null is inserted where data is missing so lines break
// over outages instead of bridging them. "Missing" is judged against the
// series' own typical spacing, since some data (disks: every 60s) is sparser
// than the bucket step.
// extra: more columns to carry along in each point, e.g. loss beside avg.
function points(ts, vals, step, f = v => v, extra = []) {
  const diffs = ts.slice(1).map((t, i) => t - ts[i]).sort((a, b) => a - b);
  const typical = Math.max(step, diffs.length ? diffs[diffs.length >> 1] : step);
  const out = [];
  for (let i = 0; i < ts.length; i++) {
    if (i > 0 && ts[i] - ts[i - 1] > typical * 2.5) out.push([(ts[i - 1] + step) * 1000, null]);
    out.push([ts[i] * 1000, vals[i] == null ? null : f(vals[i]), ...extra.map(a => a[i])]);
  }
  return out;
}

function line(name, data, extra) {
  return Object.assign({ type: 'line', name, data, showSymbol: false, connectNulls: false,
    lineStyle: { width: 1.5 }, emphasis: { disabled: true } }, extra);
}

function timeOption(from, to, yFmt, series, { yMax, y2, tipFmt = yFmt } = {}) {
  const yAxis = [{ type: 'value', min: 0, max: yMax, axisLabel: { formatter: yFmt },
    splitLine: { lineStyle: { opacity: 0.35 } } }];
  if (y2) yAxis.push(y2);
  return {
    backgroundColor: 'transparent',
    animation: false,
    grid: { left: 8, right: 12, top: 36, bottom: 8, containLabel: true },
    tooltip: { trigger: 'axis', renderMode: 'richText', confine: true,
      valueFormatter: v => v == null ? '—' : tipFmt(v) },
    legend: { top: 0, type: 'scroll' },
    xAxis: { type: 'time', min: from * 1000, max: to * 1000, axisLabel: { hideOverlap: true } },
    yAxis,
    series,
  };
}

// ---------- router ----------

const routes = [
  [/^\/$/, () => overviewPage()],
  [/^\/node\/([^/]+)$/, m => nodePage(dec(m[1]))],
  [/^\/ping$/, () => matrixPage()],
  [/^\/ping\/([^/]+)\/([^/]+)$/, m => linkPage(dec(m[1]), dec(m[2]))],
  [/^\/traffic$/, () => trafficPage()],
  [/^\/alerts$/, () => alertsPage()],
];

let page = null;
let gen = 0;
let timer = 0;

function navigate() {
  if (page && page.destroy) page.destroy();
  clearTimeout(timer);
  const path = location.hash.replace(/^#/, '') || '/';
  let found = null;
  for (const [re, make] of routes) {
    const m = path.match(re);
    if (m) { found = make(m); break; }
  }
  page = found || { nav: '', el: h('div', { class: 'empty', text: '页面不存在' }) };
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('active', a.dataset.route === page.nav);
  }
  $app.replaceChildren(page.el);
  const my = ++gen;
  const loop = async () => {
    if (!page.refresh) return;
    try {
      await page.refresh();
      if (my !== gen) return;
      $status.textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
      $status.classList.remove('error');
    } catch (e) {
      if (my !== gen) return;
      $status.textContent = '更新失败：' + e.message;
      $status.classList.add('error');
    }
    if (my === gen) timer = setTimeout(loop, page.interval ? page.interval() : 10000);
  };
  loop();
}

// Re-render (and let pages re-run refresh) without leaving the page.
function rerender() { navigate(); }

// ---------- overview ----------

// ---------- preferences ----------

// Per-browser view choices. Storage may be missing or throw (private
// windows, blocked site data); the page then just uses the defaults.
function loadPref(key, def) {
  try {
    const v = JSON.parse(localStorage.getItem('vps-probe.' + key));
    return v && typeof v === 'object' ? { ...def, ...v } : def;
  } catch {
    return def;
  }
}
function savePref(key, v) {
  try { localStorage.setItem('vps-probe.' + key, JSON.stringify(v)); } catch { /* not persisted */ }
}

// ---------- overview ----------

const NO_GROUP = '\n'; // groups can't contain control characters
const ovPrefs = loadPref('overview', { view: 'cards', sort: '', group: '' });
const setOvPref = (k, v) => { ovPrefs[k] = v; savePref('overview', ovPrefs); rerender(); };

const totalRate = (s, k) => (s.net || []).reduce((a, x) => a + x[k], 0);
const pctOf = (used, total) => total ? 100 * used / total : null;
// [value, label, key, descending]. A null key sorts last. Live metrics
// (descending sorts) also put nodes that are not online last, since their
// values are stale.
const live = f => n => n.status ? f(n.status, n) ?? null : null;
const SORTS = [
  ['', '默认顺序'],
  ['name', '名称', n => n.name],
  ['region', '地区', n => n.region || null],
  ['cpu', 'CPU', live(s => s.cpu), true],
  ['mem', '内存', live(s => pctOf(s.mem_used, s.mem_total)), true],
  ['disk', '磁盘', live(s => { const d = rootDisk(s.disks); return d ? pctOf(d.used, d.total) : null; }), true],
  ['rx', '下行网速', live(s => totalRate(s, 'rx')), true],
  ['tx', '上行网速', live(s => totalRate(s, 'tx')), true],
  ['traffic', '本周期流量', live(s => s.traffic ? s.traffic.rx + s.traffic.tx : null), true],
  ['quota', '配额使用率', live((s, n) => n.traffic_quota_gb && s.traffic
    ? pctOf(billable(s.traffic.rx, s.traffic.tx, n.traffic_quota_mode), quotaBytes(n.traffic_quota_gb)) : null), true],
  ['tcp', 'TCP 连接', live(s => s.tcp), true],
  ['expiry', '到期', n => n.expire_days ?? null],
];

function sortNodes(nodes, by) {
  const spec = SORTS.find(x => x[0] === by);
  if (!spec || !spec[2]) return nodes; // config order
  const [, , key, desc] = spec;
  const dir = desc ? -1 : 1;
  const cmp = (a, b) => typeof a === 'string' ? a.localeCompare(b, 'zh-CN') : a - b;
  return nodes.map((n, i) => ({ n, i, v: key(n), down: desc && !n.online })).sort((a, b) =>
    a.down - b.down || (a.v == null) - (b.v == null) || (a.v == null ? 0 : dir * cmp(a.v, b.v)) || a.i - b.i)
    .map(x => x.n);
}

function overviewPage() {
  const body = h('div');
  const stats = h('div', { class: 'stats panel' });
  const tabs = h('div');
  const sortSel = picker(v => setOvPref('sort', v));
  return {
    nav: 'overview',
    el: h('div', null,
      h('div', { class: 'row filters' }, h('h1', { text: '总览' }), h('span', { class: 'spacer' }),
        seg([['cards', '卡片'], ['table', '表格']], ovPrefs.view, v => setOvPref('view', v)), sortSel),
      tabs, h('div', { class: 'section' }, stats), h('div', { class: 'section' }, body)),
    async refresh() {
      const cards = ovPrefs.view !== 'table';
      const [all, sparks] = await Promise.all([api('/api/nodes'), cards ? api('/api/sparks') : null]);
      // Group tabs in the order groups first appear in the config.
      const groups = [...new Set(all.map(n => n.group || NO_GROUP))];
      let group = ovPrefs.group;
      if (group && !groups.includes(group)) group = '';
      if (all.some(n => n.group)) {
        const count = g => all.filter(n => (n.group || NO_GROUP) === g).length;
        tabs.replaceChildren(seg([['', `全部 ${all.length}`],
          ...groups.map(g => [g, `${g === NO_GROUP ? '未分组' : g} ${count(g)}`])], group, v => setOvPref('group', v)));
        tabs.className = 'section';
      } else {
        tabs.replaceChildren();
      }
      const nodes = sortNodes(group ? all.filter(n => (n.group || NO_GROUP) === group) : all, ovPrefs.sort);
      sortSel.fill(SORTS.map(([v, label]) => [v, '排序：' + label]), ovPrefs.sort);
      stats.replaceChildren(...overviewStats(nodes));
      const newest = newestAgent(all);
      if (ovPrefs.view === 'table') {
        body.className = 'panel table-wrap';
        body.replaceChildren(nodeTable(nodes, newest));
      } else {
        body.className = 'cards';
        body.replaceChildren(...nodes.map(n => nodeCard(n, newest, sparks.nodes[n.id])));
      }
    },
  };
}

// The agent-version check, shared by cards and table rows.
function agentStale(n, newest) {
  const ver = n.status && n.status.sys && n.status.sys.agent_version;
  const pv = parseVer(ver);
  return pv && newest && cmpVer(pv, newest) < 0 ? ver : null;
}

const regionBadge = n => n.region ? h('span', { class: 'region', text: n.region }) : null;

function connText(s) {
  const parts = [];
  if (s.tcp != null) parts.push(`TCP ${s.tcp}`, `UDP ${s.udp}`);
  if (s.threads != null) parts.push(`线程 ${s.threads}`);
  return parts.join(' · ');
}

// ServerStatus-style dense table: one row per node, click for details.
function nodeTable(nodes, newest) {
  const pctCell = (pct, title) => h('td', { title }, bar(pct), h('span', { class: 'num', text: fmtPct(pct, 0) }));
  const rows = nodes.map(n => {
    const s = n.status;
    const name = h('td', null, h('span', { class: 'dot ' + (n.online ? 'on' : 'off') }), ' ',
      h('a', { href: '#/node/' + enc(n.id), text: n.name }), ' ', regionBadge(n),
      agentStale(n, newest) ? h('span', { class: 'badge warn', text: '可升级', title: 'agent ' + agentStale(n, newest) }) : null);
    const tr = h('tr', { class: 'clickable' + (n.online ? '' : ' offline'),
      onclick: e => { if (e.target.tagName !== 'A') location.hash = '#/node/' + enc(n.id); } }, name);
    if (!s) {
      tr.append(h('td', { class: 'muted', text: '从未上报' }), ...Array.from({ length: 8 }, () => h('td')));
      return tr;
    }
    const d = rootDisk(s.disks);
    const t = s.traffic;
    const quota = n.traffic_quota_gb && t
      ? pctOf(billable(t.rx, t.tx, n.traffic_quota_mode), quotaBytes(n.traffic_quota_gb)) : null;
    tr.append(
      pctCell(s.cpu),
      pctCell(pctOf(s.mem_used, s.mem_total), s.mem_total ? `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}` : null),
      pctCell(d ? pctOf(d.used, d.total) : null, d ? `${d.mount} ${fmtBytes(d.used)} / ${fmtBytes(d.total)}` : null),
      h('td', { class: 'num', text: `↓ ${fmtRate(totalRate(s, 'rx'))} ↑ ${fmtRate(totalRate(s, 'tx'))}` }),
      h('td', { class: 'num', title: t ? `${t.start} 起` : null,
        text: t ? `↓ ${fmtBytes(t.rx)} ↑ ${fmtBytes(t.tx)}` + (quota != null ? ` · ${fmtPct(quota, 0)}` : '') : '—' }),
      h('td', { class: 'num', text: s.load1 != null ? s.load1.toFixed(2) : '—' }),
      h('td', { class: 'num', text: s.tcp != null ? `${s.tcp} / ${s.udp}` : '—', title: connText(s) || null }),
      h('td', { class: 'num', text: s.sys && s.sys.boot_time ? fmtDur(nowSec() - s.sys.boot_time) : '—' }),
      h('td', { class: 'num' + (expiryClass(n.expire_days) ? ' ' + expiryClass(n.expire_days) : ''),
        text: n.expire_at ? fmtExpiry(n.expire_days) : '—', title: n.expire_at ? [n.expire_at, n.price].filter(Boolean).join(' · ') : null }));
    return tr;
  });
  return h('table', { class: 'nodes' },
    h('thead', null, h('tr', null, ['节点', 'CPU', '内存', '磁盘', '网速', '本周期流量', '负载', 'TCP / UDP', '运行', '到期']
      .map(c => h('th', { text: c })))),
    h('tbody', null, rows));
}

function overviewStats(nodes) {
  const stat = (k, v, cls, title) => h('div', { class: 'stat', title },
    h('div', { class: 'k', text: k }), h('div', { class: 'v num' + (cls ? ' ' + cls : ''), text: v }));
  const online = nodes.filter(n => n.online);
  let rx = 0, tx = 0, trx = 0, ttx = 0;
  for (const n of online) for (const x of n.status.net || []) { rx += x.rx; tx += x.tx; }
  for (const n of nodes) {
    const t = n.status && n.status.traffic;
    if (t) { trx += t.rx; ttx += t.tx; }
  }
  const out = [
    stat('在线', `${online.length} / ${nodes.length}`, online.length < nodes.length ? 'warn' : null),
    stat('实时网速', `↓ ${fmtRate(rx)}   ↑ ${fmtRate(tx)}`, null, '在线节点各物理网卡之和'),
    stat('本周期流量合计', `↓ ${fmtBytes(trx)}   ↑ ${fmtBytes(ttx)}`, null, '各节点按各自的周期起始日计算后相加'),
  ];
  // The nearest upcoming date; an already expired node only when none is upcoming,
  // so one kept in the config for its history doesn't pin this forever.
  const dated = nodes.filter(n => n.expire_days != null);
  const upcoming = dated.filter(n => n.expire_days >= 0);
  const due = (upcoming.length ? upcoming : dated).sort((a, b) => Math.abs(a.expire_days) - Math.abs(b.expire_days));
  if (due.length) {
    const n = due[0];
    out.push(stat('最近到期', `${n.name} · ${fmtExpiry(n.expire_days)}`, expiryClass(n.expire_days), n.expire_at));
  }
  return out;
}

function rootDisk(disks) {
  return (disks || []).find(d => d.mount === '/') || (disks || [])[0];
}

// The last hour on a card: CPU against 100%, and both directions of the
// network against the hour's own peak.
function cardSparks(sp) {
  const peak = Math.max(1, ...sp.rx.map(v => v || 0), ...sp.tx.map(v => v || 0));
  const top = Math.max(0, ...sp.cpu.map(v => v || 0));
  const swatch = cls => h('i', { class: 'swatch ' + cls });
  return h('div', { class: 'sparks' },
    h('div', null, h('div', { class: 'spark-label', text: 'CPU · 近 1 小时' }),
      sparkline([[sp.cpu, 's1']], 100, `近 1 小时 CPU，最高 ${fmtPct(top)}（纵轴 0–100%）`)),
    h('div', null, h('div', { class: 'spark-label' }, '网速 ', swatch('s1'), '↓ ', swatch('s2'), '↑'),
      sparkline([[sp.rx, 's1'], [sp.tx, 's2']], peak, `近 1 小时网速，峰值 ${fmtRate(peak)}`)));
}

function nodeCard(n, newest, sp) {
  const s = n.status;
  const ver = agentStale(n, newest);
  const head = h('div', { class: 'card-head' },
    h('span', { class: 'dot ' + (n.online ? 'on' : 'off'), title: n.online ? '在线' : '离线' }),
    h('span', { class: 'name', text: n.name }),
    n.name !== n.id ? h('span', { class: 'id muted', text: n.id }) : null,
    regionBadge(n),
    h('span', { class: 'spacer' }),
    !s ? h('span', { class: 'badge', text: '从未上报' })
      : !n.online ? h('span', { class: 'badge bad', text: '离线 ' + fmtDur(nowSec() - s.fresh_at) }) : null,
    s && Math.abs(s.clock_skew) > 60 ? h('span', { class: 'badge warn', text: `时钟偏差 ${s.clock_skew}s` }) : null,
    n.expire_days != null && n.expire_days <= 7
      ? h('span', { class: 'badge ' + expiryClass(n.expire_days), text: fmtExpiry(n.expire_days) }) : null,
    ver ? h('span', { class: 'badge warn', text: 'agent 可升级', title: `agent ${ver}，最新 ${newest.join('.')}` }) : null);
  const card = h('a', { class: 'card panel' + (n.online ? '' : ' offline'), href: '#/node/' + enc(n.id) }, head);
  const expiry = n.expire_at ? h('div', { class: 'kv' }, h('span', { class: 'k', text: '到期' }),
    h('span', { class: 'num' + (expiryClass(n.expire_days) ? ' ' + expiryClass(n.expire_days) : ''),
      text: [n.expire_at, fmtExpiry(n.expire_days), n.price].filter(Boolean).join(' · ') })) : null;
  if (!s) {
    if (expiry) card.append(expiry);
    return card;
  }

  const sys = s.sys;
  card.append(h('div', { class: 'card-sub muted', text: sys
    ? [sys.os, sys.arch, sys.cores ? sys.cores + ' 核' : null, sys.boot_time ? '运行 ' + fmtDur(nowSec() - sys.boot_time) : null]
      .filter(Boolean).join(' · ')
    : '—' }));
  if (sp) card.append(cardSparks(sp));

  const metric = (label, pct, val) => h('div', { class: 'metric' },
    h('span', { class: 'label', text: label }), bar(pct), h('span', { class: 'val', text: val }));
  card.append(metric('CPU', s.cpu, fmtPct(s.cpu)));
  if (s.mem_total) card.append(metric('内存', 100 * s.mem_used / s.mem_total, `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}`));
  if (s.swap_total) card.append(metric('Swap', 100 * s.swap_used / s.swap_total, `${fmtBytes(s.swap_used)} / ${fmtBytes(s.swap_total)}`));
  const d = rootDisk(s.disks);
  if (d) card.append(metric('磁盘', 100 * d.used / d.total, `${fmtBytes(d.used)} / ${fmtBytes(d.total)}`));

  card.append(h('div', { class: 'kv' }, h('span', { class: 'k', text: '网速' }),
    h('span', { class: 'num', text: `↓ ${fmtRate(totalRate(s, 'rx'))}   ↑ ${fmtRate(totalRate(s, 'tx'))}` })));
  if (s.load1 != null) {
    card.append(h('div', { class: 'kv' }, h('span', { class: 'k', text: '负载' }),
      h('span', { class: 'num', text: [s.load1, s.load5, s.load15].map(v => v.toFixed(2)).join(' / ') })));
  }
  if (connText(s)) {
    card.append(h('div', { class: 'kv' }, h('span', { class: 'k', text: '连接' }), h('span', { class: 'num', text: connText(s) })));
  }
  const t = s.traffic;
  if (t) {
    const used = billable(t.rx, t.tx, n.traffic_quota_mode);
    card.append(h('div', { class: 'kv' }, h('span', { class: 'k', text: `本周期流量（${t.start} 起）` }),
      h('span', { class: 'num', text: `↓ ${fmtBytes(t.rx)}   ↑ ${fmtBytes(t.tx)}` })));
    if (n.traffic_quota_gb) {
      const q = quotaBytes(n.traffic_quota_gb);
      card.append(metric('配额', 100 * used / q, `${fmtBytes(used)} / ${fmtBytes(q)}`));
    }
  }
  if (expiry) card.append(expiry);
  return card;
}

// ---------- node detail ----------

const RANGES = [['1h', '1 小时', 3600], ['6h', '6 小时', 6 * 3600], ['24h', '24 小时', 86400],
  ['7d', '7 天', 7 * 86400], ['30d', '30 天', 30 * 86400]];
let rangeKey = '1h';
const rangeSec = () => RANGES.find(r => r[0] === rangeKey)[2];
const rangeSeg = () => seg(RANGES.map(r => [r[0], r[1]]), rangeKey, v => { rangeKey = v; rerender(); });
const rangeInterval = () => rangeSec() > 86400 ? 60000 : 10000;

function nodePage(id) {
  const title = h('h1', { text: id });
  const dot = h('span', { class: 'dot' });
  const sub = h('div', { class: 'muted' });
  const summary = h('div', { class: 'summary' });
  const group = new ChartGroup();
  const charts = {
    cpu: new Chart('CPU', group), load: new Chart('负载', group), mem: new Chart('内存 / Swap', group),
    net: new Chart('网络', group), pps: new Chart('包速率', group), disk: new Chart('磁盘使用率', group), conns: new Chart('连接与线程', group),
    ping: new Chart('时延（到各 peer）', group),
  };
  const el = h('div', null,
    h('div', { class: 'row' }, h('a', { href: '#/', text: '← 总览' })),
    h('div', { class: 'panel section' },
      h('div', { class: 'row' }, dot, title), sub, summary),
    h('div', { class: 'row section' }, h('span', { class: 'muted', text: '时间范围' }), rangeSeg()),
    h('div', { class: 'charts section' }, Object.values(charts).map(c => c.el)));

  return {
    nav: 'overview',
    el,
    interval: rangeInterval,
    destroy() { Object.values(charts).forEach(c => c.dispose()); },
    async refresh() {
      const to = nowSec(), from = to - rangeSec();
      const q = `from=${from}&to=${to}`;
      const [nodes, m, net, disks, matrix] = await Promise.all([
        api('/api/nodes'),
        api(`/api/nodes/${enc(id)}/metrics?${q}`),
        api(`/api/nodes/${enc(id)}/net?${q}`),
        api(`/api/nodes/${enc(id)}/disks?${q}`),
        api('/api/ping/matrix?window=1h'),
      ]);
      const n = nodes.find(x => x.id === id);
      if (!n) throw new Error('未知节点 ' + id);
      renderNodeHead(n, title, dot, sub, summary);

      const peers = [...new Set(matrix.links.filter(l => l.src === id).map(l => l.dst))];
      const pings = await Promise.all(peers.map(d => api(`/api/ping/${enc(id)}/${enc(d)}?${q}`)));

      const st = m.step, rollup = m.tier !== 'raw';
      const c = m.cols;
      charts.cpu.set(timeOption(from, to, v => v.toFixed(0) + '%', [
        line('CPU', points(m.ts, c.cpu, st)),
        line('Steal', points(m.ts, c.steal, st)),
        // Part of CPU, not on top of it; agents before 0.1.14 don't report it.
        c.softirq.some(v => v != null) ? line('软中断', points(m.ts, c.softirq, st)) : null,
        rollup ? line('CPU 峰值', points(m.ts, c.cpu_max, st), { lineStyle: { width: 1, type: 'dashed' } }) : null,
      ].filter(Boolean), { yMax: 100, tipFmt: v => fmtPct(v) }));
      charts.load.set(timeOption(from, to, v => v.toFixed(2), [
        line('1 分钟', points(m.ts, c.load1, st)),
        line('5 分钟', points(m.ts, c.load5, st)),
        line('15 分钟', points(m.ts, c.load15, st)),
      ]));
      const memMax = Math.max(0, ...c.mem_total.filter(v => v != null));
      charts.mem.set(timeOption(from, to, fmtBytes, [
        line('内存', points(m.ts, c.mem_used, st), { areaStyle: { opacity: 0.15 } }),
        line('Swap', points(m.ts, c.swap_used, st)),
      ], { yMax: memMax || undefined }));

      // Threads run in the hundreds and up, so they get the right axis.
      const hasConns = c.tcp.some(v => v != null) || c.threads.some(v => v != null);
      charts.conns.el.hidden = !hasConns; // agents before 0.1.9 report neither
      if (hasConns) {
        const count = v => v == null ? '—' : Math.round(v).toString();
        charts.conns.set(timeOption(from, to, count, [
          line('TCP', points(m.ts, c.tcp, st)),
          line('UDP', points(m.ts, c.udp, st)),
          line('TIME_WAIT', points(m.ts, c.tcp_tw, st), { lineStyle: { width: 1, type: 'dashed' } }),
          line('线程', points(m.ts, c.threads, st), { yAxisIndex: 1 }),
        ], { y2: { type: 'value', min: 0, splitLine: { show: false } } }));
      }

      const netSeries = [];
      for (const iface of Object.keys(net).sort()) {
        const s = net[iface];
        netSeries.push(line(`${iface} ↓`, points(s.ts, s.cols.rx, s.step)));
        netSeries.push(line(`${iface} ↑`, points(s.ts, s.cols.tx, s.step)));
      }
      charts.net.set(timeOption(from, to, fmtRate, netSeries));

      // Packets: a flood of small packets (SYN) barely shows as bytes.
      const ppsSeries = [];
      for (const iface of Object.keys(net).sort()) {
        const s = net[iface];
        if (!s.cols.rx_pps.some(v => v != null)) continue; // agent before 0.1.14
        ppsSeries.push(line(`${iface} ↓`, points(s.ts, s.cols.rx_pps, s.step)));
        ppsSeries.push(line(`${iface} ↑`, points(s.ts, s.cols.tx_pps, s.step)));
      }
      charts.pps.el.hidden = !ppsSeries.length;
      if (ppsSeries.length) charts.pps.set(timeOption(from, to, fmtPPS, ppsSeries));

      charts.disk.set(timeOption(from, to, v => v.toFixed(0) + '%',
        Object.keys(disks).sort().map(mount => {
          const s = disks[mount];
          const pct = s.cols.used.map((u, i) => u == null || !s.cols.total[i] ? null : 100 * u / s.cols.total[i]);
          return line(mount, points(s.ts, pct, s.step));
        }), { yMax: 100, tipFmt: v => fmtPct(v) }));

      const pingSeries = [];
      peers.forEach((dst, i) => {
        const s = pings[i];
        pingSeries.push(line(dst, points(s.ts, s.cols.avg, s.step)));
        pingSeries.push({ type: 'bar', name: dst + ' 丢包', yAxisIndex: 1, data: points(s.ts, s.cols.loss_pct, s.step),
          barMaxWidth: 4, itemStyle: { opacity: 0.45 }, tooltip: { valueFormatter: v => v == null ? '—' : fmtPct(v, 0) } });
      });
      const [cap, highest] = latencyCap(pings.flatMap(p => p.cols.avg.filter(v => v != null)));
      charts.ping.note(capNote(cap, highest));
      charts.ping.set(timeOption(from, to, fmtMs, pingSeries, { yMax: cap || undefined,
        y2: { type: 'value', min: 0, max: 100, axisLabel: { formatter: '{value}%' }, splitLine: { show: false } } }));
    },
  };
}

function renderNodeHead(n, title, dot, sub, summary) {
  title.textContent = n.name + (n.name !== n.id ? `（${n.id}）` : '') + (n.region ? ` · ${n.region}` : '') +
    (n.group ? ` · ${n.group}` : '');
  dot.className = 'dot ' + (n.online ? 'on' : 'off');
  const s = n.status;
  if (!s) { sub.textContent = '从未上报'; summary.replaceChildren(); return; }
  const sys = s.sys || {};
  sub.textContent = [sys.hostname, sys.os, sys.kernel, sys.arch, sys.agent_version && 'agent ' + sys.agent_version]
    .filter(Boolean).join(' · ');
  const kv = (k, v) => h('div', { class: 'kv' }, h('span', { class: 'k', text: k }), h('span', { class: 'num', text: v }));
  const d = rootDisk(s.disks);
  summary.replaceChildren(
    kv('状态', n.online ? '在线' : '离线'),
    kv('最后上报', fmtTime(s.fresh_at)),
    kv('运行时间', sys.boot_time ? fmtDur(nowSec() - sys.boot_time) : '—'),
    kv('CPU', fmtPct(s.cpu) + (s.softirq != null ? `（软中断 ${fmtPct(s.softirq)}）` : '') +
      (sys.cores ? ` / ${sys.cores} 核` : '')),
    kv('内存', s.mem_total ? `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}` : '—'),
    kv('磁盘 /', d ? `${fmtBytes(d.used)} / ${fmtBytes(d.total)}` : '—'),
    kv('时钟偏差', s.clock_skew + ' s'),
    kv('连接', s.tcp != null ? `TCP ${s.tcp} / UDP ${s.udp} / TIME_WAIT ${s.tcp_tw}` : '— （agent ≥ 0.1.9）'),
    kv('线程', s.threads != null ? String(s.threads) : '—'),
    kv('本周期流量', s.traffic ? `↓ ${fmtBytes(s.traffic.rx)} ↑ ${fmtBytes(s.traffic.tx)}` : '—'),
    kv('上报来源 IP', s.ip ? `${s.ip}（${fmtTime(s.ip_since)} 起）` : '—'),
    ...(n.expire_at ? [kv('到期', [n.expire_at, fmtExpiry(n.expire_days),
      n.renew_months ? `每 ${n.renew_months} 个月自动续费` : null, n.price].filter(Boolean).join(' · '))] : []));
}

// ---------- latency matrix ----------

let matrixWindow = '5m';

function latClass(link) {
  if (link.avg == null) return 'lat4';
  let lvl = link.avg < 30 ? 0 : link.avg < 80 ? 1 : link.avg < 150 ? 2 : link.avg < 250 ? 3 : 4;
  if (link.loss_pct >= 20) lvl = 4;
  else if (link.loss_pct >= 5) lvl = Math.max(lvl, 3);
  return 'lat' + lvl;
}

const WINDOWS = [['5m', '5 分钟'], ['15m', '15 分钟'], ['1h', '1 小时']];
const AVAIL_RANGES = [['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天']];
const availFilter = { range: '24h', src: '' };
const AVAIL_EVERY = 60000; // the strips come from 5m rollups; no need to poll them every 10s

function matrixPage() {
  const wrap = h('div', { class: 'table-wrap' });
  const quality = h('div', { class: 'quality' });
  const strips = h('div', { class: 'avail' });
  const srcSel = picker(v => { availFilter.src = v; rerender(); });
  const windowText = WINDOWS.find(([v]) => v === matrixWindow)[1];
  const rangeText = AVAIL_RANGES.find(([v]) => v === availFilter.range)[1];
  let avail = null, availAt = 0;
  const el = h('div', null,
    h('div', { class: 'row' }, h('h1', { text: '时延' }), h('span', { class: 'spacer' }),
      h('span', { class: 'muted', text: '统计窗口' }),
      seg(WINDOWS, matrixWindow, v => { matrixWindow = v; rerender(); })),
    h('div', { class: 'panel section' }, h('h2', { text: '网络质量' }), quality),
    h('div', { class: 'panel section' }, h('h2', { text: `时延矩阵（最近 ${windowText}）` }), wrap,
      h('div', { class: 'legend muted' },
        h('span', { class: 'l0', text: '< 30 ms' }), h('span', { class: 'l1', text: '< 80 ms' }),
        h('span', { class: 'l2', text: '< 150 ms' }), h('span', { class: 'l3', text: '< 250 ms 或丢包 ≥ 5%' }),
        h('span', { class: 'l4', text: '更高 / 丢包 ≥ 20% / 不通' }),
        h('span', { text: '行 = 发起方，列 = 目标；点击格子查看历史' }))),
    h('div', { class: 'panel section' },
      h('div', { class: 'row filters' }, h('h2', { text: '链路可用性' }), h('span', { class: 'spacer' }),
        seg(AVAIL_RANGES, availFilter.range, v => { availFilter.range = v; rerender(); }), srcSel),
      strips,
      h('div', { class: 'legend muted' },
        h('span', { class: 'a-up', text: '正常' }), h('span', { class: 'a-lossy', text: '丢包 ≥ 1%' }),
        h('span', { class: 'a-part', text: '部分时段不可用' }), h('span', { class: 'a-down', text: '一半以上时段不可用' }),
        h('span', { class: 'a-none', text: '无数据' }),
        h('span', { text: '某个 5 分钟丢包 ≥ 20% 记为不可用；可用率 = 可用的 5 分钟 / 有数据的 5 分钟' }))));
  return {
    nav: 'ping',
    el,
    async refresh() {
      const stale = Date.now() - availAt >= AVAIL_EVERY;
      const [data, nodes, av] = await Promise.all([api('/api/ping/matrix?window=' + matrixWindow), api('/api/nodes'),
        stale ? api('/api/ping/availability?range=' + availFilter.range) : avail]);
      if (stale) { avail = av; availAt = Date.now(); }
      const names = new Map(nodes.map(n => [n.id, n.name]));
      const nameOf = id => names.get(id) || id;
      const links = new Map(data.links.map(l => [l.src + '\n' + l.dst, l]));
      const extra = [...new Set([...data.links, ...avail.links].map(l => l.dst).filter(d => !names.has(d)))].sort();
      const cols = [...data.nodes, ...extra];
      renderQuality(quality, data.links, avail, nameOf, windowText, rangeText);
      srcSel.fill([['', '全部发起方'], ...data.nodes.map(id => [id, nameOf(id)])], availFilter.src);
      if (stale) renderStrips(strips, avail, data.nodes, cols, nameOf); // unchanged otherwise
      if (!data.links.length) {
        wrap.replaceChildren(h('div', { class: 'empty', text: '这个窗口内没有时延数据' }));
        return;
      }
      const head = h('tr', null, h('th', { text: '发起 \\ 目标' }),
        cols.map(c => h('th', { text: names.get(c) || c, title: c })));
      const rows = data.nodes.map(src => h('tr', null,
        h('td', { text: names.get(src) || src, title: src }),
        cols.map(dst => {
          if (src === dst) return h('td', { class: 'self', text: '—' });
          const l = links.get(src + '\n' + dst);
          if (!l) return h('td', { class: 'muted', text: '' });
          const tip = `${src} → ${dst}\n平均 ${fmtMs(l.avg)}  最小 ${fmtMs(l.min)}  最大 ${fmtMs(l.max)}\n` +
            `抖动 ${fmtMs(l.jitter)}  丢包 ${fmtPct(l.loss_pct)}（${l.lost}/${l.sent}）`;
          return h('td', { class: 'cell ' + latClass(l), title: tip,
            onclick: () => { location.hash = linkHref(src, dst); } },
          h('span', { class: 'ms', text: l.avg == null ? '不通' : fmtMs(l.avg) }),
          l.loss_pct > 0 && l.avg != null ? h('span', { class: 'loss', text: '丢包 ' + fmtPct(l.loss_pct, 0) }) : null);
        })));
      wrap.replaceChildren(h('table', { class: 'matrix' }, h('thead', null, head), h('tbody', null, rows)));
    },
  };
}

const linkHref = (src, dst) => `#/ping/${enc(src)}/${enc(dst)}`;

// Top three links by a few measures: what to look at first.
function renderQuality(box, links, avail, nameOf, windowText, rangeText) {
  const linkText = l => `${nameOf(l.src)} → ${nameOf(l.dst)}`;
  const col = (title, items, empty) => h('div', { class: 'q-col' }, h('div', { class: 'k', text: title }),
    items.length ? items.map(([l, v, cls]) => h('a', { class: 'q-item', href: linkHref(l.src, l.dst) },
      h('span', { class: 'q-name', text: linkText(l) }), h('span', { class: 'num' + (cls ? ' ' + cls : ''), text: v })))
      : h('div', { class: 'muted', text: empty }));
  const top = (arr, key) => arr.filter(l => key(l) > 0).sort((a, b) => key(b) - key(a)).slice(0, 3);
  // Only links still measured: a link dropped by no_ping keeps old data in the range.
  const live = new Set(links.map(l => l.src + '\n' + l.dst));
  box.replaceChildren(
    col(`丢包最多（${windowText}）`, top(links, l => l.loss_pct).map(l =>
      [l, l.avg == null ? '不通' : '丢包 ' + fmtPct(l.loss_pct, 1), l.loss_pct >= 20 ? 'bad' : 'warn']), '没有丢包'),
    col(`抖动最大（${windowText}）`, top(links.filter(l => l.jitter != null), l => l.jitter).map(l =>
      [l, '抖动 ' + fmtMs(l.jitter), null]), '没有数据'),
    col(`可用率最低（${rangeText}）`, top(avail.links.filter(a => a.avail_pct != null && live.has(a.src + '\n' + a.dst)), a => 100 - a.avail_pct).map(a =>
      [a, '可用 ' + fmtAvail(a.avail_pct), a.avail_pct < 99 ? 'bad' : 'warn']), '全部 100%'));
}

// Rounded down so that 99.996% doesn't show as 100%.
const fmtAvail = v => v == null ? '—' : v >= 100 ? '100%' : (Math.floor(v * 100) / 100).toFixed(2) + '%';

function availClass(c, i) {
  const p = c.periods[i], d = c.down[i];
  if (!p) return 'a-none';
  if (d * 2 >= p) return 'a-down';
  if (d > 0) return 'a-part';
  return c.lost[i] * 100 >= c.sent[i] ? 'a-lossy' : 'a-up';
}

const fmtCellTime = new Intl.DateTimeFormat('zh-CN',
  { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });

// One row per link: a strip of cells like a status page's heartbeat bar.
function renderStrips(box, avail, nodes, cols, nameOf) {
  const order = (arr, v) => { const i = arr.indexOf(v); return i < 0 ? arr.length : i; };
  const links = avail.links.filter(a => !availFilter.src || a.src === availFilter.src)
    .sort((a, b) => order(nodes, a.src) - order(nodes, b.src) || order(cols, a.dst) - order(cols, b.dst));
  if (!links.length) {
    box.replaceChildren(h('div', { class: 'empty', text: '这段时间内没有时延数据' }));
    return;
  }
  // Every link shares the same cells: label them once. toLocaleString with
  // options builds a new formatter per call, far too slow for thousands of cells.
  const t = i => fmtCellTime.format(new Date((avail.from + i * avail.cell) * 1000));
  const when = Array.from({ length: avail.n }, (_, i) => `${t(i)} – ${t(i + 1)}`);
  const out = [];
  let src = null;
  for (const a of links) {
    if (a.src !== src) {
      src = a.src;
      out.push(h('div', { class: 'avail-src', text: nameOf(src) + ' →' }));
    }
    const c = a.cells;
    const cells = c.periods.map((p, i) => h('span', { class: availClass(c, i), title: p
      ? `${when[i]}\n可用 ${p - c.down[i]} / ${p} 个 5 分钟\n丢包 ${fmtPct(100 * c.lost[i] / c.sent[i], 2)}  平均 ${fmtMs(c.avg[i])}`
      : `${when[i]}\n无数据` }));
    out.push(h('a', { class: 'avail-row', href: linkHref(a.src, a.dst) },
      h('span', { class: 'avail-name', text: nameOf(a.dst), title: `${a.src} → ${a.dst}` }),
      h('span', { class: 'strip' }, cells),
      h('span', { class: 'avail-stats num' },
        h('span', { class: a.avail_pct < 99 ? 'bad' : a.avail_pct < 100 ? 'warn' : null, text: fmtAvail(a.avail_pct) }),
        h('span', { class: 'muted', text: '丢包 ' + (a.sent ? fmtPct(100 * a.lost / a.sent, 2) : '—') }),
        h('span', { class: 'muted', text: fmtMs(a.avg) }))));
  }
  box.replaceChildren(...out);
}

// latencyCap is a y-axis top that fits the typical values when a few
// outliers would squash the rest (one 10 s round trip against a 40 ms
// line), or null when everything fits anyway. Clipped points run off the
// top; tooltips keep their real values. Returns [cap, highest].
function latencyCap(avgs, maxes = []) {
  const q = (a, p) => {
    if (!a.length) return 0;
    const s = [...a].sort((x, y) => x - y);
    return s[Math.min(s.length - 1, Math.floor(p * s.length))];
  };
  // The avg line must stay readable; the max line may clip more readily.
  const top = Math.max(q(avgs, 0.99), q(maxes, 0.75));
  const highest = [...avgs, ...maxes].reduce((a, v) => Math.max(a, v), 0);
  if (!top || highest <= top * 2) return [null, highest];
  const want = top * 1.15, e = 10 ** Math.floor(Math.log10(want));
  return [[1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10].map(m => m * e).find(v => v >= want), highest];
}

const capNote = (cap, highest) => cap ? `纵轴上限 ${fmtMs(cap)}，更高的点超出图表顶部（最高 ${fmtMs(highest)}），数值框里是实际值` : '';

// Loss levels of one sample, in the availability strips' colors: below 1%
// is fine there too, and a 5-minute period losing 20% or more counts as
// unavailable.
const LOSS_LEVELS = [[1, 'a-up', '< 1%'], [20, 'a-lossy', '1–20%'], [50, 'a-part', '20–50%'],
  [Infinity, 'a-down', '≥ 50%']];
const lossLevel = pct => LOSS_LEVELS.findIndex(l => pct < l[0]);
const cssVar = name => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// lossPieces colors a line by each point's loss (column 2), as a visualMap
// over time: ECharts colors line segments only by an axis dimension. Each
// point owns the time up to halfway to its neighbors.
function lossPieces(pts, colors) {
  const ps = pts.filter(p => p[1] != null && p[2] != null);
  const pieces = [];
  ps.forEach((p, i) => {
    const lo = i ? (ps[i - 1][0] + p[0]) / 2 : p[0] - 1e9;
    const hi = i < ps.length - 1 ? (p[0] + ps[i + 1][0]) / 2 : p[0] + 1e9;
    const color = colors[lossLevel(p[2])];
    const last = pieces[pieces.length - 1];
    if (last && last.color === color && last.lt === lo) last.lt = hi;
    else pieces.push({ gte: lo, lt: hi, color });
  });
  return pieces;
}

function linkPage(src, dst) {
  const chart = new Chart('时延与丢包');
  chart.el.insertBefore(h('div', { class: 'legend chart-legend' }, '平均线和丢包柱按丢包率着色：',
    LOSS_LEVELS.map(([, cls, label]) => h('span', { class: cls, text: label }))), chart.box);
  const stats = h('div', { class: 'summary' });
  const el = h('div', null,
    h('div', { class: 'row' }, h('a', { href: '#/ping', text: '← 时延矩阵' })),
    h('div', { class: 'panel section' }, h('h1', { text: `${src} → ${dst}` }), stats),
    h('div', { class: 'row section' }, h('span', { class: 'muted', text: '时间范围' }), rangeSeg()),
    h('div', { class: 'section' }, chart.el));
  return {
    nav: 'ping',
    el,
    interval: rangeInterval,
    destroy() { chart.dispose(); },
    async refresh() {
      const to = nowSec(), from = to - rangeSec();
      const s = await api(`/api/ping/${enc(src)}/${enc(dst)}?from=${from}&to=${to}`);
      const c = s.cols, st = s.step;
      const sent = c.sent.reduce((a, v) => a + (v || 0), 0), lost = c.lost.reduce((a, v) => a + (v || 0), 0);
      const vals = k => c[k].filter(v => v != null);
      const kv = (k, v) => h('div', { class: 'kv' }, h('span', { class: 'k', text: k }), h('span', { class: 'num', text: v }));
      const avgs = vals('avg');
      stats.replaceChildren(
        kv('发送', String(sent)), kv('丢包率', sent ? fmtPct(100 * lost / sent, 2) : '—'),
        kv('最小', vals('min').length ? fmtMs(Math.min(...vals('min'))) : '—'),
        kv('平均', avgs.length ? fmtMs(avgs.reduce((a, v) => a + v, 0) / avgs.length) : '—'),
        kv('最大', vals('max').length ? fmtMs(Math.max(...vals('max'))) : '—'),
        kv('数据粒度', `${s.tier} / ${st}s`));
      // Min and max stay neutral so no series color reads as a loss level.
      const colors = LOSS_LEVELS.map(l => cssVar('--' + l[1])), gray = cssVar('--muted');
      const avg = points(s.ts, c.avg, st, undefined, [c.loss_pct]);
      const [cap, highest] = latencyCap(vals('avg'), vals('max'));
      chart.note(capNote(cap, highest));
      const option = timeOption(from, to, fmtMs, [
        line('最小', points(s.ts, c.min, st), { lineStyle: { width: 1, type: 'dashed' }, itemStyle: { color: gray } }),
        line('平均', avg, { lineStyle: { width: 2 }, itemStyle: { color: colors[0] } }),
        line('最大', points(s.ts, c.max, st), { lineStyle: { width: 1, type: 'dotted' }, itemStyle: { color: gray } }),
        { type: 'bar', name: '丢包', yAxisIndex: 1, data: points(s.ts, c.loss_pct, st), barMaxWidth: 6,
          itemStyle: { opacity: 0.6, color: p => colors[p.value[1] == null ? 0 : lossLevel(p.value[1])] },
          tooltip: { valueFormatter: v => v == null ? '—' : fmtPct(v, 1) } },
      ], { yMax: cap || undefined,
        y2: { type: 'value', min: 0, max: 100, axisLabel: { formatter: '{value}%' }, splitLine: { show: false } } });
      // The bars' own color varies by level; visible ones always have loss.
      option.legend.data = ['最小', '平均', '最大', { name: '丢包', itemStyle: { color: colors[3] } }];
      option.visualMap = { type: 'piecewise', show: false, dimension: 0, seriesIndex: 1,
        pieces: lossPieces(avg, colors), outOfRange: { color: colors[0] } };
      chart.set(option);
    },
  };
}

// ---------- traffic ----------

let trafficNode = null;

function trafficPage() {
  const table = h('div', { class: 'table-wrap' });
  const detailTitle = h('h2');
  const chart = new Chart('每日流量');
  const history = h('div', { class: 'table-wrap' });
  const caption = h('div', { class: 'muted' });
  const el = h('div', null,
    h('h1', { text: '流量' }),
    h('div', { class: 'panel section' }, table),
    h('div', { class: 'section' }, detailTitle),
    h('div', { class: 'charts' }, chart.el, h('div', { class: 'panel' }, h('h2', { text: '历史周期' }), history)),
    h('div', { class: 'section' }, caption));
  return {
    nav: 'traffic',
    el,
    destroy() { chart.dispose(); },
    async refresh() {
      const [nodes, stats] = await Promise.all([api('/api/traffic'), api('/api/stats')]);
      if (!nodes.length) return;
      if (!nodes.some(n => n.id === trafficNode)) trafficNode = nodes[0].id;
      caption.textContent = `每日流量的日期按服务端时区（${stats.timezone || '—'}）划分，应与各 agent 的 traffic.timezone 一致。` +
        '配额的 GB 按 1024³ 字节计算。';

      const head = h('tr', null, ['节点', '周期起始', '下行', '上行', '合计', '计费方式', '已用 / 配额'].map(t => h('th', { text: t })));
      const rows = nodes.map(n => {
        const p = n.periods[0];
        const used = p ? billable(p.rx, p.tx, n.traffic_quota_mode) : 0;
        const q = n.traffic_quota_gb ? quotaBytes(n.traffic_quota_gb) : 0;
        return h('tr', { class: 'clickable' + (n.id === trafficNode ? ' selected' : ''),
          onclick: () => { trafficNode = n.id; rerender(); } },
        h('td', { text: n.name }),
        h('td', { text: p ? p.start : '—' }),
        h('td', { text: p ? fmtBytes(p.rx) : '—' }),
        h('td', { text: p ? fmtBytes(p.tx) : '—' }),
        h('td', { text: p ? fmtBytes(p.rx + p.tx) : '—' }),
        h('td', { text: QUOTA_MODES[n.traffic_quota_mode] || n.traffic_quota_mode }),
        h('td', null, q ? [bar(100 * used / q), `${fmtBytes(used)} / ${fmtBytes(q)}（${fmtPct(100 * used / q)}）`]
          : `${fmtBytes(used)} / 不限`));
      });
      table.replaceChildren(h('table', null, h('thead', null, head), h('tbody', null, rows)));

      const n = nodes.find(x => x.id === trafficNode);
      detailTitle.textContent = n.name + '：本周期每日流量与历史周期';
      history.replaceChildren(n.periods.length
        ? h('table', null,
          h('thead', null, h('tr', null, ['周期起始', '下行', '上行', '合计', '计费量'].map(t => h('th', { text: t })))),
          h('tbody', null, n.periods.map(p => h('tr', null,
            h('td', { text: p.start, title: p.ifaces.map(i => `${i.iface}: ↓${fmtBytes(i.rx)} ↑${fmtBytes(i.tx)}`).join('\n') }),
            h('td', { text: fmtBytes(p.rx) }), h('td', { text: fmtBytes(p.tx) }),
            h('td', { text: fmtBytes(p.rx + p.tx) }),
            h('td', { text: fmtBytes(billable(p.rx, p.tx, n.traffic_quota_mode)) })))))
        : h('div', { class: 'empty', text: '暂无数据' }));

      const daily = await api(`/api/traffic/${enc(n.id)}/daily`);
      const days = daily.days;
      chart.set({
        backgroundColor: 'transparent',
        animation: false,
        grid: { left: 8, right: 12, top: 36, bottom: 8, containLabel: true },
        tooltip: { trigger: 'axis', renderMode: 'richText', confine: true, valueFormatter: fmtBytes },
        legend: { top: 0 },
        xAxis: { type: 'category', data: days.map(d => d.day.slice(5)), axisLabel: { hideOverlap: true } },
        yAxis: { type: 'value', axisLabel: { formatter: fmtBytes }, splitLine: { lineStyle: { opacity: 0.35 } } },
        series: [
          { type: 'bar', name: '下行', stack: 't', data: days.map(d => d.rx), barMaxWidth: 28 },
          { type: 'bar', name: '上行', stack: 't', data: days.map(d => d.tx), barMaxWidth: 28 },
        ],
      });
    },
  };
}

// ---------- alerts ----------

const METRICS = { cpu: 'CPU', steal: 'Steal', load1: '负载(1m)', mem: '内存', swap: 'Swap', disk: '磁盘',
  offline: '离线', ping_loss: '丢包', ping_avg: '时延', traffic: '流量配额', expiry: '到期', ip_change: 'IP 变化',
  net_in: '入站', net_out: '出站', pps_in: '入站包速率', pps_out: '出站包速率', softirq: '软中断', period_report: '周期结算', weekly_report: '每周流量' };
const EVENTS = { firing: '告警', repeat: '仍在告警', recovered: '恢复', level: '流量档位', changed: 'IP 变化', report: '报告' };
// Rules that notify once per event rather than fire and recover.
const NOTICE_METRICS = ['traffic', 'expiry', 'ip_change', 'period_report', 'weekly_report'];
const eventText = (e, metric) => e.event === 'level' && metric === 'expiry' ? '到期提醒' : EVENTS[e.event] || e.event;

function fmtRuleValue(metric, v) {
  if (v == null) return '—';
  if (metric === 'offline') return v < 60 ? Math.round(v) + ' 秒' : fmtDur(v);
  if (metric === 'ping_avg') return fmtMs(v);
  if (metric === 'load1') return v.toFixed(2);
  if (metric === 'net_in' || metric === 'net_out') return v.toFixed(1) + ' Mbps';
  if (metric === 'pps_in' || metric === 'pps_out') return fmtPPS(v);
  return fmtPct(v);
}

// metric is undefined for events of rules no longer configured.
function historyValue(e, metric) {
  if (e.event === 'changed') return '—';
  if (e.event === 'report') return metric === 'period_report' && e.value ? e.value.toFixed(1) + '%' : '—';
  if (e.event === 'level') return metric === 'expiry' ? fmtExpiry(e.value) : e.value + '%';
  return fmtRuleValue(metric, e.value);
}

function ruleText(r) {
  if (r.metric === 'offline') return `超过 ${r.for} 没有上报`;
  if (r.metric === 'traffic') return `达到配额 ${r.levels.join(' / ')}%（每周期每档一次）`;
  if (r.metric === 'expiry') {
    return `到期前 ${[...r.levels].sort((a, b) => b - a).map(l => l || '当').join(' / ')} 天提醒（每个到期日每档一次）`;
  }
  if (r.metric === 'ip_change') return '上报来源 IP 变化时通知';
  if (r.metric === 'period_report') return '每个流量周期结束时发送结算';
  if (r.metric === 'weekly_report') return `每周${WEEKDAYS[r.at.slice(0, 3)] || ' ' + r.at.slice(0, 3)} ${r.at.slice(4)} 发送流量汇总`;
  let t = `${METRICS[r.metric] || r.metric} ${r.op} ${fmtRuleValue(r.metric, r.threshold)}`;
  if (r.ratio) t += `，且不低于${r.metric === 'net_in' || r.metric === 'pps_in' ? '出站' : '入站'}的 ${r.ratio} 倍`;
  if (r.for !== '0s') t += `，持续 ${r.for}`;
  return t;
}

const WEEKDAYS = { Mon: '一', Tue: '二', Wed: '三', Thu: '四', Fri: '五', Sat: '六', Sun: '日' };
const ALERT_RANGES = [['1d', '24 小时', 86400], ['7d', '7 天', 7 * 86400], ['30d', '30 天', 30 * 86400],
  ['90d', '90 天', 90 * 86400]];
const EVENT_FILTERS = [['', '全部事件'], ['firing,repeat', '告警'], ['recovered', '恢复'], ['level', '流量档位 / 到期提醒'],
  ['changed', 'IP 变化'], ['report', '报告']];
const alertFilter = { range: '7d', node: '', rule: '', event: '' };

function alertsPage() {
  const active = h('div', { class: 'table-wrap' });
  const history = h('div', { class: 'table-wrap' });
  const histCount = h('span', { class: 'muted' });
  const setFilter = (k, v) => { alertFilter[k] = v; rerender(); };
  const nodeSel = picker(v => setFilter('node', v));
  const ruleSel = picker(v => setFilter('rule', v));
  const eventSel = picker(v => setFilter('event', v));
  const filtered = alertFilter.node || alertFilter.rule || alertFilter.event;
  const range = ALERT_RANGES.find(r => r[0] === alertFilter.range);
  const rules = h('div', { class: 'table-wrap' });
  const tg = h('span', { class: 'badge' });
  const table = (cols, rows) => h('table', { class: 'plain' },
    h('thead', null, h('tr', null, cols.map(c => h('th', { text: c })))), h('tbody', null, rows));
  const cell = t => h('td', { text: t });
  return {
    nav: 'alerts',
    el: h('div', null,
      h('div', { class: 'row' }, h('h1', { text: '告警' }), tg),
      h('div', { class: 'panel section' }, h('h2', { text: '当前告警' }), active),
      h('div', { class: 'panel section' },
        h('div', { class: 'row filters' }, h('h2', { text: '告警历史' }), histCount, h('span', { class: 'spacer' }),
          seg(ALERT_RANGES.map(([v, label]) => [v, label]), alertFilter.range, v => setFilter('range', v)),
          nodeSel, ruleSel, eventSel,
          filtered ? seg([['clear', '清除筛选']], null, () => {
            Object.assign(alertFilter, { node: '', rule: '', event: '' });
            rerender();
          }) : null),
        history),
      h('div', { class: 'panel section' }, h('h2', { text: '规则' }), rules)),
    async refresh() {
      const to = nowSec();
      const q = new URLSearchParams({ from: to - range[2], to });
      for (const k of ['node', 'rule', 'event']) if (alertFilter[k]) q.set(k, alertFilter[k]);
      const [d, nodes] = await Promise.all([api('/api/alerts?' + q), api('/api/nodes')]);
      const names = new Map(nodes.map(n => [n.id, n.name]));
      const nodeText = id => names.get(id) || id;
      const metricOf = new Map(d.rules.map(r => [r.name, r.metric]));
      tg.textContent = d.channels.length ? '通知：' + d.channels.map(c => c === 'telegram' ? 'Telegram' : c).join('、')
        : '未配置通知渠道：告警只记录不发送';
      tg.className = 'badge' + (d.channels.length ? '' : ' warn');

      active.replaceChildren(d.active.length
        ? table(['状态', '规则', '节点', '对象', '当前值', '开始于'], d.active.map(a => h('tr', null,
          h('td', null, h('span', { class: 'badge ' + (a.state === 'firing' ? 'bad' : 'warn'),
            text: a.state === 'firing' ? '告警中' : '观察中' })),
          cell(a.rule), cell(nodeText(a.node)), cell(a.target || '—'),
          cell(fmtRuleValue(a.metric, a.value)), cell(fmtTime(a.since)))))
        : h('div', { class: 'empty', text: '没有告警' }));

      // Keep the current choice listed even when the range no longer has it.
      const withCur = (vals, cur) => cur && !vals.includes(cur) ? [...vals, cur] : vals;
      nodeSel.fill([['', '全部节点'], ...withCur(d.facets.nodes, alertFilter.node).map(id => [id, nodeText(id)])],
        alertFilter.node);
      ruleSel.fill([['', '全部规则'], ...withCur(d.facets.rules, alertFilter.rule).map(r => [r, r])], alertFilter.rule);
      eventSel.fill(EVENT_FILTERS, alertFilter.event);
      histCount.textContent = d.truncated ? `只显示最近 ${d.history.length} 条，缩小范围查看更早的`
        : `共 ${d.history.length} 条`;

      history.replaceChildren(d.history.length
        ? table(['时间', '事件', '规则', '节点', '对象', '值'], d.history.map(e => h('tr', { title: e.message },
          cell(fmtTime(e.ts)),
          h('td', null, h('span', { class: 'badge ' + (e.event === 'recovered' ? '' : e.event === 'firing' || e.event === 'repeat' ? 'bad' : 'warn'),
            text: eventText(e, metricOf.get(e.rule)) })),
          cell(e.rule), cell(e.node ? nodeText(e.node) : '全部'), cell(e.target || '—'),
          cell(historyValue(e, metricOf.get(e.rule))))))
        : h('div', { class: 'empty', text: filtered ? '没有符合条件的告警' : `最近 ${range[1]}没有告警` }));

      rules.replaceChildren(d.rules.length
        ? table(['名称', '条件', '节点', '重复提醒', '恢复通知'], d.rules.map(r => h('tr', null,
          cell(r.name), cell(ruleText(r)),
          cell(r.nodes === 'all' ? (r.exclude ? '全部，除 ' + r.exclude.map(nodeText).join('、') : '全部')
            : r.nodes.map(nodeText).join('、')),
          cell(NOTICE_METRICS.includes(r.metric) ? '—' : r.repeat === '0s' ? '不重复' : '每 ' + r.repeat),
          cell(NOTICE_METRICS.includes(r.metric) ? '—' : r.notify_recovery === false ? '否' : '是'))))
        : h('div', { class: 'empty', text: '没有配置告警规则' }));
    },
  };
}

// ---------- footer ----------

async function refreshFooter() {
  try {
    const s = await api('/api/stats');
    const i = s.ingest;
    const dropped = Object.entries(i).filter(([k, v]) => v > 0 && !['accepted', 'duplicate'].includes(k));
    $foot.replaceChildren(
      `上报 ${i.accepted} 个包（重传 ${i.duplicate}）· `,
      dropped.length ? h('span', { class: 'badge warn', text: '丢弃 ' + dropped.map(([k, v]) => `${k}=${v}`).join(' ') })
        : '无丢弃',
      ` · 数据库 ${fmtBytes(s.db_bytes)} · 服务端 ${s.version} · 服务端时间 ${fmtTime(s.server_time)}`);
  } catch {
    // The page's own refresh reports connectivity errors.
  }
}

// ---------- start ----------

// Nothing can be drawn until ECharts is there (see loadECharts).
const ifReady = () => { if (typeof echarts !== 'undefined') navigate(); };
window.addEventListener('hashchange', ifReady);
darkQuery.addEventListener('change', ifReady);
// ECharts is the largest file of the page and the one most likely to be
// missed (a server restart, a flaky link). Load it again a few times before
// asking for a reload.
const echartsRetryDelays = [2000, 5000, 10000];
function loadECharts(attempt) {
  if (typeof echarts !== 'undefined') { navigate(); return; }
  if (attempt >= echartsRetryDelays.length) {
    const reload = h('button', { class: 'reload', onclick: () => location.reload() }, '刷新页面');
    $app.replaceChildren(h('div', { class: 'error' }, '图表库（ECharts）加载失败，请刷新页面。 ', reload));
    return;
  }
  $app.replaceChildren(h('div', { class: 'empty',
    text: `图表库（ECharts）没有加载上，正在重试（${attempt + 1}/${echartsRetryDelays.length}）…` }));
  setTimeout(() => {
    const old = document.querySelector('script[src*="echarts"]');
    const s = document.createElement('script');
    s.src = old.getAttribute('src');
    // A reply that is not the script (a login page, say) still fires load:
    // what counts is whether echarts exists afterwards.
    s.addEventListener('load', () => loadECharts(attempt + 1));
    s.addEventListener('error', () => loadECharts(attempt + 1));
    old.replaceWith(s);
  }, echartsRetryDelays[attempt]);
}
loadECharts(0);
refreshFooter();
setInterval(refreshFooter, 30000);
