// The overview: every node as a card or a table row.

import { api, enc } from '../js/api.js';
import { h, cls, kv, bar, sparkline, picker, seg, table } from '../js/dom.js';
import { fmtBytes, fmtRate, fmtPct, fmtDur, fmtDay, fmtExpiry, expiryClass, nowSec, pctOf, staleAgents } from '../js/format.js';
import { show } from '../js/router.js';

// Per-browser view choices, not part of the address: how one likes to look
// at the overview. Storage may be missing or throw (private windows, blocked
// site data); the page then just uses the defaults.
const PREFS_KEY = 'vps-probe.overview';
const prefs = { view: 'cards', sort: '', group: '' };
try {
  Object.assign(prefs, JSON.parse(localStorage.getItem(PREFS_KEY)));
} catch { /* defaults */ }
function setPref(k, v) {
  prefs[k] = v;
  try { localStorage.setItem(PREFS_KEY, JSON.stringify(prefs)); } catch { /* not persisted */ }
  show();
}

const NO_GROUP = '\n'; // groups can't contain control characters
const groupOf = n => n.group || NO_GROUP;

const totalRate = (s, k) => (s.net || []).reduce((a, x) => a + x[k], 0);
const rootDisk = s => (s.disks || []).find(d => d.mount === '/') || (s.disks || [])[0];
const quotaPct = n => n.quota ? pctOf(n.quota.used, n.quota.bytes) : null;
const uptime = s => s.sys && s.sys.boot_time ? fmtDur(nowSec() - s.sys.boot_time) : null;

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
  ['disk', '磁盘', live(s => { const d = rootDisk(s); return d ? pctOf(d.used, d.total) : null; }), true],
  ['rx', '下行网速', live(s => totalRate(s, 'rx')), true],
  ['tx', '上行网速', live(s => totalRate(s, 'tx')), true],
  ['traffic', '本周期流量', live(s => s.traffic ? s.traffic.rx + s.traffic.tx : null), true],
  ['quota', '配额使用率', live((s, n) => quotaPct(n)), true],
  ['tcp', 'TCP 连接', live(s => s.tcp), true],
  ['expiry', '到期', n => n.plan ? n.plan.days : null],
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

export function overviewPage() {
  const body = h('div');
  const stats = h('div', { class: 'stats panel' });
  const tabs = h('div');
  const sortSel = picker(v => setPref('sort', v));
  return {
    nav: 'overview',
    el: h('div', null,
      h('div', { class: 'row filters' }, h('h1', { text: '总览' }), h('span', { class: 'spacer' }),
        seg([['cards', '卡片'], ['table', '表格']], prefs.view, v => setPref('view', v)), sortSel),
      tabs, h('div', { class: 'section' }, stats), h('div', { class: 'section' }, body)),
    async refresh() {
      const cards = prefs.view !== 'table';
      const [all, sparks] = await Promise.all([api('/api/nodes'), cards ? api('/api/sparks') : null]);
      // Group tabs in the order groups first appear in the config.
      const groups = [...new Set(all.map(groupOf))];
      const group = groups.includes(prefs.group) ? prefs.group : '';
      if (all.some(n => n.group)) {
        const count = g => all.filter(n => groupOf(n) === g).length;
        tabs.replaceChildren(seg([['', `全部 ${all.length}`],
          ...groups.map(g => [g, `${g === NO_GROUP ? '未分组' : g} ${count(g)}`])], group, v => setPref('group', v)));
        tabs.className = 'section';
      } else {
        tabs.replaceChildren();
      }
      const nodes = sortNodes(group ? all.filter(n => groupOf(n) === group) : all, prefs.sort);
      sortSel.fill(SORTS.map(([v, label]) => [v, '排序：' + label]), prefs.sort);
      stats.replaceChildren(...overviewStats(nodes));
      const agents = staleAgents(all);
      if (cards) {
        body.className = 'cards';
        body.replaceChildren(...nodes.map(n => nodeCard(n, agents, sparks.nodes[n.id])));
      } else {
        body.className = 'panel table-wrap';
        body.replaceChildren(nodeTable(nodes, agents));
      }
    },
  };
}

