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
  me: null, activeSide: null, tab: {}, msgFilter: 'all', actFilter: 'all',
  messages: [], lastSeq: 0, entries: [], open: new Set(), events: null, gen: 0,
  chat: null, thinking: false, poll: null, streamOpen: false,
  initialSeq: null, alertCounts: {}, summaries: [],
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
  const row = el('div', 'msg ' + (alertSev === 'high' && sev === 'ok' ? 'alert-high' : sev));
  row.id = 'msg-' + m.seq;
  // A delay that arrives while you watch catches the eye, once.
  if (alertSev && state.initialSeq !== null && m.seq > state.initialSeq && !state.glowed?.has(m.seq)) {
    row.classList.add('arrived');
    (state.glowed ??= new Set()).add(m.seq);
  }
  if (state.open.has(m.seq)) row.classList.add('open');
  const line = el('div', 'line');
  line.appendChild(stamp(m.time));
  line.appendChild(el('span', 'arrow', m.dir === 'out' ? '→' : '←'));
  const chipWrap = el('span');
  const model = m.intent === 'ulp.tnt.inquiry.response';
  const chip = el('span', 'chip fam-' + family(m.intent) + (model ? ' model' : '') + (alertSev === 'high' ? ' high' : ''),
    m.intent ? m.intent.replace(/^ulp\./, '') : '(unreadable)');
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

// latest makes a loader that runs one fetch at a time, and runs once more
// when asked during one: what arrived meanwhile is never dropped (two
// messages a few milliseconds apart, an alert after its event), and an
// older response never lands after a newer one.
function latest(load) {
  let running = null, again = false;
  return function run() {
    if (running) { again = true; return running; }
    running = (async () => {
      try { do { again = false; await load(); } while (again); } finally { running = null; }
    })();
    return running;
  };
}

const loadMessages = latest(async () => {
  const gen = state.gen;
  try {
    const r = await api('messages?' + new URLSearchParams(state.lastSeq ? { since: state.lastSeq } : { limit: 300 }));
    if (gen !== state.gen) return; // asked for by a session since signed out
    const first = state.initialSeq === null;
    if (r.messages.length) {
      state.messages.push(...r.messages);
      state.messages = state.messages.slice(-500);
      state.lastSeq = r.messages.at(-1).seq;
      renderMessages();
    } else if (!state.messages.length) {
      renderMessages();
    }
    if (first) state.initialSeq = state.lastSeq;
    renderBanner();
  } catch (e) { setStatus('bad', e.message); }
});

// ── activity ─────────────────────────────────────────────────────────
const KIND_TIPS = {
  envelope: 'A message in or out, and what the node decided about it. The message itself is in the Messages panel.',
  record: 'A track & trace event or alert: recorded (received for your shipment) or published (sent to a customer).',
  model: 'A call to an AI model: model, skills, tokens, latency, and whether its answer passed validation.',
  summary: "What an alert means for you, written by the assistant (or the template, if the model's answer failed validation).",
  voyage: "A simulated voyage starting (the demo's shipment simulator).",
  push: "A push to a customer the node gave up on, or couldn't publish.",
  tool: 'A tool one of your own outside agents called over MCP (Gemini Enterprise, say), and how it went. Never its arguments or results.',
};
const SIDE_TIPS = {
  customer: 'Your shipper or receiver role: shipments other carriers carry for you.',
  operator: 'Your carrier role: bookings you confirmed, events you published.',
  '': 'The whole node: a message no role can claim.',
};

const VIA_TIPS = {
  a2a: 'Carried over A2A (JSON-RPC at /a2a): a question to the carrier. The signed envelope is the same as over REST.',
  rest: 'Carried over REST (/ulp): bookings, pushed events and alerts, status. The signed envelope is the same as over A2A.',
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
  // The binding a message went over: the same signed envelope either way.
  const via = e.kind === 'envelope' && e.envelope && Object.hasOwn(VIA_TIPS, e.envelope.via) ? e.envelope.via : '';
  if (via) {
    const v = el('span', 'via ' + via, via.toUpperCase());
    v.dataset.tip = VIA_TIPS[via];
    head.appendChild(v);
  }
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
  if (e.kind === 'summary' && e.summary) { state.summaries.push(e.summary); renderBanner(); }
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
  if (e.kind === 'tool') loadInbox();
  if ((e.kind === 'record' || e.kind === 'voyage') && currentTab() !== 'assistant') loadLeft();
}

