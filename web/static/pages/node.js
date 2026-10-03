// One node: its state now and its history as charts on a shared time axis.

import { api, nodeURL } from '../js/api.js';
import { h, cls, kv, seg } from '../js/dom.js';
import { fmtBytes, fmtRate, fmtPct, fmtMs, fmtPPS, fmtCount, fmtDur, fmtDay, fmtTime, fmtExpiry, nowSec } from '../js/format.js';
import { Chart, ChartGroup, col, points, line, timeOption, rightAxis, latencyCap, capNote, DASHED,
  RANGES, rangeSeconds, rangeInterval } from '../js/charts.js';
import { go } from '../js/router.js';

const DEFAULTS = { range: '1h' };

export function nodePage([id], query) {
  const { range } = query(DEFAULTS);
  const title = h('h1', { text: id });
  const dot = h('span', { class: 'dot' });
  const sub = h('div', { class: 'muted' });
  const summary = h('div', { class: 'summary' });
  const group = new ChartGroup();
  const charts = {
    cpu: new Chart('CPU', group), load: new Chart('负载', group), mem: new Chart('内存 / Swap', group),
    net: new Chart('网络', group), pps: new Chart('包速率', group), disk: new Chart('磁盘使用率', group),
    conns: new Chart('连接与线程', group), ping: new Chart('时延（到各 peer）', group),
  };
  const el = h('div', null,
    h('div', { class: 'row' }, h('a', { href: '#/', text: '← 总览' })),
    h('div', { class: 'panel section' }, h('div', { class: 'row' }, dot, title), sub, summary),
    h('div', { class: 'row section' }, h('span', { class: 'muted', text: '时间范围' }),
      seg(RANGES, range, v => go({ range: v }, DEFAULTS))),
    h('div', { class: 'charts section' }, Object.values(charts).map(c => c.el)));

  return {
    nav: 'overview',
    el,
    interval: () => rangeInterval(range),
    destroy() { Object.values(charts).forEach(c => c.dispose()); },
    async refresh() {
      const to = nowSec(), from = to - rangeSeconds(range);
      const span = `from=${from}&to=${to}`;
      const [nodes, m, net, disks, pings] = await Promise.all([api('/api/nodes'),
        ...['metrics', 'net', 'disks', 'ping'].map(what => api(nodeURL(id, what, span)))]);
      const n = nodes.find(x => x.id === id);
      if (!n) throw new Error('未知节点 ' + id);
      title.textContent = [n.name + (n.name !== n.id ? `（${n.id}）` : ''), n.region, n.group].filter(Boolean).join(' · ');
      dot.className = cls('dot', n.online ? 'on' : 'off');
      sub.textContent = n.status ? subtitle(n.status.sys || {}) : '从未上报';
      summary.replaceChildren(...(n.status ? summaryOf(n) : []));

      const time = (yFmt, series, opt) => timeOption(from, to, yFmt, series, opt);
      const pct = { yMax: 100, tipFmt: v => fmtPct(v) };
      charts.cpu.set(time(v => v.toFixed(0) + '%', [
        line('CPU', col(m, 'cpu')),
        line('Steal', col(m, 'steal')),
        line('软中断', col(m, 'softirq')), // part of CPU, not on top of it
        // Averages hide short peaks once buckets are minutes or hours long.
        m.tier !== 'raw' ? line('CPU 峰值', col(m, 'cpu_max'), DASHED) : null,
      ].filter(Boolean), pct));
      charts.load.set(time(v => v.toFixed(2), [
        line('1 分钟', col(m, 'load1')), line('5 分钟', col(m, 'load5')), line('15 分钟', col(m, 'load15'))]));
      charts.mem.set(time(fmtBytes, [
        line('内存', col(m, 'mem_used'), { areaStyle: { opacity: 0.15 } }),
        line('Swap', col(m, 'swap_used')),
      ], { yMax: Math.max(0, ...m.cols.mem_total.filter(v => v != null)) || undefined }));
      // Threads run in the hundreds and up, so they get the right axis.
      charts.conns.set(time(fmtCount, [
        line('TCP', col(m, 'tcp')), line('UDP', col(m, 'udp')), line('TIME_WAIT', col(m, 'tcp_tw'), DASHED),
        line('线程', col(m, 'threads'), { yAxisIndex: 1 }),
      ], { y2: rightAxis(false) }));

      const ifaces = Object.keys(net).sort();
      const both = (down, up) => ifaces.flatMap(i => [line(`${i} ↓`, col(net[i], down)), line(`${i} ↑`, col(net[i], up))]);
      charts.net.set(time(fmtRate, both('rx', 'tx')));
      // Packets: a flood of small packets (SYN) barely shows as bytes.
      charts.pps.set(time(fmtPPS, both('rx_pps', 'tx_pps')));

      charts.disk.set(time(v => v.toFixed(0) + '%', Object.keys(disks).sort().map(mount => {
        const s = disks[mount];
        const used = s.cols.used.map((u, i) => u == null || !s.cols.total[i] ? null : 100 * u / s.cols.total[i]);
        return line(mount, points(s.ts, used, s.step));
      }), pct));

      const peers = Object.keys(pings).sort();
      const [cap, highest] = latencyCap(peers.flatMap(p => pings[p].cols.avg.filter(v => v != null)));
      charts.ping.note(capNote(cap, highest));
      charts.ping.set(time(fmtMs, peers.flatMap(p => [
        line(p, col(pings[p], 'avg')),
        { type: 'bar', name: p + ' 丢包', yAxisIndex: 1, data: col(pings[p], 'loss_pct'), barMaxWidth: 4,
          itemStyle: { opacity: 0.45 }, tooltip: { valueFormatter: v => v == null ? '—' : fmtPct(v, 0) } },
      ]), { yMax: cap || undefined, y2: rightAxis(true) }));
    },
  };
}

