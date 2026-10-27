/* Add videos & libraries page logic: plain fetch + DOM, no frameworks.
   All text comes from the server or this file, never from user input
   unescaped (esc()). */
'use strict';
const $ = id => document.getElementById(id);

let store = {
  ytJobs: [], ingestJobs: [], sources: [],
  installed: [], kDownloads: [], freeBytes: -1,
  catalog: { total: 0, start: 0, entries: [] },
  languages: [], categories: [],
  catQ: '', catLang: '', catCat: ''
};

const STATE_LABEL = { queued: 'Waiting', running: 'Working', paused: 'Paused', done: 'Done', failed: 'Failed' };
const YT_STAGE = { resolve: 'Looking it up…', download: 'Downloading', handoff: 'Adding to the shelf', expand: 'Opening the list…' };
const INGEST_STAGE = {
  probe: 'Checking the file', plan: 'Planning the conversion', show: 'Adding to the shelf',
  convert: 'Converting', verify: 'Checking the result', thumbnail: 'Making a picture', publish: 'Adding to the shelf'
};
const KIND_LABEL = { folder: 'Folder', nas: 'NAS share' };
const FLAVOUR_LABEL = { maxi: 'with pictures', nopic: 'text only', mini: 'short version' };
const LANG_FALLBACK = {
  eng: 'English', fra: 'French', deu: 'German', spa: 'Spanish', ita: 'Italian',
  por: 'Portuguese', nld: 'Dutch', rus: 'Russian', jpn: 'Japanese', zho: 'Chinese',
  kor: 'Korean', ara: 'Arabic', hin: 'Hindi', pol: 'Polish', tur: 'Turkish',
  swe: 'Swedish', dan: 'Danish', fin: 'Finnish', nor: 'Norwegian', ces: 'Czech',
  ell: 'Greek', heb: 'Hebrew', hun: 'Hungarian', ron: 'Romanian', ukr: 'Ukrainian',
  tha: 'Thai', vie: 'Vietnamese', ind: 'Indonesian', ben: 'Bengali', urd: 'Urdu',
  fas: 'Persian', swa: 'Swahili', cat: 'Catalan', slk: 'Slovak', slv: 'Slovenian',
  hrv: 'Croatian', srp: 'Serbian', bul: 'Bulgarian', lit: 'Lithuanian', lav: 'Latvian',
  est: 'Estonian', isl: 'Icelandic', gle: 'Irish', cym: 'Welsh', mlt: 'Maltese',
  sqi: 'Albanian', mkd: 'Macedonian', hye: 'Armenian', kat: 'Georgian', aze: 'Azerbaijani',
  kaz: 'Kazakh', uzb: 'Uzbek', mon: 'Mongolian', nep: 'Nepali', sin: 'Sinhala',
  khm: 'Khmer', lao: 'Lao', mya: 'Burmese', amh: 'Amharic', tgl: 'Tagalog'
};
// Browser language tags ("en-US") to the 3-letter catalog codes; unknown
// languages default to English, which every Kiwix catalog carries.
const LANG2TO3 = {
  en: 'eng', fr: 'fra', de: 'deu', es: 'spa', it: 'ita', pt: 'por', nl: 'nld',
  ru: 'rus', ja: 'jpn', zh: 'zho', ko: 'kor', ar: 'ara', hi: 'hin', pl: 'pol',
  tr: 'tur', sv: 'swe', da: 'dan', fi: 'fin', no: 'nor', cs: 'ces', el: 'ell',
  he: 'heb', hu: 'hun', ro: 'ron', uk: 'ukr', th: 'tha', vi: 'vie', id: 'ind',
  ms: 'msa', bn: 'ben', ur: 'urd', fa: 'fas', sw: 'swa', ca: 'cat', sk: 'slk',
  sl: 'slv', hr: 'hrv', sr: 'srp', bg: 'bul', lt: 'lit', lv: 'lav', et: 'est',
  is: 'isl', ga: 'gle', cy: 'cym', mt: 'mlt', sq: 'sqi', mk: 'mkd', hy: 'hye',
  ka: 'kat', az: 'aze', kk: 'kaz', uz: 'uzb', mn: 'mon', ne: 'nep', si: 'sin',
  km: 'khm', lo: 'lao', my: 'mya', am: 'amh', tl: 'tgl'
};
let langNames = {}; // ISO 639-3 code -> display name from the catalog's languages feed

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function bytes(n) {
  if (n == null || n < 0) return '';
  const gb = 1024 * 1024 * 1024, mb = 1024 * 1024;
  if (n >= 10 * gb) return Math.round(n / gb) + ' GB';
  if (n >= gb) return (n / gb).toFixed(1) + ' GB';
  if (n >= mb) return Math.ceil(n / mb) + ' MB';
  return Math.max(1, Math.ceil(n / mb)) + ' MB';
}