function setStatus(sev, text) {
  $('dot').className = 'dot' + (sev === 'ok' ? '' : ' ' + sev);
  $('status-text').textContent = text;
}

// If the stream is down (a proxy that buffers it, say), poll for new
// messages instead, until it's back.
function setPolling(on) {
  if (on && !state.poll) state.poll = setInterval(() => { loadMessages(); loadLeft(); }, 5000);
  if (!on && state.poll) { clearInterval(state.poll); state.poll = null; }
}

function connectEvents() {
  state.events?.close();
  // The node replays what it kept, then streams; EventSource reconnects
  // with Last-Event-ID by itself.
  const es = new EventSource(API + 'events');
  state.events = es;
  state.streamOpen = false;
  const fallback = setTimeout(() => { if (!state.streamOpen) { setStatus('warn', 'polling (no live stream)'); setPolling(true); } }, 8000);
  es.onopen = () => { state.streamOpen = true; clearTimeout(fallback); setPolling(false); setStatus('ok', 'node connected'); };
  es.onerror = () => {
    state.streamOpen = false;
    setStatus('warn', 'reconnecting…');
    setTimeout(() => { if (!state.streamOpen) setPolling(true); }, 8000);
  };
  for (const kind of Object.keys(KIND_TIPS)) {
    es.addEventListener(kind, (ev) => { try { addEntry(JSON.parse(ev.data)); } catch { /* a bad frame is skipped */ } });
  }
  es.addEventListener('gap', () => setStatus('warn', 'some activity was missed'));
}

// day is an ISO time as people say it: "Tue 13 Oct".
const day = (iso) => {
  const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short', timeZone: 'UTC' });
};

// ── the progress strip: the journey, from the node's records ─────────
// Every mark names the event behind it (hover), and opens its message
// (click). At sea is drawn halfway and hollow: an estimate, not a place.
function openMessageFor(eventId) {
  const m = state.messages.find((x) => x.body && (x.body.event_id === eventId || x.body.alert_id === eventId));
  if (!m) return;
  state.msgFilter = 'all';
  state.open.add(m.seq);
  renderMessages();
  $('msg-' + m.seq)?.scrollIntoView({ block: 'center', behavior: 'smooth' });
}

function momentText(m) {
  if (!m || !m.at) return '';
  const word = { planned: 'planned', estimated: 'est.', actual: '' }[m.kind] ?? '';
  return `${word} ${day(m.at)}`.trim();
}

function renderStrip(box, j) {
  if (!j || !Array.isArray(j.legs) || !j.legs.length) return;
  j.calls ||= [];
  j.position ||= {};
  const track = el('div', 'track');
  const reached = new Set(j.calls.filter((c) => c.kinds.some((k) => k !== 'unscheduled')).map((c) => c.port.unLocode));
  const unscheduled = new Set(j.calls.filter((c) => c.kinds.includes('unscheduled')).map((c) => c.port.unLocode));
  const pin = (p, src) => {
    const d = el('span', 'port' + (reached.has(p.unLocode) ? ' reached' : '') + (unscheduled.has(p.unLocode) ? ' unscheduled' : ''));
    d.dataset.tip = `${p.unLocode} ${p.name}` + (unscheduled.has(p.unLocode) ? ' · unscheduled call' : '');
    if (src) { d.dataset.src = src; d.onclick = () => openMessageFor(src); }
    return d;
  };
  j.legs.forEach((l, i) => {
    if (i === 0) track.appendChild(pin(l.from, l.departure && l.departure.source));
    const departed = l.departure && l.departure.kind === 'actual';
    const cls = { done: 'done', 'in progress': 'in-progress', delayed: 'delayed', planned: 'planned' }[l.status] || 'planned';
    const seg = el('span', 'seg ' + cls + (departed ? ' departed' : ''));
    seg.dataset.tip = `${l.from.unLocode} → ${l.to.unLocode} · ${l.mode}${l.vessel ? ' · ' + l.vessel + ' ' + l.voyage : ''} · ${l.status}` +
      (l.standard ? ` · reported in ${l.standard === 'DCSA_TNT' ? 'DCSA Track & Trace' : l.standard}` : '');
    const atSea = j.position.between && j.position.between[0] === l.from.unLocode && j.position.between[1] === l.to.unLocode;
    if (atSea) {
      const ship = el('span', 'ship estimated');
      ship.dataset.tip = 'At sea (estimated: no message says exactly where)';
      if (j.position.since) { ship.dataset.src = j.position.since; ship.onclick = () => openMessageFor(j.position.since); }
      seg.appendChild(ship);
    }
    track.appendChild(seg);
    track.appendChild(pin(l.to, l.arrival && l.arrival.source));
  });
  const first = j.legs[0], last = j.legs[j.legs.length - 1];
  const labels = el('div', 'labels');
  const from = el('span', null, `${first.from.unLocode} ${momentText(first.departure)}`);
  const mid = el('span', 'mid', j.position.at ? `at ${j.position.at.unLocode}` : 'at sea (estimated)');
  const to = el('span', 'end' + (last.status === 'delayed' ? ' late' : ''), `${last.to.unLocode} ${momentText(last.arrival)}` +
    (last.status === 'delayed' ? ' · delayed' : ''));
  labels.append(from, mid, to);
  box.replaceChildren(track, labels);
  if (j.truncated) box.appendChild(el('div', 'hint', 'some ports off the plan are not shown'));
}

