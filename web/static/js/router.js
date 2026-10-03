// A small hash router. The address holds everything that decides what a page
// shows: #/path?key=value. A page can therefore be reloaded, bookmarked or
// sent to someone and looks the same.
//
// A page is an object: el (its DOM), refresh() (async: load and render,
// called again every interval), and optionally interval() in ms, destroy()
// and nav (which top-level tab it belongs to).

import { h } from './dom.js';
import { fmtClock } from './format.js';

const $app = document.getElementById('app');
const $status = document.getElementById('status');

const routes = [];
// route registers make(match, query) for paths matching re.
export function route(re, make) { routes.push([re, make]); }

function parse() {
  const [path, qs] = (location.hash.replace(/^#/, '') || '/').split('?');
  return { path, params: new URLSearchParams(qs || '') };
}

// query reads the address's parameters; defaults says which exist and what
// they are when absent.
function query(params, defaults) {
  return Object.fromEntries(Object.entries(defaults).map(([k, d]) => [k, params.get(k) ?? d]));
}

// go changes parameters of the current page (a value that is empty or the
// default leaves the address) and shows it again.
export function go(changes, defaults = {}) {
  const { path, params } = parse();
  for (const [k, v] of Object.entries(changes)) {
    if (v == null || v === '' || v === defaults[k]) params.delete(k);
    else params.set(k, v);
  }
  const qs = params.toString();
  const next = '#' + path + (qs ? '?' + qs : '');
  if (next === location.hash) show(); // nothing in the address changed
  else location.hash = next;
}

let page = null;
let generation = 0;
let timer = 0;

// show builds the page the address names and keeps it refreshed.
export function show() {
  if (page && page.destroy) page.destroy();
  clearTimeout(timer);
  const { path, params } = parse();
  page = null;
  for (const [re, make] of routes) {
    const m = path.match(re);
    if (m) { page = make(m.slice(1).map(decode), defaults => query(params, defaults)); break; }
  }
  page ||= { nav: '', el: h('div', { class: 'empty', text: '页面不存在' }) };
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('active', a.dataset.route === page.nav);
  }
  $app.replaceChildren(page.el);
  const mine = ++generation, shown = page;
  const loop = async () => {
    if (!shown.refresh) return;
    try {
      await shown.refresh();
      if (mine !== generation) return;
      $status.textContent = '更新于 ' + fmtClock(new Date());
      $status.classList.remove('error');
    } catch (e) {
      if (mine !== generation) return;
      $status.textContent = '更新失败：' + e.message;
      $status.classList.add('error');
    }
    if (mine === generation) timer = setTimeout(loop, shown.interval ? shown.interval() : 10000);
  };
  loop();
}

function decode(s) {
  try { return decodeURIComponent(s); } catch { return s; }
}
