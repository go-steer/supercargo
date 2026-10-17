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
  chats: {}, thinking: false, poll: null, streamOpen: false,
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
const family = (intent = '') => /^ulp\.(ocean\.booking|road\.booking|forwarding)\./.test(intent) ? 'booking' : intent === 'ulp.tnt.alert' ? 'alert'
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
    case 'ulp.road.booking.request':
      return `${first.count ?? '?'} × ${first.equipment_size_type ?? '?'}, ${b.pickup?.un_location_code} → ${b.delivery?.un_location_code} · ${b.pickup?.facility || ''} → ${b.delivery?.facility || ''}`;
    case 'ulp.road.booking.amendment':
      return `${b.carrier_booking_reference} · collect ${b.pickup?.earliest || '—'} · deliver ${b.delivery?.earliest || b.delivery?.latest || '—'}`;
    case 'ulp.road.booking.confirmation':
      return [b.status, b.carrier_booking_reference, ...(b.transport_documents || []).concat(b.fiscal_documents || []).map((d) => d.type)].filter(Boolean).join(' · ');
    case 'ulp.forwarding.request':
      return `${first.count ?? '?'} × ${first.equipment_size_type ?? '?'}, ${b.place_of_receipt?.un_location_code} → ${b.place_of_delivery?.un_location_code} · ${b.incoterms?.rule || ''} · door to door`;
    case 'ulp.forwarding.confirmation':
      return [b.status, b.forwarder_reference, (b.legs || []).map((l) => `${l.mode} ${l.from}→${l.to}`).join(', ')].filter(Boolean).join(' · ');
    case 'ulp.tnt.event': {
      const src = (b.derived_from || [])[0];
      const from = src ? (src.signed ? ` · ✓ from ${src.id}` : ` · ◇ unsigned: ${src.source || ''}`) : '';
      return `${b.event_type} ${b.event_type_code} ${b.event_classifier_code} · ${b.un_location_code} · ${b.event_date_time}${from}`;
    }
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
  dock: "What your standing dock policy did for a truck you follow: booked, moved or kept its appointment, and who set the policy.",
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
  if ((e.kind === 'record' || e.kind === 'voyage' || e.kind === 'dock') && currentTab() !== 'assistant') loadLeft();
  // An order, its answer, a leg's message: the desk catches up.
  if (e.kind === 'envelope' && currentTab() === 'orders') loadLeft();
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
      ship.dataset.tip = (l.mode === 'road' ? 'On the road' : 'At sea') + ' (estimated: no message says exactly where)';
      if (j.position.since) { ship.dataset.src = j.position.since; ship.onclick = () => openMessageFor(j.position.since); }
      seg.appendChild(ship);
    }
    track.appendChild(seg);
    track.appendChild(pin(l.to, l.arrival && l.arrival.source));
  });
  const first = j.legs[0], last = j.legs[j.legs.length - 1];
  const labels = el('div', 'labels');
  const from = el('span', null, `${first.from.unLocode} ${momentText(first.departure)}`);
  // Between two places: on the leg between them (a truck's or a ship's).
  const between = j.legs.find((l) => j.position.between && j.position.between[0] === l.from.unLocode && j.position.between[1] === l.to.unLocode);
  const road = (between || first).mode === 'road';
  const mid = el('span', 'mid', j.position.at ? `at ${j.position.at.unLocode}` : road ? 'on the road (estimated)' : 'at sea (estimated)');
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
  // The shipper's assistant, on this delay: what it means for us, with our
  // private context, and what we could do. It drafts; nothing goes out
  // unless the person says so.
  if ((state.me.assistants || []).includes('customer') && s.impact !== 'ok') {
    const ask = el('button', 'ai', '✦ What can we do?');
    ask.dataset.tip = 'Ask your assistant: it reads your private context (deadlines, slots), suggests options, and asks the forwarder only if you say so.';
    ask.onclick = () => {
      state.activeSide = 'customer';
      state.tab.customer = 'assistant';
      renderRoles();
      const go = () => chatTurn(`What can we do about the delay on ${s.booking}?`);
      if (state.chats.customer) { renderChat(); go(); } else loadChat().then(go);
    };
    parts.push(ask);
  }
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
  operator: { label: 'As carrier', tabs: [['Orders', 'orders'], ['Bookings', 'bookings'], ['Assistant', 'assistant']] },
};
const tabsFor = (side) => VIEWS[side].tabs.filter(([, k]) => (k !== 'assistant' || (state.me.assistants || []).includes(side)) && (k !== 'orders' || state.me.orders));
const currentTab = () => state.tab[state.activeSide] || tabsFor(state.activeSide)[0][1];

