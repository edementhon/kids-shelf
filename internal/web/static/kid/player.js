// Kids Shelf player: tap-to-play, resume at the saved position, and live
// metering that charges only real playback seconds (the server does the
// math). Ported from the legacy portal's SCRIPT_PLAYER_TEMPLATE: a 1-second
// timer runs only while the video is genuinely advancing with a picture,
// stalled ticks are skipped, and seek/pause/end never charge.
//
// Panels (Time's up, Done!, won't play, no sound, getting ready, who's
// watching) all open through openPanel: while one is up the video is inert,
// cannot play, and nothing is charged. "Big screen" fullscreens #stage,
// which holds the video AND the panels, so a panel is never hidden.
'use strict';

const KS = window.KS || {};
const episode = KS.Episode || '';
const v = document.getElementById('v');
const stage = document.getElementById('stage');
const mediaURL = '/media/' + encodeURIComponent(episode) + '.mp4';

function $(id) { return document.getElementById(id); }

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
// heldSec holds seconds whose picture has not decoded yet: they are charged
// once a picture shows and dropped if the "won't play" panel opens. The
// timer is started by 'playing' and stopped by pause/ended/waiting/error/
// emptied; a tick whose clock did not move is never charged.
let meterT = null;
let posT = null;
let pendingSec = 0;
let heldSec = 0;
let lastTickPos = -1;
let freezeStreak = 0;
let cut = false;      // Time's up or the session is gone: never plays again
let lastBadgeMin = -1;

// Panel state. panelUp means a panel covers the player: the video is inert
// and paused, the meter is stopped and must not report. The streaks count
// consecutive real playing seconds with no decoded video (black) or no
// decoded audio (silent).
const PANELS = ['chunk', 'pending', 'playIssue', 'sound', 'end'];
let panelUp = false;
let soundWarned = false; // the kid chose "watch without sound": never ask again
let everPlayed = false;  // the first 'playing' happened
let blackStreak = 0;
let soundStreak = 0;
let lastCheckPos = -1;
let noPlayT = null;    // 10 s "never started playing" watchdog
let playCheckT = null; // 1 s black/silence check while playing

if (!pendingEpisode) v.src = mediaURL;
$('info').textContent = KS.Title || '';
document.title = KS.Title || 'Watch';

function badge(t) {
  const el = $('mb');
  if (el) el.textContent = t;
}

function showBalance(total) {
  if (typeof total !== 'number' || !isFinite(total)) return;
  // Whole minutes, rounded to the nearest: 9.9 reads "10 min left", not
  // 9, and floats stay off kid screens. Under a minute still reads 1: the
  // badge says 0 only at Time's up (0.01 or less), never below 0.
  const whole = total <= 0.01 ? 0 : Math.max(1, Math.round(total));
  if (whole === lastBadgeMin) return; // badge updates at most once per minute
  lastBadgeMin = whole;
  badge('⏳ ' + whole + ' min left');
}

// pictureOK is false while no video frame has decoded (a black screen).
// webkitVideoDecodedByteCount is a second signal where the browser has it.
function pictureOK() {
  if (v.videoWidth === 0) return false;
  return !(typeof v.webkitVideoDecodedByteCount === 'number' && v.webkitVideoDecodedByteCount === 0);
}

// ---------------------------------------------------------------------------
// Metering (kids only, non-free bins)
// ---------------------------------------------------------------------------

function meterStart() {
  if (FREE || cut || panelUp) return;
  if (meterT) return;
  meterT = setInterval(report, 1000);
  if (!posT) posT = setInterval(savePos, 10000);
}

function meterStop() {
  if (meterT) { clearInterval(meterT); meterT = null; }
  if (posT) { clearInterval(posT); posT = null; }
}

