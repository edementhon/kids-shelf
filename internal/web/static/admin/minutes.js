'use strict';
/* The phone-first Minutes page: one card per kid with big +/− buttons,
   the parent PIN box when no session exists, and balances that refresh on
   their own. Plain JS, no frameworks. It reuses admin.css. */

const $ = id => document.getElementById(id);
let STATE = null;
let refreshTimer = null;
let busy = false;
let pin = '';
let pinBusy = false;
let lockUntil = 0; // keypad lockout after too many wrong tries

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function fmt(n) { return Math.round(n * 10) / 10; }
function cls(n) { return n < 0 ? 'neg' : ''; }

/* setStatus writes the aria-live line; an error gets its own colour. */
function setStatus(text, err) {
  const s = $('status');
  s.textContent = text;
  s.classList.toggle('err', !!err);
}

/* ── API with the parent-PIN gate ────────────────────────────────────── */
async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    setStatus('Kids Shelf did not answer. Check that it is still running, then try again.', true);
    throw e;
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* non-JSON error body */ }
  if (res.status === 401) {
    const err = new Error((data && data.error) || 'Enter the parent PIN.');
    err.status = 401;
    err.setup = !!(data && data.setup);
    throw err;
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || 'That did not work. Try again.');
    err.status = res.status;
    if (data && typeof data.wait === 'number') err.wait = data.wait;
    throw err;
  }
  return data;
}

/* ── signing in ──────────────────────────────────────────────────────── */
function showPin() {
  $('main').hidden = true;
  $('pin').hidden = false;
  renderDots();
  pin = '';
  setPinErr('');
}

function hidePin() {
  $('pin').hidden = true;
  $('main').hidden = false;
}

function renderDots() {
  const dots = $('minutes-dots').children;
  for (let i = 0; i < 4; i++) dots[i].classList.toggle('on', i < pin.length);
  $('minutes-ok').disabled = pinLocked() || pin.length !== 4;
}

function setPinErr(text) {
  const e = $('minutes-pin-err');
  e.textContent = text;
  e.classList.toggle('hidden', !text);
}

function pinLocked() {
  return lockUntil > Date.now();
}

function lockKeypad(wait) {
  lockUntil = Date.now() + wait * 1000;
  const tick = () => {
    const left = Math.ceil((lockUntil - Date.now()) / 1000);
    if (left <= 0) {
      lockUntil = 0;
      setPinErr('');
      renderDots();
      return;
    }
    setPinErr('Too many tries. Wait ' + left + ' more ' + (left === 1 ? 'second' : 'seconds') + '.');
    renderDots();
    setTimeout(tick, 500);
  };
  tick();
}

function tapKey(d) {
  if (pinBusy || pinLocked()) return;
  if (d === 'del') {
    pin = pin.slice(0, -1);
    renderDots();
  } else if (d === 'ok') {
    if (pin.length === 4) submitPin();
  } else if (pin.length < 4) {
    pin += d;
    renderDots();
    if (pin.length === 4) submitPin();
  }
}

async function submitPin() {
  pinBusy = true;
  try {
    await api('/api/admin/pin', { method: 'POST', body: JSON.stringify({ pin }) });
    pin = '';
    hidePin();
    setStatus('');
    await refresh();
  } catch (e) {
    pin = '';
    if (e && e.status === 401 && e.setup) {
      setPinErr('There is no parent PIN yet. Make one on the parent page.');
    } else if (e && e.status === 429) {
      lockKeypad((e.wait && Number(e.wait)) || 30);
    } else if (e && e.status === 401) {
      setPinErr('That PIN is not right. Try again.');
    } else {
      setPinErr((e && e.message) || 'Something went wrong. Try again.');
    }
  } finally {
    pinBusy = false;
    renderDots();
  }
}

/* ── cards and minutes ───────────────────────────────────────────────── */
function render() {
  const wrap = $('cards');
  const kids = STATE.kids;
  if (!kids.length) {
    wrap.innerHTML = '<p class="placeholder">No kids yet. Add the first one on the parent page.</p>';
    return;
  }
  wrap.innerHTML = kids.map(k => {
    const n = esc(k.name);
    const take = d => `<button class="sub" data-act="adj" data-id="${esc(k.id)}" data-d="-${d}" aria-label="Take ${d} minutes away from ${n} today">−${d}</button>`;
    const give = d => `<button class="add" data-act="adj" data-id="${esc(k.id)}" data-d="${d}" aria-label="Give ${n} ${d} more minutes today">+${d}</button>`;
    return `
    <div class="minute-card" data-id="${esc(k.id)}">
      <div class="kidname">${n}</div>
      <div class="bal">
        <span>today <b class="today ${cls(k.today)}">${fmt(k.today)}</b></span>
        <span>saved <b class="saved ${cls(k.saved)}">${fmt(k.saved)}</b></span>
      </div>
      <div class="minute-btns">
        <div class="btns3">${take(10)}${take(5)}${take(1)}</div>
        <div class="btns3">${give(1)}${give(5)}${give(10)}</div>
      </div>
    </div>`;
  }).join('');
}

function updateCard(kid) {
  const card = document.querySelector(`.minute-card[data-id="${esc(kid.id)}"]`);
  if (!card) return;
  const today = card.querySelector('.today');
  const saved = card.querySelector('.saved');
  today.textContent = fmt(kid.today);
  today.className = 'today ' + cls(kid.today);
  saved.textContent = fmt(kid.saved);
  saved.className = 'saved ' + cls(kid.saved);
}

async function adjust(id, delta, name) {
  if (busy) return;
  busy = true;
  try {
    const out = await api('/api/admin/adjust', { method: 'POST', body: JSON.stringify({ kid: id, delta }) });
    const kid = STATE.kids.find(k => k.id === id);
    if (kid) {
      kid.today = out.today;
      kid.saved = out.saved;
      updateCard(kid);
    }
    const sign = delta > 0 ? '+' : '';
    setStatus(`${sign}${delta} for ${name}: ${fmt(out.today)} minutes now`);
  } catch (e) {
    if (e && e.status === 401) { showPin(); return; }
    setStatus((e && e.message) || 'That did not work. Try again.', true);
  } finally {
    busy = false;
  }
}

/* ── loading and refreshing ──────────────────────────────────────────── */
async function refresh() {
  try {
    const st = await api('/api/admin/state');
    STATE = st;
    render();
  } catch (e) {
    if (e && e.status === 401) {
      showPin();
      return;
    }
    setStatus((e && e.message) || 'Could not reach the shelf. Try again in a moment.', true);
  }
}

async function load() {
  try {
    const st = await api('/api/admin/state');
    STATE = st;
    hidePin();
    render();
    startRefresh();
  } catch (e) {
    if (e && e.status === 401) { showPin(); return; }
    setStatus((e && e.message) || 'Could not reach the shelf. Try again in a moment.', true);
  }
}

function startRefresh() {
  clearInterval(refreshTimer);
  refreshTimer = setInterval(() => refresh(), 15000);
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && STATE) refresh();
});

/* ── events ──────────────────────────────────────────────────────────── */
$('minutes-keypad').addEventListener('click', e => {
  const b = e.target.closest('button');
  if (!b) return;
  tapKey(b.dataset.d);
});

$('cards').addEventListener('click', e => {
  const b = e.target.closest('[data-act="adj"]');
  if (!b) return;
  const kid = STATE.kids.find(k => k.id === b.dataset.id);
  if (!kid) return;
  adjust(b.dataset.id, Number(b.dataset.d), kid.name);
});

load();