const loadLeft = latest(async () => {
  const view = VIEWS[state.activeSide];
  if (!view) return;
  if (currentTab() === 'assistant') { state.chats[state.activeSide] ? renderChat() : await loadChat(); return; }
  if (currentTab() === 'orders') { await loadOrders(); return; }
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
    bar.append(el('span', 'panel-title', title), el('span', 'hint', state.activeSide === 'operator' ? 'carrier role' : receiverOnly() ? 'receiver role' : 'shipper role'));
    const body = el('div', 'panel-body');
    // A forwarder's orders have their own tab.
    const list = (r[key] || []).filter((s) => !(state.me.orders && side === 'operator' && s.kind === 'order'));
    state.lastList = list;
    if (!list.length) body.appendChild(el('div', 'empty', state.activeSide === 'operator' ? 'No bookings yet.' : 'No shipments yet: book one with a carrier.'));
    for (const s of list) {
      const card = el('div', 'booking');
      const top = el('div', 'booking-top');
      top.append(el('b', null, s.bookingRef), el('span', 'dim', state.activeSide === 'operator' ? s.shipper : s.operator), el('span', 'chip fam-tnt', s.status || (s.delegatedBy ? 'followed' : 'booked')));
      if (s.delegatedBy) {
        const chip = el('span', 'chip deleg', 'delegated by ' + s.delegatedBy);
        chip.dataset.tip = `Followed by ${s.delegatedBy}'s signed delegation, until ${day(s.delegatedUntil)}: its carrier checks it on every request.`;
        top.appendChild(chip);
      }
      if ((s.sharedWith || []).length) {
        const chip = el('span', 'chip deleg', 'shared with ' + s.sharedWith.join(', '));
        chip.dataset.tip = 'Delegated by you: they follow this booking by your signed grant.';
        top.appendChild(chip);
      }
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
        facts.append(facts.textContent ? ' · ETA ' : 'ETA ');
        if (delayed) { facts.append(el('span', 'was', day(s.plannedArrival)), ' → '); }
        facts.append(el('b', null, day(s.eta)));
      }
      card.appendChild(facts);
      const key = `${s.operator}/${s.bookingRef}`;
      if (state.alertCounts[key] !== undefined && s.alerts > state.alertCounts[key]) card.classList.add('arrived');
      state.alertCounts[key] = s.alerts;
      const door = s.kind === 'order';
      const strip = el('div', door ? 'strip door' : 'strip');
      card.appendChild(strip);
      const base = state.activeSide === 'operator' ? `bookings/${encodeURIComponent(s.bookingRef)}`
        : `shipments/${encodeURIComponent(s.operator)}/${encodeURIComponent(s.bookingRef)}`;
      api(base + '/journey').then((r) => { if (r.journey) (door ? renderLegs : renderStrip)(strip, r.journey); }).catch(() => {});
      if (door) {
        // Door to door: each event says where it came from.
        const evs = el('div');
        card.appendChild(evs);
        api(base + '/events').then((r) => renderOrderEvents(evs, r.records || [])).catch(() => {});
      }
      const counts = el('div', 'summary-line dim', `${s.events} events · ${s.alerts} alerts${s.lastEvent ? ' · last: ' + s.lastEvent : ''}`);
      if (s.alerts) counts.classList.add('has-alert');
      card.appendChild(counts);
      if (state.activeSide === 'customer' && s.delegatedBy) {
        // A truck the site follows by delegation: its own dock appointment.
        const dock = el('div');
        card.appendChild(dock);
        api(`docks/trucks/${encodeURIComponent(s.operator)}/${encodeURIComponent(s.bookingRef)}`).then((r) => renderDock(dock, r.appointment)).catch(() => {});
      }
      if (state.activeSide === 'operator' && s.kind === 'road') {
        const bo = el('div');
        card.appendChild(bo);
        api(base + '/backoffice').then((r) => renderBackOffice(bo, r.steps || [])).catch(() => {});
        // The leg on a map, if the node draws one: gone if it doesn't.
        const map = el('img', 'leg-map');
        map.alt = 'The leg on a map: pickup (P), corridor, delivery (D)';
        map.onerror = () => map.remove();
        map.src = API + base + '/map';
        card.appendChild(map);
      }
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

// ── the forwarder's desk: orders, their plans, legs and proposals ────
// (docs/ui/door-to-door-mockup.html). The node plans on its lanes and
// books; this page shows what it would send, and a person says yes.
const dayTime = (iso) => {
  const d = new Date(iso);
  return isNaN(d) ? (iso || '') : d.toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone: 'UTC' });
};
const MODE = { road: 'ROAD', ocean: 'SEA', rail: 'RAIL' };
const brl = (n) => new Intl.NumberFormat('pt-BR', { style: 'currency', currency: 'BRL' }).format(n);
// A rate round and above the floor: what a person would offer.
const aboveFloor = (floor) => Math.ceil(floor / 10) * 10 + 30;
const place = (p) => p ? [p.facility, p.address && [p.address.street, p.address.locality, p.address.country].filter(Boolean).join(', '), p.un_location_code].filter(Boolean).join(' · ') : '';
const party = (p) => p ? [p.name, ...(p.identifiers || []).map((i) => `${i.scheme} ${i.value}`)].filter(Boolean).join(' · ') : '';
const equipment = (eq) => (eq || []).map((c) => `${c.count} × ${c.equipment_size_type}`).join(', ');
// How a leg's carrier is reached: a node, signed both ways, or EDI files.
const reachTag = (edi) => {
  const t = el('span', 'reach ' + (edi ? 'edi' : 'ulp'), edi ? 'EDI · UNSIGNED' : 'ULP · SIGNED');
  t.dataset.tip = edi ? 'No node: EDIFACT files. Meridian re-issues its statuses to the customer marked unsigned.'
    : 'A ULP node: every message signed, both ways.';
  return t;
};

const loadOrders = latest(async () => {
  const gen = state.gen, left = $('left');
  try {
    const r = await api('bookings');
    const orders = (r.bookings || []).filter((b) => b.kind === 'order').sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    if (!orders.some((o) => o.bookingRef === state.order)) {
      state.order = (orders.find((o) => o.status === 'pending') || orders[0] || {}).bookingRef;
    }
    const detail = state.order ? (await api('orders/' + encodeURIComponent(state.order))).order : null;
    if (gen !== state.gen || currentTab() !== 'orders') return;
    const keep = left.querySelector('.panel-body')?.scrollTop || 0;
    const bar = el('div', 'panel-bar');
    bar.append(el('span', 'panel-title', 'Orders'), el('span', 'hint', "customers' door-to-door orders"));
    const body = el('div', 'panel-body desk');
    if (!orders.length) body.appendChild(el('div', 'empty', 'No orders yet: a customer orders with ulpnode order, or its assistant.'));
    const open = detail ? detail.proposals.filter((p) => p.status === 'proposed').length : 0;
    for (const o of orders) {
      const card = el('div', 'booking order' + (o.bookingRef === state.order ? ' on' : ''));
      const top = el('div', 'booking-top');
      top.append(el('b', null, o.bookingRef), el('span', 'dim', o.shipper));
      if (o.bookingRef === state.order && open) top.appendChild(el('span', 'chip waiting', `${open} proposal${open === 1 ? '' : 's'}`));
      top.appendChild(el('span', 'chip ' + (o.status === 'pending' ? 'pending' : o.status === 'rejected' ? 'fam-error' : 'fam-tnt'),
        o.status === 'pending' ? 'pending · plan to confirm' : o.status));
      card.appendChild(top);
      card.appendChild(el('div', 'summary-line', `${o.origin?.unLocode || ''} ${o.origin?.name || ''} → ${o.destination?.unLocode || ''} ${o.destination?.name || ''}`));
      if (o.eta) card.appendChild(el('div', 'summary-line dim', 'delivery ' + day(o.eta)));
      card.onclick = () => { state.order = o.bookingRef; loadOrders(); };
      body.appendChild(card);
    }
    if (detail) body.append(...orderDetail(detail));
    left.className = 'panel tall';
    left.replaceChildren(bar, body);
    body.scrollTop = keep;
  } catch (e) { setStatus('bad', e.message); }
});

