// Kids Shelf home: the "Who's watching?" picker, the PIN keypad and the
// video shelf. Plain JS, no frameworks; everything is built with DOM APIs
// so show and episode names can never inject markup.
'use strict';

const $ = (id) => document.getElementById(id);
const EPS_PAGE = 24;
const RETRY_MS = 5000;          // the picker retries a server that is down
const SESSION_KEY = 'ks-session'; // set while a kid is unlocked in this tab
const PLACE_KEY = 'ks-place';     // the open show, search and scroll spot

const state = {
  me: null,        // /api/kid/me result
  balance: { today: 0, saved: 0, total: 0, known: false },
  shows: [],
  kw: [],          // keep-watching items
  recent: [],
  pin: '',         // digits entered on the keypad
  pinKind: 'kid',  // 'kid' or 'grownup'
  pinKid: null,    // kid id when pinKind is 'kid'
  pinName: '',     // kid display name
  pinOpener: null, // the Who's watching tile that opened the keypad
  pinBusy: false,  // a PIN is being checked
  lockUntil: {},   // keypad lockouts: 'kid:<id>' or 'grownup' to a time in ms
  lockTimer: null, // ticks the lockout countdown while the keypad shows
  retryTimer: null, // the next automatic try while the server is down
  gone: false,     // a shelf request answered 401: the session has ended
};

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

// sessionStorage can be switched off; the shelf then simply forgets.
function ssGet(k) {
  try { return sessionStorage.getItem(k); } catch (e) { return null; }
}
function ssSet(k, v) {
  try {
    if (v === null) sessionStorage.removeItem(k);
    else sessionStorage.setItem(k, v);
  } catch (e) { /* nothing to remember with */ }
}

// The toast never blocks a tap (pointer-events: none in the CSS) and goes
// away as soon as the kid touches, types or scrolls.
function toast(s) {
  const t = $('toast');
  t.textContent = s;
  t.hidden = false;
  t._at = Date.now();
  clearTimeout(t._t);
  t._t = setTimeout(hideToast, 3500);
}

function hideToast() {
  const t = $('toast');
  if (t.hidden) return;
  clearTimeout(t._t);
  t.hidden = true;
}

function msg(s) {
  const m = $('msg');
  m.textContent = s || '';
  m.hidden = !s;
}

// fmtLen and fmtCost say an amount of minutes the way a kid reads a clock:
// whole minutes, and "under 1 min" for a very short video.
function fmtLen(min) {
  if (!(min > 0)) return '';
  return min < 1 ? 'Under 1 min' : Math.round(min) + ' min';
}

function fmtCost(min) {
  return min < 1 ? 'under 1 min' : Math.round(min) + ' min';
}

// fmtWait is the lockout countdown as a clock: 0:29, 14:59.
function fmtWait(sec) {
  return Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');
}

