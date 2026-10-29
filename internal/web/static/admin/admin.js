'use strict';
/* Parent dashboard: one state fetch, sections rendered from it, every
   mutation through /api/admin/*. Plain JS, no frameworks. */

const $ = id => document.getElementById(id);
let STATE = null;
let msgTimer = null;
let logShown = 20;      // watch-log rows on screen; "Show more" adds LOG_STEP
const LOG_STEP = 20;
let lockTimer = null;   // the "Try again in N seconds" countdown
let msgAction = null;   // what the toast's action button does, if it has one
let locking = false;    // a Lock this page request is on its way

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function fmt(n) { return Math.max(0, Math.round(n * 10) / 10); }
function cls(n) { return n < 0 ? 'neg' : ''; }

/* msg shows a result or an error as a toast fixed to the bottom of the
   screen, so it is seen wherever the control that caused it sits. Errors
   stay longer; ✕ closes it at once. An action ({ label, run }) adds a
   button, and the toast then stays until it is used or closed. */
function msg(text, isErr, action) {
  const el = $('msg');
  $('msgtext').textContent = text;
  msgAction = action ? action.run : null;
  $('msgaction').textContent = action ? action.label : '';
  $('msgaction').disabled = false;
  $('msgaction').classList.toggle('hidden', !action);
  el.className = 'msg ' + (isErr ? 'err' : 'ok');
  clearTimeout(msgTimer);
  if (!action) msgTimer = setTimeout(hideMsg, isErr ? 9000 : 5000);
}
function hideMsg() {
  clearTimeout(msgTimer);
  msgAction = null;
  $('msg').className = 'msg hidden';
}

/* ── API with the parent-PIN gate ────────────────────────────────────── */
/* Every gated call that answers 401 opens the PIN lock (or the setup form
   when no PIN exists), so a lapsed session always leads to a place to type
   the PIN. Only the PIN forms themselves pass pinFlow: they show their own
   error in the form. */
async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    if (!opts.quiet) msg('kids-shelf did not answer. Check that it is still running, then try again.', true);
    throw e;
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* non-JSON error body */ }
  // No parent PIN yet (a fresh install, or a reset on the computer
  // itself): every gated call says so, and the page asks for one.
  if (res.status === 401 && data && data.setup) {
    showSetup();
    const err = new Error('setup');
    err.pinRequired = true;
    throw err;
  }
  if (res.status === 401 && !opts.pinFlow) {
    // The page was open and the session ended (12 hours passed, the PIN
    // changed elsewhere, or the page was locked): say so and ask again.
    showPin(STATE ? 'This page locked itself. Enter the PIN, then try again.' : '');
    const err = new Error('pin');
    err.pinRequired = true;
    throw err;
  }
  if (!res.ok) {
    if (!opts.quiet) msg((data && data.error) || 'That did not work. Try again.', true);
    const err = new Error((data && data.error) || String(res.status));
    err.status = res.status;
    // A 429 says how long the lockout lasts in seconds (the server's
    // "wait" field); its message has no number in it.
    if (data && typeof data.wait === 'number') err.wait = data.wait;
    throw err;
  }
  return data;
}

/* serverAnswers reports whether kids-shelf still answers its health check,
   used after a Quit or Remove whose reply never arrived. */