// orderDetail is the selected order: what the customer signed, its plan
// or legs, and its proposals.
function orderDetail(v) {
  const q = v.request, a = v.answer, out = [];
  const section = (title, hint) => { const s = el('div', 'desk-title'); s.append(el('span', 'panel-title', title), el('span', 'hint', hint)); return s; };
  out.push(section('Order ' + v.orderRef, 'as ' + v.shipper + ' signed it'));
  const facts = el('dl', 'facts');
  const fact = (k, val) => { if (val) facts.append(el('dt', null, k), el('dd', null, val)); };
  fact('Incoterms', q.incoterms && `${q.incoterms.rule} ${q.incoterms.version} · ${q.incoterms.named_place || ''}`);
  fact('Shipper', party(q.shipper));
  fact('Consignee', party(q.consignee));
  fact('From', place(q.place_of_receipt) + (q.place_of_receipt?.earliest ? ' · ready ' + dayTime(q.place_of_receipt.earliest) : ''));
  fact('To', place(q.place_of_delivery));
  fact('Deliver by', q.place_of_delivery?.latest && dayTime(q.place_of_delivery.latest));
  fact('Goods', [equipment(q.containers), q.cargo && `${q.cargo.description} · HS ${q.cargo.hs_code}`].filter(Boolean).join(' · '));
  fact('Their ref', q.shipper_reference);
  out.push(facts);
  out.push(el('div', 'private-note', 'Only what the customer chose to send: why the date matters stays with them.'));

  if ((v.withdrawn || []).length) {
    out.push(section('Withdrawn', v.withdrawn.map((l) => `${l.name || l.carrier} ${l.ref} (${l.from} → ${l.to})`).join(' · ') + ' · booked, then cancelled with the carrier'));
  }
  const pending = a.status === 'pending';
  if (pending) {
    // A booking that stopped part way: the legs booked stay booked, and
    // booking again resumes after them. Rejecting it cancels them first.
    const booked = (v.steps || []).filter((s) => s.booked).length;
    out.push(section('Plan', v.planError ? 'no lane serves this order'
      : booked ? `${booked} of ${v.steps.length} legs booked · booking again resumes after them` : 'from the routing guide (forwarder.lanes) · nothing booked yet'));
    if (v.planError) out.push(el('div', 'empty', v.planError));
    (v.steps || []).forEach((s, i) => {
      out.push(legRow(s.mode, s.name, s.carrier, s.edi, `${s.from} → ${s.to}`, `${s.fromFacility} → ${s.toFacility}`, s.booked ? { state: s.booked + ' · booked', docs: '' } : null));
      if (s.freight && !s.booked) out.push(freightCard(v, s, i + 1));
    });
    const blocked = (v.steps || []).find((s) => s.freight && !s.booked && s.freight.status !== 'ok');
    const act = el('div', 'actions');
    const reject = el('button', null, 'Reject order…');
    reject.onclick = () => rejectDialog(v, booked);
    act.appendChild(reject);
    if ((v.steps || []).length) {
      const book = el('button', 'primary', `${booked ? 'Resume booking' : 'Confirm plan & book'} ${v.steps.length - booked} leg${v.steps.length - booked === 1 ? '' : 's'}…`);
      book.onclick = () => bookOrderDialog(v);
      if (blocked) {
        book.disabled = true;
        book.dataset.tip = blocked.freight.status === 'below' ? `${blocked.name}'s rate is below the ANTT floor: change it first.` : `${blocked.name}'s floor couldn't be checked: try again shortly.`;
      }
      act.appendChild(book);
    }
    out.push(act);
  } else if (a.status === 'rejected') {
    out.push(section('Rejected', (a.messages || []).map((m) => m.detail).join(' ')));
  } else {
    out.push(section('Journey', `${v.legs.length} legs booked · ${v.shipper} has the journey`));
    v.legs.forEach((l, i) => {
      const j = (v.journey.legs || [])[i] || {};
      const st = [l.unsigned ? (l.carrierRef || l.ref) : l.ref, j.status].filter(Boolean).join(' · ');
      const docs = [l.vessel, ...(l.documents || []), ...(l.containers || [])].filter(Boolean).join(' · ');
      const late = j.plannedArrival && j.arrival?.at && j.arrival.at > j.plannedArrival
        ? Math.round((new Date(j.arrival.at) - new Date(j.plannedArrival)) / 36e5) : 0;
      out.push(legRow(l.mode, l.name, l.carrier, l.unsigned, `${l.from} → ${l.to}`,
        `${dayTime(l.departure)} → ${dayTime(l.arrival)}`, { state: st, docs, late }));
    });
    if (v.proposals.length) {
      out.push(section('Proposals', "what a leg's delay calls for · nothing is sent until you confirm"));
      for (const p of [...v.proposals].reverse()) out.push(proposalCard(v, p));
    }
  }
  return out;
}

function legRow(mode, name, carrier, edi, route, sub, booked) {
  const row = el('div', 'leg');
  row.appendChild(el('span', 'mode', MODE[mode] || mode.toUpperCase()));
  const who = el('span', 'carrier');
  const n = el('b', null, name);
  n.dataset.tip = carrier;
  who.append(n, el('br'), reachTag(edi));
  const r = el('span', 'route', route);
  r.appendChild(el('span', 'sub', sub));
  row.append(who, r);
  if (booked) {
    const st = el('span', 'legstate', '✓ ' + booked.state);
    if (booked.late) st.appendChild(el('span', 'late', ` +${booked.late} h`));
    st.appendChild(el('span', 'sub', booked.docs));
    row.appendChild(st);
  }
  return row;
}

// freightCard is a Brazilian truck leg's rate against the ANTT's floor:
// the route, its tolls (a Vale-Pedágio, apart from the freight), the
// floor and its source, and the rate's change behind a dialog.
function freightCard(v, s, leg) {
  const f = s.freight, fl = f.floor;
  const box = el('div', 'freight ' + f.status);
  const head = el('div', 'freight-head');
  const chip = { ok: ['fam-tnt', 'at or above the ANTT floor'], below: ['fam-error', `${brl(f.shortfall)} below the ANTT floor`], unchecked: ['pending', 'floor not checked'] }[f.status] || ['pending', f.status];
  head.append(el('b', null, `Freight · leg ${leg}`), el('span', 'chip ' + chip[0], chip[1]));
  if (fl) head.appendChild(el('span', 'dim', 'source: ' + fl.source));
  box.appendChild(head);
  const grid = el('dl', 'facts');
  const fact = (k, val, cls) => { if (val) { grid.append(el('dt', null, k)); grid.append(el('dd', cls || null, val)); } };
  fact('Route', `${f.route.join(' → ')}` + (fl ? ` · ${fl.distanceKm.toLocaleString('pt-BR')} km · ${Math.round(fl.duration / 36e11 * 10) / 10} h` : ''));
  fact('Priced as', `${f.axles} axles · ${f.loadType.replace(/_/g, ' ')} · table ${f.table}`);
  if (fl) {
    fact('ANTT floor', `${brl(fl.freight)} (of which ${brl(fl.loadUnload)} loading and unloading) · ${fl.resolution}`);
    const tolls = fl.tolls || [];
    fact('Tolls', tolls.length ? `${brl(fl.tollTotal)} · ${tolls.length} plaza${tolls.length === 1 ? '' : 's'}: ${tolls.map((t) => t.name).join(', ')} · paid ahead as a Vale-Pedágio` : 'none on this route');
  }
  fact('Our rate', brl(f.rate) + (f.laneRate ? ` (set for this order; the lane's is ${brl(f.laneRate)})` : ' (the lane’s, from the routing guide)'), f.status === 'below' ? 'bad' : null);
  if (f.error) fact('Why', f.error);
  box.appendChild(grid);
  if (fl) {
    const act = el('div', 'actions');
    const change = el('button', f.status === 'below' ? 'primary' : null, 'Change rate…');
    change.onclick = () => rateDialog(v, s, leg);
    act.appendChild(change);
    box.appendChild(act);
  }
  box.appendChild(el('div', 'fineprint', 'The rate stays with us: no carrier or customer sees it.'));
  return box;
}