function msg(text, kind) {
  const el = $('msg');
  el.textContent = text || '';
  el.className = 'msg ' + (kind || '');
  if (kind) el.classList.remove('hidden');
  else el.classList.add('hidden');
}

// api() throws Error(message) with the server's plain-words message; a 401
// (the optional dashboard PIN is set) opens the PIN overlay instead.
async function api(path, opts) {
  opts = opts || {};
  // A download request can stall (a slow catalog, a busy machine); the page
  // must never sit silent forever, so the caller can ask for a deadline.
  const ctl = opts.timeout ? new AbortController() : null;
  let timer = null;
  if (ctl) {
    timer = setTimeout(() => ctl.abort(), opts.timeout);
    opts.signal = ctl.signal;
  }
  let resp;
  try {
    resp = await fetch(path, opts);
  } catch (e) {
    if (ctl && ctl.signal.aborted) {
      throw new Error('Kids-shelf is taking a long time to answer. Check the Downloads list, or try again in a moment.');
    }
    throw new Error('This computer can’t be reached right now. Try again.');
  } finally {
    clearTimeout(timer);
  }
  if (resp.status === 401) {
    showPin();
    throw new Error('PIN needed');
  }
  let data = null;
  try { data = await resp.json(); } catch (e) { /* some responses have no body */ }
  if (!resp.ok) {
    // The server's plain-words message is always the one shown.
    throw new Error((data && data.error) || 'Something went wrong.');
  }
  return data;
}

async function post(path, body, timeout) {
  const opts = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  };
  if (timeout) opts.timeout = timeout;
  return api(path, opts);
}


// ── pin overlay ─────────────────────────────────────────────────────────────

function showPin() {
  $('pinval').value = '';
  $('pinerr').classList.add('hidden');
  $('pinoverlay').classList.remove('hidden');
  $('pinval').focus();
}
function hidePin() {
  $('pinoverlay').classList.add('hidden');
}
async function unlock() {
  $('pinerr').classList.add('hidden');
  try {
    await post('/api/admin/pin', { pin: $('pinval').value });
    hidePin();
    await loadAll();
  } catch (e) {
    if (e.message === 'PIN needed') return;
    $('pinerr').textContent = e.message;
    $('pinerr').classList.remove('hidden');
  }
}
$('pinok').addEventListener('click', unlock);
$('pinval').addEventListener('keydown', ev => { if (ev.key === 'Enter') unlock(); });

// ── YouTube ─────────────────────────────────────────────────────────────────

$('ytform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const url = $('yturl').value.trim();
  if (!url) return;
  try {
    await post('/api/admin/content/youtube', { url });
    $('yturl').value = '';
    msg('Added. The download starts shortly.', 'ok');
    await loadYouTube();
  } catch (e) {
    msg(e.message, 'err');
  }
});

function ytProgress(job) {
  if (!job.progress) return '';
  return `<div class="pbar"><i style="width:${Math.min(100, Math.max(0, job.progress))}%"></i></div>`;
}

