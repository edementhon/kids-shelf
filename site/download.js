// Picks the download for the visitor's computer and fills in the version
// and file sizes from GitHub's release API. Without JavaScript, or if
// GitHub doesn't answer, every link still works: they point at
// releases/latest/download/<file>, which GitHub sends to the newest file.
'use strict';

(function () {
  const REPO = 'edementhon/kids-shelf';
  const LABEL = {
    'windows': 'Download for Windows',
    'mac-arm': 'Download for Mac',
    'mac-intel': 'Download for Mac (Intel)',
    'linux': 'Download for Linux',
    'linux-arm': 'Download for Raspberry Pi'
  };

  const $ = sel => document.querySelector(sel);
  // others is the link line under the main button once the computer is
  // known; showRelease puts the version in front of it.
  let others = '';
  const item = key => document.querySelector('[data-files] li[data-key="' + key + '"]');

  // detect returns a key of LABEL, 'phone' for phones and tablets (which
  // can't run kids-shelf, only open it), or '' when we can't tell.
  async function detect() {
    const ua = navigator.userAgent;
    const uad = navigator.userAgentData;
    // iPadOS asks for desktop pages and says "Macintosh"; only touch gives it away.
    if (/iPhone|iPad|iPod|Android/i.test(ua) || (uad && uad.mobile) ||
        (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) {
      return 'phone';
    }
    const platform = (uad && uad.platform) || navigator.platform || '';
    let arch = '';
    if (uad && uad.getHighEntropyValues) {
      try {
        arch = (await uad.getHighEntropyValues(['architecture'])).architecture || '';
      } catch (e) { /* not offered: guess from the user agent */ }
    }
    if (/Win/i.test(platform) || /Windows/.test(ua)) return 'windows';
    if (/Mac/i.test(platform) || /Mac OS X/.test(ua)) {
      // Safari reports an Intel Mac on every Mac; Chrome and Edge tell the
      // truth through the architecture. Apple chips are the likelier guess.
      return arch === 'x86' ? 'mac-intel' : 'mac-arm';
    }
    if (/CrOS/.test(ua)) return '';
    if (/Linux/i.test(platform) || /Linux|X11/.test(ua)) {
      return arch === 'arm' || /aarch64|arm64/i.test(platform + ' ' + ua) ? 'linux-arm' : 'linux';
    }
    return '';
  }

  function mb(bytes) {
    return Math.round(bytes / 1e6) + ' MB';
  }

  function showRelease(rel) {
    const sizes = {};
    for (const a of rel.assets || []) sizes[a.name] = a.size;
    for (const li of document.querySelectorAll('[data-files] li')) {
      const a = li.querySelector('a');
      const name = a.href.split('/').pop();
      if (sizes[name]) li.querySelector('[data-size]').textContent = mb(sizes[name]);
    }
    const version = String(rel.tag_name || '').replace(/^v/, '');
    if (!version) return;
    const when = rel.published_at
      ? new Date(rel.published_at).toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' })
      : '';
    $('[data-release]').textContent = 'Version ' + version + (when ? ', released ' + when : '') +
      '. Free. Pick the file for the computer that will run kids-shelf.';
    if (others && /^[0-9A-Za-z.+-]+$/.test(version)) {
      $('[data-main-note]').innerHTML = 'Version ' + version + ', free. ' + others;
    }
  }

  async function main() {
    const key = await detect();
    const main = $('[data-main]');
    const note = $('[data-main-note]');
    if (key === 'phone') {
      $('[data-get]').classList.add('hidden');
      $('[data-phone-note]').classList.remove('hidden');
    } else if (LABEL[key]) {
      const li = item(key);
      li.classList.add('mine');
      main.textContent = LABEL[key];
      main.href = li.querySelector('a').href;
      others = key === 'mac-arm'
        ? 'For a Mac with an Apple chip. <a href="#download">Intel Mac or another computer?</a>'
        : '<a href="#download">Other computers</a>';
      note.innerHTML = 'Free. ' + others;
      const os = key.startsWith('mac') ? 'mac' : key.startsWith('linux') ? 'linux' : 'windows';
      const d = document.querySelector('[data-os="' + os + '"]');
      if (d) d.open = true;
    }
    try {
      const resp = await fetch('https://api.github.com/repos/' + REPO + '/releases/latest',
        { headers: { Accept: 'application/vnd.github+json' } });
      if (resp.ok) showRelease(await resp.json());
    } catch (e) { /* offline or rate-limited: the links work without it */ }
  }

  main();
})();
