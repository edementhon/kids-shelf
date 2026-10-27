'use strict';
/* Parent dashboard: one state fetch, sections rendered from it, every
   mutation through /api/admin/*. Plain JS, no frameworks. */

const $ = id => document.getElementById(id);
let STATE = null;
let msgTimer = null;

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function fmt(n) { return Math.max(0, Math.round(n * 10) / 10); }
function cls(n) { return n < 0 ? 'neg' : ''; }

function msg(text, isErr) {
  const el = $('msg');
  el.textContent = text;
  el.className = 'msg ' + (isErr ? 'err' : 'ok');
  clearTimeout(msgTimer);
  msgTimer = setTimeout(() => { el.textContent = ''; el.className = 'msg'; }, 4000);
}

/* ── API with the dashboard-PIN gate ─────────────────────────────────── */
async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    if (!opts.quiet) msg('Network error. Is kids-shelf still running?', true);
    throw e;
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* non-JSON error body */ }
  if (res.status === 401 && !opts.pinFlow) {
    showPin();
    const err = new Error('pin');
    err.pinRequired = true;
    throw err;
  }
  if (!res.ok) {
    if (!opts.quiet) msg((data && data.error) || 'Something went wrong.', true);
    const err = new Error((data && data.error) || String(res.status));
    err.status = res.status;
    throw err;
  }
  return data;
}

/* ── loading ──────────────────────────────────────────────────────────── */
async function load() {
  const st = await api('/api/admin/state'); // throws 'pin' when the page is locked
  STATE = st;
  renderAll();
  refreshProblems(); // the banner is independent of the kid-state fetch
}

function renderAll() {
  renderKids();
  renderWatchLog();
  renderBins();
  renderSettings();
  renderAddress();
  renderPinPrompt();
  renderUpdates();
  $('statusline').textContent = STATE.budgets_on
    ? ''
    : 'Budgets are off. Kids can watch as much as they like, nothing is charged.';
}

/* ── helpers and firewall problems ───────────────────────────────────── */
async function renderProblems(problems) {
  const helpers = $('helpersproblem');
  const helpersText = (problems && problems.helpers) || '';
  helpers.textContent = helpersText;
  helpers.classList.toggle('hidden', !helpersText);

  const fw = problems && problems.firewall;
  const blocked = fw && fw.blocked && !fw.unknown;
  $('firewall').classList.toggle('show', !!blocked);
  $('sec-problems').classList.toggle('show', !!(helpersText || blocked));
}

async function refreshProblems() {
  try {
    const problems = await api('/api/admin/problems', { quiet: true });
    await renderProblems(problems);
  } catch (e) { /* the PIN overlay (or the page being gone) handles it */ }
}

async function letTabletsIn() {
  try {
    const out = await api('/api/admin/firewall/allow', { method: 'POST', body: '{}' });
    const fw = out && out.firewall;
    if (fw && !fw.blocked && !fw.unknown) {
      msg('Tablets and phones can reach kids-shelf now.');
      $('firewall').classList.remove('show');
      const helpers = $('helpersproblem').textContent;
      $('sec-problems').classList.toggle('show', !!helpers);
    }
  } catch (e) { /* message already shown */ }
}

/* ── kids ─────────────────────────────────────────────────────────────── */
function renderKids() {
  const wrap = $('kids');
  const kids = STATE.kids;
  if (!kids.length) {
    wrap.innerHTML = '<p class="placeholder">No kids yet. Add the first one below.</p>';
    return;
  }
  wrap.innerHTML = kids.map(k => {
    const total = k.today + k.saved;
    const out = total <= 0;
    const sub = out ? ' disabled' : '';
    return `
    <div class="kid" data-id="${esc(k.id)}">
      <div class="kidhead">
        <span class="kidname">${esc(k.name)}</span>
        <button class="mini" data-act="kidmenu" data-id="${esc(k.id)}" aria-label="Options for ${esc(k.name)}">⋯</button>
      </div>
      <div class="bal">
        <span>today <b class="${cls(k.today)}">${fmt(k.today)}</b></span>
        <span>saved <b class="${cls(k.saved)}">${fmt(k.saved)}</b></span>
      </div>
      <p class="note">Saved minutes carry over from earlier days (practice or time not used).</p>
      <div class="dailyrow">
        <label>minutes/day</label>
        <input type="number" class="daily" data-id="${esc(k.id)}" value="${k.daily}" min="0" step="1" inputmode="decimal">
      </div>
      <div class="btns">
        <button class="sub" data-act="adj" data-id="${esc(k.id)}" data-d="-10"${sub}>−10</button>
        <button class="sub" data-act="adj" data-id="${esc(k.id)}" data-d="-5"${sub}>−5</button>
        <button class="sub" data-act="adj" data-id="${esc(k.id)}" data-d="-1"${sub}>−1</button>
        <button class="add" data-act="adj" data-id="${esc(k.id)}" data-d="1">+1</button>
        <button class="add" data-act="adj" data-id="${esc(k.id)}" data-d="5">+5</button>
        <button class="add" data-act="adj" data-id="${esc(k.id)}" data-d="10">+10</button>
      </div>
    </div>`;
  }).join('');
}