function renderYouTube() {
  const wrap = $('ytjobs');
  const jobs = store.ytJobs;
  if (!jobs.length) {
    wrap.innerHTML = '<div class="empty">Nothing added yet.</div>';
    return;
  }
  wrap.innerHTML = jobs.map(job => {
    const stage = (job.state === 'running' && YT_STAGE[job.stage]) ? ' · ' + YT_STAGE[job.stage] : '';
    const retry = job.state === 'failed'
      ? `<button class="btn mini" data-retry="${esc(job.id)}">Retry</button>` : '';
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(job.title)}</span>
          <span class="badge ${esc(job.state)}">${esc(STATE_LABEL[job.state] || job.state)}${stage}</span>
          ${retry}
        </div>
        <div class="jobsub">${esc(job.url)}</div>
        ${job.message ? `<div class="jobmsg">${esc(job.message)}</div>` : ''}
        ${ytProgress(job)}
      </div>`;
  }).join('');
}

async function retryYouTube(id) {
  try {
    await post('/api/admin/content/youtube/retry', { id });
    await loadYouTube();
  } catch (e) {
    msg(e.message, 'err');
  }
}

async function loadYouTube() {
  try {
    const d = await api('/api/admin/content/youtube');
    store.ytJobs = d.jobs || [];
    renderYouTube();
  } catch (e) {
    if (e.message !== 'PIN needed') renderYouTubeError(e.message);
  }
}

function renderYouTubeError(text) {
  $('ytjobs').innerHTML = `<div class="jobmsg">${esc(text)}</div>`;
}

// event delegation for retry buttons
$('ytjobs').addEventListener('click', ev => {
  const b = ev.target.closest('[data-retry]');
  if (b) retryYouTube(b.dataset.retry);
});

// ── sources ─────────────────────────────────────────────────────────────────

$('srcform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const path = $('srcpath').value.trim();
  if (!path) return;
  try {
    await post('/api/admin/content/sources', { path, kind: $('srckind').value });
    $('srcpath').value = '';
  } catch (e) {
    msg(e.message, 'err');
    return;
  }
  // A fresh folder needs a scan before its videos show up, so the page
  // starts one right away instead of leaving the parent to hunt for the
  // Scan now button.
  msg('Folder added. Checking it for videos now.', 'ok');
  try {
    await post('/api/admin/content/scan');
    msg('Folder added and scanned. New videos are being added to the shelf.', 'ok');
  } catch (e) {
    msg('Folder added, but the scan could not finish. ' + e.message, 'err');
  }
  await loadSources();
  await loadIngest();
});

$('scanbtn').addEventListener('click', async () => {
  try {
    await post('/api/admin/content/scan');
    msg('Scanning done. New videos are being converted.', 'ok');
    await loadSources();
    await loadIngest();
  } catch (e) {
    msg(e.message, 'err');
  }
});

function renderSources() {
  const wrap = $('sources');
  const srcs = store.sources;
  if (!srcs.length) {
    wrap.innerHTML = '<div class="empty">No folders yet. Add one above.</div>';
    return;
  }
  wrap.innerHTML = srcs.map(src => `
    <div class="source">
      <span class="sourcepath">${esc(src.path)}</span>
      <span class="tag">${esc(KIND_LABEL[src.kind] || src.kind)}</span>
      <button class="btn mini sub" data-srcpath="${esc(src.path)}">Remove</button>
    </div>`).join('');
}

async function removeSource(path) {
  try {
    await post('/api/admin/content/sources/remove', { path });
    await loadSources();
  } catch (e) {
    msg(e.message, 'err');
  }
}

$('sources').addEventListener('click', ev => {
  const b = ev.target.closest('[data-srcpath]');
  if (b) removeSource(b.dataset.srcpath);
});

async function loadSources() {
  try {
    const d = await api('/api/admin/content/sources');
    store.sources = d.sources || [];
    renderSources();
  } catch (e) {
    if (e.message !== 'PIN needed') msg(e.message, 'err');
  }
}

// ── conversions ─────────────────────────────────────────────────────────────

// ingestStatus says what is happening with a conversion in plain words: the
// pause reason or failure message when there is one, the running stage
// otherwise, 'On the shelf' once the video is published.
function ingestStatus(job) {
  switch (job.state) {
    case 'done':
      return 'On the shelf';
    case 'failed':
      return job.message || 'Failed';
    case 'paused':
      return job.message || 'Paused';
    case 'running':
      return INGEST_STAGE[job.stage] || 'Working';
    default:
      return STATE_LABEL[job.state] || job.state;
  }
}

function renderIngest() {
  const wrap = $('ingestjobs');
  const jobs = store.ingestJobs;
  if (!jobs.length) {
    wrap.innerHTML = '<div class="empty">Nothing waiting. New videos appear here.</div>';
    return;
  }
  wrap.innerHTML = jobs.map(job => {
    const status = ingestStatus(job);
    const title = job.title || job.file;
    // The message doubles as the status for failed and paused jobs, so only
    // show it again on its own line when it is not already in the badge.
    const detail = (job.message && status !== job.message)
      ? `<div class="jobmsg">${esc(job.message)}</div>` : '';
    const path = (job.file && job.file !== title)
      ? `<div class="jobsub">${esc(job.file)}</div>` : '';
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(title)}</span>
          <span class="badge ${esc(job.state)}">${esc(status)}</span>
        </div>
        ${path}${detail}
      </div>`;
  }).join('');
}