async function report(force) {
  if (FREE || cut || panelUp) return;
  if (!force && meterT === null) return; // timer stopped: paused/ended, nothing to send
  if (!v.currentTime) return;
  if (!force) {
    // Silent-freeze guard: a tick whose position did not advance is a
    // stall; skip charging it but keep the timer for when playback resumes.
    if (v.currentTime <= lastTickPos + 0.05) {
      freezeStreak++;
      return;
    }
    freezeStreak = 0;
    lastTickPos = v.currentTime;
    // The clock moves but no picture yet: hold the second. playCheck opens
    // the "won't play" panel after 3 black seconds and the hold is dropped.
    if (!pictureOK()) {
      heldSec += 1;
      return;
    }
    pendingSec += 1 + heldSec; // real seconds of playing time with a picture
    heldSec = 0;
  }
  const sec = pendingSec;
  pendingSec = 0;
  if (sec <= 0) return;
  let r;
  try {
    r = await fetch('/api/kid/consume', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ episode: episode, sec: sec }),
      keepalive: true,
    });
  } catch (e) { return; } // no answer: it may have counted, never charge twice
  if (r.status === 401) { showSessionLost(); return; }
  if (!r.ok) { pendingSec += sec; return; } // refused, so not charged: send it next tick
  const d = await r.json().catch(() => null);
  if (!d) return;
  showBalance(d.total);
  if (d.cutOff) cutOff();
}

// ---------------------------------------------------------------------------
// Panels
// ---------------------------------------------------------------------------

function fsElement() {
  return document.fullscreenElement || document.webkitFullscreenElement || null;
}

function exitFs() {
  const exit = document.exitFullscreen || document.webkitExitFullscreen;
  if (!exit) return Promise.resolve();
  const p = exit.call(document);
  return p && p.then ? p.catch(() => {}) : Promise.resolve();
}

// Only the stage may be fullscreen while a panel shows: a fullscreen video
// (iPhone's own player, or a browser that ignores nofullscreen) would hide it.
function leaveVideoFullscreen() {
  const fs = fsElement();
  if (fs && fs !== stage) exitFs();
  if (v.webkitDisplayingFullscreen && v.webkitExitFullscreen) v.webkitExitFullscreen();
}

function lockVideo(on) {
  v.inert = on;
  v.controls = !on;
  $('bar').inert = on;
  $('tap').inert = on;
  if (on) $('tap').hidden = true;
}

function hideLoading() { $('loading').hidden = true; }

// openPanel stops everything that could charge or play (owed seconds, held
// seconds, clocks, the video), locks the video, shows one panel and puts
// the keyboard on its first button.
function openPanel(id) {
  panelUp = true;
  stopProblemTimers();
  pendingSec = 0;
  heldSec = 0;
  meterStop();
  hideLoading();
  v.pause();
  lockVideo(true);
  leaveVideoFullscreen();
  for (const p of PANELS) $(p).hidden = p !== id;
  const first = Array.from($(id).querySelectorAll('button')).find((b) => !b.hidden);
  if (first) first.focus();
  wake();
}

function closePanel() {
  for (const p of PANELS) $(p).hidden = true;
  panelUp = false;
  lockVideo(false);
}

// Time's up and a lost session both end this page for good.
function stopWith(title, msg) {
  cut = true;
  $('chunkTitle').textContent = title;
  $('chunkMsg').textContent = msg;
  openPanel('chunk');
}

function cutOff() {
  if (cut) return;
  stopWith('🕐 Time\u2019s up!', 'You used all your minutes for today. Shows with 💛 Free still work. Or ask a grown-up to watch with you.');
}

// The server forgot who is watching (it restarted, or the kid was locked
// out): stop and send them back to pick their name, never play uncharged.
function showSessionLost() {
  if (cut) return;
  stopWith('👋 Who\u2019s watching?', 'Go back to the shelf and pick your name again.');
}

function setNext(btn) {
  btn.hidden = !KS.Next;
  btn.textContent = KS.NextTitle ? '▶ Next: ' + KS.NextTitle : '▶ Next episode';
}

function stopProblemTimers() {
  clearTimeout(noPlayT);
  noPlayT = null;
  clearInterval(playCheckT);
  playCheckT = null;
  blackStreak = 0;
  soundStreak = 0;
}