async function serverAnswers() {
  try {
    const ctl = new AbortController();
    const t = setTimeout(() => ctl.abort(), 3000);
    const r = await fetch('/healthz', { cache: 'no-store', signal: ctl.signal });
    clearTimeout(t);
    return r.ok;
  } catch (e) {
    return false;
  }
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

/* ── loading ──────────────────────────────────────────────────────────── */
async function load() {
  const st = await api('/api/admin/state'); // throws 'pin' when the page is locked
  STATE = st;
  document.body.classList.remove('locked');
  renderAll();
  refreshProblems(); // the banner is independent of the kid-state fetch
}

function renderAll() {
  renderKids();
  renderWatchLog();
  renderBins();
  renderSettings();
  renderAddress();
  renderUpdates();
  $('budgetswitch').checked = STATE.budgets_on;
  $('budgetlabel').textContent = 'Limit minutes: ' + (STATE.budgets_on ? 'on' : 'off');
  $('statusline').textContent = STATE.budgets_on
    ? ''
    : 'Minute limits are off. Kids can watch as much as they like; nothing is counted.';
}

/* ── helpers and firewall problems ───────────────────────────────────── */
function renderProblems(problems) {
  const helpersText = (problems && problems.helpers) || '';
  // A wait that ends by itself (Windows still checking the video tools)
  // gets only its own words: no "did not start", no "install again".
  const waiting = !!(problems && problems.helpers_waiting);
  $('helpersproblem').textContent = helpersText;
  $('helperslead').classList.toggle('hidden', waiting);
  $('helpersadvice').classList.toggle('hidden', waiting);
  $('helpers').classList.toggle('hidden', !helpersText);

  const fw = problems && problems.firewall;
  const blocked = fw && fw.blocked && !fw.unknown;
  $('firewall').classList.toggle('show', !!blocked);
  $('sec-problems').classList.toggle('show', !!(helpersText || blocked));
}

async function refreshProblems() {
  try {
    renderProblems(await api('/api/admin/problems', { quiet: true }));
  } catch (e) { /* the PIN overlay (or the page being gone) handles it */ }
}

async function letTabletsIn() {
  let out;
  try {
    out = await api('/api/admin/firewall/allow', { method: 'POST', body: '{}' });
  } catch (e) { return; /* message already shown */ }
  const fw = (out && out.firewall) || {};
  if (!fw.blocked && !fw.unknown) {
    msg('Tablets and phones can reach kids-shelf now.');
    $('firewall').classList.remove('show');
    $('sec-problems').classList.toggle('show', !$('helpers').classList.contains('hidden'));
  } else if (fw.unknown) {
    msg('kids-shelf could not check whether tablets can get in. Try opening the address on a tablet; if it does not work, press Let tablets in again.', true);
  } else {
    msg('Windows still keeps tablets out. Press Let tablets in again and choose Yes when Windows asks.', true);
  }
}

/* ── kids ─────────────────────────────────────────────────────────────── */
function renderKids() {
  const wrap = $('kids');
  const kids = STATE.kids;
  $('kidsnote').classList.toggle('hidden', !kids.length);
  if (!kids.length) {
    wrap.innerHTML = '<p class="placeholder">No kids yet. Add the first one below.</p>';
    return;
  }
  wrap.innerHTML = kids.map(k => {
    const total = k.today + k.saved;
    const sub = total <= 0 ? ' disabled' : '';
    const n = esc(k.name);
    const take = d => `<button class="sub" data-act="adj" data-id="${esc(k.id)}" data-d="-${d}"${sub} aria-label="Take ${d} minutes away from ${n} today">−${d}</button>`;
    const give = d => `<button class="add" data-act="adj" data-id="${esc(k.id)}" data-d="${d}" aria-label="Give ${n} ${d} more minutes today">+${d}</button>`;
    return `
    <div class="kid" data-id="${esc(k.id)}">
      <div class="kidhead">
        <span class="kidname">${n}</span>
        <button class="mini" data-act="kidmenu" data-id="${esc(k.id)}" aria-haspopup="menu" aria-expanded="false" aria-label="Options for ${n}">⋯</button>
      </div>
      <div class="bal">
        <span>today <b class="${cls(k.today)}">${fmt(k.today)}</b></span>
        <span>saved <b class="${cls(k.saved)}">${fmt(k.saved)}</b></span>
      </div>
      <div class="dailyrow">
        <label for="daily-${esc(k.id)}">Minutes a day (from tomorrow)</label>
        <input type="number" class="daily" id="daily-${esc(k.id)}" data-id="${esc(k.id)}" value="${k.daily}" min="0" step="1" inputmode="decimal">
      </div>
      <div class="adjrow"><span class="adjlabel">Take away today</span><div class="btns3">${take(10)}${take(5)}${take(1)}</div></div>
      <div class="adjrow"><span class="adjlabel">Give today</span><div class="btns3">${give(1)}${give(5)}${give(10)}</div></div>
    </div>`;
  }).join('');
}

function renderWatchLog() {
  const tb = document.querySelector('#watchlog tbody');
  const log = STATE.watch_log;
  if (!log.length) {
    tb.innerHTML = '<tr><td colspan="5" class="placeholder">Nothing watched yet.</td></tr>';
    $('logmore').classList.add('hidden');
    return;
  }
  tb.innerHTML = log.slice(0, logShown).map(e => `
    <tr>
      <td>${esc(e.at.replace('T', ' '))}</td>
      <td>${esc(e.kid_name || (e.kid ? '(removed kid)' : 'Grown-up (together)'))}</td>
      <td>${esc(e.show || '-')}</td>
      <td>${esc(e.title)}</td>
      <td class="${e.free ? 'free' : ''}">${e.free ? 'free' : fmt(e.cost) + ' min'}</td>
    </tr>`).join('');
  const left = log.length - logShown;
  $('logmore').classList.toggle('hidden', left <= 0);
  $('logmore').textContent = `Show ${Math.min(LOG_STEP, left)} more (${left} not shown)`;
}

/* ── bins ─────────────────────────────────────────────────────────────── */
function showCard(s) {
  return `<div class="show" draggable="true" data-id="${esc(s.id)}">
    <span class="showtext"><span class="showname">${esc(s.name)}</span>
    <span class="showeps">${s.episodes} ${s.episodes === 1 ? 'episode' : 'episodes'}</span></span>
    <button class="mini" data-act="showmenu" data-id="${esc(s.id)}" aria-haspopup="menu" aria-expanded="false" aria-label="Move ${esc(s.name)}">⋯</button>
  </div>`;
}

function renderBins() {
  for (const bin of ['fun', 'learning', 'free']) {
    const list = STATE.bins[bin] || [];
    const wrap = $(`cards-${bin}`);
    wrap.innerHTML = list.length
      ? list.map(showCard).join('')
      : `<p class="placeholder">No shows here yet.</p>`;
  }
}

function binOf(showID) {
  for (const bin of ['fun', 'learning', 'free']) {
    if ((STATE.bins[bin] || []).some(s => s.id === showID)) return bin;
  }
  return '';
}

async function moveBin(showID, bin) {
  try {
    await api('/api/admin/bin', { method: 'POST', body: JSON.stringify({ show: showID, bin }) });
    await load();
  } catch (e) { /* message already shown */ }
}

/* drag and drop (desktop) */
function setupDrag() {
  let dragging = null;
  document.addEventListener('dragstart', e => {
    const card = e.target.closest && e.target.closest('.show');
    if (!card) return;
    dragging = card.dataset.id;
    card.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
  });
  document.addEventListener('dragend', e => {
    const card = e.target.closest && e.target.closest('.show');
    if (card) card.classList.remove('dragging');
    dragging = null;
    document.querySelectorAll('.col').forEach(c => c.classList.remove('dragover'));
  });
  document.querySelectorAll('.col').forEach(col => {
    col.addEventListener('dragover', e => {
      if (dragging) { e.preventDefault(); col.classList.add('dragover'); }
    });
    col.addEventListener('dragleave', () => col.classList.remove('dragover'));
    col.addEventListener('drop', e => {
      e.preventDefault();
      col.classList.remove('dragover');
      if (dragging && col.dataset.bin) moveBin(dragging, col.dataset.bin);
      dragging = null;
    });
  });
}

/* ⋯ menus (kid options, move a show). The menu is put in the page right
   after its button, so Tab reaches it next and scrolling moves it along
   instead of closing it. Focus goes to the first item; Esc, a tap outside
   or tabbing away closes it and focus returns to the button. */
let menuButton = null;
function popupMenu(button, items) {
  const again = menuButton === button;
  hideMenu(false);
  if (again) return; // a second press on the same ⋯ closes it
  const menu = document.createElement('div');
  menu.id = 'menu';
  menu.className = 'menu';
  menu.setAttribute('role', 'menu');
  menu.innerHTML = items.map(it =>
    `<button role="menuitem" class="${it.danger ? 'danger' : ''}" data-act="${it.act}" data-id="${esc(it.id)}"${it.meta ? ` data-meta="${esc(it.meta)}"` : ''}>${esc(it.label)}</button>`
  ).join('');
  button.insertAdjacentElement('afterend', menu);
  button.setAttribute('aria-expanded', 'true');
  menuButton = button;
  menu.addEventListener('keydown', menuKeys);
  menu.addEventListener('focusout', menuFocusOut);
  menu.querySelector('button').focus();
}

function menuKeys(e) {
  const items = [...e.currentTarget.querySelectorAll('button')];
  const i = items.indexOf(document.activeElement);
  if (e.key === 'Escape') { e.preventDefault(); hideMenu(true); }
  else if (e.key === 'ArrowDown') { e.preventDefault(); items[(i + 1) % items.length].focus(); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); items[(i - 1 + items.length) % items.length].focus(); }
}

