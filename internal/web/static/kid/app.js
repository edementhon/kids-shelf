// Kids Shelf home: the "Who's watching?" picker, the PIN keypad and the
// video shelf. Plain JS, no frameworks; everything is built with DOM APIs
// so show and episode names can never inject markup.
'use strict';

const $ = (id) => document.getElementById(id);
const EMOJI = ['🦊', '🐺', '🐰', '🐻', '🦁', '🐧', '🦄', '🐨'];
const EPS_PAGE = 24;

const state = {
  me: null,        // /api/kid/me result
  balance: { today: 0, saved: 0, total: 0 },
  shows: [],
  kw: [],          // keep-watching items
  recent: [],
  pin: '',         // digits entered on the keypad
  pinKind: 'kid',  // 'kid' or 'grownup'
  pinKid: null,    // kid id when pinKind is 'kid'
  pinName: '',     // kid display name
};

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

function toast(s) {
  const t = $('toast');
  t.textContent = s;
  t.hidden = false;
  clearTimeout(t._t);
  t._t = setTimeout(() => { t.hidden = true; }, 3500);
}

function msg(s) {
  const m = $('msg');
  m.textContent = s || '';
  m.hidden = !s;
}

function fmtPos(sec) {
  const m = Math.floor(sec / 60), s = Math.floor(sec % 60);
  return m + ':' + String(s).padStart(2, '0');
}

