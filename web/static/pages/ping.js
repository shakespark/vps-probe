// Latency: the matrix of every link now, each link's availability over
// time, and one link's history.

import { api, enc, nodeURL } from '../js/api.js';
import { h, cls, kv, picker, seg, empty } from '../js/dom.js';
import { fmtMs, fmtPct, fmtAvail, nowSec } from '../js/format.js';
import { Chart, col, points, line, timeOption, rightAxis, latencyCap, capNote, DASHED,
  RANGES, rangeSeconds, rangeInterval } from '../js/charts.js';
import { go } from '../js/router.js';

const WINDOWS = [['5m', '5 分钟'], ['15m', '15 分钟'], ['1h', '1 小时']];
const AVAIL_RANGES = [['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天']];
const AVAIL_EVERY = 60000; // the strips come from 5m rollups; no need to poll them every 10s
const DEFAULTS = { window: '5m', range: '24h', src: '' };
const label = (options, value) => (options.find(o => o[0] === value) || options[0])[1];

const linkHref = (src, dst) => `#/ping/${enc(src)}/${enc(dst)}`;
const linkKey = l => l.src + '\n' + l.dst;

function latClass(link) {
  if (link.avg == null) return 'lat4';
  let lvl = link.avg < 30 ? 0 : link.avg < 80 ? 1 : link.avg < 150 ? 2 : link.avg < 250 ? 3 : 4;
  if (link.loss_pct >= 20) lvl = 4;
  else if (link.loss_pct >= 5) lvl = Math.max(lvl, 3);
  return 'lat' + lvl;
}

export function matrixPage(_, query) {
  const q = query(DEFAULTS);
  const set = changes => go(changes, DEFAULTS);
  const wrap = h('div', { class: 'table-wrap' });
  const quality = h('div', { class: 'quality' });
  const strips = h('div', { class: 'avail' });
  const srcSel = picker(v => set({ src: v }));
  const windowText = label(WINDOWS, q.window), rangeText = label(AVAIL_RANGES, q.range);
  let avail = null, availAt = 0;
  const legend = (...items) => h('div', { class: 'legend muted' }, items.map(([className, text]) => h('span', { class: className, text })));
  const el = h('div', null,
    h('div', { class: 'row' }, h('h1', { text: '时延' }), h('span', { class: 'spacer' }),
      h('span', { class: 'muted', text: '统计窗口' }), seg(WINDOWS, q.window, v => set({ window: v }))),
    h('div', { class: 'panel section' }, h('h2', { text: '网络质量' }), quality),
    h('div', { class: 'panel section' }, h('h2', { text: `时延矩阵（最近 ${windowText}）` }), wrap,
      legend(['l0', '< 30 ms'], ['l1', '< 80 ms'], ['l2', '< 150 ms'], ['l3', '< 250 ms 或丢包 ≥ 5%'],
        ['l4', '更高 / 丢包 ≥ 20% / 不通'], [null, '行 = 发起方，列 = 目标；点击格子查看历史'])),
    h('div', { class: 'panel section' },
      h('div', { class: 'row filters' }, h('h2', { text: '链路可用性' }), h('span', { class: 'spacer' }),
        seg(AVAIL_RANGES, q.range, v => set({ range: v })), srcSel),
      strips,
      legend(['a-up', '正常'], ['a-lossy', '丢包 ≥ 1%'], ['a-part', '部分时段不可用'], ['a-down', '一半以上时段不可用'],
        ['a-none', '无数据'], [null, '某个 5 分钟丢包 ≥ 20% 记为不可用；可用率 = 可用的 5 分钟 / 有数据的 5 分钟'])));
  return {
    nav: 'ping',
    el,
    async refresh() {
      const stale = Date.now() - availAt >= AVAIL_EVERY;
      const [data, nodes, av] = await Promise.all([api('/api/ping/matrix?window=' + enc(q.window)), api('/api/nodes'),
        stale ? api('/api/ping/availability?range=' + enc(q.range)) : avail]);
      if (stale) { avail = av; availAt = Date.now(); }
      const names = new Map(nodes.map(n => [n.id, n.name]));
      const nameOf = id => names.get(id) || id;
      // Columns: the nodes, then targets that are not nodes (tunnel probes).
      const extra = [...new Set([...data.links, ...avail.links].map(l => l.dst).filter(d => !names.has(d)))].sort();
      const cols = [...data.nodes, ...extra];
      quality.replaceChildren(...qualityCols(data.links, avail, nameOf, windowText, rangeText));
      srcSel.fill([['', '全部发起方'], ...data.nodes.map(id => [id, nameOf(id)])], q.src);
      if (stale) strips.replaceChildren(...stripRows(avail, q.src, data.nodes, cols, nameOf)); // unchanged otherwise
      wrap.replaceChildren(data.links.length ? matrix(data, cols, nameOf) : empty('这个窗口内没有时延数据'));
    },
  };
}