// ── the delay banner: the latest alert, with what it means for us ────
const IMPACT = { misses: 'misses your deadline', tight: 'your deadline is now tight', ok: 'still in time', unknown: 'impact unknown' };
function dismissed() {
  try { return new Set(JSON.parse(sessionStorage.getItem('ulp-dismissed') || '[]')); } catch { return new Set(); }
}
function renderBanner() {
  const b = $('banner');
  const gone = dismissed();
  const s = [...state.summaries].reverse().find((x) => x.alertId && !gone.has(x.alertId));
  if (!s || !state.me || !state.me.sides.includes('customer')) { b.hidden = true; return; }
  const alert = state.messages.find((m) => m.intent === 'ulp.tnt.alert' && m.body && m.body.alert_id === s.alertId);
  const a = alert ? alert.body : {};
  b.className = 'banner ' + (s.impact || '');
  const parts = [el('span', 'icon', '⚠'), el('span', null, `${(a.kind || 'alert').toUpperCase()} · ${s.booking}`)];
  if (a.previous_eta && a.new_eta) parts.push(el('span', null, `ETA ${day(a.previous_eta)} → ${day(a.new_eta)}`));
  parts.push(el('span', 'impact', IMPACT[s.impact] || ''), el('span', 'spacer'));
  if (alert) {
    const view = el('button', null, 'view');
    view.onclick = () => {
      state.msgFilter = 'all';
      state.open.add(alert.seq);
      renderMessages();
      $('msg-' + alert.seq)?.scrollIntoView({ block: 'center', behavior: 'smooth' });
    };
    parts.push(view);
  }
  const close = el('button', null, '×');
  close.dataset.tip = 'Dismiss: the alert stays in the messages panel.';
  close.onclick = () => {
    gone.add(s.alertId);
    try { sessionStorage.setItem('ulp-dismissed', JSON.stringify([...gone])); } catch { /* private mode */ }
    renderBanner();
  };
  parts.push(close);
  b.replaceChildren(...parts);
  b.title = s.text || '';
  b.hidden = false;
}

// ── left panel: what the active role tab shows ───────────────────────
const VIEWS = {
  customer: { label: 'As shipper', tabs: [['Assistant', 'assistant'], ['Shipments', 'shipments']] },
  operator: { label: 'As carrier', tabs: [['Bookings', 'bookings']] },
};
const tabsFor = (side) => VIEWS[side].tabs.filter(([, k]) => k !== 'assistant' || state.me.assistant);
const currentTab = () => state.tab[state.activeSide] || tabsFor(state.activeSide)[0][1];