async function loadIngest() {
  try {
    const d = await api('/api/admin/content/ingest');
    store.ingestJobs = d.jobs || [];
    renderIngest();
  } catch (e) {
    if (e.message !== 'PIN needed') {
      $('ingestjobs').innerHTML = `<div class="jobmsg">${esc(e.message)}</div>`;
    }
  }
}

// ── fixing videos ────────────────────────────────────────────────────────────

// FIX_ISSUE puts each issue code into plain words a parent understands.
// The sentences stay in sync with the kid pages' explanations.
const FIX_ISSUE = {
  sound: 'No sound in most browsers (Dolby or DTS sound)',
  picture: 'Picture may not show (HEVC or 10-bit video)',
  format: 'Will not open on iPads, iPhones or Safari (MKV file)',
};

function renderFixing() {
  const wrap = $('fixing');
  const f = store.fixing || { pending: { sound: 0, picture: 0, format: 0 }, failed: 0, failedEpisodes: [], failedMore: 0 };
  const waiting = f.pending.sound + f.pending.picture + f.pending.format;
  if (!waiting && !f.failed) {
    wrap.innerHTML = '<div class="empty">No videos are being fixed right now.</div>';
    return;
  }
  let html = '';
  if (waiting) {
    const parts = [];
    if (f.pending.sound) parts.push(`${f.pending.sound} with no sound`);
    if (f.pending.picture) parts.push(`${f.pending.picture} with no picture`);
    if (f.pending.format) parts.push(`${f.pending.format} that iPad or iPhone cannot open`);
    html += `<div class="fixingline">⏳ ${waiting} video${waiting === 1 ? '' : 's'} being fixed: ${esc(parts.join(', '))}.</div>`;
  }
  if (f.failed) {
    html += `<div class="fixingline">❌ ${f.failed} video${f.failed === 1 ? '' : 's'} could not be fixed and ${f.failed === 1 ? 'plays' : 'play'} as they are:</div>`;
    html += (f.failedEpisodes || []).map(e => `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(e.title)}</span>
          <span class="jobsub">${esc(e.show)}</span>
        </div>
        <div class="jobmsg">${esc(FIX_ISSUE[e.issue] || e.issue)}</div>
      </div>`).join('');
    if (f.failedMore) {
      html += `<div class="fixingline">and ${f.failedMore} more</div>`;
    }
  }
  wrap.innerHTML = html;
}

async function loadFixes() {
  try {
    const d = await api('/api/admin/content/fixes');
    store.fixing = d;
    renderFixing();
  } catch (e) {
    if (e.message !== 'PIN needed') {
      $('fixing').innerHTML = `<div class="jobmsg">${esc(e.message)}</div>`;
    }
  }
}

// ── libraries (Kiwix) ───────────────────────────────────────────────────────

// langName turns the catalog's comma-separated ISO 639-3 codes into display
// names: the /catalog/v2/languages feed (loaded into langNames) is the
// source of truth, the built-in table covers the common languages until that
// feed has arrived, and an unknown code is kept as-is rather than hidden.
function langName(codes) {
  return String(codes).split(',').map(c => {
    c = c.trim();
    if (!c) return '';
    return langNames[c] || LANG_FALLBACK[c] || c;
  }).filter(Boolean).join(', ');
}

function flavourLabel(f) {
  return FLAVOUR_LABEL[f] || '';
}

function zimDesc(z) {
  const bits = [];
  if (z.language) bits.push('Language: ' + esc(langName(z.language)));
  if (z.category) bits.push('Kind: ' + esc(z.category));
  if (z.article_count) bits.push(z.article_count.toLocaleString() + ' articles');
  if (z.size) bits.push('<b>' + esc(bytes(z.size)) + '</b>');
  return bits.join(' · ');
}