function fmtMin(x) {
  const r = Math.round(x * 10) / 10;
  return (r % 1 === 0 ? r : r.toFixed(1)).toString();
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

async function init() {
  try {
    const r = await fetch('/api/kid/me');
    if (r.ok) {
      state.me = await r.json();
      await onUnlocked();
      return;
    }
    if (r.status === 503) msg('The shelf isn\u2019t ready yet. Try again in a moment.');
  } catch (e) {
    msg('The shelf isn\u2019t available right now. Try again in a moment.');
  }
  showWho();
}

async function showWho() {
  $('who').hidden = false;
  const pick = $('pick');
  pick.textContent = '';
  try {
    const r = await fetch('/api/kids');
    if (!r.ok) {
      msg(r.status === 503 ? 'The shelf isn\u2019t ready yet. Try again in a moment.'
                           : 'The shelf isn\u2019t available right now. Try again in a moment.');
    } else {
      const d = await r.json();
      const kids = d.kids || [];
      if (!kids.length) {
        // Fresh install: nobody set up yet. One friendly card leads the
        // grown-up to the parent page instead of a row of dead-end tiles.
        pick.appendChild(setupCard());
      } else {
        kids.forEach((k, i) => {
          pick.appendChild(tile(EMOJI[i % EMOJI.length], k.name, () => openPin('kid', k.id, k.name)));
        });
        pick.appendChild(tile('👩', 'Watch together', () => openPin('grownup')));
        pick.appendChild(tile('🔢', 'Math Practice', () => { location.href = '/practice'; }));
        pick.appendChild(tile('📚', 'Libraries', () => { location.href = '/libraries'; }));
      }
    }
  } catch (e) {
    msg('The shelf isn\u2019t available right now. Try again in a moment.');
  }
}

function setupCard() {
  const a = document.createElement('a');
  a.className = 'tile setup';
  a.href = '/admin';
  a.appendChild(el('span', 'emoji', '👪'));
  a.appendChild(el('span', null, 'Set up kids-shelf: open the parent page'));
  return a;
}

function tile(emoji, name, onClick) {
  const b = el('button', 'tile');
  b.appendChild(el('span', 'emoji', emoji));
  b.appendChild(el('span', null, name));
  b.addEventListener('click', onClick);
  return b;
}

// ---------------------------------------------------------------------------
// PIN keypad
// ---------------------------------------------------------------------------

function openPin(kind, kidId, name) {
  state.pinKind = kind;
  state.pinKid = kidId || null;
  state.pinName = name || '';
  state.pin = '';
  $('pin-title').textContent = kind === 'grownup' ? 'Grown-up PIN' : ((name || 'Kid') + '\u2019s PIN');
  $('pin-msg').textContent = kind === 'grownup'
    ? 'Grown-up mode: watching together is free!'
    : 'Enter your PIN to spend your minutes';
  if (kind === 'grownup') checkGrownupPin();
  renderDots();
  $('pinscreen').hidden = false;
}

// checkGrownupPin explains the keypad when the household has no grown-up
// PIN yet: "That PIN didn't work" would just be a lie.
async function checkGrownupPin() {
  try {
    const r = await fetch('/api/kid/grownup-pin');
    if (r.ok) {
      const d = await r.json();
      if (!d.set) $('pin-msg').textContent = 'No grown-up PIN yet. Set one on the parent page, under Settings.';
    }
  } catch (e) { /* keep the default message */ }
}

function renderDots() {
  const dots = $('pin-dots').children;
  for (let i = 0; i < 4; i++) dots[i].classList.toggle('on', i < state.pin.length);
  $('pin-ok').disabled = state.pin.length !== 4;
}

async function tapKey(d) {
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
  try {
    const r = await postJSON(url, body);
    if (r.ok) {
      $('pinscreen').hidden = true;
      $('who').hidden = true;
      await onUnlocked();
      return;
    }
    const d = await r.json().catch(() => ({}));
    $('pin-msg').textContent = (d && d.error) ? d.error : 'That didn\u2019t work. Try again';
    const box = $('pinscreen');
    box.classList.remove('shake');
    void box.offsetWidth;
    box.classList.add('shake');
  } catch (e) {
    $('pin-msg').textContent = 'That didn\u2019t work. Try again';
  }
  state.pin = '';
  renderDots();
}

// ---------------------------------------------------------------------------
// Unlocked shelf
// ---------------------------------------------------------------------------

async function onUnlocked() {
  try {
    const r = await fetch('/api/kid/me');
    if (r.ok) state.me = await r.json();
  } catch (e) { /* keep whatever we had */ }
  $('who').hidden = true;
  $('q').hidden = false;
  renderHeader();
  await Promise.all([loadShows(), loadBalance(), loadRecent(), loadKeepWatching()]);
  render();
  if (state.me && state.me.grownup) {
    toast('Watching together with a grown-up. Free! 💛');
  } else if (state.me && state.me.name) {
    toast('Hi ' + state.me.name + '! Pick something to watch.');
  }
}

async function loadShows() {
  try {
    const r = await fetch('/api/shows');
    if (!r.ok) { msg('The shelf isn\u2019t ready yet. Try again in a moment.'); return; }
    state.shows = (await r.json()).shows || [];
  } catch (e) { /* keep the previous list */ }
}

async function loadBalance() {
  try {
    const r = await fetch('/api/balance');
    if (r.ok) {
      const d = await r.json();
      state.balance = { today: d.today || 0, saved: d.saved || 0, total: d.total || 0 };
      renderHeader();
    }
  } catch (e) { /* badge stays empty */ }
}

async function loadRecent() {
  try {
    const r = await fetch('/api/recent');
    if (r.ok) state.recent = (await r.json()).items || [];
  } catch (e) { state.recent = []; }
}

async function loadKeepWatching() {
  if (!state.me || state.me.grownup) { state.kw = []; return; }
  try {
    const r = await fetch('/api/keep-watching');
    if (r.ok) state.kw = (await r.json()).items || [];
  } catch (e) { state.kw = []; }
}

function renderHeader() {
  const t = $('tickets');
  t.textContent = '';
  if (!state.me) { t.hidden = true; return; }
  const who = el('span', 'kid');
  if (state.me.grownup) {
    who.textContent = '👩 Watching together · Free';
  } else {
    // Whole minutes, never below 0: a parent can overshoot to a negative
    // balance, and a kid must never read "-2 min left".
    const mins = Math.max(0, Math.floor(state.balance.total));
    who.textContent = (state.me.name || '') + ' · ⏳ ' + mins + ' min left';
  }
  t.appendChild(who);
  const lock = el('button', 'lock-btn', '🔒 Not you?');
  lock.addEventListener('click', lockout);
  t.appendChild(lock);
  t.hidden = false;
}

async function lockout() {
  await postJSON('/api/kid/lock', {}).catch(() => {});
  state.me = null;
  state.kw = [];
  state.balance = { today: 0, saved: 0, total: 0 };
  $('q').value = '';
  $('q').hidden = true;
  $('app').textContent = '';
  const band = $('ep-band');
  if (band) band.remove();
  renderHeader();
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
      const durMin = it.duration / 60 || 0;
      const left = Math.max(0, durMin - it.pos / 60);
      const sub = durMin ? fmtMin(left) + ' min left' : 'at ' + fmtPos(it.pos);
      row.appendChild(recItem(it.thumb, it.title, sub, () => { location.href = '/watch/' + encodeURIComponent(it.episode); }, it.issue, it.fix));
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
      row.appendChild(recItem(it.thumb, it.title, it.showName || '', () => { location.href = '/watch/' + encodeURIComponent(it.episode); }, it.issue, it.fix));
    }
    sec.appendChild(row);
    app.appendChild(sec);
  }
  const grid = el('div', 'shows');
  for (const s of state.shows) grid.appendChild(showCard(s));
  app.appendChild(grid);
}

