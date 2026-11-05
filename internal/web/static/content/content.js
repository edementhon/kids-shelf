/* Add videos & libraries page logic: plain fetch + DOM, no frameworks.
   All text comes from the server or this file, never from user input
   unescaped (esc()). */
'use strict';
const $ = id => document.getElementById(id);

let store = {
  ytJobs: [], ingestJobs: [], sources: [],
  installed: [], kDownloads: [], freeBytes: -1, zimFolders: [],
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
// KIND_NAMES puts the catalog's category names into plain words; an unknown
// one is shown with its underscores turned into spaces.
const KIND_NAMES = {
  wikipedia: 'Encyclopedia (Wikipedia)', vikidia: "Kids' encyclopedia (Vikidia)",
  wiktionary: 'Dictionary (Wiktionary)', wikibooks: 'Textbooks (Wikibooks)',
  wikisource: 'Old books and texts (Wikisource)', wikiquote: 'Quotes (Wikiquote)',
  wikiversity: 'Lessons (Wikiversity)', wikivoyage: 'Travel guides (Wikivoyage)',
  wikinews: 'News (Wikinews)', gutenberg: 'Free books (Project Gutenberg)',
  phet: 'Science games (PhET)', ted: 'Talks (TED)', mooc: 'Online courses',
  stack_exchange: 'Questions and answers (Stack Exchange)', iFixit: 'Repair guides (iFixit)',
  psiram: 'Psiram (a skeptics wiki)', other: 'Other'
};
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

function plural(n, one, many) {
  return n + ' ' + (n === 1 ? one : many);
}

// ── messages ────────────────────────────────────────────────────────────────

// toast() shows the result of an action in a bar fixed to the bottom of the
// screen, so the parent sees it wherever the page is scrolled. A success
// fades after a few seconds; an error stays until closed or replaced.
let toastTimer = null;
function toast(text, kind) {
  const el = $('toast');
  clearTimeout(toastTimer);
  if (!text) {
    el.classList.add('hidden');
    return;
  }
  $('toasttext').textContent = text;
  el.className = 'toast ' + (kind === 'err' ? 'err' : 'ok');
  el.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  if (kind !== 'err') toastTimer = setTimeout(() => el.classList.add('hidden'), 8000);
}
$('toastclose').addEventListener('click', () => toast(''));

const OFFLINE_MSG = 'Kids Shelf isn’t answering. Check that the computer Kids Shelf runs on is switched on, then reload this page.';

// loadFailed shows why a card's list could not load. A PIN prompt needs no
// words; when Kids Shelf cannot be reached at all, every card says the same
// short thing and the one full explanation goes in the message bar.
function loadFailed(wrap, e) {
  if (e.pin) return;
  if (e.offline) {
    wrap.innerHTML = '<div class="jobmsg">Not loaded.</div>';
    toast(e.message, 'err');
    return;
  }
  wrap.innerHTML = `<div class="jobmsg">${esc(e.message)}</div>`;
}

// ── fetch ───────────────────────────────────────────────────────────────────

function pinNeeded() {
  const e = new Error('PIN needed');
  e.pin = true;
  return e;
}

// api() throws Error(message) with the server's plain-words message; a 401
// opens the PIN overlay instead (once, however many calls were waiting),
// or, when no parent PIN exists yet (setup), sends the parent to /admin,
// which asks for one first. opts.pinFlow is the PIN check itself: its 401
// is a wrong PIN, shown in the overlay.
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
    const err = new Error(OFFLINE_MSG);
    err.offline = true;
    throw err;
  } finally {
    clearTimeout(timer);
  }
  let data = null;
  try { data = await resp.json(); } catch (e) { /* some responses have no body */ }
  if (resp.status === 401 && data && data.setup) {
    location.replace('/admin');
    throw pinNeeded();
  }
  if (resp.status === 401 && !opts.pinFlow) {
    showPin();
    throw pinNeeded();
  }
  if (!resp.ok) {
    // The server's plain-words message is always the one shown.
    throw new Error((data && data.error) || 'Something went wrong. Try again.');
  }
  return data;
}

async function post(path, body, timeout, pinFlow) {
  const opts = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  };
  if (timeout) opts.timeout = timeout;
  if (pinFlow) opts.pinFlow = true;
  return api(path, opts);
}

// busy() disables a button while its action runs and shows what it is
// doing; the returned function puts it back.
function busy(btn, label) {
  const old = btn.textContent;
  btn.disabled = true;
  btn.textContent = label;
  return () => { btn.disabled = false; btn.textContent = old; };
}

// ── dialogs ─────────────────────────────────────────────────────────────────

// One dialog is open at a time. Focus moves into it and stays there (Tab
// wraps); Esc and a click on the dark background run its cancel, when it
// has one. Closing puts focus back where it was, or on fallback when that
// element is gone (the list it was in was redrawn).
let dialog = null;

function focusables(el) {
  return [...el.querySelectorAll('button, a[href], input, select')]
    .filter(x => !x.disabled && x.offsetParent !== null);
}

function openDialog(id, cancel, focusId, fallback) {
  if (dialog) closeDialog(true);
  const el = $(id);
  dialog = { el, cancel, back: document.activeElement, fallback };
  el.classList.remove('hidden');
  const first = (focusId && $(focusId)) || focusables(el)[0];
  if (first) first.focus();
}

function closeDialog(silent) {
  if (!dialog) return;
  const d = dialog;
  dialog = null;
  d.el.classList.add('hidden');
  if (silent) return;
  const back = d.back && document.contains(d.back) ? d.back : d.fallback;
  if (back) back.focus();
}

document.addEventListener('keydown', ev => {
  if (!dialog) return;
  if (ev.key === 'Escape' && dialog.cancel) {
    ev.preventDefault();
    dialog.cancel();
    return;
  }
  if (ev.key !== 'Tab') return;
  const f = focusables(dialog.el);
  if (!f.length) return;
  const i = f.indexOf(document.activeElement);
  if (ev.shiftKey && i <= 0) {
    ev.preventDefault();
    f[f.length - 1].focus();
  } else if (!ev.shiftKey && (i === -1 || i === f.length - 1)) {
    ev.preventDefault();
    f[0].focus();
  }
});

for (const el of document.querySelectorAll('.overlay')) {
  el.addEventListener('click', ev => {
    if (ev.target === el && dialog && dialog.el === el && dialog.cancel) dialog.cancel();
  });
}

// ── pin overlay ─────────────────────────────────────────────────────────────

// Several calls can come back 401 at once (the page loads seven lists in
// parallel, and the poll keeps going): the form opens once and keeps what
// the parent has typed. Polling pauses while it is open.
let pinOpen = false;
function showPin() {
  if (pinOpen) return;
  pinOpen = true;
  $('pinval').value = '';
  $('pinerr').classList.add('hidden');
  openDialog('pinoverlay', null, 'pinval');
}
function hidePin() {
  pinOpen = false;
  closeDialog(true);
}
async function unlock() {
  $('pinerr').classList.add('hidden');
  try {
    await post('/api/admin/pin', { pin: $('pinval').value }, 0, true);
    hidePin();
    await loadAll();
  } catch (e) {
    if (e.pin) return;
    $('pinerr').textContent = e.message;
    $('pinerr').classList.remove('hidden');
    $('pinval').select();
  }
}
$('pinok').addEventListener('click', unlock);
$('pinval').addEventListener('keydown', ev => { if (ev.key === 'Enter') unlock(); });

// ── YouTube ─────────────────────────────────────────────────────────────────

