// Spelling practice: pick a tier (or the whole bank), hear the word and
// type it back. Audio comes from the pre-generated clip when the server has
// one; the browser's speech reads the word when it does not, and the page
// says so plainly when there is no sound at all. Every answer names its
// round and the position of the word being played (see math.js).
const kid=window.KID;
const HINTS=window.HINTS||{};
const BACK={label:'← Back',go:()=>{location.href='/practice';}};
let words=[],round='',idx=0,cur=null,results=[],t0=0,curTier=null,busy=false,play=0;

function menuEl(){show('menu');return $('menu');}

function soundBroken(){
  const h=$('hint');
  if(h&&!h.textContent.includes('Sound'))h.textContent="Sound isn't working. Ask a grown-up. "+h.textContent;
}

// playWord plays the clip for the word on screen, falling back to speech
// once (a missing clip fires both onerror and the play() rejection).
function playWord(){
  if(!cur)return;
  const w=cur.word;
  let fell=false;
  const fallback=()=>{if(!fell){fell=true;speak(w);}};
  const a=new Audio('/practice/audio/'+encodeURIComponent(w)+'.wav');
  a.onerror=fallback;
  a.play().catch(fallback);
  $('ans').focus();
}
function speak(w){
  const ss=window.speechSynthesis;
  if(!ss||!window.SpeechSynthesisUtterance){soundBroken();return;}
  try{
    ss.cancel();
    const u=new SpeechSynthesisUtterance(w);
    u.lang='en-US';
    u.onerror=soundBroken;
    if(!ss.getVoices().length)soundBroken();
    ss.speak(u);
  }catch(e){soundBroken();}
}
function hintFor(w){return HINTS[w]?HINTS[w].replace('___','_____'):'';}