function rateDialog(v, s, leg) {
  const f = s.freight, input = el('input');
  input.type = 'number'; input.step = '0.01'; input.min = '0';
  input.value = String(Math.max(f.rate, aboveFloor(f.floor.freight)));
  const r = el('div', 'row');
  r.append(el('span', 'k', 'New rate (R$):'), input);
  modal(`Freight · leg ${leg} · ${s.name}`, [['Order:', v.orderRef], ['Route:', f.route.join(' → ')], ['ANTT floor:', `${brl(f.floor.freight)} · ${f.floor.resolution}`],
    ['Tolls:', `${brl(f.floor.tollTotal)} as a Vale-Pedágio, on top`], ['Now:', brl(f.rate)]],
  'Set this rate for the order? It stays with us; booking the leg is still your step.', 'Set rate', async () => {
    await api(`orders/${encodeURIComponent(v.orderRef)}/rate`, { method: 'POST', body: JSON.stringify({ leg, rate: Number(input.value) }) });
    loadOrders();
  }, [r]);
}

// The orders' sends: each needs a person's yes in a dialog.
function bookOrderDialog(v) {
  const q = v.request, sends = el('div');
  v.steps.forEach((s, i) => {
    const box = el('div', 'send');
    const head = el('div', 'send-head');
    head.append(el('b', null, `${i + 1} · ${s.name}`), el('span', 'dim', s.carrier), s.booked
      ? el('span', 'reach', 'booked already · ' + s.booked + ' · not sent again')
      : el('span', 'reach ' + (s.edi ? 'edi' : 'ulp'), s.edi ? 'EDIFACT IFTMIN · UNSIGNED' : (s.mode === 'ocean' ? 'ocean.booking.request' : 'road.booking.request') + ' · SIGNED'));
    box.append(head, el('div', 'send-body', `${MODE[s.mode] || s.mode} ${s.from} → ${s.to} · ${s.fromFacility} → ${s.toFacility} · ${equipment(q.containers)}` +
      (i === v.steps.length - 1 && q.place_of_delivery?.latest ? ` · deliver by ${dayTime(q.place_of_delivery.latest)}` : '')));
    sends.appendChild(box);
  });
  sends.appendChild(el('div', 'fineprint', 'Each carrier sees only its leg: never the other legs, the customer beyond the consignee, or what they pay. ' +
    `Then ${v.shipper} gets the journey, signed.`));
  const left = v.steps.filter((s) => !s.booked).length, legs = `${left} leg${left === 1 ? '' : 's'}`;
  modal(`Confirm plan · ${v.orderRef}`, [], `Book the ${legs}, in order? Your node signs and sends each booking.`,
    `Book ${legs}`, async () => {
      await api(`orders/${encodeURIComponent(v.orderRef)}/book`, { method: 'POST', body: '{}' });
      loadOrders();
    }, [sends]);
}

function rejectDialog(v, booked) {
  const reason = el('input');
  reason.maxLength = 280;
  reason.value = "We can't take this order on these dates.";
  const r = el('div', 'row');
  r.append(el('span', 'k', 'Reason:'), reason);
  const legs = booked ? ` First, the ${booked} leg${booked === 1 ? '' : 's'} booked ${booked === 1 ? 'is' : 'are'} cancelled with ${booked === 1 ? 'its carrier' : 'their carriers'}, last first; if a carrier keeps its leg (its cargo has moved), the order stays pending.` : '';
  modal(`Reject · ${v.orderRef}`, [['Customer:', v.shipper]], `Reject this order? ${v.shipper} gets a signed rejection with this reason.${legs}`,
    'Reject order', async () => {
      await api(`orders/${encodeURIComponent(v.orderRef)}/reject`, { method: 'POST', body: JSON.stringify({ reason: reason.value }) });
      loadOrders();
    }, [r]);
}

function proposalCard(v, p) {
  const leg = v.legs[p.leg - 1] || {}, am = p.amendment || {};
  const open = p.status === 'proposed';
  const box = el('div', 'proposal' + (open ? '' : ' closed'));
  box.appendChild(el('div', 'proposal-title', `Re-time leg ${p.leg} · ${leg.name || p.carrier}` + (open ? '' : ` · ${p.status}`)));
  box.appendChild(el('div', null, p.why));
  const diff = el('div', 'diff');
  const line = (k, was, now) => { if (now) diff.append(el('span', 'dim', k), el('span', 'was', dayTime(was)), el('span', null, '→'), el('span', 'now', dayTime(now))); };
  line('collect from', leg.departure, am.pickup?.earliest);
  line('deliver', leg.arrival, am.delivery?.earliest || am.delivery?.latest);
  box.appendChild(diff);
  if (open) {
    box.appendChild(el('div', 'fineprint', `Sends ${leg.name || p.carrier} a signed road.booking.amendment. The customer already has the delay alert.`));
    const act = el('div', 'actions');
    const dismiss = el('button', null, 'Dismiss');
    dismiss.dataset.tip = 'Close it without sending anything: the leg stays as booked.';
    dismiss.onclick = async () => {
      try { await api(`proposals/${encodeURIComponent(p.id)}/dismiss`, { method: 'POST', body: '{}' }); } catch (e) { setStatus('bad', e.message); }
      loadOrders();
    };
    const ok = el('button', 'primary', `Confirm · send amendment to ${leg.name || p.carrier}…`);
    ok.onclick = () => modal(`Amend leg ${p.leg} · ${leg.name || p.carrier}`,
      [['Booking:', p.legRef], ['Collect from:', dayTime(am.pickup?.earliest)], ['Deliver:', dayTime(am.delivery?.earliest || am.delivery?.latest)], ['Why:', p.why]],
      `Send this amendment? Your node signs it and sends it to ${p.carrier}.`, 'Send amendment', async () => {
        await api(`proposals/${encodeURIComponent(p.id)}/confirm`, { method: 'POST', body: '{}' });
        loadOrders();
      });
    act.append(dismiss, ok);
    box.appendChild(act);
  } else if (p.outcome) {
    box.appendChild(el('div', 'fineprint', p.outcome));
  }
  return box;
}

