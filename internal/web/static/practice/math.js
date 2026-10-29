// Math practice and check-up pages share this script: one page renders in
// two modes via window.CHECKUP (round size, milestone rules, wording).
// The server grades every answer; this page only measures answer time.
// Every answer names its round and the position of the question on screen,
// and the page takes no new answer until the next question shows, so a
// double Enter or double tap can never grade a question the kid has not
// seen.
const kid=window.KID;
const CHECKUP=!!window.CHECKUP;
const MODES=['add','sub','mul','div'];
const LABELS={add:'➕ Addition',sub:'➖ Subtraction',mul:'✖️ Multiplication',div:'➗ Division'};
const SIGNS={add:'➕',sub:'➖',mul:'✖️',div:'➗'};
const BACK=CHECKUP?{label:'← Math',go:()=>{location.href=`/practice/${kid}/math`;}}
  :{label:'← Back',go:()=>{location.href='/practice';}};
let mode=MODES[0],qs=[],round='',idx=0,cur=null,results=[],t0=0,busy=false,play=0;
let openModes=new Set(MODES);

function menuEl(){show('menu');return $('menu');}

async function renderMenu(note){
  play++;cur=null;busy=false;
  const m=menuEl();
  const d=await api(`/status?kid=${kid}`);
  if(!d.ok)return failPanel(m,d,"Can't reach practice right now.",()=>renderMenu(note),BACK);
  openModes=new Set(d.open_modes||MODES);
  const capped=isCapped(d);
  let h=note?`<p class="lead note">${esc(note)}</p>`:'';
  if(CHECKUP)h+=`<p class="lead">A check-up is a quick test to see how you are doing. Do your best!</p>`;
  else h+=`<p class="lead">${capped?'Pick one and beat your records!':'Pick one. Beat your records to earn coins for watching.'}</p>`;
  if(capped)h+=`<p class="lead capped">🎮 ${CAPPED_MSG}</p>`;
  for(const mo of MODES){
    const g=(d.gates||{})[mo];
    if(openModes.has(mo)){
      h+=`<button type="button" class="choice" data-mode="${mo}">${LABELS[mo]}</button>`;
    }else{
      const pct=Math.min(100,Math.round(((g?g.have:0)/((g&&g.need)||1))*100));
      h+=`<div class="choice locked"><span>${LABELS[mo]} 🔒</span>
          <div class="progress"><div class="bar" style="width:${pct}%"></div></div>
          <span class="sub">${esc(g?g.label:'')}</span></div>`;
    }
  }
  const bits=[];
  for(const mo of MODES){
    const p=(d.promo||{})[mo];
    if(p&&openModes.has(mo))bits.push(`${SIGNS[mo]} ${p.mastered} of ${p.total} quick`);
  }
  if(bits.length)h+=`<p class="muted center">${bits.join(' · ')}</p>`;
  if(d.tables_top&&openModes.has('mul'))h+=`<p class="muted center">✖️ Times tables up to ${d.tables_top}</p>`;
  if(!capped)h+=`<p class="muted center">${esc(todayText(d,CAPPED_MSG))}</p>`;
  if(!CHECKUP){
    h+=`<a class="choice" style="border-color:#e08c3f" href="/practice/${kid}/checkup">🩺 Check-up<span class="sub">A quick test of everything</span></a>`;
    if(d.boss)h+=`<a class="choice" style="border-color:#d94b4b;background:#3d1f2a" href="/practice/${kid}/boss">⚔️ Boss: ${d.boss.emoji} ${esc(d.boss.name)}${capped?'':`<span class="sub">Win 🪙${d.boss_coins}!</span>`}</a>`;
    h+=`<a class="choice" style="border-color:#3fae6a" href="/practice/${kid}/spell">🔤 Spelling</a>`;
  }
  m.innerHTML=h;
  m.appendChild(button(BACK.label,'btn gray',BACK.go));
  for(const b of m.querySelectorAll('button[data-mode]'))b.addEventListener('click',()=>pickMode(b.dataset.mode));
  focusKey(m.querySelector('button[data-mode]'));
}