// Tabbing out of the menu closes it. A focus change with no new target
// (a tap on a menu item in Safari, which does not focus buttons) is left
// to the click handlers, so the tap still lands.
function menuFocusOut(e) {
  const to = e.relatedTarget;
  if (to && !e.currentTarget.contains(to) && to !== menuButton) hideMenu(false);
}

function hideMenu(refocus) {
  const menu = $('menu');
  const button = menuButton;
  menuButton = null;
  if (button) {
    button.setAttribute('aria-expanded', 'false');
    // Focus goes back first, so removing the menu cannot strand it.
    if (refocus && document.contains(button)) button.focus();
  }
  if (menu) {
    menu.removeEventListener('focusout', menuFocusOut);
    menu.remove();
  }
}

/* ── settings ─────────────────────────────────────────────────────────── */
function renderSettings() {
  $('defaultmin').value = STATE.default_minutes;
  $('safariswitch').checked = STATE.settings.safari_screens;
  $('hwswitch').checked = !!STATE.settings.hw_encode;
  // Until the first conversion picks one, the server says "not checked
  // yet"; say when that changes.
  const enc = STATE.settings.video_encoder;
  $('hwencoder').textContent = 'Video conversion uses: ' + (enc && enc !== 'not checked yet' ? enc
    : 'not checked yet. kids-shelf checks the first time a video needs converting.');
  const pinSet = !!STATE.settings.grownup_pin_set;
  $('grownupstate').textContent = pinSet
    ? 'A watch-together PIN is set. Type 4 new digits to change it.'
    : 'No watch-together PIN yet. Type 4 digits to make one.';
  $('grownup-save').textContent = pinSet ? 'Change PIN' : 'Save PIN';
  $('grownup-clear').classList.toggle('hidden', !pinSet);
  // Practice normally earns video minutes. Only a copy whose earnings were
  // pointed elsewhere (the old free-text counter) shows the way back.
  const counter = STATE.settings.counter;
  $('counterrow').classList.toggle('hidden', !counter || counter === 'video');
}

