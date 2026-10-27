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

// Improvement status of this episode ("" when the server does not send it).
// issue: "" | "sound" | "picture" | "format"; fix: "" | "pending" | "failed".
// A "pending" episode is not playable yet: the server refuses its media and
// its watch starts with 409, so this page shows a panel and never loads it.
const issue = KS.Issue || '';
const fix = KS.Fix || '';
const pendingEpisode = !!issue && fix === 'pending';

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

// Problem detection state. panelUp means a panel covers the player: the
// meter is stopped and must not report. The streaks count consecutive real
// playing seconds with no decoded video (black) or no decoded audio (silent).
let panelUp = false;
let soundWarned = false; // the kid chose "watch without sound": never ask again
let blackStreak = 0;
let soundStreak = 0;
let lastCheckPos = -1;
let noPlayT = null;    // 10 s "never started playing" watchdog
let playCheckT = null; // 1 s black/silence check while playing

if (!pendingEpisode) {
  v.src = '/media/' + encodeURIComponent(episode) + '.mp4';
}
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
// Problem panels: a video that cannot really play, has no sound, or is still
// being fixed gets a full-screen panel in the same family as Time's up and
// the end screen. Every panel stops the meter first: nothing is charged for
// a video that is not really playing.
// ---------------------------------------------------------------------------

function stopProblemTimers() {
  clearTimeout(noPlayT);
  noPlayT = null;
  clearInterval(playCheckT);
  playCheckT = null;
  blackStreak = 0;
  soundStreak = 0;
}

// The 10 s watchdog from the play request. waiting/stalled re-arm it, so a
// video stuck on the network is never blamed for not starting; 'playing'
// clears it. No paused check inside: while play() is still pending the
// element reports paused === false, and that is exactly the never-started
// state this watchdog exists for.
function armNoPlayWatchdog() {
  clearTimeout(noPlayT);
  noPlayT = setTimeout(() => {
    noPlayT = null;
    if (!panelUp) showCannotPlay();
  }, 10000);
}

function clearNoPlayWatchdog() {
  clearTimeout(noPlayT);
  noPlayT = null;
}

// freezeProblem stops everything that could charge: drop owed seconds, stop
// the clocks, pause the video. The pause handler below sees panelUp and
// reports nothing.
function freezeProblem() {
  panelUp = true;
  stopProblemTimers();
  pendingSec = 0;
  v.pause();
  meterStop();
}

function showPendingPanel() {
  const p = $('pending');
  if (!p || !p.hidden) return;
  freezeProblem();
  p.hidden = false;
}

function showCannotPlay() {
  if (panelUp) return;
  if (fix === 'pending') { showPendingPanel(); return; } // 409 race safety net
  freezeProblem();
  $('playIssueTitle').textContent = '🚫 This one won\u2019t play here';
  $('playIssueMsg').textContent = 'Pick another one, or ask a grown-up.';
  const next = $('playIssueNext');
  if (next) next.hidden = !KS.Next;
  $('playIssue').hidden = false;
}

function showNoSound() {
  if (panelUp || soundWarned) return;
  freezeProblem();
  $('soundTitle').textContent = fix === 'pending' ? '🔇 No sound yet' : '🔇 No sound on this one';
  $('sound').hidden = false;
}

// "Watch without sound": close the panel, resume, and never ask again. The
// meter starts again with the next 'playing' event.
function dismissNoSound() {
  const s = $('sound');
  if (s) s.hidden = true;
  panelUp = false;
  soundWarned = true;
  soundStreak = 0;
  const p = v.play();
  if (p) p.catch(() => {});
}

// playCheck runs once a second while playing. Only real playback counts: a
// tick whose clock did not advance is a stall, and stalls reset both streaks
// instead of counting toward a verdict.
function playCheck() {
  if (panelUp || v.paused || v.readyState < 3) { blackStreak = 0; soundStreak = 0; return; }
  const pos = v.currentTime;
  if (pos <= lastCheckPos + 0.05) { blackStreak = 0; soundStreak = 0; return; }
  lastCheckPos = pos;
  // Black video: frames that never decode. webkitVideoDecodedByteCount is a
  // second signal where the browser exposes it.
  const noVideo = v.videoWidth === 0 ||
    (typeof v.webkitVideoDecodedByteCount === 'number' && v.webkitVideoDecodedByteCount === 0);
  blackStreak = noVideo ? blackStreak + 1 : 0;
  if (blackStreak >= 3) { showCannotPlay(); return; }
  // Silent video: only plan "sound" episodes are checked, and only when the
  // browser tells us about audio. No counter, no guess.
  if (issue === 'sound' && !soundWarned) {
    const noAudio = v.mozHasAudio === false ||
      (typeof v.webkitAudioDecodedByteCount === 'number' && v.webkitAudioDecodedByteCount === 0);
    if (noAudio) {
      soundStreak++;
      if (soundStreak >= 3) { showNoSound(); return; }
    } else {
      soundStreak = 0;
    }
  }
}