function overviewStats(nodes) {
  const stat = (k, v, className, title) => h('div', { class: 'stat', title },
    h('div', { class: 'k', text: k }), h('div', { class: cls('v num', className), text: v }));
  const online = nodes.filter(n => n.online);
  let rx = 0, tx = 0, trx = 0, ttx = 0;
  for (const n of online) { rx += totalRate(n.status, 'rx'); tx += totalRate(n.status, 'tx'); }
  for (const n of nodes) {
    const t = n.status && n.status.traffic;
    if (t) { trx += t.rx; ttx += t.tx; }
  }
  const out = [
    stat('在线', `${online.length} / ${nodes.length}`, online.length < nodes.length ? 'warn' : null),
    stat('实时网速', `↓ ${fmtRate(rx)}   ↑ ${fmtRate(tx)}`, null, '在线节点各自计入流量的网卡之和'),
    stat('本周期流量合计', `↓ ${fmtBytes(trx)}   ↑ ${fmtBytes(ttx)}`, null, '各节点按各自的周期计算后相加'),
  ];
  // The nearest upcoming date; an already expired node only when none is
  // upcoming, so one kept in the config for its history doesn't pin this
  // forever.
  const dated = nodes.filter(n => n.plan);
  const upcoming = dated.filter(n => n.plan.days >= 0);
  const due = (upcoming.length ? upcoming : dated).sort((a, b) => Math.abs(a.plan.days) - Math.abs(b.plan.days))[0];
  if (due) {
    out.push(stat('最近到期', `${due.name} · ${fmtExpiry(due.plan.days)}`, expiryClass(due.plan.days), due.plan.expire_at));
  }
  return out;
}

const regionBadge = n => n.region ? h('span', { class: 'region', text: n.region }) : null;
const dot = n => h('span', { class: cls('dot', n.online ? 'on' : 'off'), title: n.online ? '在线' : '离线' });
const nodeHref = n => '#/node/' + enc(n.id);

function connText(s) {
  const parts = [];
  if (s.tcp != null) parts.push(`TCP ${s.tcp}`, `UDP ${s.udp}`);
  if (s.threads != null) parts.push(`线程 ${s.threads}`);
  return parts.join(' · ');
}

// A dense table in the manner of ServerStatus: one row per node, click for
// details.
function nodeTable(nodes, agents) {
  const pctCell = (pct, title) => h('td', { title }, bar(pct), h('span', { class: 'num', text: fmtPct(pct, 0) }));
  const num = (text, title, className) => h('td', { class: cls('num', className), text, title });
  const rows = nodes.map(n => {
    const s = n.status, old = agents.stale(n);
    const tr = h('tr', { class: cls('clickable', !n.online && 'offline'),
      onclick: e => { if (e.target.tagName !== 'A') location.hash = nodeHref(n); } },
    h('td', null, dot(n), ' ', h('a', { href: nodeHref(n), text: n.name }), ' ', regionBadge(n),
      old ? h('span', { class: 'badge warn', text: '可升级', title: 'agent ' + old }) : null));
    if (!s) {
      tr.append(h('td', { class: 'muted', text: '从未上报' }), ...Array.from({ length: 8 }, () => h('td')));
      return tr;
    }
    const d = rootDisk(s), t = s.traffic, q = quotaPct(n);
    tr.append(
      pctCell(s.cpu),
      pctCell(pctOf(s.mem_used, s.mem_total), s.mem_total ? `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}` : null),
      pctCell(d ? pctOf(d.used, d.total) : null, d ? `${d.mount} ${fmtBytes(d.used)} / ${fmtBytes(d.total)}` : null),
      num(`↓ ${fmtRate(totalRate(s, 'rx'))} ↑ ${fmtRate(totalRate(s, 'tx'))}`),
      num(t ? `↓ ${fmtBytes(t.rx)} ↑ ${fmtBytes(t.tx)}` + (q != null ? ` · ${fmtPct(q, 0)}` : '') : '—', t ? `${fmtDay(t.start)} 起` : null),
      num(s.load1 != null ? s.load1.toFixed(2) : '—'),
      num(s.tcp != null ? `${s.tcp} / ${s.udp}` : '—', connText(s) || null),
      num(uptime(s) || '—'),
      n.plan ? num(fmtExpiry(n.plan.days), [n.plan.expire_at, n.plan.price].filter(Boolean).join(' · '), expiryClass(n.plan.days))
        : num('—'));
    return tr;
  });
  return table(['节点', 'CPU', '内存', '磁盘', '网速', '本周期流量', '负载', 'TCP / UDP', '运行', '到期'], rows, 'nodes');
}