$('ytform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const url = $('yturl').value.trim();
  if (!url) {
    toast('Paste the web address of the video, playlist or channel first.', 'err');
    $('yturl').focus();
    return;
  }
  try {
    const d = await post('/api/admin/content/youtube', { url });
    $('yturl').value = '';
    if (d && d.follow) {
      toast(d.kind === 'channel'
        ? 'Looking up the channel. A big channel takes about a minute. Then you choose what to get.'
        : 'Looking up the playlist. Then you choose what to get.', 'ok');
      await loadFollows();
      return;
    }
    toast('Added. The download starts shortly and shows in the list below.', 'ok');
    await loadYouTube();
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

// ── job status words shared by the YouTube and conversions lists ───────────

// DONE_SHOWN is how many finished rows a list shows before "Show all".
const DONE_SHOWN = 10;
// showAllDone remembers, per list, that the parent asked to see every
// finished row.
const showAllDone = { yt: false, ingest: false };

function ordinal(n) {
  const tens = n % 100;
  if (tens >= 11 && tens <= 13) return n + 'th';
  return n + ({ 1: 'st', 2: 'nd', 3: 'rd' }[n % 10] || 'th');
}

// inLine says where a waiting job is in its line: 'next' for the first.
function inLine(place) {
  if (!place) return '';
  return place === 1 ? 'next' : ordinal(place) + ' in line';
}

// timeLeft puts an estimate in seconds into words; '' when there is none.
function timeLeft(s) {
  if (!s || s <= 0) return '';
  if (s < 60) return 'less than a minute left';
  const m = Math.round(s / 60);
  if (m < 90) return `about ${m} min left`;
  const h = Math.round(s / 3600);
  return `about ${plural(h, 'hour', 'hours')} left`;
}

// working says what a running step is doing, with its percent and the
// time left when known: 'Converting: 42%, about 6 min left'.
function working(word, pct, secs) {
  if (!pct) return word;
  const left = timeLeft(secs);
  return `${word}: ${Math.floor(pct)}%` + (left ? ', ' + left : '');
}

function pbar(pct) {
  if (!pct) return '';
  return `<div class="pbar"><i style="width:${Math.min(100, Math.max(0, pct))}%"></i></div>`;
}

// summaryLine joins the non-zero parts of a card's summary with dots.
function summaryLine(el, parts) {
  const text = parts.filter(Boolean).join(' · ');
  el.textContent = text;
  el.classList.toggle('hidden', !text);
}

// withDoneTail renders rows (already in the server's order, finished ones
// last) and keeps only the newest DONE_SHOWN finished ones unless the
// parent asked for all of them.
function withDoneTail(list, jobs, isDone, render) {
  const done = jobs.filter(isDone).length;
  let shown = jobs;
  if (!showAllDone[list] && done > DONE_SHOWN) {
    let kept = 0;
    shown = jobs.filter(j => !isDone(j) || ++kept <= DONE_SHOWN);
  }
  let more = '';
  if (done > DONE_SHOWN) {
    more = showAllDone[list]
      ? `<button type="button" class="btn chip" data-showall="${list}">Show only the newest ${DONE_SHOWN} finished</button>`
      : `<button type="button" class="btn chip" data-showall="${list}">Show all ${done} finished</button>`;
  }
  return shown.map(render).join('') + (more ? `<div class="rowbtns">${more}</div>` : '');
}

function toggleShowAll(b, rerender) {
  showAllDone[b.dataset.showall] = !showAllDone[b.dataset.showall];
  rerender();
}

// ── YouTube list ────────────────────────────────────────────────────────────

// videoCounts puts a set of videos' counts into words, for a playlist row.
function videoCounts(c) {
  const parts = [];
  if (c.onShelf) parts.push(`${c.onShelf} on the shelf`);
  if (c.downloading) parts.push(`${c.downloading} downloading`);
  if (c.converting) parts.push(`${c.converting} converting`);
  if (c.waiting) parts.push(`${c.waiting} waiting`);
  if (c.needsYou) parts.push(`${c.needsYou} could not finish`);
  return `${plural(c.videos, 'video', 'videos')}: ${parts.join(', ')}`;
}

function renderYouTubeSummary(c) {
  const el = $('ytsummary');
  if (!c || !(c.downloading || c.converting || c.waiting || c.needsYou)) {
    summaryLine(el, []);
    return;
  }
  summaryLine(el, [
    c.downloading && `Downloading ${c.downloading}`,
    c.converting && `Converting ${c.converting}`,
    c.waiting && `${c.waiting} waiting`,
    c.onShelf && `${c.onShelf} on the shelf`,
    c.needsYou && `${c.needsYou} could not finish`
  ]);
}

// ytDone reports whether a YouTube row is finished (it sits in the done
// tail of the list): the download and its conversion are over.
function ytDone(job) {
  if (job.list) return !(job.playlist && (job.playlist.downloading || job.playlist.converting || job.playlist.waiting));
  return job.state === 'done' && (!job.next || job.next.state === 'done');
}

// ytStatus tells one video's story: its download, then its conversion.
// It returns the badge class and words, the percent for the progress bar,
// and the lines underneath.
function ytStatus(job) {
  const r = { cls: job.state, label: STATE_LABEL[job.state] || job.state, pct: 0, msg: job.message || '', note: '' };
  if (job.list) {
    const active = !ytDone(job);
    r.cls = active ? 'running' : 'done';
    r.label = active ? 'Working' : 'Done';
    r.note = job.playlist ? videoCounts(job.playlist) : '';
    return r;
  }
  switch (job.state) {
    case 'queued':
      r.label = job.place ? 'Waiting: ' + inLine(job.place) : 'Waiting';
      return r;
    case 'running':
      if (job.stage === 'download') {
        r.label = working('Downloading', job.progress, job.secondsLeft);
        r.pct = job.progress;
      } else if (job.stage === 'handoff') {
        r.label = 'Downloaded';
      } else if (!job.stage && job.videoId) {
        // The video is known; its size is checked and space set aside.
        r.label = 'Getting ready to download';
      } else {
        r.label = YT_STAGE[job.stage] || 'Working';
      }
      return r;
    case 'done':
      break;
    default:
      return r;
  }
  const n = job.next;
  if (!n) {
    r.label = 'Downloaded';
    if (job.show) r.note = `It goes on the shelf in the show “${job.show}”.`;
    return r;
  }
  r.cls = n.state;
  r.msg = n.message || '';
  switch (n.state) {
    case 'queued':
      r.label = 'Downloaded, waiting to convert' + (n.place ? ` (${inLine(n.place)})` : '');
      break;
    case 'running':
      r.label = n.stage === 'convert' ? working('Converting', n.progress, n.secondsLeft) : (INGEST_STAGE[n.stage] || 'Working');
      r.pct = n.stage === 'convert' ? n.progress : 0;
      break;
    case 'done':
      r.label = 'On the shelf';
      r.msg = '';
      if (job.show) r.note = `In the show “${job.show}”.`;
      break;
    case 'failed':
      r.label = 'Could not add';
      r.note = n.hint || '';
      break;
    case 'paused':
      r.label = 'Paused';
      break;
  }
  return r;
}

function renderYouTube() {
  const wrap = $('ytjobs');
  const jobs = store.ytJobs;
  renderYouTubeSummary(store.ytSummary);
  if (!jobs.length) {
    wrap.innerHTML = '<div class="empty">Nothing added yet.</div>';
    return;
  }
  wrap.innerHTML = withDoneTail('yt', jobs, ytDone, job => {
    const title = job.title || job.url;
    const st = ytStatus(job);
    let retry = '';
    if (job.state === 'failed') {
      retry = `<button class="btn mini amber" data-retry="${esc(job.id)}">Try again</button>`;
    } else if (job.next && job.next.retry) {
      retry = `<button class="btn mini amber" data-ingestretry="${esc(job.next.id)}">Try again</button>`;
    }
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(title)}</span>
          <span class="badge ${esc(st.cls)}">${esc(st.label)}</span>
          ${retry}
        </div>
        ${job.url && job.url !== title ? `<div class="jobsub">${esc(job.url)}</div>` : ''}
        ${st.msg ? `<div class="jobmsg">${esc(st.msg)}</div>` : ''}
        ${st.note ? `<div class="jobnote">${esc(st.note)}</div>` : ''}
        ${pbar(st.pct)}
      </div>`;
  });
}

async function retryYouTube(btn) {
  const done = busy(btn, 'Trying…');
  try {
    await post('/api/admin/content/youtube/retry', { id: btn.dataset.retry });
    toast('Trying again. The download shows in the list.', 'ok');
    await loadYouTube();
  } catch (e) {
    done();
    if (!e.pin) toast(e.message, 'err');
  }
}

// retryIngest gives a failed conversion a new try, from the conversions
// list or from the YouTube row of the video it belongs to.
async function retryIngest(btn) {
  const done = busy(btn, 'Trying…');
  try {
    await post('/api/admin/content/ingest/retry', { id: btn.dataset.ingestretry });
    toast('Trying again. The list shows how it is doing.', 'ok');
    await Promise.all([loadIngest(), loadYouTube()]);
  } catch (e) {
    done();
    if (!e.pin) toast(e.message, 'err');
  }
}

async function loadYouTube() {
  try {
    const d = await api('/api/admin/content/youtube');
    store.ytJobs = d.jobs || [];
    store.ytSummary = d.summary || null;
    store.folder = d.folder || null;
    renderYouTube();
    renderFolder();
  } catch (e) {
    loadFailed($('ytjobs'), e);
  }
}

// ── following playlists and channels ────────────────────────────────────────

/* A pasted playlist or channel is looked up in the background, then shows
   a preview (in the Add from YouTube card) until Start or Cancel. Followed
   ones are listed in the Following card, with the new videos that wait for
   the parent's yes. The lists only redraw when what they show changed, so
   the 4-second poll never takes a choice or the focus away. */

const LIMIT_WORDS = { 0: 'No limit', 10: '10 min', 20: '20 min', 30: '30 min', 60: '1 hour' };
// followChoice keeps each preview's choices while the page polls.
const followChoice = {};
let followsNow = 0; // the server's clock at the last load, in ms
let followsLocalAt = 0; // the page's clock then
let stopping = null;
let scrolledToWaiting = false;

function defaultChoice(f) {
  const channel = f.kind === 'channel';
  return { take: channel ? 'newest' : 'all', limit: 0, follow: true, mode: channel ? 'ask' : 'auto' };
}

function choiceOf(f) {
  if (!followChoice[f.id]) followChoice[f.id] = defaultChoice(f);
  return followChoice[f.id];
}

// lengthWords puts a total length into words: 'about 14 hours', 'about 25 min'.
function lengthWords(s) {
  if (s < 60) return 'under a minute';
  const m = Math.round(s / 60);
  if (m < 90) return `about ${m} min`;
  return `about ${plural(Math.round(s / 3600), 'hour', 'hours')}`;
}

// videoLength is one video's length: '12 min', '45 sec', '1 h 5 min'.
function videoLength(s) {
  if (!s) return 'length not known';
  if (s < 60) return `${s} sec`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  return `${Math.floor(m / 60)} h` + (m % 60 ? ` ${m % 60} min` : '');
}

function sizeWords(n) {
  return n > 0 ? 'about ' + bytes(n) : '';
}

function dayWords(d) {
  if (!d) return '';
  const t = new Date(d + 'T00:00:00Z');
  if (isNaN(t)) return '';
  return t.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });
}

// ago says how long ago a time was, by the server's clock.
function ago(iso) {
  const now = followsNow + (Date.now() - followsLocalAt);
  const s = Math.max(0, (now - Date.parse(iso)) / 1000);
  if (s < 60) return 'just now';
  const m = Math.round(s / 60);
  if (m < 90) return plural(m, 'minute', 'minutes') + ' ago';
  const h = Math.round(s / 3600);
  if (h < 36) return plural(h, 'hour', 'hours') + ' ago';
  return plural(Math.round(s / 86400), 'day', 'days') + ' ago';
}

function kindWord(f) {
  return f.kind === 'channel' ? 'Channel' : 'Playlist';
}

function limitWords(min) {
  return min ? `Videos longer than ${LIMIT_WORDS[min]} are skipped` : 'No length limit';
}

// setHTML redraws wrap only when its HTML changed, and keeps the focus on
// the control the parent was using (found again by its id).
function setHTML(wrap, html) {
  if (wrap.dataset.html === html) return;
  const active = document.activeElement;
  const focusId = active && wrap.contains(active) ? active.id : '';
  wrap.innerHTML = html;
  wrap.dataset.html = html;
  if (focusId && $(focusId)) $(focusId).focus();
}

function radio(name, id, value, checked, label) {
  return `<label class="choice" for="${id}"><input type="radio" name="${name}" id="${id}" value="${esc(value)}"${checked ? ' checked' : ''}> <span>${label}</span></label>`;
}

function limitOptions(selected) {
  return Object.keys(LIMIT_WORDS).map(Number).sort((a, b) => a - b)
    .map(m => `<option value="${m}"${m === selected ? ' selected' : ''}>${esc(LIMIT_WORDS[m])}</option>`).join('');
}

function previewHTML(f) {
  const p = f.preview;
  const c = choiceOf(f);
  const pick = (take) => (p.choices[take] || {})[c.limit] || { videos: 0, bytes: 0, tooLong: 0, unknown: 0 };
  const facts = [plural(p.videos, 'video', 'videos')];
  if (p.seconds) facts.push(lengthWords(p.seconds));
  if (p.bytes) facts.push(sizeWords(p.bytes));
  const notes = [];
  if (p.unknown && p.unknown * 10 >= p.videos) {
    notes.push(`${plural(p.unknown, 'video has', 'videos have')} no length on YouTube, so the real total is more.`);
  }
  if (p.already) notes.push(`${p.already === 1 ? '1 of them is' : p.already + ' of them are'} in Kids Shelf already. Kids Shelf doesn’t download them again.`);
  const sz = take => { const s = sizeWords(pick(take).bytes); return s ? ` (${s})` : ''; };
  const n = `fc-${f.id}`;
  let takes = radio(`${n}-take`, `${n}-take-all`, 'all', c.take === 'all', `All ${plural(p.videos, 'video', 'videos')}${sz('all')}`);
  if (f.kind === 'channel') takes += radio(`${n}-take`, `${n}-take-newest`, 'newest', c.take === 'newest', `The newest ${store.followNewest || 20}${sz('newest')}`);
  takes += radio(`${n}-take`, `${n}-take-none`, 'none', c.take === 'none', 'None yet, only new ones');
  const now = pick(c.take);
  let result;
  if (!now.videos) {
    result = c.follow ? 'Start will download nothing now; new videos come later.' : 'Start will download nothing.';
  } else {
    result = `Start will download ${plural(now.videos, 'video', 'videos')} now` + (now.bytes ? ` (${sizeWords(now.bytes)})` : '') + '.';
  }
  if (now.tooLong) result += ` ${now.tooLong === 1 ? '1 video is' : now.tooLong + ' videos are'} too long and ${now.tooLong === 1 ? 'is' : 'are'} skipped.`;
  const modes = c.follow ? `<fieldset class="choices"><legend>New videos</legend>
      ${radio(`${n}-mode`, `${n}-mode-auto`, 'auto', c.mode === 'auto', 'Add them to the shelf straight away')}
      ${radio(`${n}-mode`, `${n}-mode-ask`, 'ask', c.mode === 'ask', 'Ask me first')}
    </fieldset>` : '';
  return `<div class="job preview" data-follow="${esc(f.id)}">
      <div class="jobhead">
        <span class="jobtitle">${esc(f.name)}</span>
        <span class="tag">${kindWord(f)}</span>
      </div>
      <div class="jobnote">${esc(facts.join(' · '))}</div>
      ${notes.map(t => `<div class="jobsub">${esc(t)}</div>`).join('')}
      <fieldset class="choices"><legend>What to get now</legend>${takes}</fieldset>
      <label class="field" for="${n}-limit">Skip videos longer than
        <select id="${n}-limit" data-choice="limit">${limitOptions(c.limit)}</select>
      </label>
      <label class="choice" for="${n}-follow"><input type="checkbox" id="${n}-follow" data-choice="follow"${c.follow ? ' checked' : ''}> <span>Also get new videos when they appear</span></label>
      ${modes}
      <div class="jobnote" role="status">${esc(result)}</div>
      <div class="rowbtns">
        <button type="button" class="btn add" data-fstart="${esc(f.id)}">Start</button>
        <button type="button" class="btn chip" data-fcancel="${esc(f.id)}">Cancel</button>
      </div>
    </div>`;
}

function pendingHTML(f) {
  if (f.status === 'preview') return previewHTML(f);
  const failed = !!f.message;
  return `<div class="job" data-follow="${esc(f.id)}">
      <div class="jobhead">
        <span class="jobtitle">${esc(f.url)}</span>
        <span class="badge ${failed ? 'failed' : 'running'}">${failed ? 'Couldn’t look it up' : 'Looking it up…'}</span>
      </div>
      ${failed ? `<div class="jobmsg">${esc(f.message)}</div>` : `<div class="jobsub">Kids Shelf is asking YouTube what’s in it. A big channel takes about a minute.</div>`}
      <div class="rowbtns">
        ${failed ? `<button type="button" class="btn amber" data-fcheck="${esc(f.id)}">Try again</button>` : ''}
        <button type="button" class="btn chip" data-fcancel="${esc(f.id)}">Cancel</button>
      </div>
    </div>`;
}

// shelfCounts says how a source's videos are doing, from their real jobs:
// '4 on the shelf · 2 on the way · 1 could not be added', then the
// parent's answers.
function shelfCounts(f) {
  const s = f.shelf || {}, c = f.counts || {};
  return [
    `${s.onShelf || 0} on the shelf`,
    s.onTheWay && `${s.onTheWay} on the way`,
    s.failed && `${s.failed} could not be added`,
    c.waiting && `${c.waiting} waiting for your yes`,
    c.skipped && `${c.skipped} skipped`,
    c.tooLong && `${c.tooLong} too long`
  ].filter(Boolean).join(' · ');
}

// onceHTML is a source added once (not followed): no checks, no settings;
// it leaves the list by itself when none of its videos is on the way.
function onceHTML(f) {
  return `<div class="job" data-follow="${esc(f.id)}">
      <div class="jobhead">
        <span class="jobtitle">${esc(f.name)}</span>
        <span class="tag">${kindWord(f)}</span>
      </div>
      <div class="jobnote">Added once · not checked for new videos</div>
      <div class="jobnote">${esc(shelfCounts(f))}</div>
    </div>`;
}

function followHTML(f) {
  if (f.status === 'once') return onceHTML(f);
  const n = `fo-${f.id}`;
  let checked;
  if (f.checking) checked = 'Checking now…';
  else if (f.checked) checked = `Checked ${ago(f.checked)}`;
  else checked = 'Not checked yet';
  return `<div class="job" data-follow="${esc(f.id)}">
      <div class="jobhead">
        <span class="jobtitle">${esc(f.name)}</span>
        <span class="tag">${kindWord(f)}</span>
      </div>
      <div class="jobnote">${f.mode === 'auto' ? 'New videos go straight to the shelf' : 'New videos wait for your yes'} · ${esc(limitWords(f.maxMinutes))}</div>
      <div class="jobnote">${esc(shelfCounts(f))}</div>
      <div class="jobsub">${esc(checked)}</div>
      ${f.message ? `<div class="jobmsg">The last check didn’t work. ${esc(f.message)}</div>` : ''}
      <div class="followset">
        <label class="field" for="${n}-mode">New videos
          <select id="${n}-mode" data-fset="mode">
            <option value="auto"${f.mode === 'auto' ? ' selected' : ''}>Add them to the shelf straight away</option>
            <option value="ask"${f.mode === 'ask' ? ' selected' : ''}>Ask me first</option>
          </select>
        </label>
        <label class="field" for="${n}-limit">Skip videos longer than
          <select id="${n}-limit" data-fset="limit">${limitOptions(f.maxMinutes)}</select>
        </label>
      </div>
      <div class="rowbtns">
        <button type="button" class="btn chip" id="${n}-check" data-fcheck="${esc(f.id)}"${f.checking ? ' disabled' : ''}>Check now</button>
        <button type="button" class="btn sub" data-fstop="${esc(f.id)}">Stop following</button>
      </div>
    </div>`;
}

function waitingHTML(f) {
  const rows = f.waiting.map(v => {
    const bits = [videoLength(v.seconds)];
    if (v.uploaded) bits.push('uploaded ' + dayWords(v.uploaded));
    const vid = esc(v.id);
    return `<div class="job waitrow">
        <div class="jobhead">
          <span class="jobtitle">${esc(v.title)}</span>
        </div>
        <div class="jobsub">${esc(bits.join(' · '))}</div>
        <div class="rowbtns">
          <a class="btn chip linkbtn" href="https://www.youtube.com/watch?v=${encodeURIComponent(v.id)}" target="_blank" rel="noopener">Watch on YouTube</a>
          <button type="button" class="btn add" id="fw-${esc(f.id)}-${vid}-add" data-fadd="${esc(f.id)}" data-video="${vid}">Add</button>
          <button type="button" class="btn chip" id="fw-${esc(f.id)}-${vid}-skip" data-fskip="${esc(f.id)}" data-video="${vid}">Skip</button>
        </div>
      </div>`;
  }).join('');
  const all = f.waiting.length > 1
    ? `<div class="rowbtns">
        <button type="button" class="btn add" data-fadd="${esc(f.id)}" data-video="">Add all ${f.waiting.length}</button>
        <button type="button" class="btn chip" data-fskip="${esc(f.id)}" data-video="">Skip all ${f.waiting.length}</button>
      </div>`
    : '';
  return `<div class="waitgroup">
      <h4 class="waitfrom">From ${esc(f.name)}</h4>
      ${all}
      ${rows}
    </div>`;
}

function renderFollows() {
  const list = store.follows || [];
  const kept = f => f.status === 'following' || f.status === 'once';
  setHTML($('ytpending'), list.filter(f => !kept(f)).map(pendingHTML).join(''));
  const sources = list.filter(kept);
  const following = sources.filter(f => f.status === 'following');
  setHTML($('follows'), sources.length
    ? sources.map(followHTML).join('')
    : '<div class="empty">Nothing here yet. Paste a playlist or a channel link above.</div>');
  const waiting = following.filter(f => f.waiting && f.waiting.length);
  setHTML($('followwaiting'), waiting.length
    ? waiting.map(waitingHTML).join('')
    : '<div class="empty">Nothing waits for your yes.</div>');
}

async function loadFollows() {
  try {
    const d = await api('/api/admin/content/youtube/follows');
    followsNow = Date.parse(d.now) || Date.now();
    followsLocalAt = Date.now();
    store.follows = d.follows || [];
    store.followNewest = d.newest;
    for (const id of Object.keys(followChoice)) {
      if (!store.follows.some(f => f.id === id && (f.status === 'lookup' || f.status === 'preview'))) delete followChoice[id];
    }
    renderFollows();
    if (!scrolledToWaiting && location.hash === '#following-waiting') {
      scrolledToWaiting = true;
      $('following-waiting').scrollIntoView();
    }
  } catch (e) {
    $('follows').dataset.html = ''; // redraw once it loads again
    loadFailed($('follows'), e);
  }
}

// followAction posts one Following action, shows its result and reloads
// the lists it touches.
async function followAction(btn, path, body, label, done) {
  const back = btn ? busy(btn, label) : () => {};
  try {
    const d = await post('/api/admin/content/youtube/follows/' + path, body);
    if (done) toast(typeof done === 'function' ? done(d) : done, 'ok');
    await Promise.all([loadFollows(), loadYouTube()]);
  } catch (e) {
    back();
    if (!e.pin) toast(e.message, 'err');
  }
}

$('ytpending').addEventListener('change', ev => {
  const box = ev.target.closest('[data-follow]');
  if (!box) return;
  const f = (store.follows || []).find(x => x.id === box.dataset.follow);
  if (!f) return;
  const c = choiceOf(f);
  const t = ev.target;
  if (t.name && t.name.endsWith('-take')) c.take = t.value;
  else if (t.name && t.name.endsWith('-mode')) c.mode = t.value;
  else if (t.dataset.choice === 'limit') c.limit = Number(t.value);
  else if (t.dataset.choice === 'follow') c.follow = t.checked;
  renderFollows();
});

$('ytpending').addEventListener('click', ev => {
  const start = ev.target.closest('[data-fstart]');
  if (start) {
    const f = (store.follows || []).find(x => x.id === start.dataset.fstart);
    if (!f) return;
    const c = choiceOf(f);
    return followAction(start, 'start', { id: f.id, take: c.take, maxMinutes: c.limit, follow: c.follow, mode: c.mode }, 'Starting…',
      () => c.follow
        ? `Following “${f.name}”. New videos ${c.mode === 'auto' ? 'go straight to the shelf' : 'wait for your yes'}.`
        : 'Started. The downloads show in the list below.');
  }
  const cancel = ev.target.closest('[data-fcancel]');
  if (cancel) return followAction(cancel, 'cancel', { id: cancel.dataset.fcancel }, 'Canceling…');
  const again = ev.target.closest('[data-fcheck]');
  if (again) return followAction(again, 'check', { id: again.dataset.fcheck }, 'Trying…');
});

$('follows').addEventListener('change', ev => {
  const sel = ev.target.closest('[data-fset]');
  const box = ev.target.closest('[data-follow]');
  if (!sel || !box) return;
  const f = (store.follows || []).find(x => x.id === box.dataset.follow);
  if (!f) return;
  const mode = sel.dataset.fset === 'mode' ? sel.value : f.mode;
  const maxMinutes = sel.dataset.fset === 'limit' ? Number(sel.value) : f.maxMinutes;
  followAction(null, 'settings', { id: f.id, mode, maxMinutes }, '', 'Saved.');
});

$('follows').addEventListener('click', ev => {
  const check = ev.target.closest('[data-fcheck]');
  if (check) return followAction(check, 'check', { id: check.dataset.fcheck }, 'Checking…', 'Checking now. New videos show here when it’s done.');
  const stop = ev.target.closest('[data-fstop]');
  if (stop) {
    const f = (store.follows || []).find(x => x.id === stop.dataset.fstop);
    if (!f) return;
    stopping = f;
    $('stopconfirmtext').textContent = `Kids Shelf stops checking “${f.name}” for new videos. The videos already on the shelf stay there.`;
    openDialog('stopconfirm', cancelStop, 'stopno', $('follows'));
  }
});

function cancelStop() {
  stopping = null;
  closeDialog();
}
$('stopno').addEventListener('click', cancelStop);
$('stopok').addEventListener('click', async () => {
  const f = stopping;
  if (!f) return;
  stopping = null;
  closeDialog();
  await followAction(null, 'stop', { id: f.id }, '', `Stopped following “${f.name}”. Its videos stay on the shelf.`);
});

$('followwaiting').addEventListener('click', ev => {
  const add = ev.target.closest('[data-fadd]');
  if (add) {
    return followAction(add, 'add', { id: add.dataset.fadd, video: add.dataset.video }, 'Adding…',
      d => `Added ${plural(d.answered, 'video', 'videos')}. ${d.answered === 1 ? 'It shows' : 'They show'} in the download list.`);
  }
  const skip = ev.target.closest('[data-fskip]');
  if (skip) {
    return followAction(skip, 'skip', { id: skip.dataset.fskip, video: skip.dataset.video }, 'Skipping…',
      d => `Skipped ${plural(d.answered, 'video', 'videos')}. Kids Shelf won’t offer ${d.answered === 1 ? 'it' : 'them'} again.`);
  }
});

// ── where new videos are saved ──────────────────────────────────────────────

/* renderFolder says where downloads and converted copies go, and whether
   a chosen folder is missing right now (its drive unplugged). */
function renderFolder() {
  const f = store.folder;
  if (!f) return;
  const free = f.free >= 0 ? ` (${bytes(f.free)} free)` : '';
  $('vfwhere').innerHTML = f.missing
    ? `New videos are saved in <b>${esc(f.path)}</b>, but that folder isn’t there right now. Plug in its drive: downloads wait until it’s back.`
    : `New videos are saved in <b>${esc(f.path)}</b>${esc(free)}. That’s where downloads go, and the copies Kids Shelf makes of videos that don’t play everywhere.`;
  $('vfwhere').classList.toggle('err', !!f.missing);
  $('vfdefault').classList.toggle('hidden', !f.chosen);
}

function showFolderForm(open) {
  $('vfform').classList.toggle('hidden', !open);
  $('vfhelp').classList.toggle('hidden', !open);
  $('vfbtns').classList.toggle('hidden', open);
  if (open) {
    $('vfpath').value = (store.folder && store.folder.chosen) || '';
    $('vfpath').focus();
  }
}

async function saveFolder(path) {
  const d = await post('/api/admin/content/videofolder', { path });
  store.folder = d.folder;
  renderFolder();
  return d.folder;
}

$('vfchange').addEventListener('click', () => showFolderForm(true));
$('vfcancel').addEventListener('click', () => showFolderForm(false));

$('vfform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const path = $('vfpath').value.trim();
  if (!path) {
    toast('Enter the path to the folder first.', 'err');
    $('vfpath').focus();
    return;
  }
  try {
    const f = await saveFolder(path);
    showFolderForm(false);
    toast(`New videos are saved in ${f.path} from now on.`, 'ok');
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

$('vfdefault').addEventListener('click', async () => {
  try {
    const f = await saveFolder('');
    toast(`New videos are saved in ${f.path} again. Videos already saved stay where they are.`, 'ok');
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

$('ytjobs').addEventListener('click', ev => {
  const b = ev.target.closest('[data-retry]');
  if (b) return retryYouTube(b);
  const i = ev.target.closest('[data-ingestretry]');
  if (i) return retryIngest(i);
  const all = ev.target.closest('[data-showall]');
  if (all) toggleShowAll(all, renderYouTube);
});

// ── sources ─────────────────────────────────────────────────────────────────

$('srcform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const path = $('srcpath').value.trim();
  if (!path) {
    toast('Enter the path to the folder first.', 'err');
    $('srcpath').focus();
    return;
  }
  const done = busy($('srcadd'), 'Checking…');
  let found;
  try {
    const d = await post('/api/admin/content/sources', { path, kind: $('srckind').value });
    $('srcpath').value = '';
    found = d.videos;
  } catch (e) {
    done();
    if (!e.pin) toast(e.message, 'err');
    return;
  }
  // A fresh folder needs a scan before its videos show up, so the page
  // starts one right away instead of leaving the parent to hunt for the
  // Scan now button.
  const head = found >= 0
    ? `Folder added. It has ${plural(found, 'video', 'videos')}.`
    : 'Folder added.';
  toast(head + ' Looking for new ones now…', 'ok');
  try {
    const s = await post('/api/admin/content/scan');
    toast(head + ' ' + scanResult(s.added), 'ok');
  } catch (e) {
    if (!e.pin) toast(head + ' The scan could not finish: ' + e.message, 'err');
  }
  done();
  await loadSources();
  await loadIngest();
});

function scanResult(added) {
  if (!added) return 'No new videos: they are already on the shelf or on their way.';
  return `${plural(added, 'new video is', 'new videos are')} being added to the shelf. Watch ${added === 1 ? 'it' : 'them'} in Conversions below.`;
}

$('scanbtn').addEventListener('click', async () => {
  const done = busy($('scanbtn'), 'Scanning…');
  try {
    const s = await post('/api/admin/content/scan');
    toast('Scan done. ' + scanResult(s.added), 'ok');
    await loadSources();
    await loadIngest();
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
  done();
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
      ${src.problem ? `<div class="jobmsg wide">${esc(src.problem)}</div>` : ''}
    </div>`).join('');
}

let pendingSource = null;

function cancelSourceRemove() {
  pendingSource = null;
  closeDialog();
}

$('sources').addEventListener('click', ev => {
  const b = ev.target.closest('[data-srcpath]');
  if (!b) return;
  pendingSource = b.dataset.srcpath;
  $('srcconfirmtext').innerHTML =
    `<b>${esc(pendingSource)}</b> leaves the list, and Kids Shelf stops looking in it for new videos. ` +
    'Videos from it that are already on the shelf stay there. Nothing in the folder is changed or deleted.';
  openDialog('srcconfirm', cancelSourceRemove, 'srcremoveno', $('srcpath'));
});

$('srcremoveno').addEventListener('click', cancelSourceRemove);

$('srcremoveok').addEventListener('click', async () => {
  const path = pendingSource;
  pendingSource = null;
  closeDialog();
  if (!path) return;
  try {
    await post('/api/admin/content/sources/remove', { path });
    toast('Folder removed from the list. Its videos already on the shelf stay there.', 'ok');
    await loadSources();
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

async function loadSources() {
  try {
    const d = await api('/api/admin/content/sources');
    store.sources = d.sources || [];
    renderSources();
  } catch (e) {
    loadFailed($('sources'), e);
  }
}

// ── conversions ─────────────────────────────────────────────────────────────

// ingestStatus says in a few words what is happening with a conversion:
// its place in line while it waits, the running stage (with the percent
// and the time left while it converts), 'On the shelf' once the video is
// published. Pause and failure reasons go on their own line underneath.
function ingestStatus(job) {
  switch (job.state) {
    case 'done':
      return 'On the shelf';
    case 'failed':
      return 'Could not add';
    case 'paused':
      return 'Paused';
    case 'running':
      if (job.stage === 'convert') return working('Converting', job.progress, job.seconds_left);
      return INGEST_STAGE[job.stage] || 'Working';
    case 'queued':
      return job.place ? 'Waiting: ' + inLine(job.place) : 'Waiting';
    default:
      return STATE_LABEL[job.state] || job.state;
  }
}

function renderIngestSummary(c) {
  const el = $('ingestsummary');
  if (!c) {
    summaryLine(el, []);
    return;
  }
  summaryLine(el, [
    c.converting && `Converting ${c.converting}`,
    c.adding && `Adding ${c.adding} to the shelf`,
    c.waiting && `${c.waiting} waiting`,
    c.needs_you && `${c.needs_you} could not finish`
  ]);
}

function renderIngest() {
  const wrap = $('ingestjobs');
  const jobs = store.ingestJobs;
  renderIngestSummary(store.ingestSummary);
  if (!jobs.length) {
    wrap.innerHTML = '<div class="empty">Nothing waiting. New videos appear here.</div>';
    return;
  }
  wrap.innerHTML = withDoneTail('ingest', jobs, job => job.state === 'done', job => {
    const title = job.title || job.file;
    const kind = job.source && job.source.Kind;
    // The file's place is useful for folder videos; a YouTube video's file
    // is a temporary download nobody needs to see.
    const path = (kind === 'folder' || kind === 'nas') && job.file && job.file !== title
      ? `<div class="jobsub">${esc(job.file)}</div>` : '';
    const detail = job.message && job.state !== 'done' ? `<div class="jobmsg">${esc(job.message)}</div>` : '';
    const hint = job.hint ? `<div class="jobnote">${esc(job.hint)}</div>` : '';
    const retry = job.retry
      ? `<button class="btn mini amber" data-ingestretry="${esc(job.id)}">Try again</button>` : '';
    const bar = job.state === 'running' && job.stage === 'convert' ? pbar(job.progress) : '';
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(title)}</span>
          <span class="badge ${esc(job.state)}">${esc(ingestStatus(job))}</span>
          ${retry}
        </div>
        ${path}${detail}${hint}${bar}
      </div>`;
  });
}

$('ingestjobs').addEventListener('click', ev => {
  const b = ev.target.closest('[data-ingestretry]');
  if (b) return retryIngest(b);
  const all = ev.target.closest('[data-showall]');
  if (all) toggleShowAll(all, renderIngest);
});

async function loadIngest() {
  try {
    const d = await api('/api/admin/content/ingest');
    store.ingestJobs = d.jobs || [];
    store.ingestSummary = d.summary || null;
    renderIngest();
  } catch (e) {
    loadFailed($('ingestjobs'), e);
  }
}

// ── fixing videos ────────────────────────────────────────────────────────────

// FIX_ISSUE puts each issue code into plain words a parent understands.
// The sentences stay in sync with the kid pages' explanations.
const FIX_ISSUE = {
  sound: 'No sound in most browsers (Dolby or DTS sound)',
  picture: 'The picture may not show in some browsers',
  format: 'Will not open on iPads, iPhones or Safari (MKV file)',
};

function renderFixing() {
  const wrap = $('fixing');
  const f = store.fixing || { pending: { sound: 0, picture: 0, format: 0 }, failed: 0, failedEpisodes: [], failedMore: 0 };
  const p = f.pending || {};
  const waiting = (p.sound || 0) + (p.picture || 0) + (p.format || 0);
  if (!waiting && !f.failed) {
    wrap.innerHTML = '<div class="empty">No videos are being fixed right now.</div>';
    return;
  }
  let html = '';
  if (waiting) {
    const parts = [];
    if (p.sound) parts.push(`${p.sound} with no sound`);
    if (p.picture) parts.push(`${p.picture} whose picture may not show`);
    if (p.format) parts.push(`${p.format} that iPad or iPhone cannot open`);
    html += `<div class="fixingline">${plural(waiting, 'video is', 'videos are')} being fixed: ${esc(parts.join(', '))}.</div>`;
  }
  if (f.failed) {
    html += f.failed === 1
      ? '<div class="fixingline">1 video could not be fixed. It plays as it is, with this problem:</div>'
      : `<div class="fixingline">${f.failed} videos could not be fixed. They play as they are, with these problems:</div>`;
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
    loadFailed($('fixing'), e);
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

function kindName(c) {
  if (KIND_NAMES[c]) return KIND_NAMES[c];
  const s = String(c).replace(/_/g, ' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function flavourLabel(f) {
  return FLAVOUR_LABEL[f] || '';
}

// zimName is a library's title with its edition, as every list and box
// shows it, so two editions of one library never look alike.
function zimName(z) {
  const f = flavourLabel(z.flavour);
  return f ? `${z.title} (${f})` : z.title;
}

function zimTitleHTML(z) {
  const f = flavourLabel(z.flavour);
  return esc(z.title) + (f ? ` <span class="flav">${esc(f)}</span>` : '');
}

function zimDesc(z) {
  const bits = [];
  if (z.language) bits.push('Language: ' + esc(langName(z.language)));
  if (z.category) bits.push('Kind: ' + esc(kindName(z.category)));
  if (z.article_count) bits.push(z.article_count.toLocaleString() + ' articles');
  if (z.size) bits.push('<b>' + esc(bytes(z.size)) + '</b>');
  return bits.join(' · ');
}

function catRow(z) {
  const action = z.installed
    ? '<span class="tag okfg">On your shelf</span>'
    : `<button class="btn add" data-dl='${esc(JSON.stringify(z))}'>Download</button>`;
  return `
    <div class="cat">
      <div class="catmeta">
        <div class="title">${zimTitleHTML(z)}</div>
        ${z.summary ? `<div class="summ">${esc(z.summary)}</div>` : ''}
        <div class="meta">${zimDesc(z)}</div>
      </div>
      ${action}
    </div>`;
}

function renderCatalog() {
  const wrap = $('catalog');
  const c = store.catalog;
  if (!c.entries.length) {
    $('catstatus').textContent = '';
    wrap.innerHTML = '<div class="empty">No libraries match that search. Try fewer words, or choose Any language and Any kind.</div>';
    return;
  }
  $('catstatus').textContent = `${plural(c.total, 'library', 'libraries')} found. Showing ${c.entries.length}.`;
  wrap.innerHTML = c.entries.map(catRow).join('');
  if (c.entries.length < c.total) {
    wrap.insertAdjacentHTML('beforeend',
      `<div class="rowbtns"><button class="btn" id="catmore">Show more</button></div>`);
    $('catmore').addEventListener('click', showMore);
  }
}

async function showMore() {
  const shown = store.catalog.entries.length;
  const done = busy($('catmore'), 'Loading…');
  if (await searchCatalog(shown)) {
    // Keep the parent's place: focus the first of the new entries.
    const rows = $('catalog').querySelectorAll('.cat');
    const first = rows[shown] && rows[shown].querySelector('button, .tag');
    if (first && first.focus) first.focus();
  } else {
    done();
  }
}

let pendingDownload = null;

function cancelDownload() {
  pendingDownload = null;
  closeDialog();
}

$('catalog').addEventListener('click', ev => {
  const b = ev.target.closest('[data-dl]');
  if (!b) return;
  let z;
  try { z = JSON.parse(b.dataset.dl); } catch (e) { return; }
  pendingDownload = z;
  const free = store.freeBytes >= 0 ? ` About ${bytes(store.freeBytes)} is free on your media drive.` : '';
  $('confirmtext').innerHTML =
    `Download <b>${esc(zimName(z))}</b>? It needs ${esc(bytes(z.size))}.${free}<br>` +
    'Kids can open it once the download has finished, with or without internet.';
  openDialog('confirm', cancelDownload, 'confirmok', $('catq'));
});

$('confirmno').addEventListener('click', cancelDownload);

$('confirmok').addEventListener('click', async () => {
  const z = pendingDownload;
  pendingDownload = null;
  closeDialog();
  if (!z) return;
  try {
    await post('/api/admin/content/kiwix/download', { id: z.id, name: z.name, flavour: z.flavour || '' }, 90000);
    toast(`Download of ${zimName(z)} started. It shows under Downloads.`, 'ok');
    await loadKiwix();
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

const MISSING_MSG = 'The file for this library is missing, so kids can’t open it. If it was on a drive, plug the drive in. Otherwise remove it and download it again.';
const FOLDER_MISSING_MSG = 'Kids Shelf can’t reach the folder this library is in, so kids can’t open it. If it is on a drive or NAS, connect it.';

// missingWhy says in a few words why libraries can't be opened: a library
// from a folder on this computer is out of reach with its folder, a
// downloaded one has lost its file.
function missingWhy(list) {
  const fromFolders = list.filter(z => z.folder).length;
  const one = list.length === 1;
  if (fromFolders === list.length) return one ? 'its folder can’t be reached' : 'their folders can’t be reached';
  if (!fromFolders) return one ? 'its file is missing' : 'their files are missing';
  return 'their files can’t be found';
}

function renderInstalled() {
  const wrap = $('installed');
  const list = store.installed;
  const missing = list.filter(z => z.missing);
  const warn = $('libwarn');
  if (missing.length) {
    warn.textContent = missing.length === 1
      ? `${zimName(missing[0])} can’t be opened: ${missingWhy(missing)}. See "On this shelf" below.`
      : `${missing.length} libraries can’t be opened: ${missingWhy(missing)}. See "On this shelf" below.`;
    warn.classList.remove('hidden');
  } else {
    warn.classList.add('hidden');
  }
  if (!list.length) {
    wrap.innerHTML = '<div class="empty">Nothing installed yet. Pick a library above.</div>';
    return;
  }
  // A library from a folder on this computer leaves the shelf only with
  // its folder, so it names the folder instead of offering Remove, and its
  // file name, so two copies of one library can be told apart.
  wrap.innerHTML = list.map(z => `
    <div class="cat">
      <div class="catmeta">
        <div class="title">${zimTitleHTML(z)}</div>
        <div class="meta">${esc(bytes(z.size))}${z.language ? ' · ' + esc(langName(z.language)) : ''}${z.folder ? ' · ' + esc(String(z.file).split(/[\\/]/).pop()) : ''}</div>
        ${z.missing ? `<div class="jobmsg">${esc(z.folder ? FOLDER_MISSING_MSG : MISSING_MSG)}</div>` : ''}
      </div>
      ${z.folder
        ? `<span class="fromfolder">From ${esc(z.folder)}</span>`
        : `<button class="btn sub" data-rm="${esc(z.id)}">Remove</button>`}
    </div>`).join('');
}

let pendingRemove = null;

function cancelRemove() {
  pendingRemove = null;
  closeDialog();
}

$('installed').addEventListener('click', ev => {
  const b = ev.target.closest('[data-rm]');
  if (!b) return;
  pendingRemove = store.installed.find(x => x.id === b.dataset.rm) || { id: b.dataset.rm, title: 'this library' };
  $('removeconfirmtext').innerHTML =
    `Remove <b>${esc(zimName(pendingRemove))}</b> from the shelf? The downloaded file is deleted.`;
  openDialog('removeconfirm', cancelRemove, 'removeno', $('catq'));
});

$('removeno').addEventListener('click', cancelRemove);

$('removeok').addEventListener('click', async () => {
  const z = pendingRemove;
  pendingRemove = null;
  closeDialog();
  if (!z) return;
  try {
    await post('/api/admin/content/kiwix/remove', { id: z.id });
    toast(`${zimName(z)} was removed and its file deleted.`, 'ok');
    await loadKiwix();
    if (store.catalog.entries.length) await searchCatalog(0, store.catalog.entries.length);
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
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
    const e = job.entry || {};
    const title = zimName({ title: e.title || e.name || 'Library', flavour: e.flavour });
    let pct = 0;
    if (e.size > 0) pct = Math.round(job.downloaded / e.size * 100);
    const prog = (job.state === 'running' || job.state === 'paused') && pct
      ? `<div class="pbar ${esc(job.state)}"><i style="width:${Math.min(100, pct)}%"></i></div>` : '';
    const size = e.size ? ` of ${bytes(e.size)}` : '';
    const label = job.state === 'done' ? 'On the shelf' : (STATE_LABEL[job.state] || job.state);
    const pctLabel = pct && job.state !== 'done' ? ` · ${pct}%${size}` : '';
    return `<div class="job">
        <div class="jobhead">
          <span class="jobtitle">${esc(title)}</span>
          <span class="badge ${esc(job.state)}">${esc(label + pctLabel)}</span>
        </div>
        ${job.message ? `<div class="jobmsg">${esc(job.message)}</div>` : ''}
        ${prog}
      </div>`;
  }).join('');
}

let kiwixInstalledKey = null; // null until the first load

async function loadKiwix() {
  try {
    const d = await api('/api/admin/content/kiwix');
    store.installed = d.installed || [];
    store.kDownloads = d.downloads || [];
    store.freeBytes = d.free_bytes != null ? d.free_bytes : -1;
    renderInstalled();
    renderKiwixDownloads();
    // A download that just finished turns its catalog row into "On your
    // shelf"; refresh the rows already shown when the installed set changes.
    const key = store.installed.map(z => z.id).join(',');
    if (kiwixInstalledKey !== null && key !== kiwixInstalledKey) {
      if (store.catalog.entries.length) searchCatalog(0, store.catalog.entries.length);
      // A look through the library folders changed the shelf: their
      // counts and sizes change with it.
      loadZimFolders();
    }
    kiwixInstalledKey = key;
  } catch (e) {
    loadFailed($('kdownloads'), e);
  }
}

// ── library files on this computer (library folders) ────────────────────────

const FOLDER_AWAY_MSG = 'Kids Shelf can’t reach this folder right now, so kids can’t open its libraries. If it is on a drive or NAS, connect it.';

$('zimform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const path = $('zimpath').value.trim();
  if (!path) {
    toast('Enter the path to the folder first.', 'err');
    $('zimpath').focus();
    return;
  }
  const done = busy($('zimadd'), 'Looking…');
  let found;
  try {
    const d = await post('/api/admin/content/kiwix/folders', { path });
    $('zimpath').value = '';
    found = d.libraries;
  } catch (e) {
    done();
    if (!e.pin) toast(e.message, 'err');
    return;
  }
  toast(found
    ? `Folder added. ${plural(found, 'library from it is', 'libraries from it are')} on the shelf now.`
    : 'Folder added. It has no library files (.zim) yet. Kids Shelf looks in it again every 10 minutes.', 'ok');
  done();
  await Promise.all([loadZimFolders(), loadKiwix()]);
});

function renderZimFolders() {
  const wrap = $('zimfolders');
  const list = store.zimFolders;
  if (!list.length) {
    wrap.innerHTML = '<div class="empty">No folders yet.</div>';
    return;
  }
  wrap.innerHTML = list.map(f => `
    <div class="source">
      <span class="sourcepath">${esc(f.path)}</span>
      <span class="tag">${esc(plural(f.libraries, 'library', 'libraries'))}${f.libraries ? ' · ' + esc(bytes(f.size)) : ''}</span>
      <button class="btn mini amber" data-zimrescan="${esc(f.path)}">Look again</button>
      <button class="btn mini sub" data-zimstop="${esc(f.path)}">Stop using this folder</button>
      ${f.reachable ? '' : `<div class="jobmsg wide">${esc(FOLDER_AWAY_MSG)}</div>`}
    </div>`).join('');
}

// zimRescanResult says what a "Look again" found.
function zimRescanResult(d) {
  if (!d.reachable) return FOLDER_AWAY_MSG;
  const parts = [];
  if (d.added) parts.push(`${plural(d.added, 'new library is', 'new libraries are')} on the shelf now.`);
  if (d.dropped) {
    parts.push(d.dropped === 1
      ? '1 library left the shelf: its file is no longer in the folder.'
      : `${d.dropped} libraries left the shelf: their files are no longer in the folder.`);
  }
  if (!parts.length) parts.push('Nothing new in this folder.');
  return 'Looked again. ' + parts.join(' ');
}

async function rescanZimFolder(btn) {
  const done = busy(btn, 'Looking…');
  try {
    const d = await post('/api/admin/content/kiwix/folders/rescan', { path: btn.dataset.zimrescan });
    toast(zimRescanResult(d), d.reachable ? 'ok' : 'err');
    await Promise.all([loadZimFolders(), loadKiwix()]);
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
  done();
}

let pendingZimStop = null;

function cancelZimStop() {
  pendingZimStop = null;
  closeDialog();
}

$('zimfolders').addEventListener('click', ev => {
  const again = ev.target.closest('[data-zimrescan]');
  if (again) {
    rescanZimFolder(again);
    return;
  }
  const b = ev.target.closest('[data-zimstop]');
  if (!b) return;
  pendingZimStop = b.dataset.zimstop;
  $('zimstoptext').innerHTML =
    `Kids Shelf stops using <b>${esc(pendingZimStop)}</b>, and its libraries leave the shelf. ` +
    'The files stay where they are: nothing in the folder is changed or deleted.';
  openDialog('zimstopconfirm', cancelZimStop, 'zimstopno', $('zimpath'));
});

$('zimstopno').addEventListener('click', cancelZimStop);

$('zimstopok').addEventListener('click', async () => {
  const path = pendingZimStop;
  pendingZimStop = null;
  closeDialog();
  if (!path) return;
  try {
    await post('/api/admin/content/kiwix/folders/remove', { path });
    toast('Kids Shelf stopped using the folder. The files in it are still there.', 'ok');
    await Promise.all([loadZimFolders(), loadKiwix()]);
  } catch (e) {
    if (!e.pin) toast(e.message, 'err');
  }
});

async function loadZimFolders() {
  try {
    const d = await api('/api/admin/content/kiwix/folders');
    store.zimFolders = d.folders || [];
    renderZimFolders();
  } catch (e) {
    loadFailed($('zimfolders'), e);
  }
}

// searchCatalog loads catalog entries from start on: 0 starts a new search
// with the current filters (count says how many, to redraw what is shown);
// more than 0 adds the next page to the list. It reports success.
async function searchCatalog(start, count) {
  if (start === 0 && !count) {
    store.catQ = $('catq').value.trim();
    store.catLang = $('catlang').value;
    store.catCat = $('catcat').value;
    markPick();
  }
  const params = new URLSearchParams();
  if (store.catQ) params.set('q', store.catQ);
  if (store.catLang) params.set('lang', store.catLang);
  if (store.catCat) params.set('category', store.catCat);
  params.set('count', String(Math.min(100, count || 20)));
  if (start) params.set('start', String(start));
  if (start === 0 && !count) $('catstatus').textContent = 'Searching…';
  try {
    const d = await api('/api/admin/content/kiwix/catalog?' + params.toString());
    const entries = d.entries || [];
    store.catalog = {
      total: d.total,
      start: 0,
      entries: start ? store.catalog.entries.concat(entries) : entries
    };
    renderCatalog();
    return true;
  } catch (e) {
    if (start === 0 && !count) {
      $('catstatus').textContent = '';
      loadFailed($('catalog'), e);
    } else if (!e.pin) {
      toast(e.message, 'err');
    }
    return false;
  }
}

$('catsearch').addEventListener('click', () => searchCatalog(0));
$('catq').addEventListener('keydown', ev => { if (ev.key === 'Enter') searchCatalog(0); });
$('catlang').addEventListener('change', () => searchCatalog(0));
$('catcat').addEventListener('change', () => searchCatalog(0));

// Kid picks fill in the search and run it; the pick that matches the
// current search shows as pressed.
function markPick() {
  for (const b of document.querySelectorAll('#catpicks [data-pick-cat], #catpicks [data-pick-q]')) {
    const on = (b.dataset.pickCat && b.dataset.pickCat === store.catCat && !store.catQ) ||
      (b.dataset.pickQ && b.dataset.pickQ === store.catQ && !store.catCat);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  }
}

$('catpicks').addEventListener('click', ev => {
  const b = ev.target.closest('[data-pick-cat], [data-pick-q]');
  if (!b) return;
  $('catq').value = b.dataset.pickQ || '';
  const cat = b.dataset.pickCat || '';
  if (cat && ![...$('catcat').options].some(o => o.value === cat)) {
    $('catcat').insertAdjacentHTML('beforeend', `<option value="${esc(cat)}">${esc(kindName(cat))}</option>`);
  }
  $('catcat').value = cat;
  searchCatalog(0);
});

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
    store.languages = (langs.languages || []).slice()
      .sort((a, b) => String(a.name).localeCompare(String(b.name)));
    store.categories = (cats.categories || []).slice()
      .sort((a, b) => kindName(a).localeCompare(kindName(b)));
    langNames = {};
    for (const l of store.languages) langNames[l.code] = l.name;
    $('catlang').innerHTML = `<option value="">Any language</option>` +
      store.languages.map(l => `<option value="${esc(l.code)}">${esc(l.name)}${l.count ? ` (${l.count})` : ''}</option>`).join('');
    $('catcat').innerHTML = `<option value="">Any kind</option>` +
      store.categories.map(c => `<option value="${esc(c)}">${esc(kindName(c))}</option>`).join('');
    $('catlang').value = store.catLang || defaultLang();
    $('catcat').value = store.catCat;
  } catch (e) {
    if (!e.pin && !e.offline) $('catstatus').textContent = e.message;
  }
}

// ── loading ─────────────────────────────────────────────────────────────────

async function loadAll() {
  // The catalog search waits for the language list, so it starts in the
  // parent's language.
  const catalog = loadCatalogMeta().then(() => {
    if (!pinOpen) return searchCatalog(0);
  });
  await Promise.all([loadYouTube(), loadFollows(), loadSources(), loadIngest(), loadFixes(), loadKiwix(), loadZimFolders(), catalog]);
}

function poll() {
  if (pinOpen || document.hidden) return;
  loadYouTube();
  loadFollows();
  loadIngest();
  loadFixes();
  loadKiwix();
}

loadAll();
setInterval(poll, 4000);