function startPlayCheck() {
  if (playCheckT) clearInterval(playCheckT);
  blackStreak = 0;
  soundStreak = 0;
  lastCheckPos = v.currentTime;
  playCheckT = setInterval(playCheck, 1000);
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
  armNoPlayWatchdog();
  const p = v.play();
  if (p) p.catch(() => { if (tap) tap.hidden = false; clearNoPlayWatchdog(); });
}
document.getElementById('tap').addEventListener('click', startPlayback);
document.getElementById('chunkBtn').addEventListener('click', () => { location.href = '/'; });
document.getElementById('endBack').addEventListener('click', () => { location.href = '/'; });
document.getElementById('endNext').addEventListener('click', () => { location.href = '/watch/' + encodeURIComponent(KS.Next); });
document.getElementById('pendingBack').addEventListener('click', () => { location.href = '/'; });
document.getElementById('playIssueBack').addEventListener('click', () => { location.href = '/'; });
document.getElementById('playIssueNext').addEventListener('click', () => { location.href = '/watch/' + encodeURIComponent(KS.Next); });
document.getElementById('soundBack').addEventListener('click', () => { location.href = '/'; });
document.getElementById('soundMute').addEventListener('click', dismissNoSound);

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
  // 403 is the server's "Time's up". 409 means this episode is still
  // getting ready: same panel as the shelf, never Time's up.
  fetch('/api/kid/started', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ episode: episode }),
  }).then((r) => { if (r.status === 403) cutOff(); else if (r.status === 409) showPendingPanel(); }).catch(() => {});
}

// Live metering: the clock runs only while the video is genuinely playing.
v.addEventListener('playing', () => {
  reportStarted();
  if (!grownup && !posT) posT = setInterval(savePos, 10000);
  if (!FREE && !cut) { freezeStreak = 0; meterStart(); }
  const tap = $('tap');
  if (tap) tap.hidden = true;
  clearNoPlayWatchdog();
  startPlayCheck();
});
// A problem panel pauses the video; the pause event must not report the
// seconds we just dropped in freezeProblem.
v.addEventListener('pause', () => { if (!panelUp && !FREE) report(true); meterStop(); if (!grownup) savePos(); });
v.addEventListener('ended', () => { if (!FREE) report(true); meterStop(); if (!grownup) clearPos(); showEnd(); });
// Network stalls do not count toward the "never started playing" verdict:
// each one re-arms the 10 s watchdog instead.
v.addEventListener('waiting', () => { meterStop(); armNoPlayWatchdog(); });
v.addEventListener('stalled', () => { meterStop(); armNoPlayWatchdog(); });
v.addEventListener('error', () => {
  meterStop();
  stopProblemTimers();
  // A known-problem original gets an explaining panel. The 503 probe below
  // stays for episodes that play everywhere (a drive or NAS went away).
  if (fix === 'pending') showPendingPanel();
  else if (issue) showCannotPlay();
  else showMediaUnavailable();
});

// A failed media load can mean the episode is an in-place original on a
// NAS or drive that is not connected: the server answers 503 with the
// message. Probe the URL once (one-byte range) to read it; any other
// failure gets a generic line. Never loops: the probe is one request.
async function showMediaUnavailable() {
  const url = '/media/' + encodeURIComponent(episode) + '.mp4';
  try {
    const r = await fetch(url, { headers: { Range: 'bytes=0-0' } });
    // 409 is the server saying this episode is still getting ready; the
    // pending panel has the right words, whatever this browser saw.
    if (r.status === 409) { showPendingPanel(); return; }
    if (r.status === 503) {
      const d = await r.json().catch(() => null);
      if (d && d.error) { $('info').textContent = d.error; return; }
    }
  } catch (e) { /* no network: generic line below */ }
  $('info').textContent = 'This video can\u2019t play right now. Try again in a moment.';
}
v.addEventListener('emptied', () => meterStop());
// Seeking is NOT watching: reset so a seek is never charged, and reset the
// problem streaks so a jump is never counted as a broken second.
v.addEventListener('seeked', () => { pendingSec = 0; lastTickPos = v.currentTime; freezeStreak = 0; lastCheckPos = v.currentTime; blackStreak = 0; soundStreak = 0; });

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
  if (pendingEpisode) {
    // The video is still being made, so this page never loaded it: same
    // panel as the shelf, with one way back. Open for anyone, grown-ups
    // included, because nobody can play it yet.
    showPendingPanel();
    return;
  }
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
