// Shared helpers for the practice pages: one JSON call that never throws,
// a plain failure panel with Try again and a way back, and the daily-cap
// words every page uses. Loaded before each page script.
const API='/api/practice';
const $=id=>document.getElementById(id);
const CAPPED_MSG='All your minutes for today are earned. Come back tomorrow for more!';
const SPELL_CAPPED_MSG='All your spelling minutes for today are earned. Come back tomorrow for more!';

function esc(s){const d=document.createElement('div');d.textContent=String(s);return d.innerHTML;}

// api calls /api/practice<path> (POST when body is given) and resolves to
// the JSON reply when it says ok. Anything else resolves to
// {ok:false,status,err,error}: status 0 means the server could not be
// reached at all.
async function api(path,body){
  let r;
  try{
    r=await fetch(API+path,body===undefined?{}:{method:'POST',
      headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  }catch(e){return {ok:false,status:0,err:'offline',error:''};}
  let d=null;
  try{d=await r.json();}catch(e){}
  if(r.ok&&d&&d.ok)return d;
  return {ok:false,status:r.status,err:(d&&d.err)||'',error:(d&&d.error)||''};
}

// show and hide the page sections by id.
function show(id){for(const s of ['menu','play','done']){const el=$(s);if(el)el.hidden=s!==id;}}

function button(label,cls,onClick){
  const b=document.createElement('button');
  b.type='button';b.className=cls;b.textContent=label;
  b.addEventListener('click',onClick);
  return b;
}

// failPanel replaces el's content with a plain message, a Try again button
// (when retry is given; retryLabel renames it) and a way back. A lost
// sign-in sends the kid to the PIN screen instead.
function failPanel(el,d,msg,retry,back,retryLabel){
  el.textContent='';
  const p=document.createElement('p');p.className='lead';
  el.appendChild(p);
  if(d&&d.err==='signin'){
    p.textContent='Please type your PIN again.';
    el.appendChild(button('Go to sign in','btn green',()=>{location.href='/practice';}));
    focusKey(el.querySelector('button'));
    return;
  }
  p.textContent=msg;
  if(retry)el.appendChild(button(retryLabel||'Try again','btn green',retry));
  if(back)el.appendChild(button(back.label,'btn gray',back.go));
  focusKey(el.querySelector('button'));
}

// answerFailed explains a failed answer in the feedback line and offers
// Try again; the question and the typed answer stay on screen.
function answerFailed(d,retry){
  const fb=$('fb');fb.className='feedback no';fb.textContent='';
  const msg=d.status===0?"Couldn't reach practice. Check the Wi-Fi, then try again."
    :"Couldn't check that one.";
  fb.appendChild(document.createTextNode(msg+' '));
  fb.appendChild(button('Try again','btn small',retry));
}

// banner shows a big, short-lived celebration (a minute earned).
let bannerTimer=null;
function banner(text){
  const b=$('banner');if(!b)return;
  b.textContent=text;b.hidden=false;
  clearTimeout(bannerTimer);
  bannerTimer=setTimeout(()=>{b.hidden=true;},6000);
}

function isCapped(d){return d&&d.cap!=null&&d.earned_today!=null&&d.earned_today>=d.cap-0.001;}
function minutesWord(n){n=Math.max(0,Math.floor(n));return n===1?'1 minute':n+' minutes';}

// jarText is the coins line: coins toward the next minute, or the one
// capped message.
function jarText(d,cappedMsg){
  if(!d||d.coins==null)return '';
  if(isCapped(d))return '🎮 '+cappedMsg;
  const left=Math.max(0,d.coins_per_minute-d.coins);
  return `🪙 ${d.coins} of ${d.coins_per_minute} coins. ${left} more for 1 more minute of watching`;
}

// todayText is the done-screen summary of today's earning.
function todayText(d,cappedMsg){
  if(!d||d.cap==null)return '';
  if(isCapped(d))return '🎮 '+cappedMsg;
  return `🎮 Earned today: ${Math.floor(d.earned_today)} of ${minutesWord(d.cap)} · 🪙 ${d.coins} of ${d.coins_per_minute} coins`;
}

// focusKey puts the keyboard focus on el, so Enter works at once on the
// Pi's keyboard. Touch screens skip it: a ring there looks like a choice
// already made.
function focusKey(el){if(el&&!matchMedia('(pointer: coarse)').matches)el.focus();}

// onEscape runs fn when Esc is pressed anywhere on the page.
function onEscape(fn){
  document.addEventListener('keydown',e=>{if(e.key==='Escape'){e.preventDefault();fn();}});
}
