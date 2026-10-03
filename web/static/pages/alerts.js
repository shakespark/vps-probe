// Alerts: what is firing now, what was sent, and the rules in effect. The
// server words the values and rules; this page only lays them out.

import { api } from '../js/api.js';
import { h, cls, picker, seg, table, empty } from '../js/dom.js';
import { fmtTime, nowSec } from '../js/format.js';
import { go } from '../js/router.js';

const RANGES = [['1d', '24 小时', 86400], ['7d', '7 天', 7 * 86400], ['30d', '30 天', 30 * 86400], ['90d', '90 天', 90 * 86400]];
const EVENTS = { firing: '告警', repeat: '仍在告警', recovered: '恢复', notice: '提醒', report: '报告' };
const EVENT_CLASS = { firing: 'bad', repeat: 'bad', recovered: null, notice: 'warn', report: 'warn' };
const EVENT_FILTERS = [['', '全部事件'], ['firing,repeat', '告警'], ['recovered', '恢复'], ['notice', '提醒'], ['report', '报告']];
const DEFAULTS = { range: '7d', node: '', rule: '', event: '' };

export function alertsPage(_, query) {
  const q = query(DEFAULTS);
  const set = changes => go(changes, DEFAULTS);
  const range = RANGES.find(r => r[0] === q.range) || RANGES[1];
  const filtered = q.node || q.rule || q.event;
  const active = h('div', { class: 'table-wrap' });
  const history = h('div', { class: 'table-wrap' });
  const rules = h('div', { class: 'table-wrap' });
  const histCount = h('span', { class: 'muted' });
  const channels = h('span', { class: 'badge' });
  const nodeSel = picker(v => set({ node: v })), ruleSel = picker(v => set({ rule: v })), eventSel = picker(v => set({ event: v }));
  const cell = t => h('td', { text: t || '—' });
  const badge = (className, text) => h('td', null, h('span', { class: cls('badge', className), text }));
  return {
    nav: 'alerts',
    el: h('div', null,
      h('div', { class: 'row' }, h('h1', { text: '告警' }), channels),
      h('div', { class: 'panel section' }, h('h2', { text: '当前告警' }), active),
      h('div', { class: 'panel section' },
        h('div', { class: 'row filters' }, h('h2', { text: '告警历史' }), histCount, h('span', { class: 'spacer' }),
          seg(RANGES, range[0], v => set({ range: v })), nodeSel, ruleSel, eventSel,
          filtered ? seg([['clear', '清除筛选']], null, () => set({ node: '', rule: '', event: '' })) : null),
        history),
      h('div', { class: 'panel section' }, h('h2', { text: '规则' }), rules)),
    async refresh() {
      const to = nowSec();
      const params = new URLSearchParams({ from: to - range[2], to });
      for (const k of ['node', 'rule', 'event']) if (q[k]) params.set(k, q[k]);
      const [d, nodes] = await Promise.all([api('/api/alerts?' + params), api('/api/nodes')]);
      const names = new Map(nodes.map(n => [n.id, n.name]));
      const nodeText = id => names.get(id) || id;

      channels.textContent = d.channels.length ? '通知：' + d.channels.join('、') : '未配置通知渠道：告警只记录不发送';
      channels.className = cls('badge', !d.channels.length && 'warn');

      active.replaceChildren(d.active.length
        ? table(['状态', '规则', '节点', '对象', '当前值', '开始于'], d.active.map(a => h('tr', null,
          badge(a.firing ? 'bad' : 'warn', a.firing ? '告警中' : '观察中'),
          cell(a.rule), cell(nodeText(a.node)), cell(a.target), cell(a.value), cell(fmtTime(a.since)))))
        : empty('没有告警'));

      // Keep the current choice listed even when the range no longer has it.
      const withCurrent = (vals, cur) => cur && !vals.includes(cur) ? [...vals, cur] : vals;
      nodeSel.fill([['', '全部节点'], ...withCurrent(d.facets.nodes, q.node).map(id => [id, nodeText(id)])], q.node);
      ruleSel.fill([['', '全部规则'], ...withCurrent(d.facets.rules, q.rule).map(r => [r, r])], q.rule);
      eventSel.fill(EVENT_FILTERS, q.event);
      histCount.textContent = d.truncated ? `只显示最近 ${d.history.length} 条，缩小范围查看更早的` : `共 ${d.history.length} 条`;

      history.replaceChildren(d.history.length
        ? table(['时间', '事件', '规则', '节点', '对象', '值'], d.history.map(e => h('tr', { title: e.message },
          cell(fmtTime(e.ts)), badge(EVENT_CLASS[e.event], EVENTS[e.event] || e.event),
          cell(e.rule), cell(e.node ? nodeText(e.node) : '全部'), cell(e.target), cell(e.value))))
        : empty(filtered ? '没有符合条件的告警' : `最近 ${range[1]}没有告警`));

      rules.replaceChildren(d.rules.length
        ? table(['名称', '条件', '节点', '重复提醒', '恢复通知'], d.rules.map(r => h('tr', null,
          cell(r.name), cell(r.condition), cell(r.nodes), cell(r.repeat), cell(r.recovery))))
        : empty('没有配置告警规则'));
    },
  };
}
