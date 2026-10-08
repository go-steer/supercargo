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

// The presenter page: the node's configured UIs, each in a frame. Each
// frame is that company's own UI on its own node, signed in by itself;
// this page sees none of what they show. Its CSP frames nothing else.
'use strict';

const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};

function setTheme(name) {
  document.body.dataset.theme = name;
  try { localStorage.setItem('ulp-theme', name); } catch { /* private mode */ }
}
try { setTheme(localStorage.getItem('ulp-theme') || 'grayscale-dark'); } catch { setTheme('grayscale-dark'); }
document.getElementById('theme').onclick = () =>
  setTheme(document.body.dataset.theme === 'grayscale-dark' ? 'grayscale-light' : 'grayscale-dark');
document.getElementById('flat').onclick = () => document.getElementById('room').classList.toggle('flat');

(async () => {
  const room = document.getElementById('room');
  const res = await fetch('api/presenter', { credentials: 'same-origin' });
  const { panels } = await res.json();
  panels.forEach((p, i) => {
    const stage = el('section', 'stage');
    // Angle toward the middle: left panels turn right, right panels left.
    const tilt = panels.length > 1 ? (((panels.length - 1) / 2 - i) * 7) : 0;
    stage.style.transform = `rotateY(${tilt}deg)`;
    const bar = el('div', 'stage-bar');
    const open = el('a', null, 'open ↗');
    open.href = p.url;
    open.target = '_blank';
    open.rel = 'noopener';
    bar.append(el('span', 'name', p.name), open);
    const frame = el('iframe');
    frame.src = p.url;
    frame.title = p.name;
    frame.referrerPolicy = 'no-referrer';
    stage.append(bar, frame);
    room.appendChild(stage);
  });
})();