function renderAddress() {
  $('addr').textContent = STATE.settings.address || '';
  const qr = $('qr');
  if (STATE.settings.qr_svg) {
    qr.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(STATE.settings.qr_svg);
    qr.classList.remove('hidden');
  } else {
    qr.classList.add('hidden');
  }
}

async function counterToVideo() {
  try {
    await api('/api/admin/settings/counter', { method: 'POST', body: JSON.stringify({ counter: 'video' }) });
    msg('Practice earns video minutes again.');
    await load();
  } catch (e) { /* message already shown */ }
}

/* ── updates ──────────────────────────────────────────────────────────── */
function renderUpdates() {
  const u = STATE.updates || {};
  $('updatemsg').textContent = u.message || 'This copy does not update itself.';
  $('restart-update').classList.toggle('hidden', u.state !== 'staged' || !!u.applying);
}

/* restartToUpdate asks kids-shelf to restart into the new version, then
   waits for it to come back (it goes away, or answers with a new version)
   and reloads the page. */
async function restartToUpdate() {
  let before = null;
  try { before = await (await fetch('/healthz', { cache: 'no-store' })).json(); } catch (e) { /* checked below */ }
  try {
    await api('/api/admin/update/restart', { method: 'POST', body: '{}' });
  } catch (e) { return; /* the PIN lock or the error message is showing */ }
  showStopped('kids-shelf is restarting with the new version.',
    'This page comes back on its own in a moment.');
  let wentAway = false;
  for (let i = 0; i < 60; i++) {
    await sleep(2000);
    let now = null;
    try {
      const r = await fetch('/healthz', { cache: 'no-store' });
      if (r.ok) now = await r.json();
    } catch (e) { /* still restarting */ }
    if (!now) { wentAway = true; continue; }
    if (wentAway || !before || now.version !== before.version || now.id !== before.id) {
      location.reload();
      return;
    }
  }
  showStopped('kids-shelf has not come back yet.',
    `Wait a minute and reload this page. If it still does not open, open kids-shelf again from ${REOPEN}.`);
}

/* ── PIN gate: the lock and the first-run setup ──────────────────────── */
function showErr(id, text) {
  $(id).textContent = text;
  $(id).classList.toggle('hidden', !text);
}

/* lockedOut shows a 429 lockout in the form's own error line and counts the
   server's "wait" seconds down live; the form's input and button wait until
   then. Without a wait it just shows the server's words. */