// The 10 s watchdog from the first play request. Before the first
// 'playing', waiting/stalled re-arm it, so a slow start is never blamed;
// after that it is never armed: a stall mid-video shows "Loading" instead.
// No paused check inside: while play() is still pending the element
// reports paused === false, and that is exactly the never-started state
// this watchdog exists for.
function armNoPlayWatchdog() {
  if (everPlayed) return;
  clearTimeout(noPlayT);
  noPlayT = setTimeout(() => {
    noPlayT = null;
    if (!panelUp && !everPlayed) showCannotPlay();
  }, 10000);
}

function clearNoPlayWatchdog() {
  clearTimeout(noPlayT);
  noPlayT = null;
}

function showPendingPanel() {
  if (cut || !$('pending').hidden) return;
  openPanel('pending');
}

function showCannotPlay() {
  if (panelUp || cut) return;
  if (fix === 'pending') { showPendingPanel(); return; } // 409 race safety net
  $('playIssueTitle').textContent = '🚫 This one won\u2019t play here';
  $('playIssueMsg').textContent = 'Pick another one, or ask a grown-up.';
  setNext($('playIssueNext'));
  $('playIssueRetry').hidden = true;
  openPanel('playIssue');
}

// The file could not be loaded (a drive or NAS that is not plugged in, or
// the server went away). Kid words only; the parent page has the details.
function showUnavailable() {
  if (panelUp || cut) return;
  $('playIssueTitle').textContent = '😕 This one can\u2019t play right now';
  $('playIssueMsg').textContent = 'Ask a grown-up for help, or pick another one.';
  setNext($('playIssueNext'));
  $('playIssueRetry').hidden = false;
  openPanel('playIssue');
}

function showNoSound() {
  if (panelUp || soundWarned || cut) return;
  $('soundTitle').textContent = fix === 'pending' ? '🔇 No sound yet' : '🔇 No sound on this one';
  openPanel('sound');
}

// "Watch without sound": close the panel, resume, and never ask again. The
// meter starts again with the next 'playing' event.
function dismissNoSound() {
  soundWarned = true;
  soundStreak = 0;
  closePanel();
  startPlayback(true);
}

// showEnd reveals the end-of-video panel with a way back, "Watch again",
// and on to the next episode when the show has one.
function showEnd() {
  if (cut) return;
  setNext($('endNext'));
  openPanel('end');
}

function watchAgain() {
  closePanel();
  v.currentTime = 0;
  startPlayback(true);
}

// playCheck runs once a second while playing. Only real playback counts: a
// tick whose clock did not advance is a stall, and stalls reset both streaks
// instead of counting toward a verdict.
function playCheck() {
  if (panelUp || v.paused || v.readyState < 3) { blackStreak = 0; soundStreak = 0; return; }
  const pos = v.currentTime;
  if (pos <= lastCheckPos + 0.05) { blackStreak = 0; soundStreak = 0; return; }
  lastCheckPos = pos;
  blackStreak = pictureOK() ? 0 : blackStreak + 1;
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
  clearInterval(playCheckT);
  blackStreak = 0;
  soundStreak = 0;
  lastCheckPos = v.currentTime;
  playCheckT = setInterval(playCheck, 1000);
}

// ---------------------------------------------------------------------------
// Resume positions (per kid; a grown-up session has none)
// ---------------------------------------------------------------------------

function postPos(pos) {
  fetch('/api/kid/position', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ episode: episode, pos: pos }),
    keepalive: true,
  }).catch(() => {});
}

// savePos keeps the kid's spot. A video that ended or is in its last 5
// seconds is finished: its spot is cleared, so it leaves "Keep watching".
function savePos() {
  if (grownup) return;
  const t = v.currentTime || 0;
  const d = v.duration;
  const done = v.ended || (t > 0 && isFinite(d) && d > 0 && t >= d - 5);
  if (!t && !done) return;
  postPos(done ? 0 : Math.floor(t));
}

function clearPos() {
  if (!grownup) postPos(0);
}

// ---------------------------------------------------------------------------
// Big screen: the stage (video plus panels) goes fullscreen, never the bare
// video. iPhones only fullscreen a video; panels take it out again.
// ---------------------------------------------------------------------------

const fsBtn = $('fsBtn');
let idleT = null;