function matrix(data, cols, nameOf) {
  const links = new Map(data.links.map(l => [linkKey(l), l]));
  const head = h('tr', null, h('th', { text: '发起 \\ 目标' }), cols.map(c => h('th', { text: nameOf(c), title: c })));
  const rows = data.nodes.map(src => h('tr', null,
    h('td', { text: nameOf(src), title: src }),
    cols.map(dst => {
      if (src === dst) return h('td', { class: 'self', text: '—' });
      const l = links.get(linkKey({ src, dst }));
      if (!l) return h('td', { class: 'muted', text: '' });
      const tip = `${src} → ${dst}\n平均 ${fmtMs(l.avg)}  最小 ${fmtMs(l.min)}  最大 ${fmtMs(l.max)}\n` +
        `抖动 ${fmtMs(l.jitter)}  丢包 ${fmtPct(l.loss_pct)}（${l.lost}/${l.sent}）`;
      return h('td', { class: 'cell ' + latClass(l), title: tip, onclick: () => { location.hash = linkHref(src, dst); } },
        h('span', { class: 'ms', text: l.avg == null ? '不通' : fmtMs(l.avg) }),
        l.loss_pct > 0 && l.avg != null ? h('span', { class: 'loss', text: '丢包 ' + fmtPct(l.loss_pct, 0) }) : null);
    })));
  return h('table', { class: 'matrix' }, h('thead', null, head), h('tbody', null, rows));
}

// The three worst links by a few measures: what to look at first.
function qualityCols(links, avail, nameOf, windowText, rangeText) {
  const column = (title, items, none) => h('div', { class: 'q-col' }, h('div', { class: 'k', text: title }),
    items.length ? items.map(([l, text, className]) => h('a', { class: 'q-item', href: linkHref(l.src, l.dst) },
      h('span', { class: 'q-name', text: `${nameOf(l.src)} → ${nameOf(l.dst)}` }), h('span', { class: cls('num', className), text })))
      : h('div', { class: 'muted', text: none }));
  const worst = (arr, key) => arr.filter(l => key(l) > 0).sort((a, b) => key(b) - key(a)).slice(0, 3);
  // Only links still measured: a link no longer pinged keeps old data in the range.
  const measured = new Set(links.map(linkKey));
  return [
    column(`丢包最多（${windowText}）`, worst(links, l => l.loss_pct).map(l =>
      [l, l.avg == null ? '不通' : '丢包 ' + fmtPct(l.loss_pct, 1), l.loss_pct >= 20 ? 'bad' : 'warn']), '没有丢包'),
    column(`抖动最大（${windowText}）`, worst(links.filter(l => l.jitter != null), l => l.jitter).map(l =>
      [l, '抖动 ' + fmtMs(l.jitter), null]), '没有数据'),
    column(`可用率最低（${rangeText}）`, worst(avail.links.filter(a => a.avail_pct != null && measured.has(linkKey(a))), a => 100 - a.avail_pct)
      .map(a => [a, '可用 ' + fmtAvail(a.avail_pct), a.avail_pct < 99 ? 'bad' : 'warn']), '全部 100%'),
  ];
}

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
function stripRows(avail, onlySrc, nodes, cols, nameOf) {
  const order = (arr, v) => { const i = arr.indexOf(v); return i < 0 ? arr.length : i; };
  const links = avail.links.filter(a => !onlySrc || a.src === onlySrc)
    .sort((a, b) => order(nodes, a.src) - order(nodes, b.src) || order(cols, a.dst) - order(cols, b.dst));
  if (!links.length) return [empty('这段时间内没有时延数据')];
  // Every link shares the same cells: label them once. Formatting a date
  // builds a formatter per call unless one is reused, far too slow for
  // thousands of cells.
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
  return out;
}

