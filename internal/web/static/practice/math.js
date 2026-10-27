// Math practice and check-up pages share this script: one page renders in
// two modes via window.CHECKUP (round size, milestone rules, wording).
// The server grades every answer; this page only measures answer time.
const API='/api/practice';
const kid=window.KID;
const CHECKUP=!!window.CHECKUP;
const MODES=['add','sub','mul','div'];
const LABELS={add:'➕ Addition',sub:'➖ Subtraction',mul:'✖️ Multiplication',div:'➗ Division'};
const $=id=>document.getElementById(id);
let mode=MODES[0],qs=[],idx=0,results=[],t0=0;

function renderMenu(){
  $('play').style.display='none';$('done').style.display='none';
  const m=$('menu');m.style.display='block';
  fetch(`${API}/status?kid=${kid}`).then(r=>r.json()).then(d=>{
    let h='';
    if(d.ok){
      const open=new Set(d.open_modes||MODES);
      h=`<div class=muted style="text-align:center">Pick a type. Beat your records and earn coins toward watching minutes</div>
         <div style="display:flex;flex-direction:column;gap:10px;margin-top:14px">`;
      for(const mo of MODES){
        const g=(d.gates||{})[mo];
        if(open.has(mo)){
          h+=`<button class="choice" onclick="pickMode('${mo}')">${LABELS[mo]}</button>`;
        }else{
          const pct=Math.round(((g?g.have:0)/(g?g.need:1))*100);
          h+=`<div class="choice" style="opacity:.55;cursor:default;flex-direction:column">
              <span>${LABELS[mo]} 🔒</span>
              <span class=muted style="font-size:.75em">${g?g.label:''}. ${pct}% there</span></div>`;
        }
      }
      if(d.tables_top)h+=`<div class=muted style="text-align:center;margin-top:4px">✖️ tables: 1 to ${d.tables_top}</div>`;
      const bits=[];
      for(const [m2,p] of Object.entries(d.promo||{})){
        if(open.has(m2))bits.push(`${LABELS[m2]}: ${Math.round(p.pct*100)}% mastered`);
      }
      h+=`</div><div id=menustatus class=muted style="text-align:center;margin-top:8px">${bits.join(' · ')}`
        +(d.earned_today>0?` · minutes earned today: ${Math.floor(d.earned_today)} of ${Math.floor(d.cap)}`:'')+`</div>`;
    }else{
      h=`<div class=muted style="text-align:center">Loading…</div>`;
    }
    if(!CHECKUP) h+=`<button class="choice" style="border-color:#e08c3f" onclick="location='/practice/${kid}/checkup'">🩺 Check-up</button>`;
    if(!CHECKUP&&d.boss) h+=`<button class="choice" style="border-color:#d94b4b;background:#3d1f2a" onclick="location='/practice/${kid}/boss'">⚔️ Boss: ${d.boss.emoji} ${d.boss.name}. Win 🪙${d.boss_coins}!</button>`;
    h+=`<button class="choice" style="background:#1f2c3d;border-color:#39506b;font-size:1em" onclick="location='/practice'">← Who's practicing?</button>`;
    m.innerHTML=h;
  });
}

function pickMode(m){mode=m;start();}

async function loadBar(){
  const d=await fetch(`${API}/status?kid=${kid}`).then(r=>r.json());
  if(d.ok){
    updateJar(d);
    if(d.best_ms&&d.best_ms[mode]!=null){
      $('bar-line').textContent=`🎯 Beat your record. Your usual speed: ${(d.best_ms[mode]/1000).toFixed(1)}s per answer. 9 out of 10 right`;
    } else {
      $('bar-line').textContent=`🎯 A first good round sets your record. Get 9 out of 10 right`;
    }
  }
}

function updateJar(d){
  const j=$('jar');if(!j)return;
  // coins toward the next 🎮 minute: 10 coins = 1 minute banked
  if(d.coins!=null&&d.cap!=null&&d.earned_today!=null){
    const room=Math.max(0, d.cap-d.earned_today);
    if(room<=0){
      j.textContent=`🪙 jar paused. You earned all ${Math.floor(d.cap)} minutes today`;
      if($('bar-line'))$('bar-line').textContent=`🎮 All ${Math.floor(d.cap)} minutes earned for today. Coins come back tomorrow!`;
      return;
    }
    const left=d.coins_per_minute-d.coins;
    j.textContent=`🪙 ${d.coins} of ${d.coins_per_minute} coins. ${left} more coins = 1 more minute of watching`;
  }
}

async function start(){
  const r=await fetch(`${API}/round?kid=${kid}&mode=${mode}`+(CHECKUP?'&checkup=1':''));
  const d=await r.json();
  if(!d.ok){
    if(d.err==='locked'){
      $('earned').textContent=`🔒 ${d.label}. ${Math.round((d.have/(d.need||1))*100)}% there!`;
      setTimeout(()=>{$('earned').textContent='';},4000);
    }else{
      $('fb').textContent='Could not load questions. Try again';
    }
    return renderMenu();
  }
  if(!d.questions||!d.questions.length){
    $('fb').textContent='Could not load questions. Try again';return renderMenu();
  }
  qs=d.questions;idx=0;results=[];
  $('menu').style.display='none';$('play').style.display='block';
  $('done').style.display='none';
  if(CHECKUP){$('earned').textContent='Check-up. Do your best!';}
  else{loadBar();}
  next();
}

