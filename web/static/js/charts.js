// Time charts on ECharts, which index.html loads as a global. Text from
// agents reaches charts as canvas text (richText tooltips), never as HTML.

import { h } from './dom.js';
import { fmtMs } from './format.js';

export const darkQuery = matchMedia('(prefers-color-scheme: dark)');

export class Chart {
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

// hideTip alone leaves a crosshair placed by showTip; ECharts' own mouse-out
// sends this leave trigger too.
function hideTip(c) {
  c.c.dispatchAction({ type: 'hideTip' });
  c.c.dispatchAction({ type: 'updateAxisPointer', currTrigger: 'leave' });
}

// ChartGroup is charts sharing a time axis: hovering one moves the crosshair
// on all of them to the same moment, so a CPU spike can be lined up with a
// latency one. Not echarts.connect: that syncs by data index, which lands on
// the wrong time (or nowhere) in charts sampled differently, like disks every
// 60s, or with gaps. Only the chart under the pointer shows its tooltip; the
// others' would cover the very lines being compared.
export class ChartGroup {
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

// points turns a series' columns into chart points. A null is inserted where
// data is missing, so lines break over outages instead of bridging them.
// "Missing" is judged against the series' own typical spacing, since some
// data (disks: every 60s) is sparser than the bucket step.
// extra: more columns to carry along in each point, e.g. loss beside avg.
export function points(ts, vals, step, f = v => v, extra = []) {
  const diffs = ts.slice(1).map((t, i) => t - ts[i]).sort((a, b) => a - b);
  const typical = Math.max(step, diffs.length ? diffs[diffs.length >> 1] : step);
  const out = [];
  for (let i = 0; i < ts.length; i++) {
    if (i > 0 && ts[i] - ts[i - 1] > typical * 2.5) out.push([(ts[i - 1] + step) * 1000, null]);
    out.push([ts[i] * 1000, vals[i] == null ? null : f(vals[i]), ...extra.map(a => a[i])]);
  }
  return out;
}

// col is a column of a series from the API as chart points.
export const col = (s, name, f) => points(s.ts, s.cols[name], s.step, f);

export function line(name, data, extra) {
  return Object.assign({ type: 'line', name, data, showSymbol: false, connectNulls: false,
    lineStyle: { width: 1.5 }, emphasis: { disabled: true } }, extra);
}
export const DASHED = { lineStyle: { width: 1, type: 'dashed' } };

// A second y axis on the right: percentages when pct, counts otherwise.
export const rightAxis = pct => ({ type: 'value', min: 0, max: pct ? 100 : undefined, splitLine: { show: false },
  axisLabel: pct ? { formatter: '{value}%' } : {} });

const BASE = {
  backgroundColor: 'transparent',
  animation: false,
  grid: { left: 8, right: 12, top: 36, bottom: 8, containLabel: true },
};
const SPLIT = { lineStyle: { opacity: 0.35 } };

// timeOption is a chart of series over [from, to] (unix seconds).
export function timeOption(from, to, yFmt, series, { yMax, y2, tipFmt = yFmt } = {}) {
  const yAxis = [{ type: 'value', min: 0, max: yMax, axisLabel: { formatter: yFmt }, splitLine: SPLIT }];
  if (y2) yAxis.push(y2);
  return {
    ...BASE,
    tooltip: { trigger: 'axis', renderMode: 'richText', confine: true, valueFormatter: v => v == null ? '—' : tipFmt(v) },
    legend: { top: 0, type: 'scroll' },
    xAxis: { type: 'time', min: from * 1000, max: to * 1000, axisLabel: { hideOverlap: true } },
    yAxis,
    series,
  };
}

// barOption is stacked bars over categories.
export function barOption(categories, yFmt, series) {
  return {
    ...BASE,
    tooltip: { trigger: 'axis', renderMode: 'richText', confine: true, valueFormatter: yFmt },
    legend: { top: 0 },
    xAxis: { type: 'category', data: categories, axisLabel: { hideOverlap: true } },
    yAxis: { type: 'value', axisLabel: { formatter: yFmt }, splitLine: SPLIT },
    series: series.map(([name, data]) => ({ type: 'bar', name, stack: 't', data, barMaxWidth: 28 })),
  };
}

// latencyCap is a y-axis top that fits the typical values when a few
// outliers would squash the rest (one 10 s round trip against a 40 ms
// line), or null when everything fits anyway. Clipped points run off the
// top; tooltips keep their real values. Returns [cap, highest].
export function latencyCap(avgs, maxes = []) {
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

export const capNote = (cap, highest) => cap
  ? `纵轴上限 ${fmtMs(cap)}，更高的点超出图表顶部（最高 ${fmtMs(highest)}），数值框里是实际值` : '';

// Time ranges of the history pages.
export const RANGES = [['1h', '1 小时', 3600], ['6h', '6 小时', 6 * 3600], ['24h', '24 小时', 86400],
  ['7d', '7 天', 7 * 86400], ['30d', '30 天', 30 * 86400]];
export const rangeSeconds = key => (RANGES.find(r => r[0] === key) || RANGES[0])[2];
// Long ranges are made of 5-minute and hourly points: no need to poll them
// every 10 seconds.
export const rangeInterval = key => rangeSeconds(key) > 86400 ? 60000 : 10000;
