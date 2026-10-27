// Kids Shelf libraries page: one big touch tile per installed library,
// opened through the /kiwix/ viewer. Free, no kid login, exactly like the
// picker tile that links here. Plain JS with DOM APIs: every name from the
// server lands in a text node, never in innerHTML, so a library title can
// never inject markup.
'use strict';

const $ = (id) => document.getElementById(id);

// langName turns the server's comma-separated ISO 639-3 codes into display
// names. The table mirrors the parent page's LANG_FALLBACK (content.js) so
// the two stay in sync; there is no catalog feed to load from on this page.
// An unknown code is shown as-is rather than hidden.
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

function langName(codes) {
  return String(codes).split(',').map(c => {
    c = c.trim();
    if (!c) return '';
    return LANG_FALLBACK[c] || c;
  }).filter(Boolean).join(', ');
}

function showUnavailable(text) {
  const m = $('msg');
  m.textContent = text;
  m.hidden = false;
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
// otherwise (the "not available right now" state still shows what is
// installed).
function tile(book, available) {
  const url = abs(book.url);
  const t = available && url ? document.createElement('a') : document.createElement('div');
  t.className = 'book' + (available && url ? '' : ' off');
  if (available && url) t.href = url;

  if (book.icon) {
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
  return t;
}

async function load() {
  let data;
  try {
    const r = await fetch('/api/libraries');
    if (!r.ok) {
      const err = await r.json().catch(() => null);
      showUnavailable(err && err.error ? err.error
        : 'The libraries aren\u2019t available right now. Try again in a moment.');
      return;
    }
    data = await r.json();
  } catch (e) {
    showUnavailable('The libraries aren\u2019t available right now. Try again in a moment.');
    return;
  }

  const list = data.books || [];
  LIB_BASE = data.libraries_url || '';
  // One message at a time: nothing installed means "ask a grown-up", and
  // only installed-but-unreachable libraries mean "try again in a moment".
  if (!data.available && list.length) {
    showUnavailable('The libraries aren\u2019t available right now. Try again in a moment.');
  }
  const wrap = $('books');
  wrap.textContent = '';
  list.forEach(b => wrap.appendChild(tile(b, data.available)));
  if (!list.length) $('empty').hidden = false;
}

document.addEventListener('DOMContentLoaded', load);