function lockedOut(errId, err, controls) {
  clearInterval(lockTimer);
  if (!err || !(err.wait > 0)) { showErr(errId, err && err.message); return; }
  let left = Math.ceil(err.wait);
  const set = dis => controls.forEach(id => { $(id).disabled = dis; });
  const tick = () => {
    if (left <= 0) {
      clearInterval(lockTimer);
      set(false);
      showErr(errId, 'You can try again now.');
      $(controls[0]).focus();
      return;
    }
    showErr(errId, `Too many wrong PINs. Try again in ${left} ${left === 1 ? 'second' : 'seconds'}.`);
    left--;
  };
  set(true);
  tick();
  lockTimer = setInterval(tick, 1000);
}

function showPin(text) {
  hideMenu(false);
  document.body.classList.add('locked');
  $('setupoverlay').classList.add('hidden');
  $('pinoverlay').classList.remove('hidden');
  showErr('pinerr', text || '');
  $('forgothelp').classList.add('hidden');
  $('forgot').setAttribute('aria-expanded', 'false');
  setTimeout(() => { if (!$('pinval').disabled) $('pinval').focus(); }, 50);
}
function hidePin() { $('pinoverlay').classList.add('hidden'); }

async function unlock() {
  const pin = $('pinval').value.trim();
  if (!/^\d{4}$/.test(pin)) { showErr('pinerr', 'The PIN is 4 digits.'); return; }
  showErr('pinerr', '');
  try {
    await api('/api/admin/pin', { method: 'POST', body: JSON.stringify({ pin }), pinFlow: true, quiet: true });
    $('pinval').value = '';
    hidePin();
    await load();
  } catch (e) {
    if (e && e.pinRequired) return; // no PIN any more: the setup form is up
    $('pinval').value = '';
    if (e && e.status === 429) { lockedOut('pinerr', e, ['pinval', 'pinok']); return; }
    showErr('pinerr', e && e.status === 401
      ? 'That PIN is not right. Try again.'
      : (e && e.status && e.message) || 'kids-shelf did not answer. Check that it is still running, then try again.');
    $('pinval').focus();
  }
}

// "Forgot the PIN?" explains where it can be reset; on the computer
// kids-shelf runs on (the server says local) the reset button shows too.
async function showForgot() {
  $('forgothelp').classList.remove('hidden');
  $('forgot').setAttribute('aria-expanded', 'true');
  $('resetrow').classList.add('hidden');
  try {
    const r = await fetch('/api/admin/pin/status');
    const d = await r.json();
    $('resetrow').classList.toggle('hidden', !d.local);
  } catch (e) { /* the words alone still help */ }
}

async function resetPin() {
  try {
    await api('/api/admin/pin/reset', { method: 'POST', body: '{}', pinFlow: true, quiet: true });
    showSetup();
  } catch (e) {
    if (e && e.pinRequired) return;
    showErr('pinerr', (e && e.message) || 'Something went wrong. Try again.');
  }
}

function showSetup() {
  hideMenu(false);
  document.body.classList.add('locked');
  hidePin();
  $('setupoverlay').classList.remove('hidden');
  $('setuppin').value = '';
  $('setuppin2').value = '';
  showErr('setuperr', '');
  setTimeout(() => $('setuppin').focus(), 50);
}

async function saveSetup(e) {
  e.preventDefault();
  const pin = $('setuppin').value.trim();
  const again = $('setuppin2').value.trim();
  if (!/^\d{4}$/.test(pin)) { showErr('setuperr', 'The PIN must be exactly 4 digits.'); $('setuppin').focus(); return; }
  if (pin !== again) {
    showErr('setuperr', 'The two PINs are not the same. Type the same 4 digits in both boxes.');
    $('setuppin2').value = '';
    $('setuppin2').focus();
    return;
  }
  try {
    await api('/api/admin/pin/setup', { method: 'POST', body: JSON.stringify({ pin }), pinFlow: true, quiet: true });
    $('setupoverlay').classList.add('hidden');
    await load();
    msg('Parent PIN saved. You will need it to open this page.');
  } catch (err) {
    if (err && err.status === 409) { showPin(err.message); return; } // someone made one meanwhile
    if (err && err.status === 429) { lockedOut('setuperr', err, ['setuppin', 'setuppin2', 'setupok']); return; }
    showErr('setuperr', (err && err.message) || 'Something went wrong. Try again.');
  }
}

