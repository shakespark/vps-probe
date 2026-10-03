// Demo data for the static demo site (docs/DESIGN.md §14). Loaded before
// app.js, it answers the UI's /api/* requests in the browser: there is no
// server. Every node, host name and address here is made up.
//
// Values are a deterministic function of the timestamp, so the overview, the
// charts at every range and the latency matrix agree with each other and a
// refresh only appends points. The "story" (an attack, an offline node, ...)
// is anchored to the moment the page was opened.
(() => {
  'use strict';

  const DAY = 86400, TZ = 8 * 3600; // the demo server's timezone is UTC+8
  const GB = 1024 ** 3, MB = 1024 ** 2;
  const MAX_POINTS = 1000;
  const now = () => Math.floor(Date.now() / 1000);
  const T0 = now();

  // ---- deterministic noise ------------------------------------------------

  function hash(s) { // FNV-1a
    let x = 2166136261;
    for (let i = 0; i < s.length; i++) { x ^= s.charCodeAt(i); x = Math.imul(x, 16777619); }
    return x >>> 0;
  }
  const seeds = new Map();
  function seed(key) {
    let v = seeds.get(key);
    if (v === undefined) seeds.set(key, v = hash(key));
    return v;
  }
  // rnd: [0, 1) for (key, integer).
  function rnd(key, n) {
    let x = (seed(key) ^ Math.imul(n | 0, 0x9e3779b1)) >>> 0;
    x ^= x >>> 16; x = Math.imul(x, 0x7feb352d);
    x ^= x >>> 15; x = Math.imul(x, 0x846ca68b);
    x ^= x >>> 16;
    return (x >>> 0) / 4294967296;
  }
  // noise: [0, 1), smooth over about `period` seconds.
  function noise(key, t, period) {
    const i = Math.floor(t / period), f = t / period - i;
    const a = rnd(key, i), b = rnd(key, i + 1);
    return a + (b - a) * f * f * (3 - 2 * f);
  }
  // Busy in the evening, quiet before dawn (server time): 0..1.
  const diurnal = t => 0.5 + 0.5 * Math.sin(2 * Math.PI * (((t + TZ) % DAY) / DAY - 0.58));

  // ---- the fleet ----------------------------------------------------------

  const DEB12 = ['Debian GNU/Linux 12 (bookworm)', '6.1.0-28-amd64'];
  const DEB13 = ['Debian GNU/Linux 13 (trixie)', '6.12.48+deb13-amd64'];
  const UBU = ['Ubuntu 24.04.3 LTS', '6.8.0-85-generic'];
  // cpu: typical %, net: typical bytes/s, daily: typical GB per day,
  // sym: relays forward traffic, so rx is about tx.
  const NODES = [
    { id: 'tyo-1', name: '东京 1', region: 'JP', group: '亚洲', at: [35.7, 139.7], host: 'vm-tyo-7f3a', ip: '203.0.113.11',
      os: DEB12, cores: 2, mem: 2, disk: 40, cpu: 14, net: 9e5, daily: 14, sym: true, quota: 1000, mode: 'sum',
      price: '¥68/月', renew: 1, expireIn: 19, upDays: 143 },
    { id: 'hk-1', name: '香港 1', region: 'HK', group: '亚洲', at: [22.3, 114.2], host: 'hk-cn2-01', ip: '203.0.113.27',
      os: DEB13, cores: 2, mem: 4, disk: 60, cpu: 22, net: 2.4e6, daily: 38, sym: true, quota: 2000, mode: 'max',
      price: '$19.9/月', renew: 1, expireIn: 11, upDays: 61, extra: [{ name: 'tyo-via-iepl', rtt: 43 }] },
    { id: 'hk-2', name: '香港 2', region: 'HK', group: '亚洲', at: [22.31, 114.21], host: 'hkt-relay-02', ip: '203.0.113.52',
      os: DEB12, cores: 1, mem: 1, disk: 20, cpu: 31, net: 4.1e6, sym: true, quota: 1000, mode: 'sum',
      price: '$7/月', renew: 1, expireIn: 4, upDays: 27, nearQuota: true },
    { id: 'sg-1', name: '新加坡', region: 'SG', group: '亚洲', at: [1.35, 103.8], host: 'sgp1-s-1vcpu', ip: '198.51.100.8',
      os: UBU, cores: 1, mem: 1, disk: 25, cpu: 9, net: 3e5, daily: 5, quota: 1000, mode: 'tx',
      price: '$35/年', expireIn: 5, upDays: 301 },
    { id: 'tw-1', name: '台北', region: 'TW', group: '亚洲', at: [25.0, 121.5], host: 'tpe-hinet-a', ip: '198.51.100.87',
      os: DEB12, cores: 4, mem: 8, disk: 160, cpu: 18, net: 6e6, daily: 95, quota: 5000, mode: 'sum',
      price: 'NT$499/月', renew: 1, expireIn: 26, upDays: 88, steal: 4, diskUsed: 0.86, mounts: ['/', '/data'] },
    { id: 'la-1', name: '洛杉矶 1', region: 'US-LA', group: '美洲', at: [34.05, -118.2], host: 'lax-9929-kvm', ip: '192.0.2.44',
      os: DEB13, cores: 2, mem: 2, disk: 30, cpu: 11, net: 1.2e6, daily: 21, sym: true, quota: 2000, mode: 'sum',
      price: '$49.9/年', expireIn: 213, upDays: 17, ddos: true },
    { id: 'sjc-1', name: '圣何塞', region: 'US', group: '美洲', at: [37.3, -121.9], host: 'sjc-dc2-112', ip: '192.0.2.118',
      os: DEB12, cores: 2, mem: 4, disk: 80, cpu: 7, net: 2e5, daily: 3.5, quota: 4000, mode: 'sum',
      price: '$29/年', expireIn: 96, upDays: 412, agent: '0.2.0' },
    { id: 'fra-1', name: '法兰克福', region: 'DE', group: '欧洲', at: [50.1, 8.7], host: 'fra-cx22', ip: '198.51.100.140',
      os: UBU, cores: 2, mem: 4, disk: 40, cpu: 26, net: 7e5, daily: 9, quota: 20000, mode: 'tx',
      price: '€4.5/月', renew: 1, expireIn: 14, upDays: 35 },
    { id: 'lon-1', name: '伦敦', region: 'GB', group: '欧洲', at: [51.5, -0.1], host: 'lon-vps-3', ip: '198.51.100.201',
      os: DEB12, cores: 1, mem: 2, disk: 30, cpu: 16, net: 4e5, daily: 6, quota: 1000, mode: 'sum',
      price: '£5/月', renew: 1, expireIn: 9, upDays: 72, offline: true },
    { id: 'syd-1', name: '悉尼', region: 'AU', group: '大洋洲', at: [-33.9, 151.2], host: 'syd-nano', ip: '192.0.2.201',
      os: DEB13, cores: 1, mem: 1, disk: 20, cpu: 8, net: 1.5e5, daily: 2.2, upDays: 9, lossyTo: 'fra-1' },
  ];
  const byId = new Map(NODES.map(n => [n.id, n]));

  // The story, relative to page load.
  const DDOS_AT = T0 - 26 * 60;    // la-1: inbound flood, still going
  const OFFLINE_AT = T0 - 41 * 60; // lon-1: stopped reporting
  const lastTick = n => { const t = n.offline ? OFFLINE_AT : now(); return t - t % 10; };
  const alive = (n, t) => !n.offline || t < OFFLINE_AT;
  const flooded = (n, t) => n.ddos && t >= DDOS_AT;

  // ---- time helpers (server timezone) -------------------------------------

  const pad = v => String(v).padStart(2, '0');
  const local = t => new Date((t + TZ) * 1000); // read with getUTC*
  const ymd = t => { const d = local(t); return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`; };
  const hms = t => { const d = local(t); return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`; };
  const stamp = t => ymd(t) + ' ' + hms(t);
  const dayIndex = t => Math.floor((t + TZ) / DAY);
  const dayStart = i => i * DAY - TZ;
  const fromYMD = s => { const [y, m, d] = s.split('-').map(Number); return Date.UTC(y, m - 1, d) / 1000 - TZ; };

  // ---- instantaneous values -----------------------------------------------

  function inst(n, t) {
    const k = n.id, d = diurnal(t), flood = flooded(n, t);
    let cpu = n.cpu * (0.55 + 0.9 * d) + 7 * noise(k + 'c1', t, 600) + 4 * noise(k + 'c2', t, 45);
    // A few minutes of something heavy, a couple of times a day.
    if (rnd(k + 'job', Math.floor(t / 3600)) > 0.92 && t % 3600 < 240) cpu += 38 + 10 * noise(k + 'c3', t, 30);
    let softirq = cpu * 0.04 + 0.2 * noise(k + 'si', t, 120);
    if (flood) { softirq = 31 + 9 * noise(k + 'fsi', t, 50); cpu = 58 + softirq * 0.6 + 6 * noise(k + 'fc', t, 40); }
    cpu = Math.min(100, cpu);
    const steal = (n.steal || 0.05) * (0.4 + 1.2 * noise(k + 'st', t, 900));

    let rx = n.net * (0.25 + 1.3 * d) * (0.5 + noise(k + 'rx1', t, 1800)) * (0.7 + 0.6 * noise(k + 'rx2', t, 35));
    let tx = n.sym ? rx * (0.92 + 0.16 * noise(k + 'tx', t, 300))
      : n.net * 0.35 * (0.3 + 1.2 * d) * (0.5 + noise(k + 'tx1', t, 1500)) * (0.7 + 0.6 * noise(k + 'tx2', t, 35));
    let rxPkt = 780 + 300 * noise(k + 'ps', t, 900), txPkt = rxPkt;
    if (flood) { rx += 1.2e7 * (0.85 + 0.3 * noise(k + 'fl', t, 25)); rxPkt = 124; }

    const memTotal = n.mem * GB - 54 * MB;
    const memUsed = memTotal * (0.27 + 0.12 * (seed(k) % 100) / 100 + 0.05 * noise(k + 'm', t, 7200) + cpu / 900);
    const swapTotal = n.mem <= 2 ? GB : 0;
    const conns = n.net / 4e4 * (0.4 + 1.1 * d) * (0.8 + 0.4 * noise(k + 'tcp', t, 300));
    return {
      cpu, steal, softirq,
      load1: n.cores * cpu / 100 * 1.15 + 0.06 * noise(k + 'l', t, 90),
      memTotal, memUsed, swapTotal, swapUsed: swapTotal * (0.01 + 0.03 * noise(k + 'sw', t, DAY)),
      tcp: 12 + conns, udp: 4 + conns / 6 + (flood ? 3 : 0), tw: conns * 0.6, threads: 96 + 12 * n.cores + 9 * noise(k + 'th', t, 3600),
      rx, tx, rxPps: rx / rxPkt, txPps: tx / txPkt,
    };
  }

  // Sample times inside the bucket [t, t+step).
  function samples(t, step, grain) {
    if (step <= grain) return [t];
    const k = Math.min(8, Math.floor(step / grain));
    return Array.from({ length: k }, (_, i) => t + Math.floor((i + 0.5) * step / k));
  }
  const mean = a => a.reduce((x, y) => x + y, 0) / a.length;
  const r3 = v => Math.round(v * 1000) / 1000;

  // ---- series -------------------------------------------------------------

  // The server's tier rules (store.pickTier): the finest table still retained
  // that covers the range, widened to at most MAX_POINTS points.
  function pickTier(from, to, has5m) {
    const span = to - from, n = now();
    let tier = '1h', base = 3600;
    if (span <= 6 * 3600 && from >= n - 48 * 3600) { tier = 'raw'; base = 10; }
    else if (has5m && span <= 7 * DAY && from >= n - 30 * DAY) { tier = '5m'; base = 300; }
    else if (!has5m && span <= 2 * DAY && from >= n - 48 * 3600) { tier = 'raw'; base = 10; }
    let step = base;
    const need = Math.ceil(span / MAX_POINTS);
    if (need > step) step = Math.ceil(need / base) * base;
    return { tier, step };
  }

  // series calls row(t, step) for every bucket with data and collects the
  // columns. grain is how often raw points exist (10s; disks 60s).
  function series(from, to, has5m, grain, since, until, row) {
    const { tier, step } = pickTier(from, to, has5m);
    const every = Math.max(step, grain);
    const out = { tier, step, ts: [], cols: {} };
    const end = Math.min(to, until + 1, now() + 1);
    for (let t = Math.floor(Math.max(from, since) / every) * every; t < end; t += every) {
      const v = row(t, every);
      if (!v) continue;
      out.ts.push(t);
      for (const [name, val] of Object.entries(v)) (out.cols[name] ||= []).push(val === null ? null : r3(val));
    }
    return out;
  }
  const since = n => T0 - n.upDays * DAY;

  function metrics(n, from, to) {
    return series(from, to, true, 10, since(n), lastTick(n), (t, step) => {
      const s = samples(t, step, 10).map(x => inst(n, x));
      const avg = f => mean(s.map(f)), max = f => Math.max(...s.map(f));
      const lag = (w) => mean([0.25, 0.5, 0.75, 1].map(f => inst(n, t - w * f).load1));
      return {
        cpu: avg(v => v.cpu), cpu_max: max(v => v.cpu), steal: avg(v => v.steal), steal_max: max(v => v.steal),
        softirq: avg(v => v.softirq), softirq_max: max(v => v.softirq),
        load1: avg(v => v.load1), load1_max: max(v => v.load1), load5: lag(300), load15: lag(900),
        mem_total: s[0].memTotal, mem_used: avg(v => v.memUsed), mem_used_max: max(v => v.memUsed),
        swap_total: s[0].swapTotal, swap_used: avg(v => v.swapUsed), swap_used_max: max(v => v.swapUsed),
        tcp: Math.round(avg(v => v.tcp)), tcp_max: Math.round(max(v => v.tcp)),
        udp: Math.round(avg(v => v.udp)), udp_max: Math.round(max(v => v.udp)),
        tcp_tw: Math.round(avg(v => v.tw)), threads: Math.round(avg(v => v.threads)), threads_max: Math.round(max(v => v.threads)),
      };
    });
  }

  // The overview's sparklines: the last hour in minutes; the last bucket
  // contains now.
  function sparks() {
    const step = 60, n = 60, from = (Math.floor(now() / step) + 1) * step - n * step;
    const nodes = {};
    for (const node of NODES) {
      const sp = { cpu: [], rx: [], tx: [] };
      for (let t = from; t < from + n * step; t += step) {
        const has = t >= since(node) && t <= lastTick(node);
        const s = has ? samples(t, Math.min(step, lastTick(node) + 10 - t), 10).map(x => inst(node, x)) : null;
        sp.cpu.push(s ? r3(mean(s.map(v => v.cpu))) : null);
        sp.rx.push(s ? Math.round(mean(s.map(v => v.rx))) : null);
        sp.tx.push(s ? Math.round(mean(s.map(v => v.tx))) : null);
      }
      nodes[node.id] = sp;
    }
    return { from, step, n, nodes };
  }

  const iface = n => n.cores > 2 ? 'ens3' : 'eth0';

  function net(n, from, to) {
    return { [iface(n)]: series(from, to, true, 10, since(n), lastTick(n), (t, step) => {
      const s = samples(t, step, 10).map(x => inst(n, x));
      const avg = f => Math.round(mean(s.map(f))), max = f => Math.round(Math.max(...s.map(f)));
      return {
        rx: avg(v => v.rx), rx_max: max(v => v.rx), tx: avg(v => v.tx), tx_max: max(v => v.tx),
        rx_pps: avg(v => v.rxPps), rx_pps_max: max(v => v.rxPps),
        tx_pps: avg(v => v.txPps), tx_pps_max: max(v => v.txPps),
      };
    }) };
  }

  function diskAt(n, mount, t) {
    const total = (mount === '/' ? n.disk : n.disk * 4) * GB * 0.98;
    // Fills up a little every day and is cleaned up every few weeks.
    const frac = (mount === '/' ? (n.diskUsed || 0.18 + 0.3 * (seed(n.id + 'd') % 100) / 100) : 0.61)
      + 0.012 * ((t / DAY) % 23) / 23 + 0.002 * noise(n.id + mount, t, 3600);
    const used = Math.round(total * frac);
    return { mount, total, used, avail: Math.round(total * 0.95) - used, inode_pct: r3(2 + 9 * frac) };
  }
  const mounts = n => n.mounts || ['/'];

  function disks(n, from, to) {
    const out = {};
    for (const m of mounts(n)) {
      out[m] = series(from, to, false, 60, since(n), lastTick(n), (t, step) => {
        const s = samples(t, step, 60).map(x => diskAt(n, m, x));
        return { total: s[0].total, used: mean(s.map(v => v.used)), used_max: Math.max(...s.map(v => v.used)),
          avail: mean(s.map(v => v.avail)), inode_pct: s[0].inode_pct };
      });
    }
    return out;
  }

  // ---- latency ------------------------------------------------------------

  function baseRTT(a, b) {
    const rad = Math.PI / 180, [la1, lo1] = a.at, [la2, lo2] = b.at;
    const h = Math.sin((la2 - la1) * rad / 2) ** 2 + Math.cos(la1 * rad) * Math.cos(la2 * rad) * Math.sin((lo2 - lo1) * rad / 2) ** 2;
    const km = 12742 * Math.asin(Math.sqrt(h));
    const pair = [a.id, b.id].sort().join('~');
    // Routes are longer than great circles, each pair by its own factor.
    return 1.2 + km / 100 * (1.25 + 0.5 * (seed(pair) % 100) / 100);
  }
  // Ping targets of a node: every other node, plus its tunnel probes.
  const peers = n => NODES.filter(m => m !== n).map(m => m.id).concat((n.extra || []).map(e => e.name));

  // One moment of the link src -> dst: rtt (ms), jitter, loss (0..1).
  function pingInst(src, dstName, t) {
    const key = src.id + '>' + dstName, dst = byId.get(dstName);
    if (!dst) { // a tunnel probe: short and steady
      const rtt = src.extra.find(e => e.name === dstName).rtt;
      return { rtt: rtt + 0.4 * noise(key, t, 60), jitter: 0.08 + 0.1 * noise(key + 'j', t, 300), loss: 0 };
    }
    if (!alive(dst, t)) return { rtt: null, jitter: null, loss: 1 };
    const base = baseRTT(src, dst), evening = diurnal(t);
    let rtt = base * (1 + 0.012 * noise(key, t, 400)) + base * 0.06 * evening * noise(key + 'e', t, 1200);
    let jitter = 0.1 + base * 0.004 + base * 0.02 * evening * noise(key + 'j', t, 300);
    let loss = rnd(key + 'blip', Math.floor(t / 300)) > 0.985 ? 0.02 + 0.06 * rnd(key + 'b2', Math.floor(t / 300)) : 0;
    const pair = [src.id, dstName].sort().join('~');
    if (src.lossyTo === dstName || dst.lossyTo === src.id) {
      // A congested route: bad for a few hours most evenings, and right now.
      const bad = t > T0 - 3 * 3600 || (evening > 0.8 && rnd(pair + 'day', dayIndex(t)) > 0.3);
      if (bad) { loss = 0.12 + 0.3 * noise(pair + 'l', t, 240); rtt += 30 * noise(pair + 'r', t, 180); jitter += 12 * noise(pair + 'lj', t, 90); }
    }
    if (flooded(dst, t) || flooded(src, t)) { // the attacked node's uplink is saturated
      loss = Math.max(loss, 0.28 + 0.4 * noise(pair + 'f', t, 150));
      rtt += 45 * noise(pair + 'fr', t, 60); jitter += 20 * noise(pair + 'fj', t, 60);
    }
    return { rtt, jitter, loss };
  }

  // The link over [from, to): one probe a second. null if src reported nothing.
  function pingAgg(src, dstName, from, to, k) {
    to = Math.min(to, lastTick(src) + 10);
    from = Math.max(from, since(src));
    if (to <= from) return null;
    const sent = to - from;
    const s = samples(from, sent, Math.max(10, Math.floor(sent / k))).map(t => pingInst(src, dstName, t));
    // Whole probes are lost: round up or down by a per-bucket coin, so a 3%
    // loss shows as an occasional lost probe instead of never or always.
    const lost = Math.min(sent, Math.floor(sent * mean(s.map(v => v.loss)) + rnd(src.id + '>' + dstName + 'd', from)));
    const ok = s.filter(v => v.rtt !== null && v.loss < 1);
    const agg = { sent, lost, loss_pct: 100 * lost / sent, min: null, avg: null, max: null, jitter: null };
    if (ok.length && lost < sent) {
      agg.avg = mean(ok.map(v => v.rtt));
      agg.jitter = mean(ok.map(v => v.jitter));
      agg.min = Math.min(...ok.map(v => v.rtt)) - agg.jitter * 0.6;
      // The worst probe of the bucket: far out when the bucket is long.
      agg.max = Math.max(...ok.map(v => v.rtt)) + agg.jitter * (2 + Math.log10(sent));
    }
    return agg;
  }

  function ping(src, dstName, from, to) {
    if (!peers(src).includes(dstName)) return { ...pickTier(from, to, true), ts: [], cols: {} };
    return series(from, to, true, 10, since(src), lastTick(src), (t, step) => pingAgg(src, dstName, t, t + step, 6));
  }
  // Every link of src that has data in the range, by peer name.
  function pings(src, from, to) {
    const out = {};
    for (const dst of peers(src)) {
      const s = ping(src, dst, from, to);
      if (s.ts.length) out[dst] = s;
    }
    return out;
  }

  function matrix(window) {
    const n = now(), links = [];
    for (const src of NODES) for (const dst of peers(src)) {
      const a = pingAgg(src, dst, n - window, n, 8);
      if (a) links.push({ src: src.id, dst, ...round(a) });
    }
    return { window: goDuration(window), nodes: NODES.map(x => x.id), links };
  }
  const round = a => Object.fromEntries(Object.entries(a).map(([k, v]) => [k, typeof v === 'number' ? r3(v) : v]));
  function goDuration(s) {
    const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
    return (h ? h + 'h' : '') + (h || m ? m + 'm' : '') + (s % 60) + 's';
  }

  const AVAIL = { '24h': [1800, 48], '7d': [3 * 3600, 56], '30d': [12 * 3600, 60] };
  const DOWN_LOSS = 20;
  const availCache = new Map();

  function availability(range) {
    const [cell, n] = AVAIL[range];
    const from = (Math.floor(now() / cell) + 1) * cell - n * cell;
    const key = range + ':' + Math.floor(now() / 300);
    if (availCache.has(key)) return availCache.get(key);
    const links = [];
    for (const src of NODES) for (const dst of peers(src)) {
      const c = { periods: [], down: [], sent: [], lost: [], avg: [] };
      const sum = { periods: 0, down: 0, sent: 0, lost: 0, rtt: 0, ok: 0 };
      for (let i = 0; i < n; i++) {
        let periods = 0, down = 0, sent = 0, lost = 0, rtt = 0, ok = 0;
        for (let t = from + i * cell; t < from + (i + 1) * cell; t += 300) {
          const a = t + 300 <= now() ? pingAgg(src, dst, t, t + 300, 1) : null; // whole 5m periods only
          if (!a) continue;
          periods++; sent += a.sent; lost += a.lost;
          if (a.loss_pct >= DOWN_LOSS) down++;
          if (a.avg !== null) { rtt += a.avg * (a.sent - a.lost); ok += a.sent - a.lost; }
        }
        c.periods.push(periods); c.down.push(down); c.sent.push(sent); c.lost.push(lost); c.avg.push(ok ? r3(rtt / ok) : null);
        sum.periods += periods; sum.down += down; sum.sent += sent; sum.lost += lost; sum.rtt += rtt; sum.ok += ok;
      }
      if (!sum.periods) continue;
      links.push({ src: src.id, dst, periods: sum.periods, down: sum.down,
        avail_pct: r3(100 * (sum.periods - sum.down) / sum.periods), sent: sum.sent, lost: sum.lost,
        avg: sum.ok ? r3(sum.rtt / sum.ok) : null, cells: c });
    }
    const out = { range, from, cell, n, down_loss_pct: DOWN_LOSS, links };
    availCache.clear();
    availCache.set(key, out);
    return out;
  }

  // ---- traffic ------------------------------------------------------------

  const resetDay = n => n.nearQuota ? Math.min(28, local(T0 - 27 * DAY).getUTCDate()) : 1;
  // Bytes on one day; today counts up to now.
  function dayTraffic(n, i) {
    if (dayStart(i) < since(n)) return { rx: 0, tx: 0 };
    let gb = n.nearQuota ? n.quota * 0.94 / 27.4 : n.daily * (0.55 + 0.9 * rnd(n.id + 'day', i));
    const end = Math.min(dayStart(i + 1), lastTick(n));
    gb *= Math.max(0, end - dayStart(i)) / DAY;
    const share = n.sym ? 0.5 : 0.72 + 0.1 * rnd(n.id + 'dir', i);
    // The flood is traffic too (assuming it started today).
    const flood = n.ddos && i === dayIndex(T0) ? 1.2e7 * (lastTick(n) - DDOS_AT) : 0;
    return { rx: Math.round(gb * GB * share + flood), tx: Math.round(gb * GB * (1 - share)) };
  }
  // First day index of the period containing day i, and of the next one.
  function periodOf(n, i) {
    const d = local(dayStart(i)), rd = resetDay(n);
    let y = d.getUTCFullYear(), m = d.getUTCMonth();
    if (d.getUTCDate() < rd) m--;
    const idx = (yy, mm) => Math.floor(Date.UTC(yy, mm, rd) / 1000 / DAY);
    return [idx(y, m), idx(y, m + 1)];
  }
  // A period [first, next) in day indexes, as the API returns it.
  function period(n, first, next) {
    let rx = 0, tx = 0;
    for (let i = first; i < next && i <= dayIndex(now()); i++) { const d = dayTraffic(n, i); rx += d.rx; tx += d.tx; }
    return { start: dayStart(first), end: dayStart(next), rx, tx, ifaces: [{ iface: iface(n), rx, tx }] };
  }
  function periods(n, limit) {
    const out = [];
    let [first, next] = periodOf(n, dayIndex(now()));
    while (out.length < limit && dayStart(next) > since(n)) {
      out.push(period(n, first, next));
      [first, next] = periodOf(n, first - 1);
    }
    return out;
  }
  // start: a period's start in unix seconds; the current period without it.
  function daily(n, start) {
    const first = start ? dayIndex(start) : periodOf(n, dayIndex(now()))[0];
    const [pFirst, next] = periodOf(n, first);
    if (pFirst !== first || dayStart(next) <= since(n)) return { period: start, days: [] };
    const days = [];
    for (let i = Math.max(first, dayIndex(since(n))); i < next && i <= dayIndex(now()); i++) days.push({ day: ymd(dayStart(i)), ...dayTraffic(n, i) });
    return { period: dayStart(first), days };
  }
  const MODES = { sum: '收+发', max: '取大', tx: '仅上行', rx: '仅下行' };
  // What counts against the quota, and the quota as the API shows it.
  const usedOf = (n, p) => ({ sum: p.rx + p.tx, max: Math.max(p.rx, p.tx), tx: p.tx, rx: p.rx })[n.mode || 'sum'];
  const quotaOf = (n, cur) => n.quota ? { bytes: n.quota * GB, mode: MODES[n.mode || 'sum'], used: cur ? usedOf(n, cur) : 0 } : null;

  // ---- nodes --------------------------------------------------------------

  const expiry = n => n.expireIn === undefined ? null : ymd(T0 + n.expireIn * DAY);

  function nodeView(n) {
    const t = lastTick(n), v = inst(n, t), boot = T0 - n.upDays * DAY + seed(n.id) % 3600;
    const cur = periods(n, 1)[0];
    const view = { id: n.id, name: n.name, region: n.region, group: n.group, online: alive(n, now()), plan: null, quota: quotaOf(n, cur) };
    if (expiry(n)) {
      view.plan = { expire_at: expiry(n), days: dayIndex(fromYMD(expiry(n))) - dayIndex(now()), price: n.price };
      if (n.renew) view.plan.renew_months = n.renew;
    }
    view.status = {
      max_ts: t, fresh_at: t, clock_skew: seed(n.id) % 3 - 1,
      sys: { hostname: n.host, os: n.os[0], kernel: n.os[1], arch: 'amd64', cores: n.cores, boot_time: boot,
        agent_version: n.agent || '0.2.1' },
      ip: n.ip, ip_since: n.id === 'tw-1' ? T0 - 3 * DAY - 4000 : since(n),
      metrics_ts: t, cpu: r3(v.cpu), steal: r3(v.steal), softirq: r3(v.softirq),
      load1: r3(v.load1), load5: r3(mean([75, 150, 225, 300].map(d => inst(n, t - d).load1))),
      load15: r3(mean([225, 450, 675, 900].map(d => inst(n, t - d).load1))),
      mem_total: v.memTotal, mem_used: Math.round(v.memUsed), swap_total: v.swapTotal, swap_used: Math.round(v.swapUsed),
      tcp: Math.round(v.tcp), udp: Math.round(v.udp), tcp_tw: Math.round(v.tw), threads: Math.round(v.threads),
      net: [{ iface: iface(n), rx: Math.round(v.rx), tx: Math.round(v.tx) }],
      disks: mounts(n).map(m => diskAt(n, m, t - t % 60)),
      traffic: cur,
    };
    return view;
  }

  // ---- alerts -------------------------------------------------------------

  // The rules and reports in effect, worded as the server words them.
  const rule = (name, condition, nodes = '全部', repeat = '不重复', recovery = '是') => ({ name, condition, nodes, repeat, recovery });
  const report = (name, condition) => rule(name, condition, '全部', '', '');
  const RULES = [
    rule('offline', '超过 1m 没有上报'),
    rule('cpu_high', 'CPU > 90.0%，持续 5m'),
    rule('mem_high', '内存 > 90.0%，持续 5m'),
    rule('disk_full', '磁盘 > 90.0%，持续 10m', '全部', '每 6h'),
    rule('link_loss', '丢包 > 20.0%，持续 3m', '香港 1、东京 1'),
    rule('ddos', '入站 >= 50.0 Mbps，且不低于出站的 4 倍，持续 2m', '全部，除 台北'),
    rule('abuse_out', '出站 >= 50.0 Mbps，且不低于入站的 4 倍，持续 5m', '全部，除 台北'),
    report('traffic_quota', '流量达到配额的 80 / 90 / 100%（每周期每档一次）'),
    report('expiry', '到期前 7 / 1 天提醒（每个到期日每档一次）'),
    report('ip_change', '上报来源 IP 变化时通知'),
    report('period', '每个流量周期结束时发送结算'),
    report('weekly', '每周一 10:00 发送流量汇总'),
  ];
  const label = n => `${n.name}（${n.id}）`;
  function fmtBytes(v) {
    if (v >= GB) return (v / GB).toFixed(2) + ' GB';
    if (v >= MB) return (v / MB).toFixed(2) + ' MB';
    return (v / 1024).toFixed(2) + ' KB';
  }
  function fmtDur(s) {
    s = Math.floor(s);
    if (s >= DAY) return `${Math.floor(s / DAY)} 天 ${Math.floor(s % DAY / 3600)} 小时`;
    if (s >= 3600) return `${Math.floor(s / 3600)} 小时 ${Math.floor(s % 3600 / 60)} 分`;
    if (s >= 60) return `${Math.floor(s / 60)} 分 ${s % 60} 秒`;
    return s + ' 秒';
  }
  const mbps = v => (v * 8 / 1e6).toFixed(1) + ' Mbps';

  function weeklyLine(n, t) {
    const [first, next] = periodOf(n, dayIndex(t));
    let rx = 0, tx = 0, week = 0;
    for (let i = first; i < dayIndex(t); i++) { const d = dayTraffic(n, i); rx += d.rx; tx += d.tx; }
    for (let i = dayIndex(t) - 7; i < dayIndex(t); i++) { const d = dayTraffic(n, i); week += d.rx + d.tx; }
    const used = usedOf(n, { rx, tx }), left = next - dayIndex(t), share = used / Math.max(1, rx + tx);
    const end = used + week * share / 7 * left;
    const quota = n.quota * GB, of = v => n.quota ? `${fmtBytes(v)} / ${fmtBytes(quota)}（${(100 * v / quota).toFixed(1)}%）` : fmtBytes(v);
    const d = local(dayStart(next));
    return `${label(n)}：本周期（${ymd(dayStart(first))} 起）${of(used)}，近 7 天 ${fmtBytes(week)}，预计周期末 ${of(end)}`
      + (n.quota && end > quota ? ' ⚠️ 可能超额' : '') + `，${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} 重置`;
  }

  // The alert log, newest first. Built once: its times hang off T0.
  const HISTORY = (() => {
    const h = [], n = id => byId.get(id);
    // Alert messages name their rule; a report's title says what it is.
    const add = (ts, rule, node, target, event, value, title, ...lines) => h.push({ ts, rule, node, target, event, value,
      message: [title + (event === 'notice' || event === 'report' ? '' : ' ' + rule) + (node ? ' · ' + label(n(node)) : ''),
        ...lines, stamp(ts)].join('\n') });
    const at = (daysAgo, hh, mm) => dayStart(dayIndex(T0) - daysAgo) + hh * 3600 + mm * 60;

    // Now: the attack on la-1 and lon-1 going dark.
    const la = n('la-1'), f = inst(la, DDOS_AT + 120);
    add(DDOS_AT + 120, 'ddos', 'la-1', '', 'firing', mbps(f.rx), '🔴 告警',
      `疑似 DDoS：入站 ${mbps(f.rx)}，出站 ${mbps(f.tx)}（${(f.rx / f.tx).toFixed(1)} 倍）（阈值 >= 50.0 Mbps，且不低于出站的 4 倍），已持续 2 分 0 秒`,
      '8 个节点中 7 个到它丢包 ≥ 20%',
      `入站 ${(f.rxPps / 1e4).toFixed(1)} 万包/秒（平均 ${Math.round(f.rx / f.rxPps)} 字节/包），出站 ${Math.round(f.txPps)} 包/秒，软中断 ${f.softirq.toFixed(1)}%`);
    add(OFFLINE_AT + 70, 'offline', 'lon-1', '', 'firing', fmtDur(70), '🔴 告警', '已 1 分 10 秒 没有上报');

    // hk-2 is eating its quota.
    const hk2 = n('hk-2'), q = hk2.quota * GB, start = ymd(dayStart(periodOf(hk2, dayIndex(T0))[0]));
    for (const [daysAgo, pct] of [[3, 80], [0.4, 90]]) {
      add(Math.floor(T0 - daysAgo * DAY), 'traffic_quota', 'hk-2', start, 'notice', pct + '%', '📊 流量',
        `本周期（${start} 起）已用 ${fmtBytes(q * pct / 100 + 3e8)}，达到配额 ${fmtBytes(q)} 的 ${pct}%（计费：${MODES[hk2.mode]}）`);
    }
    // The 7-day reminders: sg-1 expires in 5 days, hk-2 in 4.
    const sg = n('sg-1');
    add(at(2, 9, 0), 'expiry', 'sg-1', expiry(sg), 'notice', '剩 7 天', '⏰ 到期', `将于 ${expiry(sg)} 到期，还剩 7 天，价格 ${sg.price}`);
    add(at(3, 9, 0), 'expiry', 'hk-2', expiry(hk2), 'notice', '剩 7 天', '⏰ 到期',
      `将于 ${expiry(hk2)} 到期，还剩 7 天，价格 ${hk2.price}（自动续费，每 1 个月）`);

    add(T0 - 3 * DAY - 4000, 'ip_change', 'tw-1', '198.51.100.87', 'notice', '', '🔁 IP 变化', '198.51.100.23 → 198.51.100.87');

    // Incidents that came and went.
    const pair = (ts, dur, rule, node, target, fire, value, clear, back) => {
      add(ts, rule, node, target, 'firing', value, '🔴 告警', fire);
      add(ts + dur, rule, node, target, 'recovered', back, '🟢 恢复', clear);
    };
    pair(at(1, 3, 12), 1450, 'link_loss', 'hk-1', 'la-1', 'hk-1 → la-1 丢包 34.2%（阈值 > 20.0%），已持续 3 分 0 秒', '34.2%',
      'hk-1 → la-1 丢包 0.4%，异常持续约 24 分 10 秒', '0.4%');
    pair(at(2, 21, 40), 520, 'cpu_high', 'fra-1', '', 'CPU 97.3%（阈值 > 90.0%），已持续 5 分 0 秒', '97.3%', 'CPU 21.8%，异常持续约 8 分 40 秒', '21.8%');
    pair(at(5, 14, 5), 2140, 'offline', 'syd-1', '', '已 1 分 2 秒 没有上报', fmtDur(62), '已恢复上报，离线约 35 分 40 秒', fmtDur(0));
    pair(at(8, 6, 30), 5400, 'disk_full', 'tw-1', '/data', '磁盘 /data 91.4%（阈值 > 90.0%），已持续 10 分 0 秒', '91.4%',
      '磁盘 /data 62.0%，异常持续约 1 小时 30 分', '62.0%');
    pair(at(11, 23, 18), 700, 'mem_high', 'hk-2', '', '内存 93.6%（阈值 > 90.0%），已持续 5 分 0 秒', '93.6%', '内存 71.2%，异常持续约 11 分 40 秒', '71.2%');
    pair(at(16, 2, 47), 960, 'ddos', 'tyo-1', '', '疑似 DDoS：入站 212.4 Mbps，出站 6.0 Mbps（35.4 倍）（阈值 >= 50.0 Mbps，且不低于出站的 4 倍），已持续 2 分 0 秒',
      '212.4 Mbps', '入站 4.1 Mbps，出站 3.8 Mbps，异常持续约 16 分 0 秒', '4.1 Mbps');
    pair(at(22, 17, 2), 380, 'offline', 'sg-1', '', '已 1 分 5 秒 没有上报', fmtDur(65), '已恢复上报，离线约 6 分 20 秒', fmtDur(0));

    // Every Monday 10:00, and the settlement when a period ends.
    for (let i = dayIndex(T0), weeks = 0; weeks < 5; i--) {
      if (local(dayStart(i)).getUTCDay() !== 1) continue;
      weeks++;
      const ts = dayStart(i) + 10 * 3600;
      if (ts > T0) continue;
      h.push({ ts, rule: 'weekly', node: '', target: stamp(ts).slice(0, 16), event: 'report', value: '',
        message: ['📊 每周流量', ...NODES.filter(x => since(x) < ts - 7 * DAY).map(x => weeklyLine(x, ts)),
          '即将到期：新加坡（sg-1） ' + expiry(sg) + `（还剩 ${dayIndex(fromYMD(expiry(sg))) - i} 天）`, stamp(ts)].join('\n') });
    }
    for (const x of NODES) {
      const [first] = periodOf(x, dayIndex(T0)), [prev] = periodOf(x, first - 1);
      if (dayStart(prev) < since(x) || dayStart(first) < T0 - 31 * DAY) continue;
      const p = period(x, prev, first), total = p.rx + p.tx, used = usedOf(x, p);
      let peak = { v: 0 };
      for (let i = prev; i < first; i++) { const d = dayTraffic(x, i); if (d.rx + d.tx > peak.v) peak = { v: d.rx + d.tx, day: ymd(dayStart(i)) }; }
      add(dayStart(first) + 20, 'period', x.id, ymd(p.start), 'report', x.quota ? (100 * used / x.quota / GB).toFixed(1) + '%' : '', '📊 流量结算',
        `${ymd(p.start)} 至 ${ymd(p.end)}（${first - prev} 天）：下行 ${fmtBytes(p.rx)}，上行 ${fmtBytes(p.tx)}，合计 ${fmtBytes(total)}`,
        ...(x.quota ? [`配额 ${fmtBytes(x.quota * GB)}（计费：${MODES[x.mode]}），用了 ${(100 * used / x.quota / GB).toFixed(1)}%`] : []),
        `日均 ${fmtBytes(total / (first - prev))}，最多的一天 ${peak.day}（${fmtBytes(peak.v)}）`);
    }
    return h.filter(e => e.ts <= T0).sort((a, b) => b.ts - a.ts);
  })();

  function alerts(q) {
    const to = q.has('to') ? Number(q.get('to')) : now();
    const from = q.has('from') ? Number(q.get('from')) : now() - 7 * DAY;
    const inRange = HISTORY.filter(e => e.ts >= from && e.ts <= to);
    const events = q.get('event') ? q.get('event').split(',') : null;
    const history = inRange.filter(e => (!q.get('node') || e.node === q.get('node')) && (!q.get('rule') || e.rule === q.get('rule'))
      && (!events || events.includes(e.event)));
    const uniq = f => [...new Set(inRange.map(f).filter(Boolean))].sort();
    const la = byId.get('la-1');
    return {
      active: [
        { rule: 'ddos', node: 'la-1', target: '', firing: true, since: DDOS_AT, value: mbps(inst(la, lastTick(la)).rx) },
        { rule: 'offline', node: 'lon-1', target: '', firing: true, since: OFFLINE_AT, value: fmtDur(now() - OFFLINE_AT) },
        { rule: 'link_loss', node: 'hk-1', target: 'la-1', firing: false, since: now() - 95, value: '41.5%' },
      ],
      history: history.slice(0, 500), truncated: history.length > 500,
      facets: { nodes: uniq(e => e.node), rules: uniq(e => e.rule) },
      rules: RULES, channels: ['telegram', 'bark'],
    };
  }

  const stats = () => ({
    ingest: { accepted: Math.floor((now() - 1.75e9) * 2.7), auth_failed: 37, decode_failed: 0, duplicate: 1204 + Math.floor((now() - T0) / 60),
      fields_dropped: 0, malformed: 5519, store_failed: 0, ts_out_of_range: 3, unknown_node: 12, wrong_type: 0 },
    db_bytes: 187 * MB + (now() % 3600) * 600, server_time: now(), timezone: 'Asia/Shanghai', version: 'demo', interval: 10,
  });

  // ---- routing ------------------------------------------------------------

  // handle answers one GET like the real API: [status, body].
  function handle(pathAndQuery) {
    const url = new URL(pathAndQuery, 'http://demo.invalid');
    const q = url.searchParams, seg = url.pathname.split('/').filter(Boolean).map(decodeURIComponent);
    const range = () => {
      const to = q.has('to') ? Number(q.get('to')) : now();
      return [q.has('from') ? Number(q.get('from')) : to - 3600, to];
    };
    const node = byId.get(seg[2]);
    if (seg[0] !== 'api') return [404, 'not found'];
    switch (seg[1]) {
      case 'nodes':
        if (seg.length === 2) return [200, NODES.map(nodeView)];
        if (!node) return [404, 'not found'];
        if (seg[3] === 'ping' && seg.length === 5) return [200, ping(node, seg[4], ...range())];
        if (seg[3] === 'traffic' && seg[4] === 'daily' && seg.length === 5) {
          return q.has('period') && !/^\d+$/.test(q.get('period')) ? [400, 'period: want its start in unix seconds']
            : [200, daily(node, Number(q.get('period')))];
        }
        if (seg.length !== 4) return [404, 'not found'];
        if (seg[3] === 'metrics') return [200, metrics(node, ...range())];
        if (seg[3] === 'net') return [200, net(node, ...range())];
        if (seg[3] === 'disks') return [200, disks(node, ...range())];
        if (seg[3] === 'ping') return [200, pings(node, ...range())];
        break;
      case 'ping':
        if (seg[2] === 'matrix') {
          const m = /^(\d+)([smh])$/.exec(q.get('window') || '5m');
          if (!m) return [400, 'window: want a duration up to 1h'];
          return [200, matrix(Number(m[1]) * { s: 1, m: 60, h: 3600 }[m[2]])];
        }
        if (seg[2] === 'availability') {
          const r = q.get('range') || '24h';
          return AVAIL[r] ? [200, availability(r)] : [400, 'range: want 24h, 7d or 30d'];
        }
        break;
      case 'traffic':
        if (seg.length === 2) {
          const limit = Number(q.get('periods') || 24);
          return [200, NODES.map(n => {
            const ps = periods(n, limit).map(p => ({ ...p, billable: usedOf(n, p) }));
            return { id: n.id, name: n.name, quota: quotaOf(n, ps[0]), periods: ps };
          })];
        }
        break;
      case 'sparks': return [200, sparks()];
      case 'stats': return [200, stats()];
      case 'alerts': return [200, alerts(q)];
    }
    return [404, 'not found'];
  }

  globalThis.vpsProbeDemo = { handle };
  if (typeof window === 'undefined') return; // loaded by the shape check, not a browser

  const realFetch = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (url.origin !== location.origin || !url.pathname.startsWith('/api/')) return realFetch(input, init);
    const [status, body] = handle(url.pathname + url.search);
    return new Response(status === 200 ? JSON.stringify(body) : body,
      { status, headers: { 'Content-Type': status === 200 ? 'application/json; charset=utf-8' : 'text/plain; charset=utf-8' } });
  };

  // The banner. Styled through the CSSOM: the page's CSP allows no inline styles.
  document.addEventListener('DOMContentLoaded', () => {
    const bar = document.createElement('div');
    bar.textContent = '演示站：所有节点和数据都是虚构的，在浏览器里实时生成。';
    const link = document.createElement('a');
    link.href = 'https://github.com/shakespark/vps-probe';
    link.textContent = '项目主页';
    link.rel = 'noopener';
    Object.assign(link.style, { color: 'inherit', marginLeft: '0.5em', textDecoration: 'underline' });
    bar.append(link);
    Object.assign(bar.style, { background: '#1f3a5f', color: '#fff', font: '13px/1.4 system-ui, sans-serif',
      padding: '6px 12px', textAlign: 'center' });
    document.body.prepend(bar);
  });
})();