// renderDock is the site's own dock appointment for a shipment: where and
// when, and the standing policy that last booked or moved it, if one did.
function renderDock(box, a) {
  if (!a) return;
  const fmt = (t) => new Date(t).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  const row = el('div', 'summary-line dock-line');
  row.append(el('span', 'mode', 'DOCK'), ' ', el('b', null, a.dock || ''), ` · ${fmt(a.start)} – ${fmt(a.end)} · ${a.source || ''}`);
  if (a.policy) row.appendChild(el('span', 'sub', ' · by the ' + a.policy));
  row.dataset.tip = "Your own dock schedule's appointment for this truck: never sent to anyone.";
  box.replaceChildren(row);
}

// renderBackOffice is what the carrier's own systems behind its node did
// for a truck leg: its people's view, never sent to the shipper.
const SYSTEM = { tms: 'TMS', risk: 'RISK', tolls: 'TOLLS' };
function renderBackOffice(box, steps) {
  if (!steps.length) return;
  const wrap = el('div', 'backoffice');
  const head = el('div', 'backoffice-head');
  head.append(el('b', null, 'Behind this node'), el('span', 'dim', 'our systems · the shipper sees only the signed confirmation and the document keys'));
  wrap.appendChild(head);
  for (const s of steps) {
    const row = el('div', 'bo-row');
    row.append(el('span', 'mode', SYSTEM[s.system] || String(s.system || '').toUpperCase()), el('span', 'bo-source', s.source || ''));
    const what = el('span', 'bo-what', s.action || '');
    if (s.reference) what.appendChild(el('span', 'sub', s.reference + (s.detail ? ' · ' + s.detail : '')));
    row.appendChild(what);
    wrap.appendChild(row);
  }
  box.replaceChildren(wrap);
}

// ── a door-to-door shipment: the journey, leg by leg, and each event's
// source (signed by a leg's carrier, or an unsigned EDI status) ──────
function renderLegs(box, j) {
  if (!j || !Array.isArray(j.legs) || !j.legs.length) return;
  const n = j.legs.length;
  const wrap = el('div', 'legs-strip');
  wrap.style.setProperty('--n', n);
  const rails = el('div', 'rails');
  const atSea = j.position?.between;
  j.legs.forEach((l) => {
    const rail = el('div', 'rail ' + ({ done: 'done', 'in progress': 'live', delayed: l.departure?.kind === 'actual' ? 'live late' : 'late' }[l.status] || 'next'));
    if (atSea && atSea[0] === l.from.unLocode && atSea[1] === l.to.unLocode) {
      const m = el('span', 'mover', '▶');
      m.dataset.tip = l.mode === 'ocean' ? 'At sea (estimated: no message says exactly where)' : 'On the road (estimated)';
      rail.appendChild(m);
    }
    const lbl = el('div', 'lbl');
    lbl.append(el('span', 'mode-tag', MODE[l.mode] || l.mode), ' ', el('b', null, l.carrier || ''));
    lbl.dataset.tip = [l.carrier, l.vessel, `${l.from.unLocode} → ${l.to.unLocode}`, l.status].filter(Boolean).join(' · ');
    rail.appendChild(lbl);
    rails.appendChild(rail);
  });
  const stops = el('div', 'stops');
  const stop = (p, m, planned, reached) => {
    const s = el('div', 'stop' + (reached ? ' done' : ''));
    s.append(el('div', 'pin'), el('div', 'name', p.name || p.unLocode));
    const w = el('div', 'when');
    // Moved to another day: the plan's struck through. (Hours early or late
    // on the same day aren't news on a journey of weeks.)
    if (planned && m?.at && day(m.at) !== day(planned)) { w.append(el('span', 'was', day(planned)), ' ', el('b', null, day(m.at))); } else if (m?.at) w.textContent = momentText(m);
    s.appendChild(w);
    s.dataset.tip = p.unLocode;
    return s;
  };
  stops.appendChild(stop(j.legs[0].from, j.legs[0].departure, null, j.legs[0].departure?.kind === 'actual'));
  j.legs.forEach((l) => stops.appendChild(stop(l.to, l.arrival, l.plannedArrival, l.status === 'done')));
  wrap.append(rails, stops);
  box.replaceChildren(wrap);
}

// provenance is an event's source, as its re-issuer says.
function provenance(body) {
  const src = (body.derived_from || [])[0];
  if (!src) return null;
  const p = el('span', 'prov ' + (src.signed ? 'signed' : 'unsigned'), src.signed ? '✓ signed source' : '◇ unsigned source');
  p.appendChild(el('span', 'src', src.signed ? src.id : [src.source, src.reference].filter(Boolean).join(' · ')));
  p.dataset.tip = src.signed ? "Re-issued from a leg carrier's signed message, which the forwarder holds."
    : 'From a message nobody signed (an EDI file): the forwarder vouches for it. Ask it for the source if it matters.';
  return p;
}

function renderOrderEvents(box, recs) {
  const list = el('div', 'evlist');
  for (const r of [...recs].reverse().slice(0, 12)) {
    const b = r.body || {};
    const alert = r.intent === 'ulp.tnt.alert';
    const src = (b.derived_from || [])[0];
    const row = el('div', 'ev' + (alert ? ' alert' : src && !src.signed ? ' unsigned' : ''));
    // An alert has no event time of its own (when it was sent is the
    // forwarder's clock, not the journey's): it says what it moved.
    row.appendChild(el('span', 't', alert ? 'ALERT' : dayTime(b.event_date_time)));
    const what = el('span', 'what');
    what.appendChild(el('b', null, alert ? `${b.kind}: delivery ${day(b.previous_eta)} → ${day(b.new_eta)}` : `${b.event_type_code} ${b.event_classifier_code} · ${b.un_location_code}`));
    what.appendChild(el('span', 'sub', alert ? b.summary : (b.description || b.facility_name || '')));
    row.appendChild(what);
    const p = provenance(b);
    if (p) row.appendChild(p);
    row.onclick = () => openMessageFor(b.event_id || b.alert_id);
    list.appendChild(row);
  }
  box.replaceChildren(list);
}

