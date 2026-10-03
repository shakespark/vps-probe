// The read-only API.

export async function api(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`${r.status} ${(await r.text()).trim()}`);
  return r.json();
}

export const enc = encodeURIComponent;
export const nodeURL = (id, what, query = '') => `/api/nodes/${enc(id)}/${what}${query && '?' + query}`;