async function postJSON(url, body) {
  const r = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  return r;
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

// init asks who is unlocked. A live session opens the shelf; otherwise the
// picker shows. A tab that had a kid unlocked and now has none (the session
// ran out, or the shelf computer restarted) says so instead of silently
// landing on Who's watching.
async function init() {
  clearTimeout(state.retryTimer);
  let r = null;
  try {
    r = await fetch('/api/kid/me');
  } catch (e) { /* showWho explains a shelf that can't be reached */ }
  if (r && r.ok) {
    state.me = await r.json();
    await enterShelf();
    return;
  }
  if (r && r.status === 401 && ssGet(SESSION_KEY)) {
    sessionEnded();
    return;
  }
  showWho();
}

function sessionEnded() {
  resetShelf();
  showWho('Time to pick who\u2019s watching again.');
}

// resetShelf forgets the unlocked kid and empties the shelf behind the picker.
function resetShelf() {
  ssSet(SESSION_KEY, null);
  ssSet(PLACE_KEY, null);
  state.me = null;
  state.kw = [];
  state.gone = false;
  state.balance = { today: 0, saved: 0, total: 0, known: false };
  hideToast();
  msg('');
  $('q').value = '';
  $('q').hidden = true;
  $('places').hidden = true;
  $('sub').hidden = true;
  $('together-btn').hidden = true;
  $('app').textContent = '';
  const band = $('ep-band');
  if (band) band.remove();
  closeGettingReady();
  renderHeader();
}

// showWho shows the picker. notice is a friendly line above the tiles; a
// server that is down or still starting is explained inside the picker,
// with Try again, and retried on its own every few seconds.
async function showWho(notice) {
  clearTimeout(state.retryTimer);
  $('who').hidden = false;
  try {
    const r = await fetch('/api/kids');
    if (!r.ok) { whoDown(r.status === 503); return; }
    const d = await r.json();
    whoMsg(notice || '', false);
    renderPicker(d.kids || []);
  } catch (e) {
    whoDown(false);
  }
}

function whoMsg(text, down) {
  const box = $('who-msg');
  const retry = $('who-retry');
  $('who-msg-text').textContent = text;
  box.classList.toggle('down', down);
  retry.hidden = !down;
  retry.disabled = false;
  retry.textContent = 'Try again';
  box.hidden = !text;
}

function whoDown(starting) {
  $('pick').textContent = '';
  $('pick-more').textContent = '';
  $('who-hint').hidden = true;
  $('pick-places').hidden = true;
  whoMsg(starting
    ? 'The shelf is still waking up. It will be ready in a moment. We will keep trying.'
    : 'The shelf can\u2019t be reached right now. We will keep trying. If it keeps saying this, ask a grown-up to check the shelf computer.', true);
  state.retryTimer = setTimeout(init, RETRY_MS);
}

function renderPicker(kids) {
  const pick = $('pick');
  const more = $('pick-more');
  pick.textContent = '';
  more.textContent = '';
  if (!kids.length) {
    // Fresh install: nobody set up yet. One friendly card leads the
    // grown-up to the parent page instead of a row of dead-end tiles.
    pick.appendChild(setupCard());
    $('who-hint').hidden = true;
    $('pick-places').hidden = true;
    return;
  }
  // The server chooses each kid's animal once and stores it, so every
  // screen and the practice pages show the same one.
  for (const k of kids) {
    pick.appendChild(tile(k.animal, k.name, (b) => openPin('kid', k.id, k.name, b)));
  }
  pick.appendChild(tile('👪', 'Watch together', (b) => openPin('grownup', null, '', b)));
  // Places, not people: their own row under the people.
  more.appendChild(tile('🔢', 'Math Practice', () => { location.href = '/practice'; }));
  more.appendChild(tile('📚', 'Libraries', () => { location.href = '/libraries'; }));
  $('who-hint').hidden = false;
  $('pick-places').hidden = false;
}

function setupCard() {
  const a = document.createElement('a');
  a.className = 'tile setup';
  a.href = '/admin';
  a.appendChild(el('span', 'emoji', '👪'));
  a.appendChild(el('span', null, 'Set up Kids Shelf: open the parent page'));
  return a;
}

function tile(emoji, name, onClick) {
  const b = el('button', 'tile');
  b.type = 'button';
  b.appendChild(el('span', 'emoji', emoji));
  b.appendChild(el('span', 'name', name));
  b.addEventListener('click', () => onClick(b));
  return b;
}

// onPickerArrow moves between the picker tiles with the arrow keys (the
// Pi 400 keyboard): left and right walk the list, up and down jump to the
// nearest tile in the row above or below.
function onPickerArrow(ev) {
  const items = [...$('who').querySelectorAll('.tile')];
  if (!items.length) return;
  ev.preventDefault();
  const cur = document.activeElement;
  const i = items.indexOf(cur);
  if (i < 0) { items[0].focus(); return; }
  if (ev.key === 'ArrowRight') { items[(i + 1) % items.length].focus(); return; }
  if (ev.key === 'ArrowLeft') { items[(i - 1 + items.length) % items.length].focus(); return; }
  const a = cur.getBoundingClientRect();
  const ax = a.left + a.width / 2;
  const ay = a.top + a.height / 2;
  let best = null;
  let bestScore = Infinity;
  for (const b of items) {
    const r = b.getBoundingClientRect();
    const dy = r.top + r.height / 2 - ay;
    if (ev.key === 'ArrowUp' ? dy > -a.height / 2 : dy < a.height / 2) continue;
    const score = Math.round(Math.abs(dy)) * 10000 + Math.abs(r.left + r.width / 2 - ax);
    if (score < bestScore) { bestScore = score; best = b; }
  }
  if (best) best.focus();
}

// ---------------------------------------------------------------------------
// PIN keypad
// ---------------------------------------------------------------------------

// openPin shows the keypad for a kid or for Watch together. opener is the
// tile that opened it, where focus goes back when the keypad closes; the
// "Watch together" link inside the keypad keeps the original tile.
function openPin(kind, kidId, name, opener) {
  if (opener) state.pinOpener = opener;
  state.pinKind = kind;
  state.pinKid = kidId || null;
  state.pinName = name || '';
  state.pin = '';
  $('pin-title').textContent = kind === 'grownup' ? 'Grown-up PIN' : ((name || 'Kid') + '\u2019s PIN');
  setPinMsg(defaultPinMsg(), false);
  // The grown-up keypad has no "Watch together" link to itself; Back
  // returns to Who's watching from either keypad.
  $('pin-alt').hidden = kind === 'grownup';
  if (kind === 'grownup') checkGrownupPin();
  $('pinscreen').hidden = false;
  $('pin-box').classList.remove('shake');
  showLock();
  renderDots();
  $('pin-box').focus();
}

function defaultPinMsg() {
  return state.pinKind === 'grownup'
    ? 'Grown-up mode: watching together is free!'
    : 'Enter your PIN to spend your minutes';
}

// setPinMsg writes the keypad's message line; an error gets its own colour
// and flashes again on every new try, so a second wrong PIN is visible too.
function setPinMsg(text, err) {
  const m = $('pin-msg');
  m.textContent = text;
  m.classList.remove('err');
  if (err) {
    void m.offsetWidth;
    m.classList.add('err');
  }
}

// closePin is Back, Esc and a tap on the backdrop: the typed digits are
// dropped and focus returns to the tile that opened the keypad.
function closePin() {
  if ($('pinscreen').hidden) return;
  $('pinscreen').hidden = true;
  clearTimeout(state.lockTimer);
  state.pin = '';
  renderDots();
  const back = state.pinOpener;
  state.pinOpener = null;
  if (back && back.isConnected) back.focus();
}

// checkGrownupPin explains the keypad when the household has no grown-up
// PIN yet: "That PIN didn't work" would just be a lie.
async function checkGrownupPin() {
  try {
    const r = await fetch('/api/kid/grownup-pin');
    if (r.ok) {
      const d = await r.json();
      if (!d.set && state.pinKind === 'grownup' && !$('pinscreen').hidden) {
        const m = $('pin-msg');
        m.textContent = 'No grown-up PIN yet. Make one on the ';
        const a = el('a', null, 'parent page');
        a.href = '/admin';
        m.appendChild(a);
        m.appendChild(document.createTextNode(', under Settings.'));
      }
    }
  } catch (e) { /* keep the default message */ }
}

function lockKey() {
  return state.pinKind === 'grownup' ? 'grownup' : 'kid:' + state.pinKid;
}

function pinLocked() {
  return (state.lockUntil[lockKey()] || 0) > Date.now();
}

// showLock greys the keys and counts down while this keypad is locked out
// after too many wrong tries; when the wait is over the keys come back.
function showLock() {
  clearTimeout(state.lockTimer);
  const key = lockKey();
  const until = state.lockUntil[key] || 0;
  const left = Math.ceil((until - Date.now()) / 1000);
  const locked = left > 0;
  $('pin-box').classList.toggle('locked', locked);
  for (const b of $('keypad').querySelectorAll('button')) {
    b.disabled = locked || (b.id === 'pin-ok' && state.pin.length !== 4);
  }
  $('pin-wait').hidden = !locked;
  if (!locked) {
    if (until) {
      delete state.lockUntil[key];
      setPinMsg('You can try again now.', false);
    }
    return;
  }
  $('pin-wait').textContent = '⏳ ' + fmtWait(left);
  // A greyed key drops keyboard focus; keep it inside the keypad box.
  const f = document.activeElement;
  if (!$('pinscreen').hidden && (!f || f === document.body || f.disabled)) $('pin-box').focus();
  if (!$('pinscreen').hidden) state.lockTimer = setTimeout(showLock, 250);
}

function renderDots() {
  const dots = $('pin-dots').children;
  for (let i = 0; i < 4; i++) dots[i].classList.toggle('on', i < state.pin.length);
  $('pin-ok').disabled = pinLocked() || state.pin.length !== 4;
}

async function tapKey(d) {
  if (state.pinBusy || pinLocked()) return;
  if (d === 'del') {
    state.pin = state.pin.slice(0, -1);
    renderDots();
  } else if (d === 'ok') {
    if (state.pin.length === 4) await submitPin();
  } else if (state.pin.length < 4) {
    state.pin += d;
    renderDots();
    if (state.pin.length === 4) await submitPin();
  }
}

async function submitPin() {
  const body = state.pinKind === 'grownup'
    ? { pin: state.pin }
    : { kid: state.pinKid, pin: state.pin };
  const url = state.pinKind === 'grownup' ? '/api/kid/watch-together' : '/api/kid/unlock';
  state.pinBusy = true;
  try {
    const r = await postJSON(url, body);
    if (r.ok) {
      $('pinscreen').hidden = true;
      $('who').hidden = true;
      state.pinOpener = null;
      await onUnlocked();
      return;
    }
    const d = await r.json().catch(() => ({}));
    if (r.status === 429) {
      const wait = Number(d.wait) || Number(r.headers.get('Retry-After')) || 30;
      state.lockUntil[lockKey()] = Date.now() + wait * 1000;
    }
    let text = (d && d.error) ? d.error : 'That PIN didn\u2019t work. Try again.';
    if (r.status === 503) text = 'The shelf is still waking up. Try again in a moment.';
    wrongPin(text);
  } catch (e) {
    setPinMsg('Can\u2019t reach the shelf right now. Try again in a moment.', true);
  } finally {
    state.pinBusy = false;
  }
  state.pin = '';
  showLock();
  renderDots();
}

function wrongPin(text) {
  setPinMsg(text, true);
  const box = $('pin-box');
  box.classList.remove('shake');
  void box.offsetWidth;
  box.classList.add('shake');
}

// onKey lets a keyboard drive the page (the Raspberry Pi 400 is a keyboard
// computer). On the keypad digits type, Backspace deletes, Enter submits
// four digits, Esc closes and Tab stays inside. Esc also closes the
// "Still getting ready" panel, and arrow keys walk the picker tiles.
function onKey(ev) {
  if (ev.ctrlKey || ev.metaKey || ev.altKey) return;
  if (!$('pinscreen').hidden) { onPinKey(ev); return; }
  const ready = $('getting-ready');
  if (ready) {
    if (ev.key === 'Escape') { ev.preventDefault(); closeGettingReady(); }
    else if (ev.key === 'Tab') { ev.preventDefault(); ready.querySelector('button').focus(); }
    return;
  }
  if (!$('who').hidden && /^Arrow(Left|Right|Up|Down)$/.test(ev.key)) onPickerArrow(ev);
}

function onPinKey(ev) {
  if (ev.key === 'Escape') {
    ev.preventDefault();
    closePin();
  } else if (/^[0-9]$/.test(ev.key)) {
    ev.preventDefault();
    tapKey(ev.key);
  } else if (ev.key === 'Backspace') {
    ev.preventDefault();
    tapKey('del');
  } else if (ev.key === 'Enter' && state.pin.length === 4) {
    ev.preventDefault();
    tapKey('ok');
  } else if (ev.key === 'Tab') {
    const items = [...$('pin-box').querySelectorAll('button, a[href]')]
      .filter((b) => !b.disabled && b.offsetParent !== null);
    if (!items.length) return;
    const i = items.indexOf(document.activeElement);
    const next = ev.shiftKey ? (i <= 0 ? items.length - 1 : i - 1) : (i + 1) % items.length;
    ev.preventDefault();
    items[next].focus();
  }
}

// ---------------------------------------------------------------------------
// Unlocked shelf
// ---------------------------------------------------------------------------

async function onUnlocked() {
  try {
    const r = await fetch('/api/kid/me');
    if (r.ok) state.me = await r.json();
  } catch (e) { /* keep whatever we had */ }
  await enterShelf();
}

async function enterShelf() {
  clearTimeout(state.retryTimer);
  // A place saved by another kid (a session that ran out without "Not
  // you?") is not this kid's place.
  const who = state.me && state.me.grownup ? 'grownup' : 'kid:' + ((state.me && state.me.kid) || '');
  if (ssGet(SESSION_KEY) !== who) ssSet(PLACE_KEY, null);
  ssSet(SESSION_KEY, who);
  state.gone = false;
  $('who').hidden = true;
  $('q').hidden = false;
  $('places').hidden = false;
  renderHeader();
  await Promise.all([loadShows(), loadBalance(), loadRecent(), loadKeepWatching()]);
  if (state.gone) { sessionEnded(); return; }
  render();
  await restorePlace();
  greet();
}

function greet() {
  if (!state.me) return;
  if (state.me.grownup) {
    toast('Watching together with a grown-up. Free! 💛');
  } else if (noMinutes()) {
    toast('Hi ' + state.me.name + '! No minutes left today. Free shows still work!');
  } else {
    toast('Hi ' + state.me.name + '! Pick something to watch.');
  }
}

// ended reports a 401 from a shelf request: the session is over.
function ended(r) {
  if (r.status !== 401) return false;
  state.gone = true;
  return true;
}

async function loadShows() {
  try {
    const r = await fetch('/api/shows');
    if (ended(r)) return;
    if (!r.ok) { msg('The shelf isn\u2019t ready yet. Try again in a moment.'); return; }
    msg('');
    state.shows = (await r.json()).shows || [];
  } catch (e) { /* keep the previous list */ }
}

async function loadBalance() {
  try {
    const r = await fetch('/api/balance');
    if (ended(r)) return;
    if (r.ok) {
      const d = await r.json();
      state.balance = { today: d.today || 0, saved: d.saved || 0, total: d.total || 0, known: true };
      renderHeader();
    }
  } catch (e) { /* badge stays empty */ }
}

// noMinutes is true once the balance has loaded and nothing is left.
function noMinutes() {
  return state.balance.known && state.balance.total <= 0;
}

async function loadRecent() {
  try {
    const r = await fetch('/api/recent');
    if (ended(r)) return;
    if (r.ok) state.recent = (await r.json()).items || [];
  } catch (e) { state.recent = []; }
}

// almostDone is a saved spot in the last 5 seconds: the episode is
// finished, so it does not belong under Keep watching.
function almostDone(it) {
  return it.duration > 0 && it.pos >= it.duration - 5;
}

async function loadKeepWatching() {
  if (!state.me || state.me.grownup) { state.kw = []; return; }
  try {
    const r = await fetch('/api/keep-watching');
    if (ended(r)) return;
    if (r.ok) state.kw = ((await r.json()).items || []).filter((it) => !almostDone(it));
  } catch (e) { state.kw = []; }
}

function renderHeader() {
  const t = $('tickets');
  t.textContent = '';
  if (!state.me) { t.hidden = true; return; }
  const who = el('span', 'kid');
  const sub = $('sub');
  const noneLeft = !state.me.grownup && noMinutes();
  if (state.me.grownup) {
    who.textContent = '👪 Watching together · Free';
    sub.textContent = 'Pick something to watch together';
  } else {
    // Whole minutes, never below 0: a parent can overshoot to a negative
    // balance, and a kid must never read "-2 min left".
    const mins = Math.max(0, Math.floor(state.balance.total));
    who.textContent = (state.me.name || '') + ' · ⏳ ' + mins + ' min left';
    sub.textContent = noneLeft ? 'No minutes left today. Free shows still work!' : 'Pick something to watch';
  }
  sub.classList.toggle('none-left', noneLeft);
  sub.hidden = false;
  $('together-btn').hidden = !noneLeft;
  t.appendChild(who);
  const lock = el('button', 'lock-btn', state.me.grownup ? '🔒 End watching together' : '🔒 Not you?');
  lock.type = 'button';
  lock.addEventListener('click', lockout);
  t.appendChild(lock);
  t.hidden = false;
}

async function lockout() {
  await postJSON('/api/kid/lock', {}).catch(() => {});
  resetShelf();
  showWho();
}

function render() {
  const q = ($('q').value || '').trim().toLowerCase();
  if (q) { runSearch(q); return; }
  const app = $('app');
  app.textContent = '';
  if (state.kw.length) {
    const sec = el('div', 'section');
    sec.appendChild(el('h2', null, '▶ Keep watching'));
    const row = el('div', 'row');
    for (const it of state.kw) {
      // A bar on the picture shows how far the kid got; a "min left"
      // number here would read like the minutes in the header.
      const done = it.duration > 0 ? Math.min(1, it.pos / it.duration) : 0;
      row.appendChild(recItem(it.thumb, it.title, it.showName || '', () => goWatch(it.episode), it.issue, it.fix, done));
    }
    sec.appendChild(row);
    app.appendChild(sec);
  }
  if (state.recent.length) {
    const sec = el('div', 'section');
    // Per-kid now: the server filters to this kid's own watches, so the
    // plain title is honest; a grown-up session sees the household list.
    sec.appendChild(el('h2', null, state.me && state.me.grownup ? 'Recently watched on this shelf' : 'Recently watched'));
    const row = el('div', 'row');
    for (const it of state.recent) {
      row.appendChild(recItem(it.thumb, it.title, it.showName || '', () => goWatch(it.episode), it.issue, it.fix));
    }
    sec.appendChild(row);
    app.appendChild(sec);
  }
  const grid = el('div', 'shows');
  for (const s of state.shows) grid.appendChild(showCard(s));
  app.appendChild(grid);
}

// goWatch remembers where the kid is on the shelf (open show, how many
// episodes were showing, the search and the scroll spot), so coming back
// from the player lands in the same place.
function goWatch(id) {
  savePlace();
  location.href = '/watch/' + encodeURIComponent(id);
}

function savePlace() {
  const band = $('ep-band');
  const q = ($('q').value || '').trim();
  ssSet(PLACE_KEY, JSON.stringify({
    q,
    show: !q && band ? band.dataset.show : '',
    count: band ? Number(band.dataset.count) || 0 : 0,
    y: Math.round(window.scrollY),
  }));
}

async function restorePlace() {
  let p = null;
  try { p = JSON.parse(ssGet(PLACE_KEY) || 'null'); } catch (e) { p = null; }
  if (!p) return;
  if (p.q) {
    $('q').value = p.q;
    await runSearch(p.q.toLowerCase());
  } else if (p.show) {
    const s = state.shows.find((x) => x.id === p.show);
    if (s) await toggleShow(s, Math.min(200, Math.max(EPS_PAGE, p.count || 0)));
  }
  if (p.y) window.scrollTo(0, p.y);
}

function recItem(thumb, title, meta, onClick, issue, fix, done) {
  const pending = fix === 'pending';
  const b = el('button', 'rec-item' + (pending ? ' pending' : ''));
  b.type = 'button';
  const pic = el('div', 'pic');
  const img = el('img');
  img.alt = '';
  img.loading = 'lazy';
  if (thumb) {
    img.src = thumb;
    img.addEventListener('error', () => { img.style.visibility = 'hidden'; });
  } else {
    img.style.visibility = 'hidden';
  }
  pic.appendChild(img);
  if (done > 0) {
    const bar = el('div', 'prog');
    const fill = el('span');
    fill.style.width = Math.round(done * 100) + '%';
    bar.appendChild(fill);
    pic.appendChild(bar);
  }
  b.appendChild(pic);
  b.appendChild(el('div', 't', title));
  if (meta) b.appendChild(el('div', 'm', meta));
  const l = issueLabel(issue, fix);
  if (l) b.appendChild(l);
  b.addEventListener('click', () => {
    if (pending) { gettingReadyPanel(b); return; }
    onClick();
  });
  return b;
}

function showCard(s) {
  const b = el('button', 'show-card');
  b.type = 'button';
  b.id = 'show-' + s.id;
  const img = el('img');
  img.alt = '';
  img.loading = 'lazy';
  img.src = s.poster || '/static/kid/icons/poster.svg';
  img.addEventListener('error', () => { img.src = '/static/kid/icons/poster.svg'; });
  b.appendChild(img);
  b.appendChild(el('div', 't', s.name));
  b.appendChild(el('div', 'cnt', s.episodes + (s.episodes === 1 ? ' video' : ' videos')));
  b.addEventListener('click', () => toggleShow(s));
  return b;
}

function issueLabel(issue, fix) {
  // A pending episode (any issue) is still being made: nothing to play yet.
  // A failed one plays its original, with a label only for the two problems
  // that hide sound or picture in a browser. "format" fails nothing the
  // browser here sees, so it gets no label. Undefined fields read as "".
  if (fix === 'pending') {
    const l = el('span', 'issue');
    l.textContent = '⏳ Getting ready';
    return l;
  }
  if (fix === 'failed' && issue === 'sound') {
    const l = el('span', 'issue failed');
    l.textContent = '🔇 No sound';
    return l;
  }
  if (fix === 'failed' && issue === 'picture') {
    const l = el('span', 'issue failed');
    l.textContent = '🚫 May not play';
    return l;
  }
  return null;
}

// gettingReadyPanel explains a pending episode on the shelf. The video is
// not playable yet, so the card never starts it: this panel is the whole
// answer, with one way back (its button or Esc), and focus returns to the
// button that opened it.
function gettingReadyPanel(opener) {
  if ($('getting-ready')) return;
  const p = el('div', 'chunk');
  p.id = 'getting-ready';
  p.setAttribute('role', 'dialog');
  p.setAttribute('aria-modal', 'true');
  p.setAttribute('aria-labelledby', 'getting-ready-title');
  const h = el('h2', null, '⏳ Still getting ready');
  h.id = 'getting-ready-title';
  p.appendChild(h);
  p.appendChild(el('p', null, 'This one is not ready yet. Pick another one for now.'));
  const b = el('button', null, 'Back to the shelf');
  b.type = 'button';
  b.addEventListener('click', closeGettingReady);
  p.appendChild(b);
  p._opener = opener || null;
  document.body.appendChild(p);
  b.focus();
}

function closeGettingReady() {
  const p = $('getting-ready');
  if (!p) return;
  p.remove();
  if (p._opener && p._opener.isConnected) p._opener.focus();
}

function episodeCard(e) {
  const pending = e.fix === 'pending';
  const c = el('div', 'ep-card' + (pending ? ' pending' : ''));
  const img = el('img');
  img.alt = '';
  img.loading = 'lazy';
  img.src = e.thumb || '/static/kid/icons/poster.svg';
  img.addEventListener('error', () => { img.src = '/static/kid/icons/poster.svg'; });
  c.appendChild(img);
  c.appendChild(el('div', 't', e.title));
  const l = issueLabel(e.issue, e.fix);
  if (l) c.appendChild(l);
  const meta = el('div', 'meta');
  const len = fmtLen(e.durationSec / 60);
  if (len) meta.appendChild(el('span', 'len', len));
  // The cost is what the server charges per second at this show's bin
  // rate; the badge says the bin in kid words instead of a number that
  // would disagree with the real per-second charge.
  const price = el('span', 'price' + (e.free ? ' free' : '') + (e.bin === 'learning' ? ' half' : ''));
  price.textContent = e.free ? 'Free' : e.bin === 'learning' ? 'Half price' : 'Costs: ' + fmtCost(e.cost);
  meta.appendChild(price);
  c.appendChild(meta);
  const w = el('button', 'watch-btn' + (pending ? ' gray' : ''), 'Watch');
  w.type = 'button';
  w.addEventListener('click', () => {
    if (pending) { gettingReadyPanel(w); return; }
    goWatch(e.id);
  });
  c.appendChild(w);
  return c;
}

// toggleShow opens or closes a show's episode band under its card. limit
// is how many episodes to show at first (more when coming back to a show
// the kid had paged through).
async function toggleShow(s, limit) {
  const existing = $('ep-band');
  if (existing && existing.dataset.show === s.id) {
    existing.remove();
    const sel = $('show-' + s.id);
    if (sel) sel.classList.remove('selected');
    return;
  }
  if (existing) existing.remove();
  document.querySelectorAll('.show-card.selected').forEach((x) => x.classList.remove('selected'));
  const card = $('show-' + s.id);
  if (card) card.classList.add('selected');
  const band = el('div', 'eps-band');
  band.id = 'ep-band';
  band.dataset.show = s.id;
  band.dataset.count = '0';
  band.appendChild(el('h3', null, 'Loading…'));
  card ? card.after(band) : $('app').appendChild(band);
  await loadBand(s, 0, band, false, limit || EPS_PAGE);
}

async function loadBand(s, offset, band, append, limit) {
  let d = null;
  try {
    const r = await fetch('/api/shows/' + encodeURIComponent(s.id) + '/episodes?offset=' + offset + '&limit=' + limit);
    if (r.ok) d = await r.json();
  } catch (e) { d = null; }
  if (!band.isConnected) return;
  if (!d) {
    bandError(s, offset, band, append, limit);
    return;
  }
  if (!append) {
    band.textContent = '';
    band.appendChild(el('h3', null, s.name));
  }
  const grid = el('div', 'ep-grid');
  for (const e of d.episodes) grid.appendChild(episodeCard(e));
  band.appendChild(grid);
  const loaded = offset + d.episodes.length;
  band.dataset.count = String(loaded);
  if (loaded < d.total) {
    const more = el('button', 'more-btn', 'Show more (' + (d.total - loaded) + ' left)');
    more.type = 'button';
    more.addEventListener('click', () => {
      more.remove();
      loadBand(s, loaded, band, true, EPS_PAGE);
    });
    band.appendChild(more);
  }
}

// bandError says the episodes did not load and offers Try again, without
// throwing away the episodes already showing.
function bandError(s, offset, band, append, limit) {
  if (!append) {
    band.textContent = '';
    band.appendChild(el('h3', null, s.name));
  }
  const box = el('div', 'band-err');
  box.appendChild(el('p', null, 'These videos didn\u2019t load. Try again.'));
  const again = el('button', 'more-btn', 'Try again');
  again.type = 'button';
  again.addEventListener('click', () => {
    box.remove();
    loadBand(s, offset, band, append || offset > 0, limit);
  });
  box.appendChild(again);
  band.appendChild(box);
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

let searchTimer = null;

function onSearchInput() {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    const q = ($('q').value || '').trim();
    if (!q) {
      msg('');
      const band = $('ep-band');
      if (band) band.remove();
      render();
      return;
    }
    runSearch(q.toLowerCase());
  }, 250);
}

