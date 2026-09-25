package uploadserver

// uploadPage is the whole browser client: nothing to install on the laptop.
// It is self-contained (no external requests) because the laptop is on a
// cable to a board with no internet.
const uploadPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>MusallahBoard update</title>
<style>
:root{--navy:#082D5D;--gold:#F8E15D;--ink:#0f1b2d;--muted:#5b6b80;--line:#dbe2ea;--ok:#1f7a4d;--bad:#b3261e}
*{box-sizing:border-box}body{margin:0;font-family:system-ui,-apple-system,Segoe UI,sans-serif;color:var(--ink);background:#f4f6f9}
header{background:var(--navy);color:#fff;padding:20px 16px}header h1{margin:0;font-weight:500;font-size:22px}
header p{margin:6px 0 0;opacity:.8;font-size:14px}main{max-width:760px;margin:0 auto;padding:16px}
.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px}
.card h2{margin:0 0 10px;font-size:16px}dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 14px;margin:0;font-size:14px}
dt{color:var(--muted)}dd{margin:0}#drop{border:2px dashed var(--line);border-radius:10px;padding:28px;text-align:center;cursor:pointer}
#drop.over{border-color:var(--navy);background:#eef3fa}button{background:var(--navy);color:#fff;border:0;border-radius:8px;padding:10px 18px;font-size:15px;cursor:pointer}
button:disabled{opacity:.5;cursor:default}ul{padding-left:18px;margin:8px 0}progress{width:100%;height:10px}
.ok{color:var(--ok)}.bad{color:var(--bad)}.neutral{color:var(--muted)}.muted{color:var(--muted);font-size:13px}
.detail{display:block;color:var(--muted);font-size:12px}h3{margin:12px 0 4px;font-size:16px}
</style></head><body>
<header><h1>MusallahBoard update</h1><p>Send signed update packages (.mbu) to this board.</p></header>
<main>
<section class="card"><h2>This board</h2><dl id="st"><dt>Status</dt><dd>loading…</dd></dl></section>
<section class="card"><h2>Send updates</h2>
<div id="drop">Drop .mbu files here, or click to choose<input id="pick" type="file" accept=".mbu" multiple hidden></div>
<ul id="files"></ul><button id="send" disabled>Send to board</button>
<p><progress id="prog" value="0" max="1" hidden></progress></p><div id="out"></div>
<p class="muted">Content packages come from LensBridge (Devices, the board, Download offline bundle). Board software packages come from the MusallahBoard releases. The board checks every signature itself and refuses anything not meant for it.</p>
</section></main>
<script>
var chosen=[];
function el(t,c,x){var e=document.createElement(t);if(c)e.className=c;if(x!=null)e.textContent=x;return e}
function row(dl,k,v){dl.appendChild(el('dt',null,k));dl.appendChild(el('dd',null,v))}
function loadStatus(){fetch('/api/status',{cache:'no-store'}).then(function(r){return r.json()}).then(function(s){
 var dl=document.getElementById('st');dl.textContent='';
 row(dl,'Device',s.deviceId);row(dl,'Agent',s.agentVersion);row(dl,'Board app',s.app?s.app.version:'none installed');
 row(dl,'Content',s.content?(s.content.firstDay+' to '+s.content.lastDay):'none installed');
 if(s.content){row(dl,'Days left',s.staleDays>0?('ran out '+s.staleDays+' day(s) ago'):String(s.daysRemaining))}
 var drift=Math.round(s.clock.unix-Date.now()/1000);
 row(dl,'Board clock',new Date(s.clock.unix*1000).toLocaleString()+(Math.abs(drift)>5?(' ('+drift+' s off this computer; corrected when you send an update)'):''));
 row(dl,'RTC',s.rtc?'present':'not fitted');
 var src={ntp:'set from the internet',rtc:'kept by the hardware clock',uploader:'set from a laptop or phone',starting:'checking',unverified:'NOT CONFIRMED: send an update from this page to set it'};
 if(s.clock.source)row(dl,'Clock',src[s.clock.source]||s.clock.source);
}).catch(function(){document.getElementById('st').textContent='Could not reach the board.'})}
function show(){var ul=document.getElementById('files');ul.textContent='';chosen.forEach(function(f){ul.appendChild(el('li',null,f.name+' ('+Math.round(f.size/1024)+' KB)'))});
 document.getElementById('send').disabled=!chosen.length}
function add(list){var skipped=[];for(var i=0;i<list.length;i++){if(/\.mbu$/i.test(list[i].name))chosen.push(list[i]);else skipped.push(list[i].name)}
 show();var out=document.getElementById('out');out.textContent='';
 if(skipped.length)out.appendChild(el('p','bad','Left out '+skipped.join(', ')+': only .mbu update packages can be sent.'))}
var drop=document.getElementById('drop'),pick=document.getElementById('pick');
drop.onclick=function(){pick.click()};pick.onchange=function(){add(pick.files);pick.value=''};
drop.ondragover=function(e){e.preventDefault();drop.classList.add('over')};drop.ondragleave=function(){drop.classList.remove('over')};
drop.ondrop=function(e){e.preventDefault();drop.classList.remove('over');add(e.dataTransfer.files)};
document.getElementById('send').onclick=function(){
 var fd=new FormData();chosen.forEach(function(f){fd.append('package',f,f.name)});
 var x=new XMLHttpRequest(),prog=document.getElementById('prog'),out=document.getElementById('out'),btn=this;
 btn.disabled=true;prog.hidden=false;prog.value=0;out.textContent='Uploading…';
 x.upload.onprogress=function(e){if(e.lengthComputable){prog.value=e.loaded/e.total;if(e.loaded===e.total)out.textContent='Installing… the board shows its progress on screen.'}};
 x.onload=function(){prog.hidden=true;var r;try{r=JSON.parse(x.responseText)}catch(e){r={message:x.responseText}}
  out.textContent='';if(r.message)out.appendChild(el('p','bad',r.message));
  if(r.notice){out.appendChild(el('h3',r.notice.tone==='problem'?'bad':(r.notice.tone==='ok'?'ok':null),r.notice.headline))}
  var cls={installed:'ok',staged:'ok',rejected:'bad'};
  var ul=el('ul');(r.results||[]).forEach(function(it){if(it.action==='queued')return;var li=el('li',cls[it.action]||'neutral',it.file+': '+it.message);
   if(it.detail)li.appendChild(el('span','detail',it.detail));ul.appendChild(li)});out.appendChild(ul);
  if(r.clock&&r.clock.note)out.appendChild(el('p','muted','Clock: '+r.clock.note));
  chosen=[];show();loadStatus()};
 x.onerror=function(){prog.hidden=true;out.textContent='';out.appendChild(el('p','bad','The upload failed. Check the cable and try again.'));btn.disabled=false};
 x.open('POST','/api/import');x.setRequestHeader('X-MB-Client-Time',String(Math.floor(Date.now()/1000)));x.send(fd)};
loadStatus();
</script></body></html>`