/* lockPage ends this browser's session and shows the PIN lock, so a kid
   who opens the page next cannot change anything. The page says it is
   locked only once kids-shelf confirmed the session ended; if it did not
   (no connection, an error), the page stays as it was and says so, with
   Try again, because the session would still open the page. */
async function lockPage() {
  if (locking) return;
  locking = true;
  $('lock').disabled = true;
  $('msgaction').disabled = true;
  let ok = false;
  try {
    const res = await fetch('/api/admin/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    ok = res.ok;
  } catch (e) { /* no answer: said below */ }
  locking = false;
  $('lock').disabled = false;
  if (!ok) {
    msg('This page is not locked yet: kids-shelf did not confirm it. Check that this device is connected, then try again.', true,
      { label: 'Try again', run: lockPage });
    return;
  }
  hideMsg();
  STATE = null;
  showPin('This page is locked.');
}

/* ── quit and remove ──────────────────────────────────────────────────── */
/* REOPEN names where kids-shelf is opened again on this computer (the
   server fills it in: the Dock on a Mac, the desktop icon elsewhere). */
const REOPEN = document.body.dataset.reopen || 'the desktop icon';

/* showStopped shows the full-screen "stopped" message. final is for a
   removal: the program is gone, so there is nothing to close or reopen. */
function showStopped(title, text, final) {
  $('stoppedtitle').textContent = title;
  $('stoppedtext').textContent = text;
  $('stoppedrow').classList.toggle('hidden', !!final);
  $('stopped').dataset.final = final ? '1' : '';
  $('stopped').classList.remove('hidden');
  if (!final) $('stoppedclose').focus();
}

/* stopApp sends a Quit or Remove and shows the "stopped" screen only once
   kids-shelf confirmed it: an OK reply, or no reply and no longer answering.
   A refusal shows its message and the page stays usable; a lapsed session
   opens the PIN lock. */
async function stopApp(path, body, stoppedTitle, stoppedText, final) {
  try {
    await api(path, { method: 'POST', body: JSON.stringify(body), quiet: true });
  } catch (e) {
    if (e && e.pinRequired) return;
    if (e && e.status) { msg(e.message, true); return; }
    // No reply: kids-shelf may have stopped before answering. Look.
    await sleep(1000);
    if (await serverAnswers()) {
      msg('kids-shelf did not stop. Try again.', true);
      return;
    }
  }
  showStopped(stoppedTitle, stoppedText, final);
}

async function quitApp() {
  if (!confirm(`Quit kids-shelf? Nothing will run until you open it again from ${REOPEN}.`)) return;
  await stopApp('/api/admin/quit', {}, 'kids-shelf has stopped.', `Open it again from ${REOPEN}.`);
}

async function removeApp() {
  let info;
  try {
    info = await api('/api/admin/remove'); // {data_dir, media_dir} for the confirmation
  } catch (e) { return; }
  const del = $('removedata').checked;
  const dirs = [...new Set([info.data_dir, info.media_dir].filter(Boolean))];
  const folders = dirs.map(d => '\n  ' + d).join('');
  const own = 'Folders of your own videos that you added are never touched.';
  const text = del
    ? `Remove kids-shelf AND delete its videos and settings?\n\nThe program is deleted, and so is everything inside ${dirs.length === 1 ? 'this folder' : 'these folders'}:${folders}\n\nThat includes the kids, their minutes and PINs, and the videos kids-shelf saved. ${own}\n\nThis cannot be undone.`
    : `Remove kids-shelf from this computer?\n\nThe program is deleted. ${dirs.length === 1 ? 'This folder stays, with the videos and settings in it' : 'These folders stay, with the videos and settings in them'}:${folders}\n\n${own}`;
  if (!confirm(text) || (del && !confirm('Last check: delete the kids, their minutes and the saved videos for good?'))) {
    $('removedata').checked = false;
    msg('Nothing was removed.');
    return;
  }
  await stopApp('/api/admin/remove', { deleteData: del }, 'kids-shelf is removed from this computer.',
    del ? 'Its videos and settings are deleted too. You can close this window.'
      : 'Your videos and settings are kept. You can close this window.', true);
}

/* ── events ───────────────────────────────────────────────────────────── */
async function onAction(act, t) {
  const id = t.dataset.id;
  const kid = STATE && STATE.kids.find(k => k.id === id);
  switch (act) {
    case 'adj': {
      const delta = Number(t.dataset.d);
      try {
        await api('/api/admin/adjust', { method: 'POST', body: JSON.stringify({ kid: id, delta }) });
        await load();
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'kidmenu':
      popupMenu(t, [
        { act: 'renamemenu', id, label: '✏️ Rename ' + (kid ? kid.name : '') },
        { act: 'pinmenu', id, label: '🔑 Change PIN' },
        { act: 'removemenu', id, label: '🗑️ Remove kid', danger: true },
      ]);
      break;
    case 'renamemenu': {
      hideMenu(true);
      const name = prompt('New name for ' + (kid ? kid.name : 'this kid') + ':', kid ? kid.name : '');
      if (name === null) break;
      if (!name.trim()) { msg('Give the kid a name.', true); break; }
      try {
        await api('/api/admin/kids/' + encodeURIComponent(id) + '/rename', { method: 'POST', body: JSON.stringify({ name: name.trim() }) });
        msg('Name saved.');
        await load();
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'pinmenu': {
      hideMenu(true);
      const pin = prompt('New 4-digit PIN for ' + (kid ? kid.name : 'this kid') + ':');
      if (pin === null) break;
      if (!/^\d{4}$/.test(pin.trim())) { msg('The PIN must be exactly 4 digits. Nothing was changed.', true); break; }
      try {
        await api('/api/admin/kids/' + encodeURIComponent(id) + '/pin', { method: 'POST', body: JSON.stringify({ pin: pin.trim() }) });
        msg('PIN saved' + (kid ? ' for ' + kid.name : '') + '.');
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'removemenu': {
      hideMenu(true);
      const who = kid ? kid.name : 'this kid';
      if (!confirm(`Remove ${who}? Their minutes, saved minutes and PIN are deleted. What they watched stays in the watch log as "(removed kid)".`)) break;
      try {
        await api('/api/admin/kids/' + encodeURIComponent(id), { method: 'DELETE' });
        msg(who + ' was removed.');
        await load();
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'showmenu': {
      const names = { fun: '🟢 Move to Fun', learning: '🟡 Move to Learning', free: '⚪ Move to Free' };
      const here = binOf(id);
      popupMenu(t, ['fun', 'learning', 'free'].filter(b => b !== here)
        .map(b => ({ act: 'binmove', id, label: names[b], meta: b })));
      break;
    }
    case 'binmove':
      hideMenu(false);
      await moveBin(id, t.dataset.meta);
      break;
  }
}

document.addEventListener('click', e => {
  const t = e.target.closest('[data-act]');
  if (t) {
    onAction(t.dataset.act, t);
    return;
  }
  if (!e.target.closest('#menu')) hideMenu(false);
});

/* readMinutes reads a minutes box. An empty or unreadable box is never
   saved (it would mean 0 minutes): the old number goes back and the toast
   says why. */
function readMinutes(input, old) {
  const raw = input.value.trim();
  const minutes = Number(raw);
  if (raw === '' || !Number.isFinite(minutes)) {
    input.value = old;
    msg(`Type a number of minutes, like 30. Nothing was changed: it is still ${old}.`, true);
    return null;
  }
  if (minutes < 0) {
    input.value = old;
    msg(`Minutes cannot be less than 0. Nothing was changed: it is still ${old}.`, true);
    return null;
  }
  return minutes;
}

/* writes (change events) */
document.addEventListener('change', async e => {
  if (e.target.classList && e.target.classList.contains('daily')) {
    const id = e.target.dataset.id;
    const kid = STATE.kids.find(k => k.id === id);
    const minutes = readMinutes(e.target, kid ? kid.daily : '');
    if (minutes === null) return;
    try {
      await api('/api/admin/kids/' + encodeURIComponent(id) + '/minutes', { method: 'POST', body: JSON.stringify({ minutes }) });
      msg(`${kid ? kid.name : 'This kid'} gets ${minutes} minutes a day from tomorrow. To change today, use Give or Take away.`);
    } catch (err) { /* message already shown */ }
    load().catch(() => {});
    return;
  }
  if (e.target.id === 'budgetswitch') {
    try {
      await api('/api/admin/settings/budgets', { method: 'POST', body: JSON.stringify({ on: e.target.checked }) });
      msg(e.target.checked ? 'Minute limits are on.' : 'Minute limits are off. Kids can watch as much as they like.');
      await load();
    } catch (err) { e.target.checked = !e.target.checked; }
    return;
  }
  if (e.target.id === 'safariswitch') {
    try {
      await api('/api/admin/settings/safari', { method: 'POST', body: JSON.stringify({ on: e.target.checked }) });
      msg(e.target.checked ? 'New videos in Safari-incompatible formats will be converted after they appear.' : 'New videos in Safari-incompatible formats stay as they are.');
      await load();
    } catch (err) { e.target.checked = !e.target.checked; }
    return;
  }
  if (e.target.id === 'hwswitch') {
    try {
      await api('/api/admin/settings/hwencode', { method: 'POST', body: JSON.stringify({ on: e.target.checked }) });
      msg(e.target.checked ? 'New videos that need converting will use the graphics chip when it can.' : 'New videos that need converting will use the processor.');
      await load();
    } catch (err) { e.target.checked = !e.target.checked; }
    return;
  }
});

/* forms */
$('addkid').addEventListener('submit', async e => {
  e.preventDefault();
  const name = $('newname').value.trim();
  const pin = $('newpin').value.trim();
  if (!name) { msg('Type the kid\'s name.', true); $('newname').focus(); return; }
  if (!/^\d{4}$/.test(pin)) { msg('Give ' + name + ' a PIN of 4 digits.', true); $('newpin').focus(); return; }
  try {
    await api('/api/admin/kids', { method: 'POST', body: JSON.stringify({ name, pin }) });
    $('newname').value = '';
    $('newpin').value = '';
    msg(name + ' was added.');
    await load();
  } catch (err) { /* message already shown */ }
});

$('defaultform').addEventListener('submit', async e => {
  e.preventDefault();
  const minutes = readMinutes($('defaultmin'), STATE.default_minutes);
  if (minutes === null) return;
  try {
    await api('/api/admin/settings/default', { method: 'POST', body: JSON.stringify({ minutes }) });
    msg(`Each kid gets ${minutes} minutes a day from tomorrow, unless their own card says otherwise.`);
    await load();
  } catch (err) { /* message already shown */ }
});

// savePin saves the 4 digits typed in input; clear sends "" instead (only
// the watch-together PIN can be turned off: the parent page always has one).
async function savePin(input, path, savedText, clear) {
  const pin = clear ? '' : input.value.trim();
  if (!clear && !/^\d{4}$/.test(pin)) { msg('The PIN must be exactly 4 digits.', true); input.focus(); return; }
  try {
    await api(path, { method: 'POST', body: JSON.stringify({ pin }) });
    input.value = '';
    msg(savedText);
    await load();
  } catch (err) { /* the PIN lock or the error message is showing */ }
}

$('grownup-save').addEventListener('click', () =>
  savePin($('grownup'), '/api/admin/settings/grownup', 'Watch-together PIN saved.', false));
$('grownup-clear').addEventListener('click', () => {
  if (!confirm('Turn off the watch-together PIN? Grown-ups cannot watch for free until you make a new one.')) return;
  savePin($('grownup'), '/api/admin/settings/grownup', 'Watch-together PIN turned off.', true);
});
$('dashpin-save').addEventListener('click', () =>
  savePin($('dashpin'), '/api/admin/settings/dashboard', 'Parent-page PIN changed.', false));
$('counter-video').addEventListener('click', counterToVideo);
$('restart-update').addEventListener('click', restartToUpdate);
$('logmore').addEventListener('click', () => { logShown += LOG_STEP; renderWatchLog(); });
$('pinok').addEventListener('click', unlock);
$('pinval').addEventListener('keydown', e => { if (e.key === 'Enter') unlock(); });
$('forgot').addEventListener('click', showForgot);
$('pinreset').addEventListener('click', resetPin);
$('setupform').addEventListener('submit', saveSetup);
$('lock').addEventListener('click', lockPage);
$('quit').addEventListener('click', quitApp);
$('remove').addEventListener('click', removeApp);
$('fw-allow').addEventListener('click', letTabletsIn);
$('msgclose').addEventListener('click', hideMsg);
$('msgaction').addEventListener('click', () => { if (msgAction) msgAction(); });
$('stoppedclose').addEventListener('click', () => $('stopped').classList.add('hidden'));
document.addEventListener('keydown', e => {
  const s = $('stopped');
  if (e.key === 'Escape' && !s.classList.contains('hidden') && !s.dataset.final) s.classList.add('hidden');
});

setupDrag();
hidePin();
load().catch(() => { /* the lock or setup overlay is already showing */ });
