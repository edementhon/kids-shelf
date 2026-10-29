// Practice home: a kid picks their name and types their PIN (the same
// unlock as the shelf) before practicing; a signed-in kid can hand the
// device over with "Not you?".
let pinKid='';

function openPin(id,name){
  pinKid=id;
  $('pick').hidden=true;$('pin').hidden=false;
  $('pinwho').textContent=`Hi ${name}! Type your PIN.`;
  $('pinmsg').textContent='';
  $('pinbox').value='';
  $('pinbox').focus();
}
function closePin(){
  pinKid='';
  $('pin').hidden=true;$('pick').hidden=false;
  focusKey(document.querySelector('#pick .choice'));
}
async function sendPin(){
  const pin=$('pinbox').value.trim();
  if(!pin){$('pinbox').focus();return;}
  $('pingo').disabled=true;
  let r,d=null;
  try{
    r=await fetch('/api/kid/unlock',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({kid:pinKid,pin})});
    try{d=await r.json();}catch(e){}
  }catch(e){r=null;}
  $('pingo').disabled=false;
  if(r&&r.ok){location.reload();return;}
  $('pinmsg').textContent=r?((d&&d.error)||'That PIN didn\'t work. Try again.')
    :"Couldn't reach the shelf. Check the Wi-Fi, then try again.";
  $('pinbox').value='';$('pinbox').focus();
}

if($('pick')){
  for(const b of document.querySelectorAll('#pick .choice'))
    b.addEventListener('click',()=>openPin(b.dataset.kid,b.dataset.name));
  $('pingo').addEventListener('click',sendPin);
  $('pinback').addEventListener('click',closePin);
  $('pinbox').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();sendPin();}});
  onEscape(()=>{if(!$('pin').hidden)closePin();else location.href='/';});
  focusKey(document.querySelector('#pick .choice'));
}
if($('notme')){
  $('notme').addEventListener('click',async()=>{
    $('notme').disabled=true;
    try{
      const r=await fetch('/api/kid/lock',{method:'POST'});
      if(r.ok){location.reload();return;}
    }catch(e){}
    $('notme').disabled=false;
    $('notme').textContent="Couldn't sign out. Try again";
  });
  onEscape(()=>{location.href='/';});
}