async function runSearch(q) {
  const app = $('app');
  try {
    const r = await fetch('/api/search?q=' + encodeURIComponent(q));
    if (!r.ok) { msg('Search isn\u2019t working right now. Try again.'); return; }
    const d = await r.json();
    msg('');
    app.textContent = '';
    if (!d.shows.length && !d.episodes.length) {
      app.appendChild(el('div', 'section', 'Nothing found for \u201c' + $('q').value + '\u201d.'));
      return;
    }
    if (d.shows.length) {
      const grid = el('div', 'shows');
      for (const s of d.shows) grid.appendChild(showCard(s));
      app.appendChild(grid);
    }
    if (d.episodes.length) {
      const sec = el('div', 'section');
      sec.appendChild(el('h2', null, 'Episodes'));
      const grid = el('div', 'ep-grid');
      for (const e of d.episodes) grid.appendChild(episodeCard(e));
      sec.appendChild(grid);
      app.appendChild(sec);
    }
  } catch (e) {
    msg('Search isn\u2019t working right now. Try again.');
  }
}

// Opened from the parent page's QR code (?connect=1): drop the marker from
// the address bar, tell kids-shelf once (so the parent page can say a
// tablet connected), then move to the computer's name when it works from
// this tablet, so a home-screen icon saved afterwards survives the router
// giving the computer a new address.
async function connectPing() {
  const q = new URLSearchParams(location.search);
  if (q.get('connect') !== '1') return;
  q.delete('connect');
  const rest = q.toString();
  history.replaceState(history.state, '', location.pathname + (rest ? '?' + rest : '') + location.hash);
  await fetch('/api/connected', { method: 'POST' }).catch(() => { /* only a nicety for the parent page */ });
  await moveToName();
}