function toggleFullscreen() {
  if (panelUp) return;
  if (fsElement()) { exitFs(); return; }
  const req = stage.requestFullscreen || stage.webkitRequestFullscreen;
  if (req) {
    const p = req.call(stage);
    if (p && p.catch) p.catch(() => {});
  } else if (v.webkitEnterFullscreen) {
    v.webkitEnterFullscreen();
  }
}

// In big screen the top bar fades after 3 s of playing untouched; any
// touch, mouse move or key brings it back.
function wake() {
  stage.classList.remove('idle');
  clearTimeout(idleT);
  idleT = setTimeout(() => {
    if (fsElement() === stage && !v.paused && !panelUp) stage.classList.add('idle');
  }, 3000);
}

function onFullscreenChange() {
  const fs = fsElement();
  fsBtn.textContent = fs === stage ? '📺 Small screen' : '📺 Big screen';
  // The bare video went fullscreen anyway: swap it for the stage.
  if (fs && fs !== stage) {
    exitFs().then(() => { if (!panelUp) toggleFullscreen(); });
  }
  wake();
}

if (!stage.requestFullscreen && !stage.webkitRequestFullscreen && !v.webkitEnterFullscreen) fsBtn.hidden = true;
fsBtn.addEventListener('click', toggleFullscreen);
document.addEventListener('fullscreenchange', onFullscreenChange);
document.addEventListener('webkitfullscreenchange', onFullscreenChange);
v.addEventListener('webkitbeginfullscreen', () => { if (panelUp) leaveVideoFullscreen(); });
v.addEventListener('dblclick', (e) => { e.preventDefault(); toggleFullscreen(); });
for (const ev of ['pointermove', 'pointerdown', 'keydown']) {
  document.addEventListener(ev, wake, { passive: true });
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function showTap() {
  const tap = $('tap');
  tap.hidden = false;
  tap.focus();
}

// startPlayback asks the video to play. A refusal (the browser wants a tap
// first) shows the big "Tap to play" button; a refusal because a panel
// opened or the file failed shows nothing, the panel has the way back.
function startPlayback(focusVideo) {
  if (panelUp || cut || pendingEpisode) return;
  $('tap').hidden = true;
  if (focusVideo) v.focus();
  armNoPlayWatchdog();
  const p = v.play();
  if (p) {
    p.catch(() => {
      clearNoPlayWatchdog();
      if (panelUp || cut || v.error) return;
      showTap();
    });
  }
}

// Every way out replaces this page in history, so the browser's Back on
// the shelf never lands on (and replays) the video just left. pagehide
// reports the last seconds and the spot.
function goShelf() { location.replace('/'); }
function goNext() { location.replace('/watch/' + encodeURIComponent(KS.Next)); }

$('tap').addEventListener('click', () => startPlayback(true));
$('shelfBtn').addEventListener('click', goShelf);
$('chunkBtn').addEventListener('click', goShelf);
$('endBack').addEventListener('click', goShelf);
$('endNext').addEventListener('click', goNext);
$('endAgain').addEventListener('click', watchAgain);
$('pendingBack').addEventListener('click', goShelf);
$('playIssueBack').addEventListener('click', goShelf);
$('playIssueNext').addEventListener('click', goNext);
$('playIssueRetry').addEventListener('click', () => location.reload());
$('soundBack').addEventListener('click', goShelf);
$('soundMute').addEventListener('click', dismissNoSound);

// Esc on a panel is its way back to the shelf.
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || !panelUp) return;
  const open = PANELS.map($).find((p) => !p.hidden);
  const back = open && open.querySelector('[data-shelf]');
  if (back) { e.preventDefault(); back.click(); }
});

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
  }).then((r) => {
    if (r.status === 403) cutOff();
    else if (r.status === 409) showPendingPanel();
    else if (r.status === 401) showSessionLost();
  }).catch(() => {});
}

// Nothing plays behind a panel: any play (a media key, a script, a click
// that slipped through) is paused again at once.
v.addEventListener('play', () => { if (panelUp || cut) v.pause(); });