// The last hour on a card: CPU against 100%, and both directions of the
// network against the hour's own peak.
function cardSparks(sp) {
  const peak = Math.max(1, ...sp.rx.map(v => v || 0), ...sp.tx.map(v => v || 0));
  const top = Math.max(0, ...sp.cpu.map(v => v || 0));
  const swatch = className => h('i', { class: 'swatch ' + className });
  return h('div', { class: 'sparks' },
    h('div', null, h('div', { class: 'spark-label', text: 'CPU · 近 1 小时' }),
      sparkline([[sp.cpu, 's1']], 100, `近 1 小时 CPU，最高 ${fmtPct(top)}（纵轴 0–100%）`)),
    h('div', null, h('div', { class: 'spark-label' }, '网速 ', swatch('s1'), '↓ ', swatch('s2'), '↑'),
      sparkline([[sp.rx, 's1'], [sp.tx, 's2']], peak, `近 1 小时网速，峰值 ${fmtRate(peak)}`)));
}

function nodeCard(n, agents, sp) {
  const s = n.status, old = agents.stale(n), plan = n.plan;
  const badge = (className, text, title) => h('span', { class: cls('badge', className), text, title });
  const card = h('a', { class: cls('card panel', !n.online && 'offline'), href: nodeHref(n) },
    h('div', { class: 'card-head' },
      dot(n),
      h('span', { class: 'name', text: n.name }),
      n.name !== n.id ? h('span', { class: 'id muted', text: n.id }) : null,
      regionBadge(n),
      h('span', { class: 'spacer' }),
      !s ? badge(null, '从未上报') : !n.online ? badge('bad', '离线 ' + fmtDur(nowSec() - s.fresh_at)) : null,
      s && Math.abs(s.clock_skew) > 60 ? badge('warn', `时钟偏差 ${s.clock_skew}s`) : null,
      plan && plan.days <= 7 ? badge(expiryClass(plan.days), fmtExpiry(plan.days)) : null,
      old ? badge('warn', 'agent 可升级', `agent ${old}，最新 ${agents.newest}`) : null));
  const expiry = plan ? kv('到期', [plan.expire_at, fmtExpiry(plan.days), plan.price].filter(Boolean).join(' · '),
    expiryClass(plan.days)) : null;
  if (!s) {
    if (expiry) card.append(expiry);
    return card;
  }

  const sys = s.sys;
  card.append(h('div', { class: 'card-sub muted', text: sys
    ? [sys.os, sys.arch, sys.cores ? sys.cores + ' 核' : null, uptime(s) && '运行 ' + uptime(s)].filter(Boolean).join(' · ')
    : '—' }));
  if (sp) card.append(cardSparks(sp));

  const metric = (label, used, total, val) => h('div', { class: 'metric' },
    h('span', { class: 'label', text: label }), bar(pctOf(used, total)),
    h('span', { class: 'val', text: val || `${fmtBytes(used)} / ${fmtBytes(total)}` }));
  if (s.cpu != null) card.append(metric('CPU', s.cpu, 100, fmtPct(s.cpu)));
  if (s.mem_total) card.append(metric('内存', s.mem_used, s.mem_total));
  if (s.swap_total) card.append(metric('Swap', s.swap_used, s.swap_total));
  const d = rootDisk(s);
  if (d) card.append(metric('磁盘', d.used, d.total));

  card.append(kv('网速', `↓ ${fmtRate(totalRate(s, 'rx'))}   ↑ ${fmtRate(totalRate(s, 'tx'))}`));
  if (s.load1 != null) card.append(kv('负载', [s.load1, s.load5, s.load15].map(v => v.toFixed(2)).join(' / ')));
  if (connText(s)) card.append(kv('连接', connText(s)));
  if (s.traffic) {
    card.append(kv(`本周期流量（${fmtDay(s.traffic.start)} 起）`, `↓ ${fmtBytes(s.traffic.rx)}   ↑ ${fmtBytes(s.traffic.tx)}`));
    if (n.quota) card.append(metric('配额', n.quota.used, n.quota.bytes));
  }
  if (expiry) card.append(expiry);
  return card;
}