function renderWatchLog() {
  const tb = document.querySelector('#watchlog tbody');
  const log = STATE.watch_log;
  if (!log.length) {
    tb.innerHTML = '<tr><td colspan="5" class="placeholder">Nothing watched yet.</td></tr>';
    return;
  }
  tb.innerHTML = log.map(e => `
    <tr>
      <td>${esc(e.at.replace('T', ' '))}</td>
      <td>${esc(e.kid_name || (e.kid ? '-' : 'Grown-up (together)'))}</td>
      <td>${esc(e.show || '-')}</td>
      <td>${esc(e.title)}</td>
      <td class="${e.free ? 'free' : ''}">${e.free ? 'free' : fmt(e.cost) + ' min'}</td>
    </tr>`).join('');
}

/* ── bins ─────────────────────────────────────────────────────────────── */
function showCard(s) {
  return `<div class="show" draggable="true" data-id="${esc(s.id)}">
    <span class="showname">${esc(s.name)}</span>
    <span class="showeps">${s.episodes} ep</span>
    <button class="mini" data-act="showmenu" data-id="${esc(s.id)}" aria-label="Move ${esc(s.name)}">⋯</button>
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
    const card = e.target.closest('.show');
    if (!card) return;
    dragging = card.dataset.id;
    card.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
  });
  document.addEventListener('dragend', e => {
    const card = e.target.closest('.show');
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

/* tap-to-move menu (touch screens) and the kid options menu */
function popupMenu(x, y, items) {
  const menu = $('menu');
  menu.innerHTML = items.map(it =>
    `<button class="${it.danger ? 'danger' : ''}" data-act="${it.act}" data-id="${esc(it.id)}">${esc(it.label)}</button>`
  ).join('');
  menu.classList.remove('hidden');
  const rect = menu.getBoundingClientRect();
  let left = Math.min(x, window.innerWidth - rect.width - 8);
  let top = Math.min(y, window.innerHeight - rect.height - 8);
  menu.style.left = Math.max(8, left) + 'px';
  menu.style.top = Math.max(8, top) + 'px';
}
function hideMenu() { $('menu').classList.add('hidden'); }

/* ── settings ─────────────────────────────────────────────────────────── */
function renderSettings() {
  $('defaultmin').value = STATE.default_minutes;
  $('budgetswitch').checked = STATE.budgets_on;
  const hasCounter = 'counter' in STATE.settings;
  $('counter').disabled = !hasCounter;
  $('counter').value = hasCounter ? STATE.settings.counter : '';
  $('counter').placeholder = hasCounter ? 'video' : 'practice is not running';
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

// The pin prompt: once a kid exists, a page without a parent-page PIN is a
// door any tablet can open, so the dashboard says so right at the top with
// the field next to the words.
function renderPinPrompt() {
  const show = STATE.kids.length > 0 && !STATE.settings.dashboard_pin_set;
  $('sec-pinprompt').classList.toggle('hidden', !show);
}

$('pinpromptform').addEventListener('submit', async e => {
  e.preventDefault();
  await savePinInput($('pinprompt'), '/api/admin/settings/dashboard', 'Parent-page PIN');
});

async function saveCounter() {
  const counter = $('counter').value.trim();
  try {
    await api('/api/admin/settings/counter', { method: 'POST', body: JSON.stringify({ counter }) });
    msg('Practice earnings now go to "' + counter + '".');
    await load();
  } catch (e) { /* message already shown */ }
}

/* ── updates ──────────────────────────────────────────────────────────── */
function renderUpdates() {
  const u = STATE.updates || {};
  $('updatemsg').textContent = u.message || 'This copy does not update itself.';
  $('restart-update').classList.toggle('hidden', u.state !== 'staged');
}

/* ── PIN gate ─────────────────────────────────────────────────────────── */
function showPin() {
  $('pinoverlay').classList.remove('hidden');
  $('pinerr').textContent = '';
  setTimeout(() => $('pinval').focus(), 50);
}
function hidePin() { $('pinoverlay').classList.add('hidden'); }

async function unlock() {
  const pin = $('pinval').value.trim();
  if (!/^\d{4}$/.test(pin)) { $('pinerr').textContent = 'The PIN is 4 digits.'; return; }
  try {
    await api('/api/admin/pin', { method: 'POST', body: JSON.stringify({ pin }), pinFlow: true });
    $('pinval').value = '';
    hidePin();
    await load();
  } catch (e) {
    if (e && e.status === 401) { $('pinerr').textContent = 'That PIN is not right. Try again.'; }
  }
}

/* ── quit ─────────────────────────────────────────────────────────────── */
async function quitApp() {
  if (!confirm('Quit kids-shelf? Nothing will run until you open it again from the desktop icon.')) return;
  $('stopped').classList.remove('hidden'); // optimistically: the app may die before answering
  try {
    await api('/api/admin/quit', { method: 'POST', pinFlow: true, quiet: true });
  } catch (e) { /* the app usually stops before the reply lands */ }
}

/* ── remove ──────────────────────────────────────────────────────────── */
async function removeApp() {
  let info;
  try {
    info = await api('/api/admin/remove'); // {data_dir, media_dir} for the confirmation
  } catch (e) { return; }
  const del = $('removedata').checked;
  const folders = `\n  ${info.data_dir}\n  ${info.media_dir}`;
  const text = del
    ? `Remove kids-shelf from this computer? The program, the videos, the settings and everything inside these folders will be deleted:${folders}`
    : `Remove kids-shelf from this computer? The program is deleted. These folders stay:${folders}`;
  if (!confirm(text)) return;
  $('stopped').classList.remove('hidden'); // optimistically: the app may die before answering
  try {
    await api('/api/admin/remove', {
      method: 'POST',
      body: JSON.stringify({ deleteData: del }),
      pinFlow: true,
      quiet: true,
    });
  } catch (e) { /* the app usually stops before the reply lands */ }
}

/* ── events ───────────────────────────────────────────────────────────── */
async function onAction(act, t) {
  const id = t.dataset.id;
  const kid = STATE.kids.find(k => k.id === id);
  switch (act) {
    case 'adj': {
      const delta = Number(t.dataset.d);
      try {
        await api('/api/admin/adjust', { method: 'POST', body: JSON.stringify({ kid: id, delta }) });
        await load();
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'kidmenu': {
      const r = t.getBoundingClientRect();
      popupMenu(r.right, r.bottom, [
        { act: 'renamemenu', id, label: '✏️ Rename ' + (kid ? kid.name : '') },
        { act: 'pinmenu', id, label: '🔑 Change PIN' },
        { act: 'removemenu', id, label: '🗑️ Remove kid', danger: true },
      ]);
      break;
    }
    case 'renamemenu': {
      hideMenu();
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
      hideMenu();
      const pin = prompt('New 4-digit PIN for ' + (kid ? kid.name : 'this kid') + ':');
      if (pin === null) break;
      if (!/^\d{4}$/.test(pin)) { msg('The PIN must be exactly 4 digits.', true); break; }
      try {
        await api('/api/admin/kids/' + encodeURIComponent(id) + '/pin', { method: 'POST', body: JSON.stringify({ pin }) });
        msg('PIN saved.');
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'removemenu': {
      hideMenu();
      if (!confirm('Remove ' + (kid ? kid.name : 'this kid') + '? Their minutes, PIN and watch history are deleted.')) break;
      try {
        await api('/api/admin/kids/' + encodeURIComponent(id), { method: 'DELETE', pinFlow: true });
        msg('Kid removed.');
        await load();
      } catch (e) { /* message already shown */ }
      break;
    }
    case 'showmenu': {
      const r = t.getBoundingClientRect();
      popupMenu(r.right, r.bottom, [
        { act: 'binmove', id, label: '🟢 Move to Fun' },
        { act: 'binmove', id, label: '🟡 Move to Learning' },
        { act: 'binmove', id, label: '⚪ Move to Free' },
      ]);
      $('menu').querySelectorAll('button')[0].dataset.meta = 'fun';
      $('menu').querySelectorAll('button')[1].dataset.meta = 'learning';
      $('menu').querySelectorAll('button')[2].dataset.meta = 'free';
      break;
    }
    case 'binmove':
      hideMenu();
      await moveBin(id, t.dataset.meta);
      break;
  }
}

document.addEventListener('click', e => {
  if (e.target.closest('[data-act]')) {
    onAction(e.target.closest('[data-act]').dataset.act, e.target.closest('[data-act]'));
    return;
  }
  if (!e.target.closest('#menu')) hideMenu();
});

/* writes (change events) */
document.addEventListener('change', async e => {
  if (e.target.classList && e.target.classList.contains('daily')) {
    const id = e.target.dataset.id;
    const minutes = Number(e.target.value);
    if (!(minutes >= 0)) { msg('Minutes cannot be negative.', true); load(); return; }
    try {
      await api('/api/admin/kids/' + encodeURIComponent(id) + '/minutes', { method: 'POST', body: JSON.stringify({ minutes }) });
      msg('Daily minutes saved.');
    } catch (err) { /* message already shown */ }
    load();
    return;
  }
  if (e.target.id === 'budgetswitch') {
    try {
      await api('/api/admin/settings/budgets', { method: 'POST', body: JSON.stringify({ on: e.target.checked }) });
      msg(e.target.checked ? 'Budgets are on.' : 'Budgets are off.');
      await load();
    } catch (err) { /* message already shown */ }
  }
});

/* forms */
$('addkid').addEventListener('submit', async e => {
  e.preventDefault();
  const name = $('newname').value.trim();
  const pin = $('newpin').value.trim();
  if (!name || !/^\d{4}$/.test(pin)) { msg('Give a name and a 4-digit PIN.', true); return; }
  try {
    await api('/api/admin/kids', { method: 'POST', body: JSON.stringify({ name, pin }) });
    $('newname').value = '';
    $('newpin').value = '';
    msg('Kid added.');
    await load();
  } catch (err) { /* message already shown */ }
});

$('defaultform').addEventListener('submit', async e => {
  e.preventDefault();
  const minutes = Number($('defaultmin').value);
  if (!(minutes >= 0)) { msg('Minutes cannot be negative.', true); return; }
  try {
    await api('/api/admin/settings/default', { method: 'POST', body: JSON.stringify({ minutes }) });
    msg('Default minutes saved.');
    await load();
  } catch (err) { /* message already shown */ }
});

async function savePinInput(input, path, what) {
  const pin = input.value.trim();
  if (pin !== '' && !/^\d{4}$/.test(pin)) { msg('The PIN must be exactly 4 digits.', true); return; }
  try {
    await api(path, { method: 'POST', body: JSON.stringify({ pin }), pinFlow: true });
    input.value = '';
    msg(what + (pin ? ' saved.' : ' cleared.'));
    await load();
  } catch (err) { /* message already shown */ }
}

$('grownup-save').addEventListener('click', () =>
  savePinInput($('grownup'), '/api/admin/settings/grownup', 'Watch-together PIN'));
$('grownup-clear').addEventListener('click', () =>
  savePinInput($('grownup'), '/api/admin/settings/grownup', 'Watch-together PIN'));
$('dashpin-save').addEventListener('click', () =>
  savePinInput($('dashpin'), '/api/admin/settings/dashboard', 'Parent-page PIN'));
$('dashpin-clear').addEventListener('click', () =>
  savePinInput($('dashpin'), '/api/admin/settings/dashboard', 'Parent-page PIN'));
$('counter-save').addEventListener('click', saveCounter);
$('restart-update').addEventListener('click', async () => {
  try {
    await api('/api/admin/update/restart', { method: 'POST', pinFlow: true, quiet: true });
    msg('Restarting to apply the update…');
  } catch (e) { /* message already shown */ }
});
$('pinok').addEventListener('click', unlock);
$('pinval').addEventListener('keydown', e => { if (e.key === 'Enter') unlock(); });
$('quit').addEventListener('click', quitApp);
$('remove').addEventListener('click', removeApp);
$('fw-allow').addEventListener('click', letTabletsIn);
window.addEventListener('scroll', hideMenu, { passive: true });
window.addEventListener('resize', hideMenu);

setupDrag();
hidePin();
load().catch(() => { /* the pin overlay is already showing */ });