function renderLeftNow() {
  if (currentTab() === 'assistant') { state.chats[state.activeSide] ? renderChat() : loadChat(); } else { loadLeft(); }
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

// Each side the persona acts in may have its own assistant: the
// shipper's on the customer side, the carrier's on the operator side,
// each with its own conversation (state.chats[side]).
const ASSISTANT = {
  customer: { hint: 'shipper role · it drafts; you decide', empty: 'Ask about a carrier, book a shipment, or check on one.',
    placeholder: 'Ask about a carrier, book a shipment, or check on one…' },
  operator: { hint: 'carrier role · it drafts; you decide', empty: 'Ask about our bookings and voyages, or prepare a delay.',
    placeholder: 'Ask about bookings or a voyage, or prepare a delay…' },
  // A forwarder's operator side is its desk.
  desk: { hint: 'forwarder’s desk · it drafts; you decide', empty: 'Ask about our orders, their legs and proposals: “what should we do about MF-0007?”',
    placeholder: 'Ask about orders, legs and proposals…' },
  // A receiver (no shipper role) follows what others ship to it.
  receiver: { hint: 'receiver role · it drafts; you decide', empty: 'Ask about the deliveries coming to you: “where is the truck from Vale Verde?”',
    placeholder: 'Ask about a delivery coming to you…' },
};
// receiverOnly: the persona's customer side is a receiver's, without the
// shipper role.
const receiverOnly = () => !state.me.persona.roles.includes('shipper');
// aq is an assistant route for side.
const aq = (side, path = '') => 'assistant' + path + (side === 'operator' ? '?side=operator' : '');

async function loadChat() {
  const side = state.activeSide;
  try { state.chats[side] = await api(aq(side)); renderChat(); } catch (e) { setStatus('bad', e.message); }
}

// draftDialog opens a draft's confirmation dialog: a booking or a delay.
function draftDialog(d, side) {
  if (d.disruption) disruptionDraftDialog(d, side);
  else if (d.order) orderDraftDialog(d, chatVia(side));
  else if (d.proposal) proposalDraftDialog(d, chatVia(side));
  else if (d.rate) rateDraftDialog(d, chatVia(side));
  else if (d.dock) dockDraftDialog(d, chatVia(side));
  else if (d.road) roadDraftDialog(d, chatVia(side));
  else if (d.grant) grantDraftDialog(d, chatVia(side));
  else if (d.question) questionDraftDialog(d, chatVia(side));
  else bookingDialog(d, chatVia(side));
}

// questionDraftDialog is a drafted question: the exact text the operator
// will read, signed, and that nothing private is in it.
function questionDraftDialog(d, via) {
  const q = d.question;
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Question draft ${d.draftId}`, [['To:', q.operator], ['About:', q.bookingRef], ['Question:', q.question],
    ['Not sent:', 'your private dates and notes: checked, none is in it', 'notsent']],
  `Ask ${q.operator} this? Your node signs it and sends it; its agent answers from the booking's records.`, 'Ask', () => via.send(d), [discard]);
}

// proposalDraftDialog is a drafted proposal's confirmation: the amendment
// to the leg's carrier, as the Orders tab's dialog shows it.
function proposalDraftDialog(d, via) {
  const p = d.proposal, am = p.amendment || {};
  const rows = [['Order:', p.orderRef], ['Leg:', `${p.leg} · ${p.carrierName} (${p.carrier})`], ['Booking:', p.legRef]];
  if (am.pickup) rows.push(['Collect from:', `${dayTime(am.pickup.earliest)}` + (p.wasPickup ? ` (was ${dayTime(p.wasPickup)})` : '')]);
  if (am.delivery) rows.push(['Deliver:', `${dayTime(am.delivery.earliest || am.delivery.latest)}` + (p.wasDelivery ? ` (was ${dayTime(p.wasDelivery)})` : '')]);
  rows.push(['Why:', p.why]);
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Amend leg ${p.leg} · ${p.carrierName}`, rows, `Send this amendment? Your node signs it and sends it to ${p.carrier}.`, 'Send amendment',
    () => via.send(d), [discard]);
}

// stopText is a truck's stop: its facility, address and UN/LOCODE (its
// Plus Code has a row of its own).
const stopText = (s) => [s.facility, s.address && [s.address.street, s.address.locality, s.address.region].filter(Boolean).join(', '),
  s.un_location_code].filter(Boolean).join(' · ');

// roadDraftDialog is a drafted truckload: all that goes to the carrier,
// and what stays here.
function roadDraftDialog(d, via) {
  const q = d.road;
  const rows = [['Carrier:', d.to], ...(d.newPartner ? [['', 'A new partner: not in your address book, and never booked with before. Check the domain.', 'newpartner']] : []),
    ['Pickup:', stopText(q.pickup) + (q.pickup.earliest ? ' · from ' + dayTime(q.pickup.earliest) : '')]];
  if (q.pickup.plus_code) rows.push(['Plus Code:', q.pickup.plus_code + ' (Google Open Location Code: the loading point, no street address needed)', 'sent']);
  rows.push(['Delivery:', stopText(q.delivery) + (q.delivery.latest ? ' · by ' + dayTime(q.delivery.latest) : '')]);
  if (q.delivery.plus_code) rows.push(['Plus Code:', q.delivery.plus_code + ' (the delivery point)', 'sent']);
  if (q.consignee) rows.push(['Consignee:', party(q.consignee)]);
  rows.push(['Goods:', `${equipment(q.containers)}, ${(q.containers[0] || {}).gross_weight_kg} kg · ${q.cargo.description} · HS ${q.cargo.hs_code}`]);
  rows.push(['Reference:', q.shipper_reference]);
  if (q.subscription) rows.push(['Updates:', `${(q.subscription.intents || []).join(', ')} → ${q.subscription.endpoint}`]);
  for (const [k, v] of Object.entries(d.notSent || {})) rows.push(['Not sent:', `${NOT_SENT[k] || k}, ${v} (it stays with us)`, 'notsent']);
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Truckload draft ${d.draftId}`, rows, `Book this truck? It is signed by your node and sent to ${d.to}.`, 'Book truck', () => via.send(d), [discard]);
}

