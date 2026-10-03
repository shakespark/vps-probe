// Traffic: every node's current billing period, and one node's daily use
// and past periods.

import { api, nodeURL } from '../js/api.js';
import { h, cls, bar, table, empty } from '../js/dom.js';
import { fmtBytes, fmtPct, fmtDay } from '../js/format.js';
import { Chart, barOption } from '../js/charts.js';
import { go } from '../js/router.js';

const DEFAULTS = { node: '' };

export function trafficPage(_, query) {
  const picked = query(DEFAULTS).node;
  const overview = h('div', { class: 'table-wrap' });
  const detailTitle = h('h2');
  const chart = new Chart('每日流量');
  const history = h('div', { class: 'table-wrap' });
  const text = (v, title) => h('td', { text: v, title });
  const el = h('div', null,
    h('h1', { text: '流量' }),
    h('div', { class: 'panel section' }, overview),
    h('div', { class: 'section' }, detailTitle),
    h('div', { class: 'charts' }, chart.el, h('div', { class: 'panel' }, h('h2', { text: '历史周期' }), history)),
    h('div', { class: 'section muted', text: '每日流量按服务端的时区分日。配额的 GB 按 1024³ 字节计算。' }));
  return {
    nav: 'traffic',
    el,
    destroy() { chart.dispose(); },
    async refresh() {
      const nodes = await api('/api/traffic');
      if (!nodes.length) return;
      const n = nodes.find(x => x.id === picked) || nodes[0];

      overview.replaceChildren(table(['节点', '周期', '下行', '上行', '合计', '计费方式', '已用 / 配额'], nodes.map(x => {
        const p = x.periods[0], q = x.quota;
        return h('tr', { class: cls('clickable', x === n && 'selected'), onclick: () => go({ node: x.id }, DEFAULTS) },
          text(x.name),
          text(p ? `${fmtDay(p.start)} 起` : '—', p ? `${fmtDay(p.start)} 至 ${fmtDay(p.end)}` : null),
          text(p ? fmtBytes(p.rx) : '—'), text(p ? fmtBytes(p.tx) : '—'), text(p ? fmtBytes(p.rx + p.tx) : '—'),
          text(q ? q.mode : '—'),
          h('td', null, q ? [bar(100 * q.used / q.bytes), `${fmtBytes(q.used)} / ${fmtBytes(q.bytes)}（${fmtPct(100 * q.used / q.bytes)}）`]
            : `${fmtBytes(p ? p.billable : 0)} / 不限`));
      }), null));

      detailTitle.textContent = n.name + '：本周期每日流量与历史周期';
      history.replaceChildren(n.periods.length
        ? table(['周期', '下行', '上行', '合计', '计费量'], n.periods.map(p => h('tr', null,
          text(`${fmtDay(p.start)} 至 ${fmtDay(p.end)}`, p.ifaces.map(i => `${i.iface}: ↓${fmtBytes(i.rx)} ↑${fmtBytes(i.tx)}`).join('\n')),
          text(fmtBytes(p.rx)), text(fmtBytes(p.tx)), text(fmtBytes(p.rx + p.tx)), text(fmtBytes(p.billable)))), null)
        : empty('暂无数据'));

      const { days } = await api(nodeURL(n.id, 'traffic/daily'));
      chart.set(barOption(days.map(d => d.day.slice(5)), fmtBytes, [['下行', days.map(d => d.rx)], ['上行', days.map(d => d.tx)]]));
    },
  };
}