function pickMode(m){mode=m;start();}

function goalLine(d){
  if(CHECKUP)return 'Check-up. Do your best!';
  if(d.best_ms&&d.best_ms[mode]!=null)
    return `🎯 Beat your record: about ${(d.best_ms[mode]/1000).toFixed(1)}s per answer. Get 9 of 10 right.`;
  return '🎯 Get 9 of 10 right to set your first record.';
}

async function refreshJar(){
  const d=await api(`/status?kid=${kid}`);
  if(d.ok){$('jar').textContent=jarText(d,CAPPED_MSG);return d;}
  return null;
}

async function start(){
  const token=++play;
  const m=menuEl();
  m.innerHTML='<p class="lead">Loading…</p>';
  const d=await api(`/round?kid=${kid}&mode=${mode}`+(CHECKUP?'&checkup=1':''));
  if(token!==play)return;
  if(!d.ok){
    if(d.status===403)return renderMenu(`🔒 ${LABELS[mode]} is still locked. Keep practicing the open ones.`);
    return failPanel(m,d,"Couldn't load the questions.",start,{label:'Menu',go:()=>renderMenu()});
  }
  if(!d.questions||!d.questions.length)
    return failPanel(m,d,"Couldn't load the questions.",start,{label:'Menu',go:()=>renderMenu()});
  qs=d.questions;round=d.round;idx=0;results=[];
  show('play');
  $('earned').textContent='';$('jar').textContent='';$('banner').hidden=true;
  $('bar-line').textContent=CHECKUP?goalLine({}):'';
  if(!CHECKUP){
    api(`/status?kid=${kid}`).then(s=>{
      if(!s.ok||token!==play)return;
      $('jar').textContent=jarText(s,CAPPED_MSG);
      $('bar-line').textContent=goalLine(s);
    });
  }
  next();
}

// stopRound leaves the round without finishing it; answers already graded
// stay graded, the rest of the round is just skipped.
function stopRound(){renderMenu();}

function setBusy(b){busy=b;$('ans').disabled=b;$('gobtn').disabled=b;}

function next(){
  if(idx>=qs.length)return finish();
  const q=qs[idx];
  cur={n:idx,q};
  $('q').textContent=`${q.a} ${q.op==='x'?'×':q.op==='/'?'÷':q.op} ${q.b} =`;
  $('ans').value='';$('fb').textContent='';$('fb').className='feedback';
  $('bar').style.width=(100*idx/qs.length)+'%';
  setBusy(false);
  $('ans').focus();t0=Date.now();
}

function submit(){
  if(busy||!cur)return;
  const v=$('ans').value.trim();
  if(v===''){$('ans').focus();return;}
  if(!/^\d+$/.test(v)){
    $('fb').className='feedback no';$('fb').textContent='Type a number.';
    $('ans').focus();return;
  }
  send({n:cur.n,q:cur.q,given:parseInt(v,10),ms:Date.now()-t0});
}

async function send(p){
  const token=play;
  setBusy(true);
  const d=await api('/answer',{kid,mode,round,n:p.n,a:p.q.a,b:p.q.b,op:p.q.op,given:p.given,ms:p.ms});
  if(token!==play)return;
  if(!d.ok){
    if(d.status===409||d.err==='signin'){
      return failPanel(menuEl(),d,"That round is over. Let's start a new one.",start,{label:'Menu',go:()=>renderMenu()},'New round');
    }
    setBusy(false);
    answerFailed(d,()=>send(p));
    return;
  }
  results.push({a:p.q.a,b:p.q.b,op:p.q.op,given:p.given,ms:p.ms,correct:!!d.correct});
  const fb=$('fb');
  if(d.correct){
    fb.className='feedback ok';
    fb.textContent=d.record?`✓ ⚡ ${(p.ms/1000).toFixed(1)}s. Your fastest yet!`:'✓';
  }else{
    fb.className='feedback no';
    fb.textContent=`✗  ${$('q').textContent} ${d.answer}`;
  }
  if(d.coin){
    if(d.coin.banked)banner('🎉 +1 minute of watching earned!');
    else if(d.coin.kind==='coin'){
      $('earned').textContent=`🪙 +1 coin! ${d.coin.coins} of ${d.coin.need}`;
      setTimeout(()=>{if($('earned').textContent.includes('🪙'))$('earned').textContent='';},2500);
    }
  }
  if(!CHECKUP)refreshJar();
  idx=p.n+1;cur=null;
  setTimeout(()=>{if(token===play)next();},d.correct?350:1400);
}

