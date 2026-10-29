// Boss arena: 13 sequential fights, 8 hearts each, 30 seconds per question.
// Art is inline SVG (the legacy boss_page.py sprites, verbatim) so the page
// never loads anything from the internet. A wrong answer heals the boss;
// lethal bosses whack the player's hearts per miss. Every answer names the
// fight and the position of the question on screen, and the page takes no
// new answer until the next question shows (see math.js).
const kid=window.KID;
const BACK={label:'← Math',go:()=>{location.href=`/practice/${kid}/math`;}};
let qs=[],round='',idx=0,cur=null,t0=0,hp=8,hearts=8,boss=null,rewardCoins=3,php=0,phearts=0,lethal=false;
let fightHits=0,fightMisses=[],busy=false,play=0,capped=false,lastMsg=null;

const BOSS_ANIM_CSS='@keyframes bossIdle{0%,100%{transform:translateY(0)}50%{transform:translateY(-8px)}}'
  +'@keyframes bossHit{0%{transform:translate(0,0)}30%{transform:translate(-10px,4px) rotate(-6deg)}100%{transform:translate(0,0)}}'
  +'@keyframes bossDefeat{0%{transform:rotate(0)}100%{transform:rotate(75deg) translateY(40px);opacity:.25}}'
  +'@keyframes fishWhack{0%{transform:rotate(0)}40%{transform:rotate(-18deg) translate(6px,-6px)}70%{transform:rotate(10deg)}100%{transform:rotate(0)}}';
const ANIM={idle:'bossIdle 2.2s ease-in-out infinite',hit:'bossHit .35s ease',
  defeat:'bossDefeat .9s ease forwards',whack:'fishWhack .4s ease'};
function bossSVG(id,state){
  return '<svg viewBox="0 0 100 100" width="180" height="180" xmlns="http://www.w3.org/2000/svg">'
    +'<style>'+BOSS_ANIM_CSS+'</style>'
    +'<g style="animation:'+(ANIM[state]||ANIM.idle)+';transform-origin:center">'+BOSS_ART[id]+'</g></svg>';
}