function renderCatalog() {
  const wrap = $('catalog');
  $('catstatus').textContent = store.catalog.total
    ? `${store.catalog.total} libraries found`
    : (store.catQ || store.catLang || store.catCat ? 'No libraries match that search.' : '');
  if (!store.catalog.entries.length) {
    wrap.innerHTML = '<div class="empty">Search above to browse the catalog.</div>';
    return;
  }
  wrap.innerHTML = store.catalog.entries.map(z => `
    <div class="cat">
      <div class="catmeta">
        <div class="title">${esc(z.title)}${z.flavour ? ` <span class="flav">${esc(flavourLabel(z.flavour))}</span>` : ''}</div>
        ${z.summary ? `<div class="summ">${esc(z.summary)}</div>` : ''}
        <div class="meta">${zimDesc(z)}</div>
      </div>
      <button class="btn add" data-dl='${esc(JSON.stringify(z))}'>Download</button>
    </div>`).join('');
  if (store.catalog.start + store.catalog.entries.length < store.catalog.total) {
    wrap.insertAdjacentHTML('beforeend',
      `<div class="rowbtns"><button class="btn" id="catmore">Show more</button></div>`);
    $('catmore').addEventListener('click', () => searchCatalog(store.catalog.start + store.catalog.entries.length));
  }
}

let pendingDownload = null;

$('catalog').addEventListener('click', ev => {
  const b = ev.target.closest('[data-dl]');
  if (!b) return;
  let z;
  try { z = JSON.parse(b.dataset.dl); } catch (e) { return; }
  pendingDownload = z;
  const free = store.freeBytes >= 0 ? ` About ${bytes(store.freeBytes)} is free on your media drive.` : '';
  $('confirmtext').innerHTML =
    `Download <b>${esc(z.title)}</b>? It needs ${esc(bytes(z.size))}.${free}<br>` +
    `It only works offline once the download has finished.`;
  $('confirm').classList.remove('hidden');
});

$('confirmno').addEventListener('click', () => { $('confirm').classList.add('hidden'); });

$('confirmok').addEventListener('click', async () => {
  $('confirm').classList.add('hidden');
  if (!pendingDownload) return;
  const z = pendingDownload;
  pendingDownload = null;
  try {
    await post('/api/admin/content/kiwix/download', { id: z.id, name: z.name, flavour: z.flavour || '' }, 90000);
    msg('Download started. It runs in the background.', 'ok');
    await loadKiwix();
  } catch (e) {
    msg(e.message, 'err');
  }
});

function renderInstalled() {
  const wrap = $('installed');
  const list = store.installed;
  if (!list.length) {
    wrap.innerHTML = '<div class="empty">Nothing installed yet. Pick a library above.</div>';
    return;
  }
  wrap.innerHTML = list.map(z => `
    <div class="cat">
      <div class="catmeta">
        <div class="title">${esc(z.title)}</div>
        <div class="meta">${esc(bytes(z.size))}${z.language ? ' · ' + esc(langName(z.language)) : ''}</div>
      </div>
      <button class="btn sub" data-rm="${esc(z.id)}">Remove</button>
    </div>`).join('');
}

let pendingRemove = null;

$('installed').addEventListener('click', ev => {
  const b = ev.target.closest('[data-rm]');
  if (!b) return;
  pendingRemove = b.dataset.rm;
  const z = store.installed.find(x => x.id === pendingRemove);
  $('removeconfirmtext').innerHTML =
    `Remove <b>${esc(z ? z.title : 'this library')}</b> from the shelf? The downloaded file is deleted.`;
  $('removeconfirm').classList.remove('hidden');
});

$('removeno').addEventListener('click', () => { $('removeconfirm').classList.add('hidden'); });

$('removeok').addEventListener('click', async () => {
  $('removeconfirm').classList.add('hidden');
  if (!pendingRemove) return;
  const id = pendingRemove;
  pendingRemove = null;
  try {
    await post('/api/admin/content/kiwix/remove', { id });
    await loadKiwix();
  } catch (e) {
    msg(e.message, 'err');
  }
});