const loadLeft = latest(async () => {
  const view = VIEWS[state.activeSide];
  if (!view) return;
  if (currentTab() === 'assistant') { state.chat ? renderChat() : await loadChat(); return; }
  const gen = state.gen, side = state.activeSide, tab = currentTab();
  const left = $('left');
  try {
    const [title, path] = tabsFor(side).find(([, k]) => k === tab);
    const key = path;
    const r = await api(path);
    // Moved on meanwhile (another tab, side or session): the rerun, or
    // whatever moved, draws the panel.
    if (gen !== state.gen || side !== state.activeSide || tab !== currentTab()) return;
    const bar = el('div', 'panel-bar');
    bar.append(el('span', 'panel-title', title), el('span', 'hint', state.activeSide === 'operator' ? 'carrier role' : 'shipper role'));
    const body = el('div', 'panel-body');
    const list = r[key] || [];
    state.lastList = list;
    if (!list.length) body.appendChild(el('div', 'empty', state.activeSide === 'operator' ? 'No bookings yet.' : 'No shipments yet: book one with a carrier.'));
    for (const s of list) {
      const card = el('div', 'booking');
      const top = el('div', 'booking-top');
      top.append(el('b', null, s.bookingRef), el('span', 'dim', state.activeSide === 'operator' ? s.shipper : s.operator), el('span', 'chip fam-tnt', s.status || 'booked'));
      card.appendChild(top);
      const route = `${s.origin?.unLocode || ''} ${s.origin?.name || ''} → ${s.destination?.unLocode || ''} ${s.destination?.name || ''}`;
      card.appendChild(el('div', 'summary-line', route));
      const delayed = s.alerts > 0 && s.plannedArrival && s.eta && s.eta > s.plannedArrival;
      if (delayed) {
        card.classList.add('delayed');
        top.querySelector('.chip').before(el('span', 'delayed-badge', 'DELAYED'));
      }
      const facts = el('div', 'summary-line dim', [s.vessel && `${s.vessel} ${s.voyage}`, s.transportDocumentReference].filter(Boolean).join(' · '));
      if (s.eta) {
        facts.append(' · ETA ');
        if (delayed) { facts.append(el('span', 'was', day(s.plannedArrival)), ' → '); }
        facts.append(el('b', null, day(s.eta)));
      }
      card.appendChild(facts);
      const key = `${s.operator}/${s.bookingRef}`;
      if (state.alertCounts[key] !== undefined && s.alerts > state.alertCounts[key]) card.classList.add('arrived');
      state.alertCounts[key] = s.alerts;
      const strip = el('div', 'strip');
      card.appendChild(strip);
      const jpath = state.activeSide === 'operator' ? `bookings/${encodeURIComponent(s.bookingRef)}/journey`
        : `shipments/${encodeURIComponent(s.operator)}/${encodeURIComponent(s.bookingRef)}/journey`;
      api(jpath).then((r) => { if (r.journey) renderStrip(strip, r.journey); }).catch(() => {});
      const counts = el('div', 'summary-line dim', `${s.events} events · ${s.alerts} alerts${s.lastEvent ? ' · last: ' + s.lastEvent : ''}`);
      if (s.alerts) counts.classList.add('has-alert');
      card.appendChild(counts);
      if (state.activeSide === 'operator') {
        const act = el('div', 'actions');
        const b = el('button', null, 'Disrupt…');
        b.dataset.tip = 'Delay this booking, or its whole voyage: each customer gets a signed delay alert. You confirm first.';
        b.onclick = () => disruptDialog(s);
        act.appendChild(b);
        card.appendChild(act);
      }
      body.appendChild(card);
    }
    left.className = 'panel tall';
    left.replaceChildren(bar, body);
  } catch (e) { setStatus('bad', e.message); }
});

function renderLeftNow() {
  if (currentTab() === 'assistant') { state.chat ? renderChat() : loadChat(); } else { loadLeft(); }
}