// moveToName loads a check image through the announced name (for example
// kids-shelf.local); only when it comes back from this same kids-shelf
// (same instance id) does the page reload there. Anything else, a name
// this tablet cannot resolve, another kids-shelf, a slow network, keeps
// the address that already works.
async function moveToName() {
  try {
    const r = await fetch('/api/name');
    if (!r.ok) return;
    const { name, id } = await r.json();
    if (!name || !id || location.hostname === name) return;
    const origin = location.protocol + '//' + name + (location.port ? ':' + location.port : '');
    const reached = await new Promise((resolve) => {
      const img = new Image();
      const timer = setTimeout(() => resolve(false), 3000);
      img.onload = () => { clearTimeout(timer); resolve(true); };
      img.onerror = () => { clearTimeout(timer); resolve(false); };
      img.src = origin + '/api/name-check?id=' + encodeURIComponent(id) + '&t=' + Date.now();
    });
    if (reached) location.replace(origin + location.pathname + location.search + location.hash);
  } catch (e) { /* stay on the address */ }
}

// ---------------------------------------------------------------------------
// Wire up
// ---------------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('keypad').addEventListener('click', (ev) => {
    const b = ev.target.closest('button[data-d]');
    if (b) tapKey(b.dataset.d);
  });
  document.getElementById('pin-grownup').addEventListener('click', () => openPin('grownup'));
  document.getElementById('pin-back').addEventListener('click', closePin);
  // Out of minutes on the shelf: the grown-up keypad opens right here, and
  // Back returns to the shelf.
  document.getElementById('together-btn').addEventListener('click', (ev) => openPin('grownup', null, '', ev.currentTarget));
  document.getElementById('who-retry').addEventListener('click', (ev) => {
    ev.currentTarget.disabled = true;
    ev.currentTarget.textContent = 'Trying\u2026';
    init();
  });
  // A tap on the dimmed backdrop closes the keypad; a press that starts on
  // a key and slides off it does not.
  const pinscreen = document.getElementById('pinscreen');
  let downOnBackdrop = false;
  pinscreen.addEventListener('pointerdown', (ev) => { downOnBackdrop = ev.target === pinscreen; });
  pinscreen.addEventListener('click', (ev) => {
    if (ev.target === pinscreen && downOnBackdrop) closePin();
    downOnBackdrop = false;
  });
  document.addEventListener('keydown', onKey);
  // Any touch, key or scroll puts the greeting away.
  document.addEventListener('pointerdown', hideToast, true);
  document.addEventListener('keydown', hideToast, true);
  // The scroll that restores the kid's place right before the greeting
  // does not count.
  window.addEventListener('scroll', () => {
    if (Date.now() - ($('toast')._at || 0) > 600) hideToast();
  }, { passive: true });
  // Back from the player can restore this page from the browser's cache:
  // ask again, so the minutes and Keep watching are fresh.
  window.addEventListener('pageshow', (ev) => { if (ev.persisted) init(); });
  window.addEventListener('online', () => { if (!$('who-retry').hidden) init(); });
  document.getElementById('q').addEventListener('input', onSearchInput);
  connectPing();
  init();
});
