// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// supercargo browser UI (#30): the approved mockup, on the node's API.
//
// Everything a partner wrote reaches the page as text (textContent, never
// HTML). The node decides what this persona sees: every read is already
// cut to its roles' sides when it arrives here. Nothing in this file signs,
// sends, or decides anything a partner sees.
'use strict';

const API = 'api/';
const state = {
  me: null, activeSide: null, msgFilter: 'all', actFilter: 'all',
  messages: [], lastSeq: 0, entries: [], open: new Set(), events: null, loading: false,
};

// ── helpers ──────────────────────────────────────────────────────────
const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};

async function api(path, opts = {}) {
  const res = await fetch(API + path, { credentials: 'same-origin', ...opts,
    headers: { ...(opts.body ? { 'Content-Type': 'application/json' } : {}), ...(opts.headers || {}) } });
  if (res.status === 401) { showLogin(); throw new Error('signed out'); }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

const clock = (iso) => {
  const d = new Date(iso);
  if (isNaN(d)) return ['', ''];
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return [`${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`, `.${p(d.getMilliseconds(), 3)}`];
};
const stamp = (iso) => { const [hms, ms] = clock(iso); const t = el('span', 'ts', hms); t.appendChild(el('i', null, ms)); return t; };
const sideLabel = (side) => side === 'operator' ? 'as carrier' : 'as shipper';

// ── theme ────────────────────────────────────────────────────────────
function setTheme(name) {
  document.body.dataset.theme = name;
  try { localStorage.setItem('ulp-theme', name); } catch { /* private mode */ }
}
setTheme((() => {
  try { const t = localStorage.getItem('ulp-theme'); if (t) return t; } catch { /* private mode */ }
  return matchMedia('(prefers-color-scheme: light)').matches ? 'grayscale-light' : 'grayscale-dark';
})());

// ── JSON, as an editor shows it ──────────────────────────────────────
function jsonNode(v, indent, last, foldFrom = Infinity) {
  const pad = '  '.repeat(indent), comma = last ? '' : ',';
  const frag = document.createDocumentFragment();
  const tok = (cls, t) => frag.appendChild(el('span', cls, t));
  if (v === null || typeof v === 'boolean') { tok('j-lit', String(v)); tok('j-p', comma); return frag; }
  if (typeof v === 'number') { tok('j-num', String(v)); tok('j-p', comma); return frag; }
  if (typeof v === 'string') { tok('j-str', JSON.stringify(v)); tok('j-p', comma); return frag; }
  const arr = Array.isArray(v), entries = arr ? v.map((x, i) => [i, x]) : Object.entries(v);
  const wrap = el('span');
  const short = arr && entries.every(([, x]) => typeof x !== 'object' || x === null);
  const fold = el('span', 'fold', '▾');
  fold.onclick = (e) => { e.stopPropagation(); wrap.classList.toggle('folded'); fold.textContent = wrap.classList.contains('folded') ? '▸' : '▾'; };
  if (entries.length && !short) {
    wrap.appendChild(fold);
    if (indent >= foldFrom) { wrap.classList.add('folded'); fold.textContent = '▸'; }
  }
  wrap.appendChild(el('span', 'j-p', arr ? '[' : '{'));
  if (short || !entries.length) {
    entries.forEach(([, x], i) => { if (i) wrap.appendChild(el('span', 'j-p', ' ')); wrap.appendChild(jsonNode(x, 0, i === entries.length - 1)); });
    wrap.appendChild(el('span', 'j-p', (arr ? ']' : '}') + comma));
    frag.appendChild(wrap);
    return frag;
  }
  wrap.appendChild(el('span', 'ellipsis', ' … ' + (arr ? ']' : '}') + comma));
  const kids = el('span', 'kids');
  entries.forEach(([k, x], i) => {
    kids.appendChild(document.createTextNode('\n' + pad + '  '));
    if (!arr) { kids.appendChild(el('span', 'j-key', JSON.stringify(k))); kids.appendChild(el('span', 'j-p', ': ')); }
    kids.appendChild(jsonNode(x, indent + 1, i === entries.length - 1, foldFrom));
  });
  wrap.appendChild(kids);
  const close = el('span', 'close-line');
  close.appendChild(document.createTextNode('\n' + pad));
  close.appendChild(el('span', 'j-p', (arr ? ']' : '}') + comma));
  wrap.appendChild(close);
  frag.appendChild(wrap);
  return frag;
}
const jsonBlock = (v, foldFrom = Infinity) => { const d = el('div', 'json'); d.appendChild(jsonNode(v, 0, true, foldFrom)); return d; };

// ── messages ─────────────────────────────────────────────────────────
const family = (intent = '') => intent.startsWith('ulp.ocean.booking') ? 'booking' : intent === 'ulp.tnt.alert' ? 'alert'
  : intent.startsWith('ulp.tnt.inquiry') ? 'inquiry' : intent === 'ulp.error' || !intent.startsWith('ulp.') ? 'error' : 'tnt';
const FAMILY_NAMES = { booking: 'booking', alert: 'alert', inquiry: 'inquiry', tnt: 'track & trace', error: 'error' };

// verdict: [severity, text] from what the node decided.
function verdict(m) {
  if (m.dir === 'out') return m.outcome === 'failed' ? ['bad', '✗ NOT DELIVERED'] : ['ok', '✓ SIGNED'];
  if (m.verified) {
    if (m.outcome === 'accepted' || m.outcome === 'response') return ['ok', '✓ VERIFIED'];
    return ['warn', `✓ VERIFIED · REFUSED ${m.code || ''}`.trim()];
  }
  const names = { bad_signature: 'BAD SIGNATURE', unknown_key: 'UNKNOWN KEY', forbidden: 'REFUSED' };
  return ['bad', '✗ ' + (m.outcome === 'malformed' ? 'MALFORMED' : names[m.code] || ('REFUSED ' + (m.code || '')).trim())];
}

// summary: one line from the body (or the node's reason).
function summary(m) {
  const b = m.body || {};
  const first = (b.containers || [])[0] || {};
  switch (m.intent) {
    case 'ulp.ocean.booking.request':
      return `${first.count ?? '?'} × ${first.equipment_size_type ?? '?'}, ${b.port_of_loading} → ${b.port_of_discharge}, from ${b.earliest_departure}`;
    case 'ulp.ocean.booking.confirmation': {
      const e = b.ebl || {};
      return [b.status, b.booking_reference, e.transport_document_reference, e.vessel_name && `${e.vessel_name} ${e.voyage_number}`].filter(Boolean).join(' · ');
    }
    case 'ulp.tnt.event':
      return `${b.event_type} ${b.event_type_code} ${b.event_classifier_code} · ${b.un_location_code} · ${b.event_date_time}`;
    case 'ulp.tnt.alert':
      return `${b.kind}: ETA ${(b.previous_eta || '').slice(0, 10)} → ${(b.new_eta || '').slice(0, 10)} · ${b.summary}`;
    case 'ulp.tnt.inquiry': return b.question || '';
    case 'ulp.tnt.inquiry.response': return b.answer || '';
    case 'ulp.ocean.tnt.request': return `status of ${b.transport_document_reference || ''}`;
    case 'ulp.ocean.tnt.response': return `${b.dcsa_status || ''} · ${(b.events || []).length} events`;
    case 'ulp.error': return `${b.code}: ${b.detail}`;
    default: return m.reason || '';
  }
}

function messageRow(m) {
  const [sev, vtext] = verdict(m);
  const alertSev = m.intent === 'ulp.tnt.alert' && m.body ? m.body.severity : null;
  const row = el('div', 'msg ' + (alertSev === 'high' && sev === 'ok' ? 'warn' : sev));
  if (state.open.has(m.seq)) row.classList.add('open');
  const line = el('div', 'line');
  line.appendChild(stamp(m.time));
  line.appendChild(el('span', 'arrow', m.dir === 'out' ? '→' : '←'));
  const chipWrap = el('span');
  const model = m.intent === 'ulp.tnt.inquiry.response';
  const chip = el('span', 'chip fam-' + family(m.intent) + (model ? ' model' : ''), m.intent ? m.intent.replace(/^ulp\./, '') : '(unreadable)');
  if (alertSev) chip.appendChild(el('span', 'sev sev-' + alertSev, '· ' + String(alertSev).toUpperCase()));
  chipWrap.appendChild(chip);
  line.appendChild(chipWrap);
  const peer = m.dir === 'out' ? m.receiver : m.sender;
  const p = el('span', 'peer', peer || '—');
  if (m.via) p.appendChild(el('span', 'via', m.via));
  line.appendChild(p);
  const v = el('span', 'verdict ' + sev, vtext);
  if (sev !== 'bad' && m.kid) v.appendChild(el('span', 'kid', ` (${m.kid})`));
  line.appendChild(v);
  row.appendChild(line);
  row.appendChild(el('div', 'summary', summary(m) + (model ? '   ✦ written by a model' : '')));

  const detail = el('div', 'detail');
  detail.onclick = (e) => e.stopPropagation();
  const raw = el('div', 'raw', m.wire + (m.truncated ? '\n… (cut)' : ''));
  if (!m.verified && m.dir === 'in') {
    const box = el('div', 'unverified');
    box.appendChild(el('div', 'tag', 'UNVERIFIED · shown as text, nothing here is proven'));
    if (m.reason) box.appendChild(el('div', 'summary', m.reason));
    box.appendChild(raw);
    detail.appendChild(box);
  } else {
    const bar = el('div', 'detail-bar');
    const views = { payload: m.body ? jsonBlock(m.body) : el('div', 'raw', '(no body)'), header: m.header ? jsonBlock(m.header) : el('div', 'raw', '(no header)'), 'raw JWS': raw };
    let shown = views.payload;
    const buttons = Object.keys(views).map((k) => {
      const b = el('button', k === 'payload' ? 'on' : null, k);
      b.onclick = () => { buttons.forEach((x) => x.classList.remove('on')); b.classList.add('on'); detail.replaceChild(views[k], shown); shown = views[k]; };
      return b;
    });
    bar.append(...buttons);
    if (m.reason) bar.appendChild(el('span', 'dim', m.reason));
    detail.append(bar, shown);
  }
  row.appendChild(detail);
  row.onclick = () => { row.classList.toggle('open'); row.classList.contains('open') ? state.open.add(m.seq) : state.open.delete(m.seq); };
  return row;
}

// Conversations: by correlationId, except that a booking's one-way pushes
// (its events and alerts, each its own conversation on the wire) share one
// group. What was refused before verification has no proven conversation,
// so it gets a group of its own.
const PUSHES = ['ulp.tnt.event', 'ulp.tnt.alert'];
function groupKey(m) {
  if (!m.verified && m.dir === 'in') return 'unverified';
  if (PUSHES.includes(m.intent) && m.header && m.header.bookingRef) return 'pushes ' + (m.dir === 'out' ? m.receiver : m.sender) + ' ' + m.header.bookingRef;
  return m.conversation || 'seq-' + m.seq;
}
function conversations() {
  const groups = new Map();
  for (const m of state.messages) {
    if (state.msgFilter === 'refused' && verdict(m)[0] === 'ok') continue;
    const key = groupKey(m);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(m);
  }
  return groups;
}

function convoTitle(key, msgs) {
  if (key === 'unverified') return 'refused before verification';
  const m = msgs[0];
  const ref = (m.header && m.header.bookingRef) || (m.body && m.body.booking_reference) || '';
  const peer = m.dir === 'out' ? m.receiver : m.sender;
  const name = key.startsWith('pushes ') ? 'track & trace' : FAMILY_NAMES[family(m.intent)];
  const parts = [name, ref, (m.dir === 'out' ? 'with ' : 'from ') + peer].filter(Boolean);
  if (state.me && state.me.sides.length > 1 && m.side) parts.push(sideLabel(m.side));
  return parts.join(' · ');
}

function renderMessages() {
  const box = $('messages');
  // Follow new messages, as a chat does, only if already at the bottom.
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
  const keepScroll = box.scrollTop;
  box.replaceChildren();
  const groups = conversations();
  if (!groups.size) { box.appendChild(el('div', 'empty', state.msgFilter === 'refused' ? 'Nothing refused.' : 'No messages yet.')); return; }
  for (const [key, msgs] of groups) {
    box.appendChild(el('div', 'convo', convoTitle(key, msgs)));
    msgs.forEach((m) => box.appendChild(messageRow(m)));
  }
  box.scrollTop = atBottom ? box.scrollHeight : keepScroll;
}

async function loadMessages() {
  if (state.loading) return;
  state.loading = true;
  try {
    const r = await api('messages?' + new URLSearchParams(state.lastSeq ? { since: state.lastSeq } : { limit: 300 }));
    if (r.messages.length) {
      state.messages.push(...r.messages);
      state.messages = state.messages.slice(-500);
      state.lastSeq = r.messages.at(-1).seq;
      renderMessages();
    } else if (!state.messages.length) {
      renderMessages();
    }
  } catch (e) { setStatus('bad', e.message); }
  finally { state.loading = false; }
}

// ── activity ─────────────────────────────────────────────────────────
const KIND_TIPS = {
  envelope: 'A message in or out, and what the node decided about it. The message itself is in the Messages panel.',
  record: 'A track & trace event or alert: recorded (received for your shipment) or published (sent to a customer).',
  model: 'A call to an AI model: model, skills, tokens, latency, and whether its answer passed validation.',
  summary: "What an alert means for you, written by the assistant (or the template, if the model's answer failed validation).",
  voyage: "A simulated voyage starting (the demo's shipment simulator).",
  push: "A push to a customer the node gave up on, or couldn't publish.",
};
const SIDE_TIPS = {
  customer: 'Your shipper or receiver role: shipments other carriers carry for you.',
  operator: 'Your carrier role: bookings you confirmed, events you published.',
  '': 'The whole node: a message no role can claim.',
};

function entryView(e) {
  const box = el('div', 'entry');
  const head = el('div', 'entry-head');
  const badge = el('span', 'kind' + (e.kind === 'model' ? ' model' : ''));
  badge.appendChild(el('span', null, e.kind.toUpperCase()));
  badge.dataset.tip = KIND_TIPS[e.kind] || e.kind;
  const tag = el('span', 'side', e.side || 'node');
  tag.dataset.tip = SIDE_TIPS[e.side || ''];
  head.append(stamp(e.time), badge, tag);
  box.append(head, jsonBlock(e[e.kind] || {}, 1));
  return box;
}

function renderActivity() {
  const box = $('activity');
  box.replaceChildren();
  const shown = state.entries.filter((e) => state.actFilter === 'all' || e.kind === state.actFilter);
  if (!shown.length) { box.appendChild(el('div', 'empty', 'Nothing yet.')); return; }
  // Newest first: a live feed.
  for (let i = shown.length - 1; i >= 0; i--) box.appendChild(entryView(shown[i]));
}

function addEntry(e) {
  state.entries.push(e);
  if (state.entries.length > 300) state.entries.shift();
  if (state.actFilter === 'all' || e.kind === state.actFilter) {
    const box = $('activity');
    box.querySelector('.empty')?.remove();
    box.prepend(entryView(e));
    while (box.children.length > 300) box.lastChild.remove();
  }
  // A message in or out: the messages panel catches up.
  if (e.kind === 'envelope') loadMessages();
  if (e.kind === 'record' || e.kind === 'voyage') loadLeft();
}

function setStatus(sev, text) {
  $('dot').className = 'dot' + (sev === 'ok' ? '' : ' ' + sev);
  $('status-text').textContent = text;
}

function connectEvents() {
  state.events?.close();
  // The node replays what it kept, then streams; EventSource reconnects
  // with Last-Event-ID by itself.
  const es = new EventSource(API + 'events');
  state.events = es;
  es.onopen = () => setStatus('ok', 'node connected');
  es.onerror = () => setStatus('warn', 'reconnecting…');
  for (const kind of Object.keys(KIND_TIPS)) {
    es.addEventListener(kind, (ev) => { try { addEntry(JSON.parse(ev.data)); } catch { /* a bad frame is skipped */ } });
  }
  es.addEventListener('gap', () => setStatus('warn', 'some activity was missed'));
}

// ── left panel: what the active role tab shows ───────────────────────
const VIEWS = {
  customer: { label: 'As shipper', tabs: [['Shipments', 'shipments', 'shipments']] },
  operator: { label: 'As carrier', tabs: [['Bookings', 'bookings', 'bookings']] },
};

async function loadLeft() {
  const view = VIEWS[state.activeSide];
  if (!view) return;
  const [title, path, key] = view.tabs[0];
  const left = $('left');
  try {
    const r = await api(path);
    const bar = el('div', 'panel-bar');
    bar.append(el('span', 'panel-title', title), el('span', 'hint', state.activeSide === 'operator' ? 'carrier role' : 'shipper role'));
    const body = el('div', 'panel-body');
    const list = r[key] || [];
    if (!list.length) body.appendChild(el('div', 'empty', state.activeSide === 'operator' ? 'No bookings yet.' : 'No shipments yet: book one with a carrier.'));
    for (const s of list) {
      const card = el('div', 'booking');
      const top = el('div', 'booking-top');
      top.append(el('b', null, s.bookingRef), el('span', 'dim', state.activeSide === 'operator' ? s.shipper : s.operator), el('span', 'chip fam-tnt', s.status || 'booked'));
      card.appendChild(top);
      const route = `${s.origin?.unLocode || ''} ${s.origin?.name || ''} → ${s.destination?.unLocode || ''} ${s.destination?.name || ''}`;
      card.appendChild(el('div', 'summary-line', route));
      const facts = [s.vessel && `${s.vessel} ${s.voyage}`, s.transportDocumentReference, s.eta && `ETA ${s.eta}`].filter(Boolean).join(' · ');
      card.appendChild(el('div', 'summary-line dim', facts));
      const counts = el('div', 'summary-line dim', `${s.events} events · ${s.alerts} alerts${s.lastEvent ? ' · last: ' + s.lastEvent : ''}`);
      if (s.alerts) counts.classList.add('has-alert');
      card.appendChild(counts);
      body.appendChild(card);
    }
    left.className = 'panel tall';
    left.replaceChildren(bar, body);
  } catch (e) { setStatus('bad', e.message); }
}

function renderRoles() {
  const nav = $('roles');
  nav.replaceChildren();
  const sides = state.me.sides;
  if (!sides.includes(state.activeSide)) state.activeSide = sides[0];
  for (const side of sides) {
    const g = el('div', 'rolegroup');
    if (sides.length > 1) g.appendChild(el('span', 'label', VIEWS[side].label));
    for (const [title] of VIEWS[side].tabs) {
      const b = el('button', 'tab' + (side === state.activeSide ? ' on' : ''), title);
      b.onclick = () => { state.activeSide = side; renderRoles(); loadLeft(); };
      g.appendChild(b);
    }
    nav.appendChild(g);
  }
}

// ── sign-in ──────────────────────────────────────────────────────────
async function showLogin() {
  state.events?.close();
  $('login').classList.add('show');
  try {
    const res = await fetch(API + 'personas', { credentials: 'same-origin' });
    const r = await res.json();
    $('login-title').textContent = `Sign in to ${r.company} · ${r.domain}`;
    const box = $('personas');
    box.replaceChildren();
    for (const p of r.personas) {
      const b = el('button', 'persona');
      b.append(el('span', 'avatar', p.name[0]));
      const t = el('span');
      t.append(el('div', null, p.name + (p.title ? ' · ' + p.title : '')), el('div', 'roles-of', 'acts as: ' + p.roles.join(', ')));
      b.appendChild(t);
      b.onclick = () => signIn(p.id);
      box.appendChild(b);
    }
  } catch { $('login-title').textContent = 'The node is unreachable.'; }
}

async function signIn(id) {
  try {
    await api('login', { method: 'POST', body: JSON.stringify({ persona: id }) });
    $('login').classList.remove('show');
    await start();
  } catch (e) { $('login-title').textContent = e.message; }
}

async function start() {
  state.me = await api('me');
  state.messages = []; state.lastSeq = 0; state.entries = []; state.open.clear();
  const { me } = state;
  $('company').textContent = me.company;
  $('domain').textContent = me.domain;
  $('persona').textContent = me.persona.name + (me.persona.title ? ', ' + me.persona.title.split(',')[0] : '');
  $('persona-roles').textContent = `(${me.persona.roles.join(' + ')})`;
  $('demo').hidden = !me.demo;
  document.title = `${me.company} · supercargo`;
  renderRoles();
  renderActivity();
  await Promise.all([loadLeft(), loadMessages()]);
  connectEvents();
}

// ── wiring ───────────────────────────────────────────────────────────
$('theme').onclick = () => setTheme(document.body.dataset.theme === 'grayscale-dark' ? 'grayscale-light' : 'grayscale-dark');
$('signout').onclick = async () => { try { await api('logout', { method: 'POST', body: '{}' }); } catch { /* signed out anyway */ } showLogin(); };
for (const [id, set] of [['msg-filters', (f) => { state.msgFilter = f; renderMessages(); }], ['act-filters', (f) => { state.actFilter = f; renderActivity(); }]]) {
  $(id).onclick = (e) => {
    const f = e.target.dataset.f;
    if (!f) return;
    document.querySelectorAll(`#${id} button`).forEach((b) => b.classList.toggle('on', b === e.target));
    set(f);
  };
}
start().catch(() => { /* showLogin ran on 401 */ });