// Live metering: the clock runs only while the video is genuinely playing.
v.addEventListener('playing', () => {
  if (panelUp || cut) { v.pause(); return; }
  everPlayed = true;
  reportStarted();
  if (!grownup && !posT) posT = setInterval(savePos, 10000);
  if (!FREE) { freezeStreak = 0; meterStart(); }
  $('tap').hidden = true;
  hideLoading();
  clearNoPlayWatchdog();
  startPlayCheck();
});
// A panel pauses the video; the pause event must not report the seconds
// openPanel just dropped (report itself refuses while a panel is up).
v.addEventListener('pause', () => { hideLoading(); if (!FREE) report(true); meterStop(); savePos(); wake(); });
v.addEventListener('ended', () => { if (!FREE) report(true); meterStop(); clearPos(); showEnd(); });
// Before the first frame, waiting/stalled re-arm the 10 s watchdog. After
// it, a wait shows "Loading" until 'playing' or 'pause'. 'stalled' alone
// does not stop the meter: it can fire while the video still plays from
// its buffer, and report never charges a tick whose clock did not move.
v.addEventListener('waiting', () => {
  meterStop();
  if (!everPlayed) armNoPlayWatchdog();
  else if (!v.paused && !panelUp) $('loading').hidden = false;
});
v.addEventListener('stalled', () => { if (!everPlayed) armNoPlayWatchdog(); });
v.addEventListener('error', () => {
  meterStop();
  stopProblemTimers();
  $('tap').hidden = true;
  // A known-problem original gets an explaining panel. The probe below is
  // for episodes that play everywhere (a drive or NAS went away).
  if (fix === 'pending') showPendingPanel();
  else if (issue) showCannotPlay();
  else showMediaUnavailable();
});

// A failed media load: probe the URL once (one-byte range). 409 is the
// server saying this episode is still getting ready; anything else (503
// for a drive or NAS that is not connected, no network) gets the kid
// "can't play right now" panel. Never loops: the probe is one request.
async function showMediaUnavailable() {
  try {
    const r = await fetch(mediaURL, { headers: { Range: 'bytes=0-0' } });
    if (r.status === 409) { showPendingPanel(); return; }
  } catch (e) { /* no network: same panel */ }
  showUnavailable();
}
v.addEventListener('emptied', () => meterStop());
// Seeking is NOT watching: reset so a seek is never charged, and reset the
// problem streaks so a jump is never counted as a broken second.
v.addEventListener('seeked', () => {
  pendingSec = 0;
  heldSec = 0;
  lastTickPos = v.currentTime;
  freezeStreak = 0;
  lastCheckPos = v.currentTime;
  blackStreak = 0;
  soundStreak = 0;
});

window.addEventListener('pagehide', () => { savePos(); report(true); });
window.addEventListener('beforeunload', () => { savePos(); report(true); meterStop(); });

// main finds out who is watching, then (for a kid on a metered show) the
// balance, and the saved spot for any kid, before asking the video to play:
// at Time's up the video never starts, and a resume never plays from 0 first.
async function main() {
  let r;
  try {
    r = await fetch('/api/kid/me');
  } catch (e) {
    location.replace('/');
    return;
  }
  if (r.status === 401) { location.replace('/'); return; }
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
    startPlayback(false);
    return;
  }
  const waits = [];
  if (FREE) {
    badge('💛 Free');
  } else {
    waits.push(fetch('/api/balance').then(async (rr) => {
      if (rr.status === 401) { showSessionLost(); return; }
      if (!rr.ok) return;
      const d = await rr.json();
      showBalance(d.total);
      if (typeof d.total === 'number' && d.total <= 0.01) cutOff();
    }).catch(() => {}));
  }
  waits.push(fetch('/api/kid/resume/' + encodeURIComponent(episode)).then(async (rr) => {
    if (!rr.ok) return;
    const d = await rr.json();
    const pos = d.pos || 0;
    if (pos > 5 && (!v.duration || pos < v.duration - 5)) v.currentTime = pos;
  }).catch(() => {}));
  await Promise.all(waits);
  startPlayback(false);
}

main();