// ── the assistant ────────────────────────────────────────────────────
// markdown renders the small Markdown subset models answer in (paragraphs,
// bullet and numbered lists, one level nested, headings, code blocks,
// **bold**, *italics*, `code`) as elements and text nodes only: the text
// is model output, so nothing in it ever becomes markup, a link or an
// attribute. (A numbered list's start is a number the code parsed.)
function markdown(text) {
  const box = el('div', 'md');
  let list = null, para = null, fence = null, gap = false;
  const newList = (kind, n) => { const l = el(kind); if (kind === 'ol' && n > 1) l.start = n; return l; };
  for (const raw of String(text).split('\n')) {
    if (fence) {
      if (/^\s*```/.test(raw)) fence = null;
      else fence.appendChild(document.createTextNode((fence.firstChild ? '\n' : '') + raw));
      continue;
    }
    const line = raw.trimEnd();
    let m;
    if (/^\s*```/.test(line)) {
      list = null; para = null;
      const pre = el('pre');
      fence = pre.appendChild(el('code'));
      box.appendChild(pre);
    } else if (!line.trim()) {
      // A list survives a blank line (models write loose lists); a
      // paragraph doesn't.
      para = null; gap = true;
    } else if ((m = line.match(/^\s*(#{1,4})\s+(.*)$/))) {
      list = null; para = null;
      box.appendChild(inline(el('div', 'md-h'), m[2]));
    } else if ((m = line.match(/^(\s*)(?:[-*•]|(\d{1,3})[.)])\s+(.*)$/))) {
      para = null; gap = false;
      const kind = m[2] ? 'ol' : 'ul', n = Number(m[2] || 1);
      if (list && m[1].length >= 2 && list.lastChild) {
        // Indented under an item: its sub-list.
        const li = list.lastChild;
        let sub = li.lastChild;
        if (!sub || !sub.tagName || sub.tagName.toLowerCase() !== kind) sub = li.appendChild(newList(kind, n));
        sub.appendChild(inline(el('li'), m[3]));
      } else {
        if (!list || list.tagName.toLowerCase() !== kind) list = box.appendChild(newList(kind, n));
        list.appendChild(inline(el('li'), m[3]));
      }
    } else if (list && !gap && /^\s{2,}\S/.test(raw) && list.lastChild) {
      // A list item's continuation line.
      inline(list.lastChild, ' ' + line.trim());
    } else {
      list = null; gap = false;
      if (para) para.appendChild(document.createElement('br'));
      else para = box.appendChild(el('p'));
      inline(para, line);
    }
  }
  return box;
}

// inline appends text to node with **bold**, *italics* and `code` as
// elements; anything else, unmatched markers included, as text. An
// asterisk right after a letter or digit (5*3*2) isn't emphasis.
function inline(node, text) {
  const re = /(`[^`\n]+`)|(\*\*(?:[^*\n]|\*(?!\*))+\*\*)|(\*[^*\s](?:[^*\n]*[^*\s])?\*)/g;
  let at = 0, m;
  while ((m = re.exec(text))) {
    if (m[3] && m.index > 0 && /[\p{L}\p{N}]/u.test(text[m.index - 1])) { re.lastIndex = m.index + 1; continue; }
    if (m.index > at) node.appendChild(document.createTextNode(text.slice(at, m.index)));
    const t = m[0];
    if (m[1]) node.appendChild(el('code', null, t.slice(1, -1)));
    else if (m[2]) node.appendChild(inline(el('strong'), t.slice(2, -2)));
    else node.appendChild(inline(el('em'), t.slice(1, -1)));
    at = m.index + t.length;
  }
  if (at < text.length) node.appendChild(document.createTextNode(text.slice(at)));
  return node;
}

// toolView is one tool call: its name, its arguments in short, and how it
// went (or that it's running).
function toolView(tool, running) {
  const c = el('div', 'toolcall' + (running ? ' running' : ''), '▸ ' + tool.name);
  if (tool.detail) c.appendChild(el('span', 'args', ' ' + tool.detail));
  c.appendChild(document.createTextNode(' '));
  if (running) c.appendChild(el('span', 'spin', '…'));
  else c.appendChild(el('span', tool.error ? 'bad' : 'okk', tool.error ? '✗ ' + tool.error : '✓'));
  return c;
}

async function loadChat() {
  try { state.chat = await api('assistant'); renderChat(); } catch (e) { setStatus('bad', e.message); }
}

function renderChat() {
  if (currentTab() !== 'assistant') return;
  const left = $('left');
  const keep = left.querySelector('textarea')?.value || '';
  left.className = 'panel tall ai';
  const bar = el('div', 'panel-bar');
  bar.append(el('span', 'panel-title', 'Assistant'), el('span', 'hint', 'shipper role · it drafts; you decide'));
  const body = el('div', 'panel-body chat');
  const lines = state.chat.lines || [];
  const inbox = inboxView();
  if (inbox) body.appendChild(inbox);
  if (!lines.length) body.appendChild(el('div', 'empty', 'Ask about a carrier by its domain, book a shipment, or check on one.'));
  for (const l of lines) {
    const t = el('div', 'turn ' + (l.who === 'you' ? 'you' : l.who === 'note' ? 'note' : 'ai'));
    if (l.who !== 'note') t.appendChild(el('div', 'who', (l.who === 'you' ? 'YOU' : 'ASSISTANT') + ' · ' + clock(l.time)[0]));
    for (const tool of l.tools || []) t.appendChild(toolView(tool, false));
    t.appendChild(l.who === 'assistant' ? markdown(l.text) : el('div', 'text', l.text));
    for (const d of l.drafts || []) {
      if (!(state.chat.drafts || []).some((x) => x.draftId === d.draftId)) continue;
      const b = el('button', 'draftlink', `Review draft ${d.draftId}…`);
      b.onclick = () => bookingDialog(d);
      t.appendChild(b);
    }
    body.appendChild(t);
  }
  if (state.thinking) {
    // What the assistant is doing: the calls so far, the one running, or
    // (between them) the model thinking.
    const p = state.progress || {};
    const work = el('div', 'turn working');
    for (const tool of p.tools || []) work.appendChild(toolView(tool, false));
    if (p.running) work.appendChild(toolView(p.running, true));
    else work.appendChild(el('div', 'thinking', (p.tools || []).length ? '✦ thinking about the results…' : '✦ thinking…'));
    body.appendChild(work);
  }
  const comp = el('div', 'composer');
  const ta = el('textarea');
  ta.placeholder = 'Ask about a carrier, book a shipment, or check on one…  (Enter sends, Shift+Enter: new line)';
  ta.value = keep;
  ta.disabled = state.thinking;
  ta.maxLength = 4000;
  const send = el('button', null, 'Send');
  send.disabled = state.thinking;
  const go = () => { const v = ta.value.trim(); if (v) { ta.value = ''; chatTurn(v); } };
  send.onclick = go;
  ta.onkeydown = (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); go(); } };
  comp.append(ta, send);
  left.replaceChildren(bar, body, comp);
  body.scrollTop = body.scrollHeight;
  if (!state.thinking) ta.focus();
}

// watchProgress polls what the assistant is doing while a turn runs, and
// redraws the chat when it changes. It stops when the turn is over.
function watchProgress() {
  state.progress = null;
  const turn = state.turnSeq = (state.turnSeq || 0) + 1;
  const live = () => state.thinking && state.turnSeq === turn; // this turn, not a later one
  let last = '';
  const tick = async () => {
    if (!live()) return;
    try {
      const p = await api('assistant/progress');
      const key = JSON.stringify(p);
      if (live() && p.working && key !== last) { last = key; state.progress = p; renderChat(); }
    } catch { return; /* signed out, say: the turn itself reports failures */ }
    if (live()) setTimeout(tick, 600);
  };
  setTimeout(tick, 300);
}

async function chatTurn(text) {
  state.thinking = true;
  watchProgress();
  state.chat.lines = [...(state.chat.lines || []), { who: 'you', text, time: new Date().toISOString() }];
  renderChat();
  try {
    state.chat = await api('assistant/turn', { method: 'POST', body: JSON.stringify({ text }) });
  } catch (e) {
    state.chat.lines.push({ who: 'note', text: e.message, time: new Date().toISOString() });
  } finally {
    state.thinking = false;
    state.progress = null;
    renderChat();
  }
  // A new draft opens its dialog, as in ulpchat.
  const newest = (state.chat.drafts || []).at(-1);
  if (newest && !state.shownDrafts?.has(newest.draftId)) { (state.shownDrafts ??= new Set()).add(newest.draftId); bookingDialog(newest); }
}

// modal shows a dialog whose confirm button arms after a delay: a click
// (or a stray Enter) can't confirm what was never on screen.
function modal(title, rows, question, confirmText, onConfirm, extra = []) {
  const back = el('div', 'modal');
  const d = el('div', 'dialog');
  d.setAttribute('role', 'dialog');
  d.appendChild(el('h2', null, title));
  for (const [k, v, cls] of rows) {
    const row = el('div', 'row' + (cls ? ' ' + cls : ''));
    row.append(el('span', 'k', k), el('span', 'v', v));
    d.appendChild(row);
  }
  extra.forEach((x) => d.appendChild(x));
  d.appendChild(el('div', 'q', question));
  const err = el('div', 'err');
  const buttons = el('div', 'buttons');
  const cancel = el('button', null, 'Cancel');
  const ok = el('button', 'primary', confirmText);
  ok.disabled = true;
  const armed = setTimeout(() => { ok.disabled = false; }, 1600);
  const close = () => { clearTimeout(armed); back.remove(); document.removeEventListener('keydown', esc); };
  const esc = (e) => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', esc);
  cancel.onclick = close;
  ok.onclick = async () => {
    ok.disabled = true; cancel.disabled = true; err.textContent = 'sending…';
    try { await onConfirm(); close(); } catch (e) { err.textContent = e.message; cancel.disabled = false; }
  };
  buttons.append(cancel, ok);
  d.append(err, buttons);
  back.appendChild(d);
  back.onclick = (e) => { if (e.target === back) close(); };
  document.body.appendChild(back);
  cancel.focus();
  return { close };
}

// ── the inbox: drafts your outside agents made over MCP ──────────────
async function loadInbox() {
  if (!(state.me && state.me.sides.includes('customer'))) return;
  try {
    const r = await api('inbox');
    state.inbox = r.drafts || [];
    if (currentTab() === 'assistant') renderChat();
    // A link from the agent (?draft=MD-…) opens that draft once.
    const want = new URLSearchParams(location.search).get('draft');
    const d = want && state.inbox.find((x) => x.draftId === want);
    if (d && !state.openedLink) { state.openedLink = true; bookingDialog(d, INBOX); }
  } catch { /* no inbox, or not this persona's */ }
}

// How a booking dialog sends and discards: the assistant's drafts, or the
// inbox's.
const CHAT = {
  discard: async (d) => { state.chat = await api('assistant/discard', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) }); renderChat(); },
  send: async (d) => {
    try { state.chat = await api('assistant/send', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) }); } finally { await loadChat(); }
  },
};
const INBOX = {
  discard: async (d) => { state.inbox = (await api('inbox/discard', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) })).drafts || []; renderChat(); },
  send: async (d) => {
    try {
      const r = await api('inbox/send', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) });
      if (r.note) setStatus('ok', r.note);
    } finally { await loadInbox(); loadMessages(); }
  },
};

// inboxView is the assistant panel's list of drafts from outside agents.
function inboxView() {
  const drafts = state.inbox || [];
  if (!drafts.length) return null;
  const box = el('div', 'inbox');
  box.appendChild(el('div', 'inbox-title', 'From your agents (MCP): waiting for a person'));
  for (const d of drafts) {
    const row = el('div', 'inbox-row');
    row.append(el('span', null, `${d.draftId} → ${d.to}`), el('span', 'dim', `${d.request?.port_of_loading || ''} → ${d.request?.port_of_discharge || ''}`));
    const b = el('button', 'draftlink', 'Review…');
    b.onclick = () => bookingDialog(d, INBOX);
    row.appendChild(b);
    box.appendChild(row);
  }
  return box;
}

function bookingDialog(d, via = CHAT) {
  const q = d.request || {};
  const eq = (q.containers || []).map((c) => `${c.count} × ${c.equipment_size_type}, ${c.gross_weight_kg} kg each`).join('; ');
  const rows = [
    ['Operator:', d.to], ...(d.newPartner ? [['', 'A new partner: not in your address book, and never booked with before. Check the domain.', 'newpartner']] : []),
    ['Shipper:', q.shipper_name || ''],
    ['Route:', `${q.port_of_loading} → ${q.port_of_discharge}`], ['Equipment:', eq],
    ['Cargo:', `${q.cargo?.description || ''}, HS ${q.cargo?.hs_code || ''}`], ['Departure:', 'on/after ' + (q.earliest_departure || '')],
    ['Reference:', q.shipper_reference || ''],
  ];
  if (q.service_contract_number) rows.push(['Contract:', q.service_contract_number]);
  if (q.consignee) rows.push(['Consignee:', `${q.consignee.name} (${q.consignee.domain})`]);
  if (d.newConsignee) rows.push(['', 'A consignee not in your address book. Check the domain: it goes to the carrier.', 'newpartner']);
  if (q.subscription) rows.push(['Updates:', `${(q.subscription.intents || []).join(', ')} → ${q.subscription.endpoint}`]);
  for (const [k, v] of Object.entries(d.notSent || {})) rows.push(['Not sent:', `your ${k === 'requiredBy' ? 'deadline' : k}, ${v} (it stays with us)`, 'notsent']);
  const discard = el('div', 'fineprint');
  const link = el('button', null, 'Discard this draft');
  link.onclick = async () => {
    try { await via.discard(d); dlg.close(); } catch (e) { discard.lastChild.textContent = e.message; }
  };
  discard.append(link, el('span'));
  const dlg = modal(`Booking draft ${d.draftId}`, rows, 'Send this booking request? It is signed by your node and sent to ' + d.to + '.',
    'Send booking request', () => via.send(d), [discard]);
}

function disruptDialog(s) {
  const delay = el('select');
  for (const h of ['12h', '24h', '48h', '72h']) { const o = el('option', null, h); o.value = h; delay.appendChild(o); }
  delay.value = '72h';
  const reason = el('input');
  reason.maxLength = 280;
  reason.value = 'Engine repair, unscheduled call at Halifax.';
  // One booking, or every booking on its voyage: one decision, a signed
  // alert to each booking's customer (the node picks the bookings under way).
  const onVoyage = (state.lastList || []).filter((b) => s.voyage && b.voyage === s.voyage && b.vessel === s.vessel);
  const scope = el('select');
  const one = el('option', null, `this booking (${s.bookingRef})`); one.value = 'booking';
  scope.appendChild(one);
  if (s.voyage) {
    // The node delays only the bookings still under way, which may be
    // fewer than the list shows.
    const all = el('option', null, `the whole voyage: ${s.vessel || ''} ${s.voyage} (up to ${onVoyage.length} booking${onVoyage.length === 1 ? '' : 's'})`);
    all.value = 'voyage';
    scope.appendChild(all);
  }
  const note = el('div', 'fineprint');
  const explain = () => {
    note.textContent = scope.value === 'voyage'
      ? `Every booking on ${s.vessel || ''} ${s.voyage} that hasn't arrived gets the delay, and each customer a signed alert with this reason.`
      : `${s.shipper} gets a signed delay alert with this reason, and the arrival estimate moves.`;
  };
  scope.onchange = explain;
  explain();
  const fields = el('div');
  const r0 = el('div', 'row'); r0.append(el('span', 'k', 'Delay:'), scope);
  const r1 = el('div', 'row'); r1.append(el('span', 'k', 'How long:'), delay);
  const r2 = el('div', 'row'); r2.append(el('span', 'k', 'Reason:'), reason);
  fields.append(r0, r1, r2, note);
  modal(`Disrupt · ${s.vessel || ''} ${s.voyage || ''}`, [['Booking:', s.bookingRef], ['Customer:', s.shipper], ['Vessel:', `${s.vessel || ''} ${s.voyage || ''}`]],
    'Publish this delay?', 'Publish delay', async () => {
      const req = scope.value === 'voyage'
        ? { vessel: s.vessel || '', voyage: s.voyage, delay: delay.value, reason: reason.value.trim() }
        : { bookingRef: s.bookingRef, delay: delay.value, reason: reason.value.trim() };
      const r = await api('disrupt', { method: 'POST', body: JSON.stringify(req) });
      // A voyage answers per booking: say what didn't go out. Once any
      // delay is applied, the dialog closes either way: publishing again
      // would delay those bookings twice.
      const failed = (r.alerts || []).filter((a) => !a.alert);
      if (failed.length) {
        const applied = failed.filter((a) => a.applied).length;
        const sent = (r.alerts || []).length - failed.length;
        setStatus(applied ? 'bad' : 'warn', `Voyage delay: ${sent} alert${sent === 1 ? '' : 's'} sent; ` +
          failed.map((a) => `${a.bookingRef} ${a.applied ? 'DELAYED, alert not sent' : 'not delayed'}`).join(', ') +
          (applied ? '. Don\'t publish again: it would delay them twice.' : ''));
        if (!sent && !applied) throw new Error(failed[0].error || 'no booking took the delay');
      }
    }, [fields]);
}

function renderRoles() {
  const nav = $('roles');
  nav.replaceChildren();
  const sides = state.me.sides;
  if (!sides.includes(state.activeSide)) state.activeSide = sides[0];
  for (const side of sides) {
    const g = el('div', 'rolegroup');
    if (sides.length > 1) g.appendChild(el('span', 'label', VIEWS[side].label));
    for (const [title, key] of tabsFor(side)) {
      const on = side === state.activeSide && key === currentTab();
      const b = el('button', 'tab' + (on ? ' on' : ''), title);
      b.onclick = () => { state.activeSide = side; state.tab[side] = key; renderRoles(); renderLeftNow(); };
      g.appendChild(b);
    }
    nav.appendChild(g);
  }
}

// ── sign-in ──────────────────────────────────────────────────────────
async function showLogin() {
  state.events?.close();
  setPolling(false);
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
  state.gen++;
  state.messages = []; state.lastSeq = 0; state.entries = []; state.open.clear(); state.chat = null; state.tab = {}; state.shownDrafts = new Set();
  state.initialSeq = null; state.alertCounts = {}; state.summaries = []; state.inbox = []; $('banner').hidden = true;
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
  if (currentTab() === 'assistant') renderChat();
  connectEvents();
  loadInbox();
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