// grantDraftDialog is a drafted delegation: who may follow which booking,
// how, until when.
function grantDraftDialog(d, via) {
  const g = d.grant;
  const what = { 'tnt:read': 'ask the carrier about it', 'tnt:subscribe': 'receive its signed events and alerts' };
  const rows = [['Booking:', `${g.bookingRef} with ${g.carrier}`], ['Share with:', g.grantee],
    ...(d.newPartner ? [['', 'Not in your address book: check the domain.', 'newpartner']] : []),
    ['It may:', g.scope.map((s) => what[s] || s).join(' and ')], ['For:', `${g.days} day${g.days === 1 ? '' : 's'}`]];
  if (g.note) rows.push(['Note:', g.note]);
  rows.push(['Not shared:', 'your private dates, notes and everything else you hold', 'notsent']);
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Share ${g.bookingRef}`, rows, `Grant ${g.grantee} this? Your node signs the delegation; ${g.carrier} checks it on every use.`, 'Grant access', () => via.send(d), [discard]);
}

// dockDraftDialog is a drafted move of one of our dock appointments: from
// where to where, in the site's own time, in our own schedule.
const siteTime = (iso) => {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})/.exec(iso || '');
  if (!m) return iso || '';
  const d = new Date(m[1] + 'T00:00:00Z');
  return `${d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short', timeZone: 'UTC' })} ${m[2]}`;
};
function dockDraftDialog(d, via) {
  const k = d.dock;
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Dock appointment ${k.appointmentId}`, [['Shipment:', k.shipperRef], ['Site:', `${k.site} · ${k.source}`],
    ['Now:', `${k.wasDock} · ${siteTime(k.wasStart)}–${siteTime(k.wasEnd).slice(-5)}`], ['Move to:', `${k.dock} · ${siteTime(k.start)}–${siteTime(k.end).slice(-5)}`],
    ['Not sent:', 'our dock schedule: no carrier or forwarder sees it', 'notsent']],
  'Move this appointment? It changes in our dock schedule; nothing is sent.', 'Move appointment', () => via.send(d), [discard]);
}

// rateDraftDialog is a drafted freight rate: what it replaces, and the
// floor and tolls it was checked against.
function rateDraftDialog(d, via) {
  const r = d.rate;
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Freight · leg ${r.leg} · ${r.carrierName}`, [['Order:', r.orderRef], ['Route:', r.route], ['Now:', brl(r.was)],
    ['New rate:', brl(r.rate)], ['ANTT floor:', `${brl(r.floor)} · ${r.resolution} · ${r.source}`], ['Tolls:', `${brl(r.tollTotal)} as a Vale-Pedágio, on top`],
    ['Not sent:', 'the rate stays with us: no carrier or customer sees it', 'notsent']],
  'Set this rate for the order? Booking the leg is still your step, in the Orders tab.', 'Set rate', () => via.send(d), [discard]);
}

// orderDraftDialog is a drafted door-to-door order's confirmation: all
// that goes to the forwarder, and the private context that stays here.
const NOT_SENT = { requiredBy: 'your deadline', receivingSlot: 'your receiving slot', notes: 'your note' };
function orderDraftDialog(d, via) {
  const q = d.order;
  const rows = [
    ['Forwarder:', d.to], ...(d.newPartner ? [['', 'A new partner: not in your address book, and never ordered with before. Check the domain.', 'newpartner']] : []),
    ['Incoterms:', `${q.incoterms.rule} ${q.incoterms.version} · ${q.incoterms.named_place}`],
    ['Shipper:', party(q.shipper)], ['Consignee:', party(q.consignee)],
    ['From (door):', place(q.place_of_receipt) + (q.place_of_receipt.earliest ? ' · ready from ' + dayTime(q.place_of_receipt.earliest) : '')],
    ['To (door):', place(q.place_of_delivery)],
    ['Deliver by:', `${dayTime(q.place_of_delivery.latest)} UTC (sent: the forwarder plans to it)`, 'sent'],
    ['Goods:', [equipment(q.containers) + `, ${(q.containers[0] || {}).gross_weight_kg} kg each`, `${q.cargo.description} · HS ${q.cargo.hs_code}`].join(' · ')],
    ['Reference:', q.shipper_reference],
  ];
  if (q.subscription) rows.push(['Updates:', `${(q.subscription.intents || []).join(', ')} → ${q.subscription.endpoint}`]);
  // The deadline first, then the slot, then the notes.
  const notSent = Object.entries(d.notSent || {}).sort(([a], [b]) => Object.keys(NOT_SENT).indexOf(a) - Object.keys(NOT_SENT).indexOf(b));
  for (const [k, v] of notSent) rows.push(['Not sent:', `${NOT_SENT[k] || k}, ${v} (it stays with us)`, 'notsent']);
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Order draft ${d.draftId}`, rows, `Send this order? It is signed by your node and sent to ${d.to}.`, 'Send order', () => via.send(d), [discard]);
}

function renderChat() {
  if (currentTab() !== 'assistant') return;
  const side = state.activeSide, chat = state.chats[side], words = ASSISTANT[side === 'operator' && state.me.orders ? 'desk' : side === 'customer' && receiverOnly() ? 'receiver' : side];
  if (!chat || !words) return;
  // One turn at a time in this page: every side's composer waits, and the
  // turn's own side shows its progress.
  const busy = !!state.thinking, mine = state.thinking === side;
  const left = $('left');
  const keep = left.querySelector('textarea')?.value || '';
  left.className = 'panel tall ai';
  const bar = el('div', 'panel-bar');
  bar.append(el('span', 'panel-title', 'Assistant'), el('span', 'hint', words.hint));
  const body = el('div', 'panel-body chat');
  const lines = chat.lines || [];
  const inbox = side === 'customer' ? inboxView() : null;
  if (inbox) body.appendChild(inbox);
  if (!lines.length) body.appendChild(el('div', 'empty', words.empty));
  for (const l of lines) {
    const t = el('div', 'turn ' + (l.who === 'you' ? 'you' : l.who === 'note' ? 'note' : 'ai'));
    if (l.who !== 'note') t.appendChild(el('div', 'who', (l.who === 'you' ? 'YOU' : 'ASSISTANT') + ' · ' + clock(l.time)[0]));
    for (const tool of l.tools || []) t.appendChild(toolView(tool, false));
    t.appendChild(l.who === 'assistant' ? markdown(l.text) : el('div', 'text', l.text));
    for (const d of l.drafts || []) {
      if (!(chat.drafts || []).some((x) => x.draftId === d.draftId)) continue;
      const b = el('button', 'draftlink', `Review draft ${d.draftId}…`);
      b.onclick = () => draftDialog(d, side);
      t.appendChild(b);
    }
    body.appendChild(t);
  }
  if (mine) {
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
  ta.placeholder = words.placeholder + '  (Enter sends, Shift+Enter: new line)';
  ta.value = keep;
  ta.disabled = busy;
  ta.maxLength = 4000;
  const send = el('button', null, 'Send');
  send.disabled = busy;
  const go = () => { const v = ta.value.trim(); if (v) { ta.value = ''; chatTurn(v); } };
  send.onclick = go;
  ta.onkeydown = (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); go(); } };
  comp.append(ta, send);
  left.replaceChildren(bar, body, comp);
  body.scrollTop = body.scrollHeight;
  if (!busy) ta.focus();
}

// watchProgress polls what side's assistant is doing while a turn runs,
// and redraws the chat when it changes. It stops when the turn is over.
function watchProgress(side) {
  state.progress = null;
  const turn = state.turnSeq = (state.turnSeq || 0) + 1;
  const live = () => state.thinking === side && state.turnSeq === turn; // this turn, not a later one
  let last = '';
  const tick = async () => {
    if (!live()) return;
    try {
      const p = await api(aq(side, '/progress'));
      const key = JSON.stringify(p);
      if (live() && p.working && key !== last) { last = key; state.progress = p; renderChat(); }
    } catch { return; /* signed out, say: the turn itself reports failures */ }
    if (live()) setTimeout(tick, 600);
  };
  setTimeout(tick, 300);
}

async function chatTurn(text) {
  const side = state.activeSide, gen = state.gen;
  const chat = state.chats[side];
  if (state.thinking || !chat) return;
  state.thinking = side;
  watchProgress(side);
  chat.lines = [...(chat.lines || []), { who: 'you', text, time: new Date().toISOString() }];
  renderChat();
  try {
    const r = await api(aq(side, '/turn'), { method: 'POST', body: JSON.stringify({ text }) });
    if (gen !== state.gen) return; // signed out meanwhile: not this session's
    state.chats[side] = r;
  } catch (e) {
    if (gen !== state.gen) return;
    chat.lines.push({ who: 'note', text: e.message, time: new Date().toISOString() });
  } finally {
    if (gen === state.gen) {
      state.thinking = false;
      state.progress = null;
      renderChat();
    }
  }
  if (gen !== state.gen) return;
  // A new draft opens its dialog, as in ulpchat.
  const newest = (state.chats[side].drafts || []).at(-1);
  if (newest && !state.shownDrafts?.has(newest.draftId)) { (state.shownDrafts ??= new Set()).add(newest.draftId); draftDialog(newest, side); }
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
    if (d && !state.openedLink) { state.openedLink = true; openInboxDraft(d.draftId); }
  } catch { /* no inbox, or not this persona's */ }
}

// reloadChat fetches side's chat again (after a send) and redraws it if
// it's the one on screen.
async function reloadChat(side) {
  try { state.chats[side] = await api(aq(side)); if (side === state.activeSide) renderChat(); } catch (e) { setStatus('bad', e.message); }
}

// A draft dialog sends and discards through a "via": a side's assistant
// chat (chatVia), or the inbox of drafts from outside agents (INBOX).
const chatVia = (side) => ({
  discard: async (d) => {
    state.chats[side] = await api(aq(side, '/discard'), { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) });
    if (side === state.activeSide) renderChat();
  },
  // The node sends it; then the chat shows the outcome and the assistant's report.
  send: async (d) => {
    try {
      state.chats[side] = await api(aq(side, '/send'), { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) });
    } finally { await reloadChat(side); }
  },
});
const INBOX = {
  discard: async (d) => { state.inbox = (await api('inbox/discard', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) })).drafts || []; renderChat(); },
  send: async (d) => {
    try {
      const r = await api('inbox/send', { method: 'POST', body: JSON.stringify({ draftId: d.draftId }) });
      if (r.note) setStatus('ok', r.note);
    } finally { await loadInbox(); loadMessages(); }
  },
};

// openInboxDraft fetches one inbox draft for this page's dialog (the node
// starts its arming delay for this session then) and opens the dialog.
async function openInboxDraft(id) {
  try {
    const r = await api('inbox/draft?id=' + encodeURIComponent(id));
    const d = (r.drafts || [])[0];
    if (d) bookingDialog(d, INBOX);
  } catch (e) { setStatus('bad', e.message); }
}

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
    b.onclick = () => openInboxDraft(d.draftId);
    row.appendChild(b);
    box.appendChild(row);
  }
  return box;
}

// discardLink is a draft dialog's "Discard this draft".
function discardLink(d, via, close) {
  const discard = el('div', 'fineprint');
  const link = el('button', null, 'Discard this draft');
  link.onclick = async () => {
    try { await via.discard(d); close(); } catch (e) { discard.lastChild.textContent = e.message; }
  };
  discard.append(link, el('span'));
  return discard;
}

// disruptionDraftDialog is a drafted delay's confirmation: exactly what
// goes to each customer, and which bookings it reaches.
function disruptionDraftDialog(d, side) {
  const x = d.disruption;
  const scope = x.voyage ? `the whole voyage: ${x.vessel || ''} ${x.voyage}`.replace('  ', ' ') : `booking ${x.bookingRef}`;
  const rows = [
    ['Delay:', scope], ['How long:', x.delay.replace(/0m0s$/, '').replace(/h$/, ' hours')], ['Reason:', x.reason],
    ['Bookings:', (x.bookings || []).join(', ') + (x.voyage ? '  (as of the draft)' : '')],
    ['', x.voyage ? 'When you publish, the node delays every booking still under way on this voyage, and each customer gets a signed alert with this reason.'
      : 'The customer gets a signed delay alert with this reason, and the arrival estimate moves.', 'scope'],
  ];
  let dlg = null;
  const via = chatVia(side);
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Delay draft ${d.draftId}`, rows, 'Publish this delay? Your node signs and sends the alerts.',
    'Publish delay', () => via.send(d), [discard]);
}

