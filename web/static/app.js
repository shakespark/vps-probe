// VPS probe UI: a hash-routed single-page app over the read-only API, in
// plain modules with no build step.
//
//   js/      what the pages share: DOM, formatting, API, charts, the router
//   pages/   one module per page

import { api } from './js/api.js';
import { h } from './js/dom.js';
import { fmtBytes, fmtTime, setTimezone } from './js/format.js';
import { darkQuery } from './js/charts.js';
import { route, show } from './js/router.js';
import { overviewPage } from './pages/overview.js';
import { nodePage } from './pages/node.js';
import { matrixPage, linkPage } from './pages/ping.js';
import { trafficPage } from './pages/traffic.js';
import { alertsPage } from './pages/alerts.js';

route(/^\/$/, overviewPage);
route(/^\/node\/([^/]+)$/, nodePage);
route(/^\/ping$/, matrixPage);
route(/^\/ping\/([^/]+)\/([^/]+)$/, linkPage);
route(/^\/traffic$/, trafficPage);
route(/^\/alerts$/, alertsPage);

const $app = document.getElementById('app');
const $foot = document.getElementById('foot');

// The footer: how the server is doing. Its first answer also tells the
// timezone the server counts days in.
async function refreshFooter() {
  try {
    const s = await api('/api/stats');
    setTimezone(s.timezone);
    const dropped = Object.entries(s.ingest).filter(([k, v]) => v > 0 && !['accepted', 'duplicate'].includes(k));
    $foot.replaceChildren(
      `上报 ${s.ingest.accepted} 个包（重传 ${s.ingest.duplicate}）· `,
      dropped.length ? h('span', { class: 'badge warn', text: '丢弃 ' + dropped.map(([k, v]) => `${k}=${v}`).join(' ') }) : '无丢弃',
      ` · 数据库 ${fmtBytes(s.db_bytes)} · 服务端 ${s.version} · 服务端时间 ${fmtTime(s.server_time)}（${s.timezone}）`);
  } catch {
    // The page's own refresh reports connectivity errors.
  }
}

// Nothing can be drawn until ECharts is there. It is the largest file of the
// page and the one most likely to be missed (a server restart, a flaky
// link), so it is loaded again a few times before asking for a reload.
const ECHARTS_RETRY = [2000, 5000, 10000];
const ready = () => typeof echarts !== 'undefined';

function loadECharts(attempt, then) {
  if (ready()) { then(); return; }
  if (attempt >= ECHARTS_RETRY.length) {
    const reload = h('button', { class: 'reload', onclick: () => location.reload() }, '刷新页面');
    $app.replaceChildren(h('div', { class: 'error' }, '图表库（ECharts）加载失败，请刷新页面。 ', reload));
    return;
  }
  $app.replaceChildren(h('div', { class: 'empty',
    text: `图表库（ECharts）没有加载上，正在重试（${attempt + 1}/${ECHARTS_RETRY.length}）…` }));
  setTimeout(() => {
    const old = document.querySelector('script[src*="echarts"]');
    const s = document.createElement('script');
    s.src = old.getAttribute('src');
    // A reply that is not the script (a login page, say) still fires load:
    // what counts is whether echarts exists afterwards.
    s.addEventListener('load', () => loadECharts(attempt + 1, then));
    s.addEventListener('error', () => loadECharts(attempt + 1, then));
    old.replaceWith(s);
  }, ECHARTS_RETRY[attempt]);
}

const showIfReady = () => { if (ready()) show(); };
window.addEventListener('hashchange', showIfReady);
darkQuery.addEventListener('change', showIfReady); // charts are drawn in the theme's colors

await refreshFooter(); // before the first page: dates need the server's timezone
setInterval(refreshFooter, 30000);
loadECharts(0, show);