const subtitle = sys => [sys.hostname, sys.os, sys.kernel, sys.arch, sys.agent_version && 'agent ' + sys.agent_version]
  .filter(Boolean).join(' · ');

function summaryOf(n) {
  const s = n.status, sys = s.sys || {}, plan = n.plan, t = s.traffic;
  const d = (s.disks || []).find(x => x.mount === '/') || (s.disks || [])[0];
  const of = (used, total) => total ? `${fmtBytes(used)} / ${fmtBytes(total)}` : '—';
  return [
    kv('状态', n.online ? '在线' : '离线'),
    kv('最后上报', fmtTime(s.fresh_at)),
    kv('运行时间', sys.boot_time ? fmtDur(nowSec() - sys.boot_time) : '—'),
    kv('CPU', fmtPct(s.cpu) + (s.softirq != null ? `（软中断 ${fmtPct(s.softirq)}）` : '') + (sys.cores ? ` / ${sys.cores} 核` : '')),
    kv('内存', of(s.mem_used, s.mem_total)),
    kv(d ? '磁盘 ' + d.mount : '磁盘', d ? of(d.used, d.total) : '—'),
    kv('连接', s.tcp != null ? `TCP ${s.tcp} / UDP ${s.udp} / TIME_WAIT ${s.tcp_tw}` : '—'),
    kv('线程', fmtCount(s.threads)),
    kv('本周期流量', t ? `↓ ${fmtBytes(t.rx)} ↑ ${fmtBytes(t.tx)}（${fmtDay(t.start)} 起，${fmtDay(t.end)} 重置）` : '—'),
    n.quota ? kv('配额', `${of(n.quota.used, n.quota.bytes)}（${fmtPct(100 * n.quota.used / n.quota.bytes)}，计费：${n.quota.mode}）`) : null,
    kv('上报来源 IP', s.ip ? `${s.ip}（${fmtTime(s.ip_since)} 起）` : '—'),
    kv('时钟偏差', s.clock_skew + ' s'),
    plan ? kv('到期', [plan.expire_at, fmtExpiry(plan.days), plan.renew_months && `每 ${plan.renew_months} 个月自动续费`, plan.price]
      .filter(Boolean).join(' · ')) : null,
  ].filter(Boolean);
}
