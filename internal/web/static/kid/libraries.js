// Kids Shelf libraries page: one big touch tile per installed library,
// opened through the /kiwix/ viewer. Free, no kid login, exactly like the
// picker tile that links here. Plain JS with DOM APIs: every name from the
// server lands in a text node, never in innerHTML, so a library title can
// never inject markup.
'use strict';

const $ = (id) => document.getElementById(id);

// langName turns the server's comma-separated ISO 639-3 codes into display
// names. LANG_FALLBACK mirrors the parent page's table (content.js) so the
// two stay in sync; there is no catalog feed to load from on this page.
// LANG_MORE adds the regional languages kids' wikis (Vikidia, Wikipedia)
// come in, which browsers often cannot name: Chromium's own language
// list leaves out "scn" (Sicilian), for one. Any other code is named by
// the browser (Intl.DisplayNames), and only a code nobody knows is shown
// as-is.
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

const LANG_MORE = {
  scn: 'Sicilian', nap: 'Neapolitan', vec: 'Venetian', lmo: 'Lombard', pms: 'Piedmontese',
  lij: 'Ligurian', fur: 'Friulian', srd: 'Sardinian', cos: 'Corsican', oci: 'Occitan',
  bre: 'Breton', eus: 'Basque', glg: 'Galician', ast: 'Asturian', arg: 'Aragonese',
  ltz: 'Luxembourgish', fry: 'Frisian', gsw: 'Swiss German', bar: 'Bavarian', roh: 'Romansh',
  wln: 'Walloon', sco: 'Scots', gla: 'Scottish Gaelic', glv: 'Manx', cor: 'Cornish',
  epo: 'Esperanto', lat: 'Latin', ina: 'Interlingua', yid: 'Yiddish', bel: 'Belarusian',
  bos: 'Bosnian', afr: 'Afrikaans', msa: 'Malay', tam: 'Tamil', tel: 'Telugu',
  mar: 'Marathi', guj: 'Gujarati', kan: 'Kannada', mal: 'Malayalam', pan: 'Punjabi',
  pus: 'Pashto', kur: 'Kurdish', tat: 'Tatar', zul: 'Zulu', xho: 'Xhosa',
  yor: 'Yoruba', ibo: 'Igbo', hau: 'Hausa', som: 'Somali', mlg: 'Malagasy',
  haw: 'Hawaiian', mri: 'Maori', nob: 'Norwegian', nno: 'Norwegian', mul: 'Many languages'
};

const langNames = (typeof Intl !== 'undefined' && Intl.DisplayNames)
  ? (() => { try { return new Intl.DisplayNames(['en'], { type: 'language' }); } catch (e) { return null; } })()
  : null;

function langName(codes) {
  return String(codes).split(',').map(c => {
    c = c.trim();
    if (!c) return '';
    if (LANG_FALLBACK[c] || LANG_MORE[c]) return LANG_FALLBACK[c] || LANG_MORE[c];
    if (langNames) {
      try {
        const n = langNames.of(c);
        if (n && n.toLowerCase() !== c.toLowerCase()) return n;
      } catch (e) { /* not a language code: shown as-is */ }
    }
    return c;
  }).filter(Boolean).join(', ');
}

// Kid words for the one banner the page may show.
const MSG_DOWN = 'The libraries aren\u2019t working right now. Try again in a little while.';
const MSG_ALL_MISSING = 'These books aren\u2019t here right now. Ask a grown-up to help.';

function showMessage(text) {
  $('msg-text').textContent = text;
  $('msg').hidden = false;
}

function hideMessage() {
  $('msg').hidden = true;
}

// The libraries origin this page's books live on, when the app serves
// them on their own port: book links and icons are relative to it. Empty:
// no separate origin, so URLs stay on the app origin.
let LIB_BASE = '';

// abs resolves a book URL or icon against the libraries origin; a URL that
// is already absolute (or there being no separate origin) passes through.
function abs(u) {
  if (!u) return '';
  return LIB_BASE ? new URL(u, LIB_BASE).href : u;
}