function recItem(thumb, title, meta, onClick, issue, fix) {
  const pending = fix === 'pending';
  const b = el('button', 'rec-item' + (pending ? ' pending' : ''));
  const img = el('img');
  img.alt = '';
  img.loading = 'lazy';
  if (thumb) {
    img.src = thumb;
    img.addEventListener('error', () => { img.style.display = 'none'; });
  } else {
    img.style.display = 'none';
  }
  b.appendChild(img);
  b.appendChild(el('div', 't', title));
  if (meta) b.appendChild(el('div', 'm', meta));
  const l = issueLabel(issue, fix);
  if (l) b.appendChild(l);
  b.addEventListener('click', () => {
    if (pending) { gettingReadyPanel(); return; }
    onClick();
  });
  return b;
}

function showCard(s) {
  const b = el('button', 'show-card');
  b.id = 'show-' + s.id;
  const img = el('img');
  img.alt = '';
  img.loading = 'lazy';
  img.src = s.poster || '/static/kid/icons/poster.svg';
  img.addEventListener('error', () => { img.src = '/static/kid/icons/poster.svg'; });
  b.appendChild(img);
  b.appendChild(el('div', 't', s.name));
  b.appendChild(el('div', 'cnt', s.episodes + (s.episodes === 1 ? ' ep' : ' eps')));
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
// answer, with one way back.
function gettingReadyPanel() {
  if ($('getting-ready')) return;
  const p = el('div', 'chunk');
  p.id = 'getting-ready';
  p.appendChild(el('h2', null, '⏳ Still getting ready'));
  p.appendChild(el('p', null, 'This one is still getting ready. kids-shelf is working on it. Pick another one for now.'));
  const b = el('button', null, 'Back to the shelf');
  b.addEventListener('click', () => p.remove());
  p.appendChild(b);
  document.body.appendChild(p);
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
  const durMin = Math.max(0.5, e.durationSec / 60);
  meta.appendChild(el('span', 'len', fmtMin(durMin) + ' min'));
  // The cost is what the server charges per second at this show's bin
  // rate; the badge says the bin in kid words instead of a number that
  // would disagree with the real per-second charge.
  const price = el('span', 'price' + (e.free ? ' free' : '') + (e.bin === 'learning' ? ' half' : ''));
  price.textContent = e.free ? 'Free' : e.bin === 'learning' ? 'Half price' : 'Costs: ' + fmtMin(e.cost) + ' min';
  meta.appendChild(price);
  c.appendChild(meta);
  const w = el('button', 'watch-btn' + (pending ? ' gray' : ''), 'Watch');
  w.addEventListener('click', () => {
    if (pending) { gettingReadyPanel(); return; }
    location.href = '/watch/' + encodeURIComponent(e.id);
  });
  c.appendChild(w);
  return c;
}


async function toggleShow(s) {
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
  band.appendChild(el('h3', null, 'Loading…'));
  card ? card.after(band) : $('app').appendChild(band);
  await loadBand(s, 0, band, false);
}

async function loadBand(s, offset, band, append) {
  try {
    const r = await fetch('/api/shows/' + encodeURIComponent(s.id) + '/episodes?offset=' + offset + '&limit=' + EPS_PAGE);
    if (!r.ok) { band.textContent = ''; band.appendChild(el('h3', null, 'Couldn\u2019t load.')); return; }
    const d = await r.json();
    if (!append) {
      band.textContent = '';
      band.appendChild(el('h3', null, s.name));
    }
    const grid = el('div', 'ep-grid');
    for (const e of d.episodes) grid.appendChild(episodeCard(e));
    band.appendChild(grid);
    const loaded = offset + d.episodes.length;
    if (loaded < d.total) {
      const more = el('button', 'more-btn', 'Show more (' + (d.total - loaded) + ' left)');
      more.addEventListener('click', () => {
        more.remove();
        loadBand(s, loaded, band, true);
      });
      band.appendChild(more);
    }
  } catch (e) {
    band.textContent = '';
    band.appendChild(el('h3', null, 'Couldn\u2019t load.'));
  }
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
      for (const e of d.episodes) sec.appendChild(episodeCard(e));
      app.appendChild(sec);
    }
  } catch (e) {
    msg('Search isn\u2019t working right now. Try again.');
  }
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
  document.getElementById('q').addEventListener('input', onSearchInput);
  init();
});