async function start(tier){
  const token=++play;
  curTier=tier;
  const m=menuEl();
  m.innerHTML='<p class="lead">Loading…</p>';
  const q=tier==null?'':`&tier=${tier}`;
  const d=await api(`/spell_round?kid=${kid}${q}`);
  if(token!==play)return;
  if(!d.ok||!d.words||!d.words.length)
    return failPanel(m,d,"Couldn't load the words.",()=>start(tier),{label:'Menu',go:()=>renderMenu()});
  words=d.words;round=d.round;idx=0;results=[];
  show('play');
  $('earned').textContent='';$('jar').textContent='';$('banner').hidden=true;
  refreshJar();
  next();
}
function tierBtn(t){
  const pct=t.total?Math.round(t.have/t.total*100):0;
  return `<button type="button" class="choice" data-tier="${t.tier}"><span>${esc(t.name)}</span>
    <div class="progress"><div class="bar" style="width:${pct}%"></div></div>
    <span class="sub">${t.have} of ${t.total} words</span></button>`;
}
async function renderMenu(){
  play++;cur=null;busy=false;
  const m=menuEl();
  const d=await api(`/spell_status?kid=${kid}`);
  if(!d.ok)return failPanel(m,d,"Can't reach practice right now.",renderMenu,BACK);
  const capped=isCapped(d);
  const here=(d.tiers||[]).filter(t=>t.have<t.total)[0]||null;
  let h=`<p class="lead">Hear the word, then type it.${capped?'':' Quick and right earns 🪙'}</p>`
    +`<p class="muted center">Words you know: ${d.recorded} of ${d.total}</p>`
    +(here?`<p class="muted center">You are here: ${esc(here.name)} (${here.have} of ${here.total})</p>`:'')
    +(capped?`<p class="lead capped">🎮 ${SPELL_CAPPED_MSG}</p>`:'')
    +`<button type="button" class="choice" data-tier="">🎲 Mix it up<span class="sub">All ${d.total} words</span></button>`
    +(d.tiers||[]).map(tierBtn).join('')
    +(capped?'':`<p class="muted center">${esc(todayText(d,SPELL_CAPPED_MSG))}</p>`)
    +`<a class="choice" style="border-color:#4a90d9" href="/practice/${kid}/math">🔢 Math</a>`;
  m.innerHTML=h;
  m.appendChild(button(BACK.label,'btn gray',BACK.go));
  for(const b of m.querySelectorAll('button[data-tier]'))
    b.addEventListener('click',()=>start(b.dataset.tier===''?null:Number(b.dataset.tier)));
  focusKey(m.querySelector('button[data-tier]'));
}
function setBusy(b){busy=b;$('ans').disabled=b;$('gobtn').disabled=b;}
function next(){
  if(idx>=words.length){return finish();}
  const w=words[idx];
  cur={n:idx,word:w};
  $('bar').style.width=(idx/words.length*100)+'%';
  $('hint').textContent=hintFor(w);
  $('fb').textContent='';$('fb').className='feedback';$('ans').value='';
  setBusy(false);
  t0=Date.now();
  playWord();
  $('ans').focus();
}
function submit(){
  if(busy||!cur)return;
  const given=$('ans').value.trim();
  if(!given)return playWord();
  send({n:cur.n,word:cur.word,given,ms:Date.now()-t0});
}
async function send(p){
  const token=play;
  setBusy(true);
  const d=await api('/spell_answer',{kid,round,n:p.n,word:p.word,given:p.given,ms:p.ms});
  if(token!==play)return;
  if(!d.ok){
    if(d.status===409||d.err==='signin')
      return failPanel(menuEl(),d,"That round is over. Let's start a new one.",()=>start(curTier),{label:'Menu',go:()=>renderMenu()},'New round');
    setBusy(false);
    answerFailed(d,()=>send(p));
    return;
  }
  results.push({word:p.word,given:p.given,correct:!!d.correct});
  const fb=$('fb');
  if(d.correct){
    fb.className='feedback ok';
    fb.textContent=d.record?`✓ ⚡ ${(p.ms/1000).toFixed(1)}s. Your fastest yet!`:'✓';
  }else{
    fb.className='feedback no';
    fb.textContent=`✗  It's spelled: ${d.word}`;
  }
  if(d.coin){
    if(d.coin.banked)banner('🎉 +1 minute of watching earned!');
    else if(d.coin.kind==='coin'){
      $('earned').textContent=`🪙 +1 coin! ${d.coin.coins} of ${d.coin.need}`;
      setTimeout(()=>{if($('earned').textContent.includes('🪙'))$('earned').textContent='';},2500);
    }
  }
  refreshJar();
  idx=p.n+1;cur=null;
  setTimeout(()=>{if(token===play)next();},d.correct?400:1800);
}
async function refreshJar(){
  const d=await api(`/spell_status?kid=${kid}`);
  if(d.ok)$('jar').textContent=jarText(d,SPELL_CAPPED_MSG);
}
async function finish(){
  const token=play;
  cur=null;
  const done=$('done');show('done');
  const correct=results.filter(r=>r.correct).length;
  let h=`<h1>${correct} / ${results.length} spelled right</h1>`;
  if(results.length&&correct===results.length)h+=`<div class="award lg">🌟 Every word right! Amazing!</div>`;
  const wrong=results.filter(r=>!r.correct);
  if(wrong.length){
    h+=`<div class="card"><b>Practice these:</b><ul>${wrong.map(r=>`<li>${esc(r.word)}</li>`).join('')}</ul></div>`;
  }
  h+=`<p class="lead" id="today"></p>`;
  done.innerHTML=h;
  done.appendChild(button('More words','btn green',()=>start(curTier)));
  done.appendChild(button('Menu','btn gray',()=>renderMenu()));
  focusKey(done.querySelector('.btn.green'));
  const s=await api(`/spell_status?kid=${kid}`);
  if(s.ok&&token===play&&$('today'))$('today').textContent=todayText(s,SPELL_CAPPED_MSG);
}
$('stopbtn').addEventListener('click',()=>renderMenu());
$('gobtn').addEventListener('click',submit);
$('playbtn').addEventListener('click',playWord);
$('ans').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();submit();}});
onEscape(()=>{if(!$('play').hidden||!$('done').hidden)renderMenu();else BACK.go();});
// Chromium fills the voice list late; asking early makes it ready by the
// time a missing clip needs it.
if(window.speechSynthesis)speechSynthesis.getVoices();
renderMenu();