function renderKiwixDownloads() {
  const wrap = $('kdownloads');
  const jobs = store.kDownloads;
  if (!jobs.length) {
    wrap.innerHTML = '<div class="empty">No downloads running.</div>';
    return;
  }
  wrap.innerHTML = jobs.map(job => {
    const title = (job.entry && job.entry.title) || (job.entry && job.entry.name) || 'Library';
    let pct = 0;
    if (job.entry && job.entry.size > 0) pct = Math.round(job.downloaded / job.entry.size * 100);
    const prog = (job.state === 'running' || job.state === 'paused') && pct
      ? `<div class="pbar ${esc(job.state)}"><i style="width:${Math.min(100, pct)}%"></i></div>` : '';
    const size = job.entry && job.entry.size ? ` of ${bytes(job.entry.size)}` : '';
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(title)}</span>
          <span class="badge ${esc(job.state)}">${esc(STATE_LABEL[job.state] || job.state)}${pct ? ` · ${pct}%${esc(size)}` : ''}</span>
        </div>
        ${job.message ? `<div class="jobmsg">${esc(job.message)}</div>` : ''}
        ${prog}
      </div>`;
  }).join('');
}

async function loadKiwix() {
  try {
    const d = await api('/api/admin/content/kiwix');
    store.installed = d.installed || [];
    store.kDownloads = d.downloads || [];
    store.freeBytes = d.free_bytes != null ? d.free_bytes : -1;
    renderInstalled();
    renderKiwixDownloads();
  } catch (e) {
    if (e.message !== 'PIN needed') {
      $('kdownloads').innerHTML = `<div class="jobmsg">${esc(e.message)}</div>`;
    }
  }
}

async function searchCatalog(start) {
  const q = $('catq').value.trim();
  const lang = $('catlang').value;
  const cat = $('catcat').value;
  if (start === 0) {
    store.catQ = q; store.catLang = lang; store.catCat = cat;
  }
  const params = new URLSearchParams();
  if (store.catQ) params.set('q', store.catQ);
  if (store.catLang) params.set('lang', store.catLang);
  if (store.catCat) params.set('category', store.catCat);
  params.set('count', '20');
  if (start) params.set('start', String(start));
  try {
    const d = await api('/api/admin/content/kiwix/catalog?' + params.toString());
    store.catalog = { total: d.total, start: d.start, entries: d.entries || [] };
    renderCatalog();
  } catch (e) {
    $('catalog').innerHTML = `<div class="jobmsg">${esc(e.message)}</div>`;
  }
}

$('catsearch').addEventListener('click', () => searchCatalog(0));
$('catq').addEventListener('keydown', ev => { if (ev.key === 'Enter') searchCatalog(0); });
$('catlang').addEventListener('change', () => searchCatalog(0));
$('catcat').addEventListener('change', () => searchCatalog(0));

// defaultLang picks the language filter the catalog should start with: the
// browser's own language when the catalog has libraries in it, English
// otherwise. 'Any language' stays selectable in the dropdown.
function defaultLang() {
  const tag = (navigator.language || 'en').split('-')[0].toLowerCase();
  const want = LANG2TO3[tag] || 'eng';
  const have = code => store.languages.some(l => l.code === code);
  if (have(want)) return want;
  if (have('eng')) return 'eng';
  return '';
}

async function loadCatalogMeta() {
  try {
    const [langs, cats] = await Promise.all([
      api('/api/admin/content/kiwix/languages'),
      api('/api/admin/content/kiwix/categories')
    ]);
    store.languages = langs.languages || [];
    store.categories = cats.categories || [];
    langNames = {};
    for (const l of store.languages) langNames[l.code] = l.name;
    $('catlang').innerHTML = `<option value="">Any language</option>` +
      store.languages.map(l => `<option value="${esc(l.code)}">${esc(l.name)}${l.count ? ` (${l.count})` : ''}</option>`).join('');
    $('catcat').innerHTML = `<option value="">Any kind</option>` +
      store.categories.map(c => `<option value="${esc(c)}">${esc(c)}</option>`).join('');
    $('catlang').value = store.catLang || defaultLang();
    $('catcat').value = store.catCat;
  } catch (e) {
    if (e.message !== 'PIN needed') msg(e.message, 'err');
  }
}

// ── loading ─────────────────────────────────────────────────────────────────

async function loadAll() {
  await Promise.all([loadYouTube(), loadSources(), loadIngest(), loadFixes(), loadKiwix(), loadCatalogMeta()]);
}

function poll() {
  loadYouTube();
  loadIngest();
  loadFixes();
  loadKiwix();
}

loadAll();
setInterval(poll, 4000);