const BOSS_ART={
adder:`<rect x="20" y="18" width="60" height="60" rx="12" fill="#5b8dd9" stroke="#2c4a7c" stroke-width="4"/><rect x="32" y="30" width="14" height="14" rx="3" fill="#fff"/><rect x="54" y="30" width="14" height="14" rx="3" fill="#fff"/><rect x="37" y="35" width="4" height="4" fill="#222"/><rect x="59" y="35" width="4" height="4" fill="#222"/><rect x="46" y="52" width="8" height="20" rx="2" fill="#fff"/><rect x="40" y="58" width="20" height="8" rx="2" fill="#fff"/><rect x="10" y="40" width="10" height="26" rx="3" fill="#3a5f9e"/><rect x="80" y="40" width="10" height="26" rx="3" fill="#3a5f9e"/><rect x="40" y="6" width="20" height="10" rx="3" fill="#3a5f9e"/>`,
minustaur:`<ellipse cx="50" cy="55" rx="32" ry="26" fill="#a0522d" stroke="#5c2e0e" stroke-width="4"/><circle cx="38" cy="45" r="5" fill="#fff"/><circle cx="62" cy="45" r="5" fill="#fff"/><circle cx="38" cy="45" r="2" fill="#222"/><circle cx="62" cy="45" r="2" fill="#222"/><ellipse cx="50" cy="62" rx="12" ry="8" fill="#d9a066"/><circle cx="46" cy="61" r="2" fill="#5c2e0e"/><circle cx="54" cy="61" r="2" fill="#5c2e0e"/><path d="M18 35 Q10 20 22 14 Q20 26 28 32 Z" fill="#e8dcc8" stroke="#5c2e0e" stroke-width="3"/><path d="M82 35 Q90 20 78 14 Q80 26 72 32 Z" fill="#e8dcc8" stroke="#5c2e0e" stroke-width="3"/><path d="M30 82 Q50 92 70 82" stroke="#5c2e0e" stroke-width="4" fill="none"/>`,
times3:`<ellipse cx="50" cy="58" rx="30" ry="22" fill="#3fae6a" stroke="#1e6e3c" stroke-width="4"/><circle cx="36" cy="46" r="6" fill="#fff"/><circle cx="60" cy="46" r="6" fill="#fff"/><circle cx="36" cy="46" r="2.5" fill="#222"/><circle cx="60" cy="46" r="2.5" fill="#222"/><path d="M28 30 Q34 14 44 24 Q38 30 36 38 Z" fill="#2c8a4d" stroke="#1e6e3c" stroke-width="3"/><path d="M72 30 Q66 14 56 24 Q62 30 64 38 Z" fill="#2c8a4d" stroke="#1e6e3c" stroke-width="3"/><path d="M20 62 Q6 66 12 78 Q20 72 26 70 Z" fill="#2c8a4d" stroke="#1e6e3c" stroke-width="3"/><path d="M80 62 Q94 66 88 78 Q80 72 74 70 Z" fill="#2c8a4d" stroke="#1e6e3c" stroke-width="3"/><path d="M40 68 Q50 76 60 68 L56 74 Q50 78 44 74 Z" fill="#ffd76e" stroke="#1e6e3c" stroke-width="2"/>`,
golem:`<ellipse cx="50" cy="58" rx="30" ry="26" fill="#8d8d8d" stroke="#4d4d4d" stroke-width="4"/><polygon points="24,44 34,32 48,38 42,50" fill="#a3a3a3" stroke="#4d4d4d" stroke-width="3"/><polygon points="58,34 74,38 70,52 56,48" fill="#a3a3a3" stroke="#4d4d4d" stroke-width="3"/><circle cx="40" cy="48" r="5" fill="#ffd76e"/><circle cx="62" cy="48" r="5" fill="#ffd76e"/><circle cx="40" cy="48" r="2" fill="#222"/><circle cx="62" cy="48" r="2" fill="#222"/><path d="M38 68 Q50 74 62 68" stroke="#4d4d4d" stroke-width="4" fill="none" stroke-linecap="round"/><rect x="8" y="52" width="14" height="20" rx="4" fill="#6f6f6f" stroke="#4d4d4d" stroke-width="3"/><rect x="78" y="52" width="14" height="20" rx="4" fill="#6f6f6f" stroke="#4d4d4d" stroke-width="3"/><polygon points="44,12 56,12 62,22 50,28 38,22" fill="#6f6f6f" stroke="#4d4d4d" stroke-width="3"/>`,
diver:`<ellipse cx="50" cy="48" rx="26" ry="24" fill="#8e6bd9" stroke="#4b3a8c" stroke-width="4"/><circle cx="40" cy="42" r="6" fill="#fff"/><circle cx="60" cy="42" r="6" fill="#fff"/><circle cx="40" cy="42" r="2.5" fill="#222"/><circle cx="60" cy="42" r="2.5" fill="#222"/><path d="M30 68 Q26 84 18 86" stroke="#4b3a8c" stroke-width="6" fill="none" stroke-linecap="round"/><path d="M40 72 Q40 88 32 90" stroke="#4b3a8c" stroke-width="6" fill="none" stroke-linecap="round"/><path d="M60 72 Q60 88 68 90" stroke="#4b3a8c" stroke-width="6" fill="none" stroke-linecap="round"/><path d="M70 68 Q74 84 82 86" stroke="#4b3a8c" stroke-width="6" fill="none" stroke-linecap="round"/><circle cx="50" cy="30" r="4" fill="#ffd76e"/>`,
buzzle:`<ellipse cx="50" cy="56" rx="26" ry="22" fill="#ffd23f" stroke="#8a6d1a" stroke-width="4"/><path d="M36 36 Q36 76 36 78" stroke="#8a6d1a" stroke-width="7"/><path d="M50 34 Q50 78 50 78" stroke="#8a6d1a" stroke-width="7"/><path d="M64 36 Q64 76 64 78" stroke="#8a6d1a" stroke-width="7"/><ellipse cx="30" cy="30" rx="16" ry="9" fill="#cfefff" opacity=".8" stroke="#8ab8d9" stroke-width="2" transform="rotate(-25 30 30)"/><ellipse cx="70" cy="30" rx="16" ry="9" fill="#cfefff" opacity=".8" stroke="#8ab8d9" stroke-width="2" transform="rotate(25 70 30)"/><circle cx="40" cy="46" r="6" fill="#fff"/><circle cx="62" cy="46" r="6" fill="#fff"/><circle cx="40" cy="46" r="2.5" fill="#222"/><circle cx="62" cy="46" r="2.5" fill="#222"/><path d="M46 20 L42 8 M54 20 L58 8" stroke="#8a6d1a" stroke-width="3" stroke-linecap="round"/><circle cx="42" cy="7" r="2.5" fill="#8a6d1a"/><circle cx="58" cy="7" r="2.5" fill="#8a6d1a"/><path d="M76 60 Q88 62 84 70" stroke="#222" stroke-width="3" fill="none" stroke-linecap="round"/>`,
times7:`<ellipse cx="52" cy="58" rx="28" ry="22" fill="#f5f0fa" stroke="#9b7fc7" stroke-width="4"/><circle cx="40" cy="48" r="5.5" fill="#fff"/><circle cx="60" cy="48" r="5.5" fill="#fff"/><circle cx="40" cy="48" r="2.2" fill="#5b3a9e"/><circle cx="60" cy="48" r="2.2" fill="#5b3a9e"/><path d="M50 22 L56 6 L58 24 Z" fill="#ffd76e" stroke="#c9a43a" stroke-width="3"/><path d="M24 40 Q14 30 20 22 Q26 32 32 36 Z" fill="#f5c6e8" stroke="#9b7fc7" stroke-width="3"/><path d="M78 44 Q92 40 90 52 Q80 50 76 50 Z" fill="#f5c6e8" stroke="#9b7fc7" stroke-width="3"/><path d="M36 30 Q44 18 56 26 Q48 32 46 38 Z" fill="#f5c6e8" stroke="#9b7fc7" stroke-width="3"/>`,
alienfish:`<ellipse cx="46" cy="52" rx="28" ry="20" fill="#5ec98a" stroke="#1e6e3c" stroke-width="4"/><path d="M72 52 L94 38 L90 52 L94 66 Z" fill="#3fae6a" stroke="#1e6e3c" stroke-width="3"/><ellipse cx="40" cy="42" rx="12" ry="9" fill="#cfefff" opacity=".85" stroke="#1e6e3c" stroke-width="2"/><ellipse cx="58" cy="42" rx="12" ry="9" fill="#cfefff" opacity=".85" stroke="#1e6e3c" stroke-width="2"/><circle cx="40" cy="42" r="4" fill="#222"/><circle cx="58" cy="42" r="4" fill="#222"/><path d="M34 30 Q46 22 58 30" stroke="#1e6e3c" stroke-width="3" fill="none"/><path d="M28 62 Q40 68 52 62" stroke="#1e6e3c" stroke-width="3" fill="none" stroke-linecap="round"/><circle cx="46" cy="30" r="3" fill="#ff6b6b"/>`,
ogre:`<ellipse cx="46" cy="58" rx="30" ry="24" fill="#c0392b" stroke="#6e1f14" stroke-width="4"/><circle cx="36" cy="48" r="6" fill="#fff"/><circle cx="58" cy="48" r="6" fill="#fff"/><circle cx="36" cy="48" r="2.5" fill="#222"/><circle cx="58" cy="48" r="2.5" fill="#222"/><path d="M36 64 Q46 72 58 64 L54 70 Q46 74 40 70 Z" fill="#fff" stroke="#6e1f14" stroke-width="2"/><path d="M30 30 L34 18 L40 30 M52 30 L56 18 L62 30" stroke="#6e1f14" stroke-width="4" fill="none"/><rect x="76" y="40" width="7" height="34" rx="3" fill="#8a5a2b" stroke="#5c3a17" stroke-width="2"/><circle cx="80" cy="34" r="11" fill="#a3773f" stroke="#5c3a17" stroke-width="3"/><circle cx="76" cy="30" r="2" fill="#5c3a17"/><circle cx="84" cy="30" r="2" fill="#5c3a17"/><circle cx="80" cy="38" r="2" fill="#5c3a17"/>`,
tofu:`<rect x="22" y="26" width="56" height="52" rx="10" fill="#fdfbf3" stroke="#c9c2a6" stroke-width="4"/><rect x="30" y="34" width="40" height="36" rx="7" fill="#f5f1df" stroke="#dcd5bb" stroke-width="2"/><circle cx="40" cy="48" r="4" fill="#333"/><circle cx="60" cy="48" r="4" fill="#333"/><circle cx="41.5" cy="46.5" r="1.2" fill="#fff"/><circle cx="61.5" cy="46.5" r="1.2" fill="#fff"/><path d="M42 62 Q50 66 58 62" stroke="#333" stroke-width="3" fill="none" stroke-linecap="round"/><circle cx="34" cy="30" r="2.5" fill="#e8e2c8"/><circle cx="66" cy="34" r="2" fill="#e8e2c8"/><ellipse cx="50" cy="82" rx="24" ry="4" fill="#000" opacity=".12"/>`,
cavebeast:`<ellipse cx="50" cy="56" rx="30" ry="24" fill="#6b4e9e" stroke="#3a2a5e" stroke-width="4"/><circle cx="38" cy="34" r="12" fill="#6b4e9e" stroke="#3a2a5e" stroke-width="4"/><circle cx="62" cy="34" r="12" fill="#6b4e9e" stroke="#3a2a5e" stroke-width="4"/><circle cx="36" cy="32" r="3.5" fill="#ffd76e"/><circle cx="64" cy="32" r="3.5" fill="#ffd76e"/><circle cx="36" cy="32" r="1.5" fill="#222"/><circle cx="64" cy="32" r="1.5" fill="#222"/><path d="M40 56 L44 62 L48 56 L52 62 L56 56 L60 62 L64 56" fill="#fff" stroke="#3a2a5e" stroke-width="2"/><path d="M30 30 L26 22 L34 26 M70 30 L74 22 L66 26" stroke="#3a2a5e" stroke-width="3" fill="none"/><path d="M18 52 L8 44 M18 60 L6 60 M18 68 L8 74" stroke="#3a2a5e" stroke-width="4" stroke-linecap="round"/><path d="M82 52 L92 44 M82 60 L94 60 M82 68 L92 74" stroke="#3a2a5e" stroke-width="4" stroke-linecap="round"/>`,
mixed:`<circle cx="50" cy="50" r="26" fill="#7a4bd9" stroke="#3d2380" stroke-width="4"/><path d="M28 34 L18 22 L36 26 Z" fill="#3d2380"/><path d="M72 34 L82 22 L64 26 Z" fill="#3d2380"/><path d="M30 62 L18 72 L34 70 Z" fill="#3d2380"/><path d="M70 62 L82 72 L66 70 Z" fill="#3d2380"/><circle cx="40" cy="44" r="7" fill="#ffd76e"/><circle cx="60" cy="44" r="7" fill="#ffd76e"/><circle cx="40" cy="44" r="3" fill="#222"/><circle cx="60" cy="44" r="3" fill="#222"/><path d="M38 62 Q50 70 62 62" stroke="#222" stroke-width="4" fill="none" stroke-linecap="round"/>`,
wyrm:`<path d="M14 78 Q30 70 42 58 Q54 46 68 44 Q84 42 88 30" stroke="#2d1b4e" stroke-width="16" fill="none" stroke-linecap="round"/><path d="M14 78 Q30 70 42 58 Q54 46 68 44 Q84 42 88 30" stroke="#5e3a9e" stroke-width="9" fill="none" stroke-linecap="round"/><circle cx="86" cy="26" r="10" fill="#2d1b4e" stroke="#5e3a9e" stroke-width="3"/><circle cx="84" cy="24" r="3" fill="#ffd76e"/><circle cx="90" cy="26" r="3" fill="#ffd76e"/><circle cx="84" cy="24" r="1.2" fill="#222"/><circle cx="90" cy="26" r="1.2" fill="#222"/><path d="M90 32 L96 36 L88 38 Z" fill="#ff6b6b"/><path d="M24 74 L14 70 L22 66 Z" fill="#5e3a9e"/><path d="M36 64 L28 58 L36 56 Z" fill="#5e3a9e"/><path d="M52 52 L46 44 L54 44 Z" fill="#5e3a9e"/><path d="M70 48 L66 40 L74 42 Z" fill="#5e3a9e"/>`
};