function fmtMed(ms){return ms?(ms/1000).toFixed(1)+'s':'';}

async function finish(){
  const token=play;
  cur=null;
  $('bar').style.width='100%';
  const done=$('done');show('done');
  done.innerHTML='<p class="lead">Saving your round…</p>';
  const d=await api('/round_result',{kid,mode,checkup:CHECKUP,results});
  if(token!==play)return;
  if(!d.ok){
    return failPanel(done,d,"Couldn't save this round. Check the Wi-Fi, then try again.",finish,
      {label:'Menu',go:()=>renderMenu()});
  }
  let h=`<h1>${d.correct} / ${d.n} right</h1>`;
  if(d.n>0&&d.correct===d.n)h+=`<div class="award lg">🌟 ${CHECKUP?'A perfect check-up!':'Every one right! Amazing!'}</div>`;
  if(d.median_ms)h+=`<p class="muted center">About ${fmtMed(d.median_ms)} per answer</p>`;
  for(const mo of MODES){
    const p=(d.promo||{})[mo];
    if(!p||!openModes.has(mo))continue;
    h+=`<div class="progress"><div class="bar" style="width:${p.pct*100}%;background:#3fae6a"></div></div>
        <p class="muted center">${LABELS[mo]}: ${p.mastered} of ${p.total} quick</p>`;
  }
  // THE verdict: record celebration, missed-the-bar or accuracy-fail, always explicit
  for(const a of (d.awards||[])){
    if(a.kind==='tables'&&a.ok){
      h+=`<div class="award lg">🚀 ${esc(a.text)} New tables to practice!</div>`;
    }else if(a.kind!=='record')continue;
    else if(a.ok){
      h+=`<div class="award lg">⚡ NEW RECORD! About ${(a.new_best_ms/1000).toFixed(1)}s per answer${a.was_best_ms!=null?` (it was ${(a.was_best_ms/1000).toFixed(1)}s)`:''}.</div>`;
    }else if(a.miss&&!a.acc_fail&&a.was_best_ms!=null){
      const need=(a.was_best_ms*0.98/1000).toFixed(1);
      h+=`<div class="award blue">🎯 ${(a.this_ms/1000).toFixed(1)}s. Your record to beat: ${need}s. So close!</div>`;
    }else if(a.miss&&a.acc_fail){
      h+=`<div class="award blue">🎯 Get 9 of 10 right to beat your record. This round: ${d.correct} of ${d.n}.</div>`;
    }
  }
  h+=`<p class="lead" id="today"></p>`;
  done.innerHTML=h;
  done.appendChild(button('Another round','btn green',start));
  done.appendChild(button('Menu','btn gray',()=>renderMenu()));
  focusKey(done.querySelector('.btn.green'));
  const s=await api(`/status?kid=${kid}`);
  if(s.ok&&token===play&&$('today'))$('today').textContent=todayText(s,CAPPED_MSG);
}

$('stopbtn').addEventListener('click',stopRound);
$('gobtn').addEventListener('click',submit);
$('ans').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();submit();}});
onEscape(()=>{if(!$('play').hidden||!$('done').hidden)renderMenu();else BACK.go();});
renderMenu();
