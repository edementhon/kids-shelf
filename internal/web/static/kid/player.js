// Kids Shelf player: tap-to-play, resume at the saved position, and live
// metering that charges only real playback seconds (the server does the
// math). Ported from the legacy portal's SCRIPT_PLAYER_TEMPLATE: a 1-second
// timer runs only while the video is genuinely advancing, stalled ticks are
// skipped, and seek/pause/end never charge.
'use strict';

const KS = window.KS || {};
const episode = KS.Episode || '';
const v = document.getElementById('v');

let grownup = false;  // grown-up watch-together session (no kid account)
let FREE = !!KS.Free; // free bin or grown-up: never metered, never cut off
let me = null;        // /api/kid/me result

// Metering state. pendingSec holds real playback seconds not yet reported;
// the timer is started by 'playing' and stopped by pause/ended/waiting/
// stalled/error/emptied, so it can never run while the video isn't advancing.
let meterT = null;
let posT = null;
let pendingSec = 0;
let lastTickPos = -1;
let freezeStreak = 0;
let cut = false;
let lastBadgeMin = -1;

v.src = '/media/' + encodeURIComponent(episode) + '.mp4';
$('info').textContent = KS.Title || '';

function $(id) { return document.getElementById(id); }

function badge(t) {
  const el = $('mb');
  if (el) el.textContent = t;
}

function showBalance(total) {
  // Whole minutes, never below 0: over-shot allowances must read "0 min
  // left", not "-2 min left", and floats stay off kid screens.
  const whole = Math.max(0, Math.floor(total || 0));
  if (whole === lastBadgeMin) return; // badge updates at most once per minute
  lastBadgeMin = whole;
  badge('⏳ ' + whole + ' min left');
}

// ---------------------------------------------------------------------------
// Metering (kids only, non-free bins)
// ---------------------------------------------------------------------------

function meterStart() {
  if (FREE || cut) return;
  if (meterT) return;
  meterT = setInterval(report, 1000);
  if (!posT) posT = setInterval(savePos, 10000);
}

function meterStop() {
  if (meterT) { clearInterval(meterT); meterT = null; }
  if (posT) { clearInterval(posT); posT = null; }
}

async function report(force) {
  if (FREE || cut) return;
  if (!force && meterT === null) return; // timer stopped → paused/ended: nothing to send
  if (!v.currentTime) return;
  // Silent-freeze guard: a tick whose position did not advance is a stall;
  // skip charging it but keep the timer: it resumes when playback resumes.
  if (!force) {
    if (v.currentTime <= lastTickPos + 0.05) {
      freezeStreak++;
      return;
    }
    freezeStreak = 0;
    lastTickPos = v.currentTime;
    pendingSec += 1; // one real second of playing time per tick
  }
  if (!force && pendingSec < 1) return;
  const sec = pendingSec;
  pendingSec = 0;
  if (sec <= 0) return;
  try {
    const r = await fetch('/api/kid/consume', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ episode: episode, sec: sec }),
    });
    const d = await r.json();
    if (d.cutOff) cutOff();
    showBalance(d.total);
  } catch (e) { /* next tick will retry; never charge twice */ }
}

function cutOff() {
  if (cut) return;
  cut = true;
  v.pause();
  meterStop();
  $('chunkTitle').textContent = '🕐 Time\u2019s up!';
  $('chunkMsg').textContent = 'You used all your minutes for today. Ask a grown-up to watch together :)';
  $('chunk').hidden = false;
}

// ---------------------------------------------------------------------------
// Resume positions (per kid; a grown-up session has none)
// ---------------------------------------------------------------------------

function savePos() {
  if (grownup || !v.currentTime) return;
  fetch('/api/kid/position', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ episode: episode, pos: Math.floor(v.currentTime) }),
  }).catch(() => {});
}