// tile builds one library tile: a link to the book's viewer page on the
// libraries origin while the backend is up, a plain non-interactive card
// otherwise (the "not working right now" state still shows what is
// installed). A book whose file is gone says so and asks for a grown-up.
function tile(book, available) {
  const url = abs(book.url);
  const open = available && url && !book.missing;
  const t = open ? document.createElement('a') : document.createElement('div');
  t.className = 'book' + (open ? '' : ' off') + (book.missing ? ' missing' : '');
  if (open) t.href = url;

  if (book.icon && !book.missing) {
    const img = document.createElement('img');
    img.src = abs(book.icon);
    img.alt = '';
    t.appendChild(img);
  } else {
    const emoji = document.createElement('span');
    emoji.className = 'emoji';
    emoji.textContent = '📚';
    t.appendChild(emoji);
  }

  const title = document.createElement('div');
  title.className = 't';
  title.textContent = book.title || book.name;
  t.appendChild(title);

  if (book.language) {
    const lang = document.createElement('div');
    lang.className = 'l';
    lang.textContent = langName(book.language);
    t.appendChild(lang);
  }
  if (book.missing) {
    const gone = document.createElement('div');
    gone.className = 'gone';
    gone.textContent = 'Not here right now. Ask a grown-up.';
    t.appendChild(gone);
  }
  return t;
}

// While anything is not working, the page checks again on its own every
// RECHECK_MS, quietly: a library that comes back opens without a reload.
const RECHECK_MS = 20000;
let recheckTimer = 0;
let lastData = '';

function scheduleRecheck(needed) {
  clearTimeout(recheckTimer);
  recheckTimer = needed ? setTimeout(load, RECHECK_MS) : 0;
}

async function load() {
  let data;
  try {
    const r = await fetch('/api/libraries', { cache: 'no-store' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    data = await r.json();
  } catch (e) {
    showMessage(MSG_DOWN);
    scheduleRecheck(true);
    return;
  }

  const list = data.books || [];
  const missing = list.filter(b => b.missing).length;
  scheduleRecheck(list.length > 0 && (!data.available || missing > 0));
  // One message at a time: nothing installed means "ask a grown-up" (the
  // empty state), every file gone means "ask a grown-up" too, and only
  // installed-but-unreachable libraries mean "try again".
  if (list.length && missing === list.length) showMessage(MSG_ALL_MISSING);
  else if (list.length && !data.available) showMessage(MSG_DOWN);
  else hideMessage();

  // Same answer as last time: leave the tiles (and the kid's focus) alone.
  const key = JSON.stringify(data);
  if (key === lastData) return;
  lastData = key;
  LIB_BASE = data.libraries_url || '';

  const wrap = $('books');
  const focused = document.activeElement && wrap.contains(document.activeElement)
    ? [...wrap.children].indexOf(document.activeElement) : -1;
  wrap.textContent = '';
  list.forEach(b => wrap.appendChild(tile(b, data.available)));
  if (focused >= 0 && wrap.children[focused] && wrap.children[focused].tagName === 'A') {
    wrap.children[focused].focus();
  }
  $('empty').hidden = list.length > 0;
}

// Arrow keys move between the tiles that open (Tab and Enter work too):
// left and right go one tile, up and down go to the nearest tile in the
// row above or below.
function onArrow(e) {
  const keys = { ArrowLeft: 1, ArrowRight: 1, ArrowUp: 1, ArrowDown: 1 };
  if (!keys[e.key]) return;
  const tiles = [...$('books').querySelectorAll('a.book')];
  const i = tiles.indexOf(document.activeElement);
  if (!tiles.length) return;
  if (i < 0) { tiles[0].focus(); e.preventDefault(); return; }
  let next = null;
  if (e.key === 'ArrowLeft') next = tiles[i - 1];
  else if (e.key === 'ArrowRight') next = tiles[i + 1];
  else {
    const cur = tiles[i].getBoundingClientRect();
    const down = e.key === 'ArrowDown';
    let best = Infinity;
    tiles.forEach(t => {
      const r = t.getBoundingClientRect();
      const dy = down ? r.top - cur.bottom : cur.top - r.bottom;
      if (dy < -1) return;
      const score = dy * 10 + Math.abs(r.left - cur.left);
      if (t !== tiles[i] && score < best) { best = score; next = t; }
    });
  }
  if (next) { next.focus(); e.preventDefault(); }
}

document.addEventListener('DOMContentLoaded', () => {
  $('retry').addEventListener('click', () => { hideMessage(); load(); });
  document.addEventListener('keydown', onArrow);
  // Coming back from a book (browser Back keeps this page in memory) or
  // to a tab left open: check the list again.
  window.addEventListener('pageshow', (e) => { if (e.persisted) load(); });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) load(); });
  load();
});