function heartsHtml(){
  let h='';
  for(let i=0;i<hearts;i++){
    h+=`<span class="heart ${i<hp?'':'empty'}">♥</span>`;
  }
  return `<div aria-label="${esc(boss?boss.name:'boss')}: ${hp} of ${hearts} hearts">${h}</div>`;
}
function playerHeartsHtml(){
  if(!lethal)return '';
  let h='';
  for(let i=0;i<phearts;i++){
    h+=`<span class="pheart ${i<php?'':'empty'}">🧡</span>`;
  }
  return `<div class="you" aria-label="You: ${php} of ${phearts} hearts">You: ${h}</div>`;
}
function menuEl(){show('menu');return $('menu');}
function qText(q){return `${q.a} ${q.op==='x'?'×':q.op==='/'?'÷':q.op} ${q.b}`;}

async function loadStatus(note){
  play++;cur=null;busy=false;clearInterval(timerId);
  const m=menuEl();
  const d=await api(`/boss_status?kid=${kid}`);
  if(!d.ok)return failPanel(m,d,"Can't reach the arena right now.",()=>loadStatus(note),BACK);
  if(!d.boss){renderDefeatedAll(d);return;}
  boss=d.boss;hearts=d.hearts;hp=d.hearts;rewardCoins=d.reward_coins||3;
  lethal=!!boss.lethal;
  phearts=boss.player_hearts||3;
  php=d.php!=null?d.php:phearts;
  capped=isCapped(d);
  renderIntro(d,note);
}
function renderIntro(d,note){
  const m=menuEl();
  let def='';
  if(d.defeated&&d.defeated.length)def=`<p class="muted center">Bosses beaten: ${d.defeated.length} 🏆</p>`;
  m.innerHTML=`${note?`<p class="lead note">${esc(note)}</p>`:''}
    <div class="center">${bossSVG(boss.id,'idle')}</div>
    <h2 class="center">${boss.emoji} ${esc(boss.name)}</h2>
    <p class="lead">${esc(boss.desc)}</p>
    <p class="lead">Get all ${hearts} right to win!${capped?'':` Win = 🪙${rewardCoins}`}</p>
    <div class="center" style="font-size:1.5em;margin:10px 0">${heartsHtml()}${playerHeartsHtml()}</div>
    <p class="muted center">${esc(capped?'🎮 '+CAPPED_MSG:jarText(d,CAPPED_MSG))}</p>
    ${def}`;
  const fight=button('⚔️ Fight!','btn red lg',startFight);
  m.appendChild(fight);
  m.appendChild(button(BACK.label,'btn gray',BACK.go));
  focusKey(fight);
}
async function startFight(){
  const token=++play;
  const m=menuEl();
  m.innerHTML='<p class="lead">Loading…</p>';
  const d=await api(`/boss_round?kid=${kid}`);
  if(token!==play)return;
  if(!d.ok){
    if(d.status===404)return loadStatus();
    return failPanel(m,d,"Couldn't start the fight.",startFight,{label:'Menu',go:()=>loadStatus()});
  }
  qs=d.questions;round=d.round;idx=0;hp=d.hearts;hearts=d.hearts;
  boss=d.boss||boss;
  fightHits=0;fightMisses=[];lastMsg=null;
  lethal=!!(boss&&boss.lethal);
  phearts=boss&&boss.player_hearts?boss.player_hearts:3;
  php=d.php!=null&&lethal?d.php:phearts;
  show('play');
  $('art').innerHTML=bossSVG(boss.id,'idle');
  updateHearts();
  $('jar').textContent=capped?'🎮 Coins are full for today. Win for the trophy!':`🪙 Win = +${rewardCoins} coins`;
  nextQ();
}
function setBusy(b){busy=b;$('ans').disabled=b;$('gobtn').disabled=b;}
function nextQ(){
  if(idx>=qs.length){return fightDone();}
  const q=qs[idx];
  cur={n:idx,q};
  $('q').textContent=`${qText(q)} = ?`;
  $('ans').value='';$('fb').textContent='';$('fb').className='feedback';
  setBusy(false);
  t0=Date.now();
  startTimer();
  $('ans').focus();
}
const BOSS_T=30; // seconds per question
let timerId=null;
function startTimer(){
  clearInterval(timerId);
  const end=t0+BOSS_T*1000;
  const tick=()=>{
    const left=Math.max(0,end-Date.now());
    const frac=left/(BOSS_T*1000);
    const bar=$('tbar');
    bar.style.width=(frac*100)+'%';
    bar.style.background=frac>.5?'#4ba3e0':(frac>.2?'#e0a34b':'#e04b4b');
    $('ttxt').textContent=left>0?`⏱ ${Math.ceil(left/1000)}s`:'';
    if(left<=0){
      clearInterval(timerId);
      if(busy||!cur)return;
      $('ans').value='';
      send({n:cur.n,q:cur.q,timedOut:true}); // timeout: boss strikes
    }
  };
  tick();timerId=setInterval(tick,100);
}
function updateHearts(){
  const h=$('hearts');
  const prev=h.querySelectorAll('.heart:not(.empty)').length;
  h.innerHTML=heartsHtml()+playerHeartsHtml();
  if(hp<prev){ // a heart just drained: pop it for visibility
    const el=h.querySelectorAll('.heart')[hp];
    if(el)el.classList.add('pop');
  }
}
function showBoss(state){
  $('art').innerHTML=bossSVG(boss.id,state);
}
function submit(){
  if(busy||!cur)return;
  const v=$('ans').value.trim();
  if(v===''){$('ans').focus();return;}
  if(!/^\d+$/.test(v)){
    $('fb').className='feedback no';$('fb').textContent='Type a number.';
    $('ans').focus();return;
  }
  clearInterval(timerId);
  send({n:cur.n,q:cur.q,given:parseInt(v,10),timedOut:false});
}
async function send(p){
  const token=play;
  setBusy(true);
  clearInterval(timerId);
  const d=await api('/boss_answer',{kid,round,n:p.n,a:p.q.a,b:p.q.b,op:p.q.op,
    given:p.timedOut?-1:p.given});
  if(token!==play)return;
  if(!d.ok){
    if(d.err==='boss_changed')return loadStatus("That boss is gone. Let's go to the next one.");
    if(d.status===409||d.err==='signin')
      return failPanel(menuEl(),d,"That fight is over. Let's start a new one.",startFight,{label:'Menu',go:()=>loadStatus()},'⚔️ New fight');
    setBusy(false);
    answerFailed(d,()=>send(p));
    return;
  }
  const msg=d.msg||{};
  lastMsg=msg;
  const fb=$('fb');
  if(d.correct){
    fb.className='feedback ok';fb.textContent='💥 Hit!';
  }else if(p.timedOut){
    fb.className='feedback no';
    fb.textContent=`⏱ Too slow! ${boss.name} strikes. The answer was ${d.answer}`;
  }else{
    fb.className='feedback no';
    fb.textContent=lethal?`💥 WHACK! The answer was ${d.answer}`:`✗ The answer was ${d.answer}`;
  }
  // a knock-out reports the hearts the next fight starts with; show the
  // rows as they were when the kid ran out instead
  if(msg.killed)php=0;else{hp=msg.hp;if(msg.php!=null)php=msg.php;}
  updateHearts();
  showBoss(msg.victory?'defeat':d.correct?'hit':(lethal?'whack':'idle'));
  if(d.correct)fightHits++;
  else fightMisses.push({q:qText(p.q),a:d.answer});
  idx=p.n+1;cur=null;
  if(msg.killed){setTimeout(()=>{if(token===play)killedScreen();},900);return;}
  if(msg.victory){setTimeout(()=>{if(token===play)fightDone();},900);return;}
  // more hearts left than questions: the fight can't be won any more, so
  // end it now instead of playing on (and, on a lethal boss, losing hearts)
  if(hp>qs.length-idx){setTimeout(()=>{if(token===play)fightDone();},1400);return;}
  setTimeout(()=>{if(token===play)nextQ();},d.correct?450:1400);
}
function killedScreen(){
  play++;cur=null;
  const done=$('done');show('done');
  done.innerHTML=`<div class="center">${bossSVG(boss.id,'idle')}</div>
    <h1>${boss.emoji} ${esc(boss.name)} won this time!</h1>
    <p class="lead">${phearts} misses and you're out.</p>
    <p class="lead">Practice up and try again. It will still be here.</p>`;
  done.appendChild(button('⚔️ Fight again','btn red',startFight));
  done.appendChild(button('Menu','btn gray',()=>loadStatus()));
  focusKey(done.querySelector('.btn.red'));
}
function rewardText(r){
  if(!r||r.kind==='capped')return 'You won! Coins are full for today.';
  if(r.banked)return `+🪙${rewardCoins} coins. 🎉 +1 minute of watching earned!`;
  return `+🪙${rewardCoins} coins`;
}
function fightDone(){
  play++;cur=null;clearInterval(timerId);
  const done=$('done');show('done');
  const total=fightHits+fightMisses.length;
  const missList=fightMisses.map(m=>`<p class="muted center">✗ ${esc(m.q)} = ${m.a}</p>`).join('');
  const coach=total>0?`<p class="muted center">You hit ${fightHits} of ${total}.</p>`:'';
  const tips=missList?`<p class="center"><b>Practice these:</b></p>${missList}`:'';
  if(lastMsg&&lastMsg.victory){
    const next=lastMsg.next_boss;
    let h=`<div class="center">${bossSVG(boss.id,'defeat')}</div>
      <h1>🏆 ${esc(boss.name)} is beaten!</h1>
      <div class="award lg">${esc(rewardText(lastMsg.reward))}</div>${coach}`;
    if(next)h+=`<p class="lead">Next up: ${next.emoji} ${esc(next.name)}!</p>`;
    done.innerHTML=h;
    done.appendChild(button('Continue','btn green',()=>loadStatus()));
    done.appendChild(button(BACK.label,'btn gray',BACK.go));
  }else{
    done.innerHTML=`<div class="center">${bossSVG(boss.id,'idle')}</div>
      <h1>${boss.emoji} ${esc(boss.name)} is still standing!</h1>
      <p class="lead">A miss makes it strong again. Get all ${hearts} right in one fight to win!</p>${coach}${tips}`;
    done.appendChild(button('⚔️ Fight again','btn red',startFight));
    done.appendChild(button('Menu','btn gray',()=>loadStatus()));
  }
  focusKey(done.querySelector('button'));
}
function renderDefeatedAll(d){
  const m=menuEl();
  m.innerHTML=`<h2 class="center">👑 You beat every boss!</h2>
    <p class="lead">🏆 ${d.defeated.length} wins</p>`;
  m.appendChild(button(BACK.label,'btn gray',BACK.go));
  focusKey(m.querySelector('button'));
}
function stopFight(){
  const name=boss?boss.name:'The boss';
  loadStatus(`You stopped the fight. ${name} healed up.`);
}
$('stopbtn').addEventListener('click',stopFight);
$('gobtn').addEventListener('click',()=>submit());
$('ans').addEventListener('keydown',e=>{
  if(e.key==='Enter'){e.preventDefault();submit();}
});
onEscape(()=>{if(!$('play').hidden)stopFight();else if(!$('done').hidden)loadStatus();else BACK.go();});
loadStatus();