// ---- one link ----

// Loss levels of one sample, in the availability strips' colors: below 1%
// is fine there too, and a 5-minute period losing 20% or more counts as
// unavailable.
const LOSS_LEVELS = [[1, 'a-up', '< 1%'], [20, 'a-lossy', '1–20%'], [50, 'a-part', '20–50%'], [Infinity, 'a-down', '≥ 50%']];
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

const LINK_DEFAULTS = { range: '1h' };

export function linkPage([src, dst], query) {
  const { range } = query(LINK_DEFAULTS);
  const chart = new Chart('时延与丢包');
  chart.el.insertBefore(h('div', { class: 'legend chart-legend' }, '平均线和丢包柱按丢包率着色：',
    LOSS_LEVELS.map(([, className, text]) => h('span', { class: className, text }))), chart.box);
  const stats = h('div', { class: 'summary' });
  const el = h('div', null,
    h('div', { class: 'row' }, h('a', { href: '#/ping', text: '← 时延矩阵' })),
    h('div', { class: 'panel section' }, h('h1', { text: `${src} → ${dst}` }), stats),
    h('div', { class: 'row section' }, h('span', { class: 'muted', text: '时间范围' }),
      seg(RANGES, range, v => go({ range: v }, LINK_DEFAULTS))),
    h('div', { class: 'section' }, chart.el));
  return {
    nav: 'ping',
    el,
    interval: () => rangeInterval(range),
    destroy() { chart.dispose(); },
    async refresh() {
      const to = nowSec(), from = to - rangeSeconds(range);
      const s = await api(nodeURL(src, 'ping/' + enc(dst), `from=${from}&to=${to}`));
      const c = s.cols;
      const sum = k => c[k].reduce((a, v) => a + (v || 0), 0);
      const vals = k => c[k].filter(v => v != null);
      const sent = sum('sent'), avgs = vals('avg');
      const stat = (k, list, f) => kv(k, list.length ? fmtMs(f(list)) : '—');
      stats.replaceChildren(
        kv('发送', String(sent)), kv('丢包率', sent ? fmtPct(100 * sum('lost') / sent, 2) : '—'),
        stat('最小', vals('min'), l => Math.min(...l)),
        stat('平均', avgs, l => l.reduce((a, v) => a + v, 0) / l.length),
        stat('最大', vals('max'), l => Math.max(...l)),
        kv('数据粒度', `${s.tier} / ${s.step}s`));

      // Min and max stay neutral so no series color reads as a loss level.
      const colors = LOSS_LEVELS.map(l => cssVar('--' + l[1])), gray = { itemStyle: { color: cssVar('--muted') } };
      const avg = points(s.ts, c.avg, s.step, undefined, [c.loss_pct]);
      const [cap, highest] = latencyCap(avgs, vals('max'));
      chart.note(capNote(cap, highest));
      const option = timeOption(from, to, fmtMs, [
        line('最小', col(s, 'min'), { ...DASHED, ...gray }),
        line('平均', avg, { lineStyle: { width: 2 }, itemStyle: { color: colors[0] } }),
        line('最大', col(s, 'max'), { lineStyle: { width: 1, type: 'dotted' }, ...gray }),
        { type: 'bar', name: '丢包', yAxisIndex: 1, data: col(s, 'loss_pct'), barMaxWidth: 6,
          itemStyle: { opacity: 0.6, color: p => colors[p.value[1] == null ? 0 : lossLevel(p.value[1])] },
          tooltip: { valueFormatter: v => v == null ? '—' : fmtPct(v, 1) } },
      ], { yMax: cap || undefined, y2: rightAxis(true) });
      // The bars' own color varies by level; visible ones always have loss.
      option.legend.data = ['最小', '平均', '最大', { name: '丢包', itemStyle: { color: colors[3] } }];
      option.visualMap = { type: 'piecewise', show: false, dimension: 0, seriesIndex: 1,
        pieces: lossPieces(avg, colors), outOfRange: { color: colors[0] } };
      chart.set(option);
    },
  };
}