// stopRound leaves the round without finishing it; answers already graded
// stay graded, the rest of the round is just skipped.
function stopRound(){renderMenu();}

function next(){
  if(idx>=qs.length)return finish();
  const q=qs[idx];
  $('q').textContent=`${q.a} ${q.op} ${q.b} =`;
  $('ans').value='';$('fb').textContent='';$('fb').className='feedback';
  $('bar').style.width=(100*idx/qs.length)+'%';
  $('ans').focus();t0=Date.now();
}

async function submit(){
  const q=qs[idx];if(!q)return;
  const v=$('ans').value.trim();
  if(v===''){$('ans').focus();return;}
  const given=parseInt(v,10);
  const ms=Date.now()-t0;
  let d={correct:true,answer:given};
  try{
    const r=await fetch(`${API}/answer`,{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({kid,mode,a:q.a,b:q.b,op:q.op,given,ms})});
    d=await r.json();
  }catch(e){}
  results.push({a:q.a,b:q.b,op:q.op,given,ms});
  if(d.correct){
    $('fb').className='feedback ok';
    $('fb').textContent=d.record?`✓ 🏅 ${(ms/1000).toFixed(1)}s. New record!`:'✓';
  }else{
    $('fb').className='feedback no';
    $('fb').textContent=`✗  ${q.a} ${q.op} ${q.b} = ${d.answer}`;
  }
  if(d.coin){
    if(d.coin.banked){
      $('earned').textContent=`🎉 +1 🎮 minute earned!`;
      setTimeout(()=>{if($('earned').textContent.includes('minute earned'))$('earned').textContent='';},3500);
    }else if(d.coin.kind==='capped'||d.coin.err==='daily cap reached'){
      $('earned').textContent=`🎮 You earned all your minutes for today. Records start again tomorrow!`;
    }else if(d.coin.kind==='coin'){
      $('earned').textContent=`🪙 ${d.coin.coins}/${d.coin.need}`;
      setTimeout(()=>{if($('earned').textContent.includes('🪙'))$('earned').textContent='';},2500);
    }
  }
  refreshJar();
  idx++;
  setTimeout(next, d.correct?350:1400);
}

async function refreshJar(){
  try{
    const d=await fetch(`${API}/status?kid=${kid}`).then(r=>r.json());
    if(d.ok)updateJar(d);
  }catch(e){}
}

function fmtMed(ms){return ms?(ms/1000).toFixed(1)+'s':'';}

async function finish(){
  $('play').style.display='none';$('bar').style.width='100%';
  let d={ok:false};
  try{
    d=await fetch(`${API}/round_result`,{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({kid,mode,checkup:CHECKUP,results})}).then(r=>r.json());
  }catch(e){}
  const done=$('done');done.style.display='block';
  if(!d.ok){
    done.innerHTML=`<h1>Round done!</h1><div class=muted>results saved</div>
      <button class="btn green" onclick="start()">Another round</button>
      <button class="btn gray" onclick="renderMenu()">Menu</button>`;
    return;
  }
  let h=`<h1>${d.correct} / ${d.n} correct</h1>
    <div class=muted>your usual speed per answer: ${fmtMed(d.median_ms)||'none yet'}</div>`;
  if(d.promo)for(const [m,p] of Object.entries(d.promo)){
    h+=`<div class=progress style="max-width:400px;margin-top:8px"><div class=bar style="width:${p.pct*100}%;background:#3fae6a"></div></div>
        <div class=muted>${LABELS[m]} mastery: ${p.mastered}/${p.total} (${Math.round(p.pct*100)}%)</div>`;
  }
  // the jar on the round-done screen too
  try{
    const s=await fetch(`${API}/status?kid=${kid}`).then(r=>r.json());
    if(s.ok)updateJar(s);
  }catch(e){}
  // THE verdict: record celebration, missed-the-bar or accuracy-fail, always explicit
  for(const a of (d.awards||[])){
    if(a.kind==='tables'&&a.ok){
      h+=`<div class="award" style="font-size:1.25em;border-color:#3fae6a;background:#3fae6a22">🚀 ${a.text} New territory!</div>`;
    }else if(a.kind!=='record')continue;
    else if(a.ok){
      h+=`<div class="award" style="font-size:1.2em">⚡ NEW RECORD! Your usual speed: ${(a.new_best_ms/1000).toFixed(1)}s per answer${a.was_best_ms!=null?` (was ${(a.was_best_ms/1000).toFixed(1)}s)`:''}. Keep the coins coming!</div>`;
    }else if(a.miss&&!a.acc_fail&&a.was_best_ms!=null){
      const need=(a.was_best_ms*0.98/1000).toFixed(1);
      h+=`<div class=award style="border-color:#4a90d9;background:#4a90d922">🎯 ${(a.this_ms/1000).toFixed(1)}s. The record to beat: ${need}s. So close!</div>`;
    }else if(a.miss&&a.acc_fail){
      h+=`<div class=award style="border-color:#4a90d9;background:#4a90d922">🎯 Get 9 out of 10 right to race the record. This round: ${d.correct} of ${d.n}.</div>`;
    }
  }
  h+=`<div style="margin-top:14px">
      <button class="btn green" onclick="start()">Another round</button>
      <button class="btn gray" onclick="renderMenu()">Menu</button></div>`;
  done.innerHTML=h;
}

$('stopbtn').addEventListener('click',stopRound);
$('gobtn').addEventListener('click',submit);
document.addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();submit();}});
renderMenu();
