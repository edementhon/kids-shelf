// Spelling practice: pick a tier (or the whole bank), hear the word and
// type it back. Audio comes from the pre-generated clip when the server has
// one; the browser's speechSynthesis reads the word when it does not.
const API='/api/practice';
const kid=window.KID;
const HINTS=window.HINTS||{};
let words=[],idx=0,results=[],t0=0,curTier=null;
const $=id=>document.getElementById(id);

function playWord(){
  const w=words[idx];
  const a=new Audio('/practice/audio/'+encodeURIComponent(w)+'.wav');
  a.onerror=function(){speak(w);};
  a.play().catch(function(){speak(w);});
  $('ans').focus();
}
function speak(w){
  try{
    speechSynthesis.cancel();
    const u=new SpeechSynthesisUtterance(w);
    u.lang='en-US';
    speechSynthesis.speak(u);
  }catch(e){}
}
function hintFor(w){return HINTS[w]?HINTS[w].replace('___','… … …'):'';}

async function start(tier){
  const q=tier==null?'':`&tier=${tier}`;
  const r=await fetch(`${API}/spell_round?kid=${kid}${q}`);
  const d=await r.json();
  if(!d.ok||!d.words||!d.words.length){return renderMenu();}
  words=d.words;idx=0;results=[];curTier=tier;
  $('menu').style.display='none';$('play').style.display='block';$('done').style.display='none';
  refreshJar();
  next();
}
function tierBtn(t){
  return `<button class="choice" style="margin:6px auto;flex-direction:column" onclick="start(${t.tier})">
    <span>${t.name}</span>
    <div class="progress" style="width:100%;margin-top:4px"><div class=bar style="width:${t.total?Math.round(t.have/t.total*100):0}%"></div></div>
    <span class=muted style="font-size:.85em">${t.have}/${t.total} words</span></button>`;
}
function renderMenu(){
  $('play').style.display='none';$('done').style.display='none';
  const m=$('menu');m.style.display='block';
  fetch(`${API}/spell_status?kid=${kid}`).then(r=>r.json()).then(d=>{
    let h='';
    if(d.ok){
      const here=(d.tiers||[]).filter(t=>t.have<t.total)[0]||null;
      h=`<div class=muted style="text-align:center">Words with records: ${d.recorded}/${d.total}</div>`
        +`<div class=muted style="text-align:center;margin-top:4px">Spell the word you hear. Fast and right earns 🪙</div>`
        +(here?`<div class=muted style="text-align:center;margin-top:2px">You are here: ${here.name} (${here.have}/${here.total})</div>`:'')
        +`<button class="choice" style="margin:14px auto" onclick="start(null)">🎲 Mix it up. All ${d.total} words</button>`
        +(d.tiers||[]).map(tierBtn).join('')
        +`<div class=muted style="text-align:center;margin-top:8px">🪙 ${d.coins} of ${d.coins_per_minute} coins · today: ${Math.floor(d.earned_today)} of ${Math.floor(d.cap)} minutes</div>`;
    }else{h='<div class=muted style="text-align:center">Loading…</div>';}
    h+=`<button class="choice" style="background:#1f2c3d;border-color:#39506b;font-size:1em" onclick="location='/practice'">← Who's practicing?</button>`;
    m.innerHTML=h;
  });
}
function next(){
  if(idx>=words.length){return finish();}
  const w=words[idx];
  $('bar').style.width=(idx/words.length*100)+'%';
  $('hint').textContent=hintFor(w);
  $('fb').textContent='';$('ans').value='';
  t0=Date.now();
  playWord();
  $('ans').focus();
}
async function submit(){
  if(idx>=words.length)return;
  const given=$('ans').value.trim();
  if(!given)return playWord();
  const ms=Date.now()-t0;
  const w=words[idx];
  const r=await fetch(`${API}/spell_answer`,{method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({kid,given,ms,word:w})});
  const d=await r.json();
  results.push({word:w,given,ms});
  if(d.correct){
    $('fb').className='feedback ok';
    $('fb').textContent=d.record?`✓ 🏅 ${(ms/1000).toFixed(1)}s. New record!`:'✓';
  }else{
    $('fb').className='feedback no';
    $('fb').textContent=`✗  It's spelled: ${d.word}`;
  }
  if(d.coin){
    if(d.coin.banked){
      $('earned').textContent=`🎉 +1 🎮 minute banked!`;
      setTimeout(()=>{if($('earned').textContent.includes('banked'))$('earned').textContent='';},3500);
    }else if(d.coin.kind==='capped'||d.coin.err==='daily cap reached'){
      $('earned').textContent=`🎮 You earned all your spelling minutes for today. Records start again tomorrow!`;
    }else if(d.coin.kind==='coin'){
      $('earned').textContent=`🪙 ${d.coin.coins}/${d.coin.need}`;
      setTimeout(()=>{if($('earned').textContent.includes('🪙'))$('earned').textContent='';},2500);
    }
  }
  refreshJar();
  idx++;
  setTimeout(next, d.correct?400:1800);
}
async function refreshJar(){
  try{const d=await fetch(`${API}/spell_status?kid=${kid}`).then(r=>r.json());
      if(d.ok)updateJar(d);}catch(e){}
}
function updateJar(d){
  const j=$('jar'); if(!j)return;
  if(d.coins!=null&&d.cap!=null&&d.earned_today!=null){
    const room=Math.max(0,d.cap-d.earned_today);
    if(room<=0){j.textContent=`🪙 jar paused. All ${Math.floor(d.cap)} spelling minutes earned today`;return;}
    const left=d.coins_per_minute-d.coins;
    j.textContent=`🪙 ${d.coins} of ${d.coins_per_minute} coins. ${left} more coins = 1 more minute of watching`;
  }
}
async function finish(){
  $('play').style.display='none';$('done').style.display='block';
  const correct=results.filter(r=>r.given.toLowerCase()===r.word.toLowerCase()).length;
  const n=results.length;
  let h=`<h1>${correct} / ${n} spelled right</h1>`;
  const wrong=results.filter(r=>r.given.toLowerCase()!==r.word.toLowerCase());
  if(wrong.length){
    h+=`<div class=card><b>Practice these:</b><ul>${wrong.map(r=>`<li>${r.word}</li>`).join('')}</ul></div>`;
  }
  h+=`<div style="margin-top:14px">
      <button class="btn green" onclick="start(curTier)">More words</button>
      <button class="btn gray" onclick="renderMenu()">Menu</button></div>`;
  $('done').innerHTML=h;
}
$('stopbtn').addEventListener('click',renderMenu);
$('gobtn').addEventListener('click',submit);
document.addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();submit();}});
renderMenu();
