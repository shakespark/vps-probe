// DOM helpers. Strings reported by agents (hostnames, interfaces, mounts,
// peer names) are untrusted: they only ever reach the DOM as text through
// h(), and nothing here builds HTML from strings.

export function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'href' || k === 'title' || k.startsWith('data-')) el.setAttribute(k, v);
    else throw new Error('h: unsupported prop ' + k);
  }
  for (const c of kids.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

// cls joins class names, skipping the empty ones.
export const cls = (...names) => names.filter(Boolean).join(' ');

// A label and its value on one line.
export const kv = (k, v, valueClass) => h('div', { class: 'kv' },
  h('span', { class: 'k', text: k }), h('span', { class: cls('num', valueClass), text: v }));

export const table = (cols, rows, className = 'plain') => h('table', { class: className },
  h('thead', null, h('tr', null, cols.map(c => h('th', { text: c })))), h('tbody', null, rows));

export const empty = text => h('div', { class: 'empty', text });

// A progress bar; width is set through CSSOM, which CSP allows.
export function bar(pct) {
  const fill = h('span');
  const p = Math.max(0, Math.min(100, pct || 0));
  fill.style.width = p + '%';
  return h('div', { class: cls('bar', p >= 90 ? 'bad' : p >= 80 ? 'warn' : null) }, fill);
}

// A sparkline: an SVG line per series over the same x range, each scaled to
// max. A gap in the data breaks the line. series: [values, class] pairs.
const SVG_NS = 'http://www.w3.org/2000/svg';
export function sparkline(series, max, title) {
  const W = 120, H = 30;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', 'spark');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('preserveAspectRatio', 'none');
  const tip = document.createElementNS(SVG_NS, 'title');
  tip.textContent = title;
  svg.append(tip);
  for (const [vals, className] of series) {
    let d = '', pen = false;
    vals.forEach((v, i) => {
      if (v == null) { pen = false; return; }
      const x = i * W / (vals.length - 1), y = H - 1.5 - Math.min(1, v / (max || 1)) * (H - 3);
      d += (pen ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
      pen = true;
    });
    const path = document.createElementNS(SVG_NS, 'path');
    path.setAttribute('d', d);
    path.setAttribute('class', className);
    svg.append(path);
  }
  return svg;
}

// A <select> over [value, label] pairs. fill() only rebuilds the options when
// they change, so the periodic refresh doesn't close an open menu.
export function picker(onPick) {
  const el = h('select', { onchange: () => onPick(el.value) });
  let key = '';
  el.fill = (options, current) => {
    const k = JSON.stringify([options, current]);
    if (k === key) return;
    key = k;
    el.replaceChildren(...options.map(([value, label]) => {
      const o = h('option', { text: label });
      o.value = value;
      return o;
    }));
    el.value = current;
  };
  return el;
}

// A row of buttons, one of them active.
export function seg(options, current, onPick) {
  return h('div', { class: 'seg' }, options.map(([value, label]) =>
    h('button', { class: value === current ? 'active' : null, text: label, onclick: () => onPick(value) })));
}