function bookingDialog(d, via = chatVia('customer')) {
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
  let dlg = null;
  const discard = discardLink(d, via, () => dlg.close());
  dlg = modal(`Booking draft ${d.draftId}`, rows, 'Send this booking request? It is signed by your node and sent to ' + d.to + '.',
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
    $('login-foot').hidden = !r.demo;
    if (r.login) {
      $('login-help').textContent = "Sign in with your company Google account. You act in the roles it is allowed; you only ever see those roles' messages and activity.";
      // Google sign-in: a round trip through Google, which drops the
      // page's ?draft= link; it waits here for the way back.
      const b = el('button', 'persona google', 'Sign in with Google');
      b.onclick = () => {
        const d = new URLSearchParams(location.search).get('draft');
        if (d) sessionStorage.setItem(DRAFT_KEY, d);
        location.assign(r.login);
      };
      box.appendChild(b);
    }
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
  state.messages = []; state.lastSeq = 0; state.entries = []; state.open.clear(); state.chats = {}; state.tab = {}; state.shownDrafts = new Set();
  state.initialSeq = null; state.alertCounts = {}; state.summaries = []; state.inbox = []; state.openedLink = false; $('banner').hidden = true;
  const { me } = state;
  $('company').textContent = me.company;
  $('brand').textContent = me.brand || '';
  $('brand').hidden = !me.brand;
  $('domain').textContent = me.domain;
  $('persona').textContent = me.persona.name + (me.persona.title ? ', ' + me.persona.title.split(',')[0] : '');
  $('persona-roles').textContent = `(${me.persona.roles.join(' + ')})`;
  if (me.persona.email) $('persona').dataset.tip = 'Signed in with Google as ' + me.persona.email;
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
// Back from Google sign-in: restore the draft link it started from.
const DRAFT_KEY = 'ulp-draft';
{
  const d = sessionStorage.getItem(DRAFT_KEY);
  sessionStorage.removeItem(DRAFT_KEY);
  if (d && !location.search && /^[A-Z0-9-]{1,40}$/.test(d)) history.replaceState(null, '', '?draft=' + encodeURIComponent(d));
}
start().catch(() => { /* showLogin ran on 401 */ });
