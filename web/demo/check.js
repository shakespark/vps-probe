// Holds demo.js to the structure of the real API (docs/DESIGN.md §14.3):
// for every endpoint in api-shape.json, the demo's answer must have the same
// fields and types. Run with `make demo-check`; needs only node.
'use strict';
const fs = require('fs');
const path = require('path');

require(path.join(__dirname, 'demo.js')); // defines globalThis.vpsProbeDemo
const endpoints = JSON.parse(fs.readFileSync(path.join(__dirname, 'api-shape.json'), 'utf8'));

// The same reduction as shapeOf in internal/server/api/shape_test.go.
function shapeOf(v, at, dynamic) {
  if (v === null) return 'null';
  if (typeof v === 'boolean') return 'bool';
  if (typeof v === 'number') return 'number';
  if (typeof v === 'string') return 'string';
  if (Array.isArray(v)) return { '[]': v.reduce((el, x) => merge(el, shapeOf(x, at, dynamic)), 'null') };
  if (dynamic.includes(at)) return { '{*}': Object.values(v).reduce((el, x) => merge(el, shapeOf(x, at + '.*', dynamic)), 'null') };
  const fields = {};
  for (const [k, x] of Object.entries(v)) fields[k] = shapeOf(x, at ? at + '.' + k : k, dynamic);
  return { '{}': fields };
}

function merge(a, b) {
  if (a === 'null') return b;
  if (b === 'null') return a;
  if (typeof a === 'string' || typeof b === 'string') return a === b ? a : 'any';
  for (const kind of ['[]', '{*}']) {
    if (kind in a) return kind in b ? { [kind]: merge(a[kind], b[kind]) } : 'any';
  }
  if (!('{}' in a) || !('{}' in b)) return 'any';
  const out = { ...a['{}'] };
  for (const [k, y] of Object.entries(b['{}'])) out[k] = k in out ? merge(out[k], y) : y;
  return { '{}': out };
}

// diff lists where the demo's shape departs from the API's. "null" on either
// side matches anything: the value is absent there, not wrong.
function diff(want, got, at, out) {
  if (want === 'null' || got === 'null') return;
  if (typeof want === 'string' || typeof got === 'string') {
    if (want !== got) out.push(`${at}: API has ${JSON.stringify(want)}, demo has ${JSON.stringify(got)}`);
    return;
  }
  for (const kind of ['[]', '{*}']) {
    if (kind in want || kind in got) {
      if (kind in want && kind in got) diff(want[kind], got[kind], at + kind, out);
      else out.push(`${at}: API and demo disagree on the container type`);
      return;
    }
  }
  const w = want['{}'], g = got['{}'];
  for (const k of Object.keys(w)) {
    if (k in g) diff(w[k], g[k], at + '.' + k, out);
    else out.push(`${at}.${k}: missing in the demo`);
  }
  for (const k of Object.keys(g)) if (!(k in w)) out.push(`${at}.${k}: not in the API`);
}

let failed = 0;
for (const e of endpoints) {
  const [status, body] = globalThis.vpsProbeDemo.handle(e.path);
  const out = [];
  if (status !== 200) out.push(`${e.path}: demo answered ${status}`);
  // Through JSON, as the browser sees it: undefined fields drop out.
  else diff(e.shape, shapeOf(JSON.parse(JSON.stringify(body)), '', e.dynamic || []), e.name, out);
  for (const line of out) console.error(line);
  failed += out.length;
}
if (failed) {
  console.error(`demo-check: ${failed} difference(s) between web/demo/demo.js and the API (web/demo/api-shape.json)`);
  process.exit(1);
}
console.log(`demo-check: demo.js matches the API on ${endpoints.length} endpoints`);
