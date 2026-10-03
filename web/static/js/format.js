// How values are written. Times are shown in the browser's timezone, except
// calendar dates the server decided (billing periods, expiry), which are
// shown in the server's.

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
export function fmtBytes(n) {
  if (n == null || !isFinite(n)) return '—';
  let v = Math.abs(n), i = 0;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  const s = i === 0 ? v.toFixed(0) : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2);
  return (n < 0 ? '-' : '') + s + ' ' + UNITS[i];
}
export const fmtRate = n => n == null ? '—' : fmtBytes(n) + '/s';
export const fmtPct = (v, d = 1) => v == null || !isFinite(v) ? '—' : v.toFixed(d) + '%';
export const fmtMs = v => v == null ? '—' : (v < 10 ? v.toFixed(2) : v < 100 ? v.toFixed(1) : v.toFixed(0)) + ' ms';
export const fmtPPS = v => v == null ? '—' : v >= 1e4 ? (v / 1e4).toFixed(1) + ' 万包/秒' : Math.round(v) + ' 包/秒';
export const fmtCount = v => v == null ? '—' : Math.round(v).toString();
// Rounded down so that 99.996% doesn't show as 100%.
export const fmtAvail = v => v == null ? '—' : v >= 100 ? '100%' : (Math.floor(v * 100) / 100).toFixed(2) + '%';
export const pctOf = (used, total) => total ? 100 * used / total : null;

export function fmtDur(sec) {
  if (sec == null || sec < 0) return '—';
  const d = Math.floor(sec / 86400), hr = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d > 0) return `${d} 天 ${hr} 小时`;
  if (hr > 0) return `${hr} 小时 ${m} 分`;
  return `${m} 分`;
}
export const nowSec = () => Math.floor(Date.now() / 1000);
export const fmtTime = unix => new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false });
export const fmtClock = date => date.toLocaleTimeString('zh-CN', { hour12: false });

// fmtDay writes a period boundary as a date in the server's timezone, with
// the time when it is not midnight. The timezone comes from the server (see
// setTimezone); until then, and if the browser doesn't know it, the
// browser's own is used.
const DAY_PARTS = { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };
let dayParts = new Intl.DateTimeFormat('en-CA', DAY_PARTS);
export function setTimezone(timeZone) {
  try {
    dayParts = new Intl.DateTimeFormat('en-CA', { ...DAY_PARTS, timeZone });
  } catch { /* an unknown zone name: keep the browser's */ }
}
export function fmtDay(unix) {
  const p = Object.fromEntries(dayParts.formatToParts(new Date(unix * 1000)).map(x => [x.type, x.value]));
  const day = `${p.year}-${p.month}-${p.day}`;
  return p.hour === '00' && p.minute === '00' ? day : `${day} ${p.hour}:${p.minute}`;
}

// A plan's days left, counted by the server in its timezone: 0 = today.
export function fmtExpiry(days) {
  if (days > 0) return `剩 ${days} 天`;
  return days === 0 ? '今天到期' : `已过期 ${-days} 天`;
}
export const expiryClass = days => days <= 0 ? 'bad' : days <= 7 ? 'warn' : null;

// staleAgents returns a function telling whether a node's agent is older
// than the newest one among nodes, and that newest version. Versions that
// are not like "0.2.1" (e.g. "dev") are not compared.
export function staleAgents(nodes) {
  const parse = v => { const m = /^v?(\d+)\.(\d+)\.(\d+)/.exec(v || ''); return m ? m.slice(1).map(Number) : null; };
  const cmp = (a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2];
  const version = n => n.status && n.status.sys && n.status.sys.agent_version;
  let newest = null;
  for (const n of nodes) {
    const v = parse(version(n));
    if (v && (!newest || cmp(v, newest) > 0)) newest = v;
  }
  const stale = n => { const v = parse(version(n)); return v && newest && cmp(v, newest) < 0 ? version(n) : null; };
  return { stale, newest: newest && newest.join('.') };
}