function clearPos() {
  if (grownup) return;
  fetch('/api/kid/position', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ episode: episode, pos: 0 }),
  }).catch(() => {});
}
// showEnd reveals the end-of-video panel with a way back (and on to the
// next episode when the show has one).
function showEnd() {
  const next = $('endNext');
  if (next) next.hidden = !KS.Next;
  const e = $('end');
  if (e) e.hidden = false;
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function startPlayback() {
  const tap = $('tap');
  if (tap) tap.hidden = true;
  const p = v.play();
  if (p) p.catch(() => { if (tap) tap.hidden = false; });
}
document.getElementById('tap').addEventListener('click', startPlayback);
document.getElementById('chunkBtn').addEventListener('click', () => { location.href = '/'; });
document.getElementById('endBack').addEventListener('click', () => { location.href = '/'; });
document.getElementById('endNext').addEventListener('click', () => { location.href = '/watch/' + encodeURIComponent(KS.Next); });

// The watch log is written only when playback really starts: the first
// 'playing' event posts /api/kid/started, and a play blocked by "Time's up"
// never fires 'playing', so it never leaves a log row. Only the server's
// explicit "no minutes left" (403) stops the video here: any other failure
// just loses the log row, and the live meter (/api/kid/consume) still cuts
// off a kid who really runs out.
let startedSent = false;
function reportStarted() {
  if (startedSent) return;
  startedSent = true;
  fetch('/api/kid/started', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ episode: episode }),
  }).then((r) => { if (r.status === 403) cutOff(); }).catch(() => {});
}

// Live metering: the clock runs only while the video is genuinely playing.
v.addEventListener('playing', () => {
  reportStarted();
  if (!grownup && !posT) posT = setInterval(savePos, 10000);
  if (!FREE && !cut) { freezeStreak = 0; meterStart(); }
  const tap = $('tap');
  if (tap) tap.hidden = true;
});
v.addEventListener('pause', () => { if (!FREE) report(true); meterStop(); if (!grownup) savePos(); });
v.addEventListener('ended', () => { if (!FREE) report(true); meterStop(); if (!grownup) clearPos(); showEnd(); });
v.addEventListener('waiting', () => meterStop());
v.addEventListener('stalled', () => meterStop());
v.addEventListener('error', () => {
  meterStop();
  showMediaUnavailable();
});

// A failed media load can mean the episode is an in-place original on a
// NAS or drive that is not connected: the server answers 503 with the
// message. Probe the URL once (one-byte range) to read it; any other
// failure gets a generic line. Never loops: the probe is one request.
async function showMediaUnavailable() {
  const url = '/media/' + encodeURIComponent(episode) + '.mp4';
  try {
    const r = await fetch(url, { headers: { Range: 'bytes=0-0' } });
    if (r.status === 503) {
      const d = await r.json().catch(() => null);
      if (d && d.error) { $('info').textContent = d.error; return; }
    }
  } catch (e) { /* no network: generic line below */ }
  $('info').textContent = 'This video can\u2019t play right now. Try again in a moment.';
}
v.addEventListener('emptied', () => meterStop());
// Seeking is NOT watching: reset so a seek is never charged.
v.addEventListener('seeked', () => { pendingSec = 0; lastTickPos = v.currentTime; freezeStreak = 0; });

window.addEventListener('pagehide', () => { if (!grownup) savePos(); report(true); });
window.addEventListener('beforeunload', () => { if (!grownup) savePos(); report(true); meterStop(); });

async function main() {
  let r;
  try {
    r = await fetch('/api/kid/me');
  } catch (e) {
    location.href = '/';
    return;
  }
  if (r.status === 401) { location.href = '/'; return; }
  me = await r.json().catch(() => null);
  if (me && me.grownup) {
    grownup = true;
    FREE = true;
    badge('💛 Free');
    return;
  }
  if (FREE) {
    badge('💛 Free');
    return;
  }
  // Kid on a metered bin: show the starting balance (and cut off right away
  // when the minutes are already gone) and resume past the 5-second mark.
  fetch('/api/balance').then((rr) => rr.json()).then((d) => {
    showBalance(d.total);
    if (d.total <= 0.01) cutOff();
  }).catch(() => {});
  fetch('/api/kid/resume/' + encodeURIComponent(episode)).then((rr) => rr.json()).then((d) => {
    const pos = d.pos || 0;
    if (pos > 5 && (!v.duration || pos < v.duration - 5)) v.currentTime = pos;
  }).catch(() => {});
}

main();
