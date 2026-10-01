package main

const setupWizardHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Tsumugi · 首次设置</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto+Flex:opsz,wght,wdth@8..144,300..800,75..125&display=swap">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0">
<style>
:root{
  --pri:#E8607C;--on-pri:#FFFFFF;--pri-ctn:#FFF0F3;--on-pri-ctn:#8B1A3A;
  --sec:#8D6E73;--on-sec:#FFFFFF;--sec-ctn:#F3E5E8;--on-sec-ctn:#4A3538;
  --ter:#F48FB1;--on-ter:#FFFFFF;--ter-ctn:#FCE4EC;--on-ter-ctn:#8B1A3A;
  --err:#B3261E;--on-err:#FFFFFF;--err-ctn:#F9DEDC;--on-err-ctn:#410E0B;
  --sf:#FFFAF9;--on-sf:#2D2028;--sf-dim:#F5E6EA;
  --sf-var:#F0E0E6;--on-sf-var:#6D5D63;
  --ol:#9E8E96;--ol-var:#E0D4D8;
  --sc0:#FFFFFF;--sc1:#FFF5F7;--sc2:#FFF0F3;
  --sc3:#ECE6F0;--sc3est:#E6E0E9;
  --inv-sf:#3D2832;--on-inv-sf:#F5E6EA;--inv-pri:#F48FB1;
  --pri-20:#7B1A38;--pri-40:#E8607C;--ter-40:#D84A6A;
  --pri-90:#FCCDD6;--pri-99:#FFFAF9;
  --dur-m2:300ms;--dur-s4:200ms;
  --ease-emph:cubic-bezier(0.2,0,0,1);
  --ease-emph-dec:cubic-bezier(0.05,0.7,0.1,1);
  --r-md:12px;--r-lg:16px;--r-xl:28px;--r-full:9999px;
}
@media(prefers-color-scheme:dark){:root{
  --pri:#F8A4B8;--on-pri:#5C1228;--pri-ctn:#B33060;--on-pri-ctn:#FFE4EB;
  --sec:#C9ADA2;--on-sec:#3D3035;--sec-ctn:#6D5060;--on-sec-ctn:#F3E5E8;
  --ter:#F8A4B8;--on-ter:#5C1228;--ter-ctn:#9C3060;--on-ter-ctn:#FFE4EB;
  --err:#F2B8B5;--on-err:#601410;--err-ctn:#8C1D18;--on-err-ctn:#F9DEDC;
  --sf:#1A1215;--on-sf:#F0E0E6;--sf-dim:#141218;
  --sf-var:#3D2D35;--on-sf-var:#D4B8C0;
  --ol:#9E8E96;--ol-var:#3D2D35;
  --sc0:#120D10;--sc1:#1E1619;--sc2:#231A1E;
  --sc3:#2B2930;--sc3est:#36343B;
  --inv-sf:#F0E0E6;--on-inv-sf:#3D2832;--inv-pri:#E8607C;
  --pri-20:#7B1A38;--pri-40:#E8607C;--ter-40:#D84A6A;
  --pri-90:#FCCDD6;--pri-99:#FFFAF9;
}}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Roboto Flex','Roboto',system-ui,sans-serif;background:var(--sf);color:var(--on-sf);min-height:100vh;display:flex;align-items:center;justify-content:center;font-optical-sizing:auto;-webkit-font-smoothing:antialiased}
.wizard{width:min(560px,92vw);padding:48px 40px}
.step{display:none;animation:stepIn .5s cubic-bezier(.22,1,.36,1) both}
.step.active{display:block}
@keyframes stepIn{from{opacity:0;transform:translateY(14px) scale(.99)}to{opacity:1;transform:translateY(0) scale(1)}}
.hero{width:96px;height:96px;border-radius:var(--r-xl);display:grid;place-items:center;background:linear-gradient(135deg,var(--pri-20),var(--pri) 55%,var(--ter));color:var(--on-pri);margin:0 auto 28px;box-shadow:0 4px 16px rgba(232,96,124,.25),0 8px 32px rgba(232,96,124,.12);transition:transform var(--dur-m2) var(--ease-emph)}
.hero:hover{transform:scale(1.06) rotate(-3deg)}
.hero .mat{font-size:44px}
h1{font-size:28px;font-weight:800;text-align:center;margin-bottom:8px;letter-spacing:-.5px;font-variation-settings:'wght' 800,'wdth' 100}
.sub{text-align:center;color:var(--on-sf-var);font-size:14px;margin-bottom:32px;line-height:1.6}
.label{font-size:12px;font-weight:600;color:var(--on-sf-var);margin-bottom:6px;display:block;letter-spacing:.4px}
.txt{width:100%;padding:14px 16px;border-radius:var(--r-md);border:1px solid var(--ol);font-size:14px;background:var(--sc0);color:var(--on-sf);transition:all var(--dur-s4) var(--ease-emph);font-family:inherit}
.txt:focus{outline:none;border-color:var(--pri);box-shadow:0 0 0 2px var(--pri-90)}
.pwd-box{background:var(--sc2);border-radius:var(--r-lg);padding:24px;text-align:center;margin:20px 0;border:none;position:relative;overflow:hidden}
.pwd-box::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
.pwd-val{font-size:22px;font-weight:800;letter-spacing:3px;color:var(--pri);font-family:'Fira Code',Consolas,monospace;margin:10px 0;font-variation-settings:'wght' 800}
.pwd-hint{font-size:12px;color:var(--ol);font-weight:500}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;border:none;cursor:pointer;padding:14px 32px;border-radius:var(--r-full);font-size:15px;font-weight:600;transition:all var(--dur-m2) var(--ease-emph);width:100%;font-family:inherit;position:relative;overflow:hidden}
.btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--dur-s4) cubic-bezier(0.2,0,0,1)}
.btn:hover::before{opacity:.08}
.btn:active::before{opacity:.12}
.btn-fill{background:var(--pri);color:var(--on-pri);box-shadow:0 1px 3px rgba(0,0,0,.15),0 2px 6px rgba(232,96,124,.15)}
.btn-fill:hover{box-shadow:0 2px 6px rgba(0,0,0,.2),0 4px 12px rgba(232,96,124,.25);transform:translateY(-1px)}
.btn-fill:active{transform:translateY(0);box-shadow:0 1px 2px rgba(0,0,0,.15)}
.btn-fill:disabled{opacity:.38;cursor:not-allowed;transform:none;box-shadow:none}
.btn-tonal{background:var(--sec-ctn);color:var(--on-sec-ctn);box-shadow:0 1px 2px rgba(0,0,0,.1)}
.progress{display:flex;gap:8px;justify-content:center;margin-bottom:36px}
.progress .dot{width:8px;height:8px;border-radius:50%;background:var(--ol-var);transition:all var(--dur-m2) var(--ease-emph)}
.progress .dot.done{background:var(--pri)}
.progress .dot.current{background:var(--pri);width:24px;border-radius:4px}
.terms{background:var(--sc1);border-radius:var(--r-lg);padding:20px;max-height:240px;overflow-y:auto;overflow-x:hidden;margin:20px 0;font-size:13px;line-height:1.8;color:var(--on-sf);border:1px solid var(--ol-var);position:relative;-webkit-overflow-scrolling:touch}
.terms::-webkit-scrollbar{width:8px}
.terms::-webkit-scrollbar-track{background:transparent}
.terms::-webkit-scrollbar-thumb{background:var(--ol-var);border-radius:8px;border:2px solid var(--sc1)}
.terms h4{font-size:14px;font-weight:700;margin:12px 0 6px;color:var(--pri)}
.terms p{margin-bottom:8px}
.check-row{display:flex;align-items:center;gap:10px;margin:16px 0;cursor:pointer}
.check-row input{width:18px;height:18px;accent-color:var(--pri)}
.check-row label{font-size:14px;cursor:pointer}
.danger-box{background:var(--err-ctn);color:var(--on-err-ctn);border-radius:var(--r-lg);padding:16px 20px;margin:16px 0;font-size:13px;line-height:1.6}
.danger-box b{display:block;margin-bottom:4px;font-size:14px}
.err-msg{color:var(--err);font-size:13px;margin-top:8px;text-align:center}
.feature-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin:20px 0}
.feature{display:flex;align-items:center;gap:10px;padding:12px 14px;border-radius:var(--r-md);background:var(--sc1);font-size:13px;font-weight:500;color:var(--on-sf-var)}
.feature .mat{font-size:20px;color:var(--pri)}
/* 语言选择：苹果风格大球+小球环形 */
.apple-ring{position:relative;height:236px;margin:24px 0 8px;user-select:none;-webkit-user-select:none;touch-action:pan-y}
.apple-ring::before{content:'';position:absolute;left:50%;top:54%;width:176px;height:176px;transform:translate(-50%,-50%);border-radius:50%;background:radial-gradient(circle at 50% 34%, rgba(232,96,124,.14), rgba(232,96,124,0) 72%);pointer-events:none}
.apple-orb{position:absolute;left:50%;top:60%;width:112px;height:112px;border-radius:50%;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:3px;cursor:pointer;text-align:center;will-change:transform,opacity;
  background:radial-gradient(circle at 32% 22%, #FFB0C8, #E8607C 38%, #B33060 82%);
  box-shadow:0 14px 34px rgba(180,48,96,.35), inset 0 1px 0 rgba(255,255,255,.35);
  transition:transform .68s cubic-bezier(.34,1.45,.64,1), opacity .46s cubic-bezier(.22,1,.36,1), box-shadow .46s cubic-bezier(.22,1,.36,1)}
.apple-orb::after{content:'';position:absolute;inset:0;border-radius:50%;background:radial-gradient(circle at 30% 22%, rgba(255,255,255,.55), rgba(255,255,255,0) 46%)}
.apple-orb .n{position:relative;z-index:2;font-size:19px;font-weight:800;color:#FFFFFF;text-shadow:0 1px 8px rgba(0,0,0,.35);padding:0 8px;line-height:1.2;max-width:96px;font-variation-settings:'wght' 800}
.apple-orb .s{position:relative;z-index:2;font-size:8.5px;text-transform:uppercase;letter-spacing:.7px;opacity:.9;font-weight:700;color:#fff;text-shadow:0 1px 4px rgba(0,0,0,.25)}
.apple-orb.active{box-shadow:0 18px 44px rgba(180,48,96,.45), 0 0 0 4px rgba(255,255,255,.18), inset 0 1px 0 rgba(255,255,255,.4)}
.apple-orb.min{box-shadow:0 10px 22px rgba(180,48,96,.25), inset 0 1px 0 rgba(255,255,255,.3)}
.apple-orb.hidden{opacity:0;pointer-events:none}
.lang-hint{text-align:center;color:var(--ol);font-size:12px;margin:8px 0 10px;display:flex;align-items:center;justify-content:center;gap:6px}
</style>
</head>
<body>
<div class="wizard">
  <div class="progress" id="progress">
    <div class="dot current" id="pd1"></div>
    <div class="dot" id="pd2"></div>
    <div class="dot" id="pd3"></div>
    <div class="dot" id="pd4"></div>
    <div class="dot" id="pd5"></div>
  </div>

  <!-- Step 1: 选择语言 -->
  <div class="step active" id="step1">
    <div class="hero"><span class="mat material-symbols-outlined">translate</span></div>
    <h1 data-i18n="stepLangTitle">选择界面语言</h1>
    <p class="sub" data-i18n="stepLangSub">向左或向右滑动浏览，点击圆形即选中</p>
    <div class="apple-ring" id="langRing"></div>
    <button class="btn btn-fill" id="btnLang" disabled onclick="goStep(2)"><span data-i18n="btnLang">继续</span> <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 2: 欢迎 -->
  <div class="step" id="step2">
    <div class="hero"><span class="mat material-symbols-outlined">database</span></div>
    <h1 data-i18n="wizWelcome">欢迎使用 Tsumugi</h1>
    <p class="sub" data-i18n="wizWelcomeSub">轻量级高性能内存数据库<br>支持原生二进制协议与 MySQL 兼容协议</p>
    <div class="feature-grid">
      <div class="feature"><span class="mat material-symbols-outlined">bolt</span><span data-i18n="fBolt">亚毫秒级延迟</span></div>
      <div class="feature"><span class="mat material-symbols-outlined">save</span><span data-i18n="fSave">WAL 持久化</span></div>
      <div class="feature"><span class="mat material-symbols-outlined">dns</span><span data-i18n="fDns">MySQL 兼容</span></div>
      <div class="feature"><span class="mat material-symbols-outlined">security</span><span data-i18n="fSec">多用户权限</span></div>
    </div>
    <button class="btn btn-fill" onclick="goStep(3)"><span data-i18n="btnStart">开始设置</span> <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 3: 创建管理员账号 -->
  <div class="step" id="step3">
    <div class="hero"><span class="mat material-symbols-outlined">person_add</span></div>
    <h1 data-i18n="accTitle">创建管理员账号</h1>
    <p class="sub" data-i18n="accSub">设置你的管理员用户名和密码<br>此账号将拥有全部管理权限</p>
    <label class="label" data-i18n="lblUser">用户名</label>
    <input class="txt" id="newUser" data-ph="phUser" placeholder="admin" style="margin-bottom:14px" autocomplete="off">
    <label class="label" data-i18n="lblPass">密码</label>
    <input class="txt" id="newPass" type="password" data-ph="phPass" placeholder="至少 6 位" style="margin-bottom:14px" autocomplete="new-password">
    <label class="label" data-i18n="lblPass2">确认密码</label>
    <input class="txt" id="newPass2" type="password" data-ph="phPass2" placeholder="再次输入密码" style="margin-bottom:20px" autocomplete="new-password">
    <div id="step2Err" class="err-msg" style="display:none"></div>
    <button class="btn btn-fill" onclick="doSetup()"><span data-i18n="btnCreateAcc">创建账号并继续</span> <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 4: 服务条款 -->
  <div class="step" id="step4">
    <div class="hero"><span class="mat material-symbols-outlined">gavel</span></div>
    <h1 data-i18n="termsTitle">服务条款</h1>
    <p class="sub" data-i18n="termsSub">使用前请阅读以下条款</p>
    <div class="terms" id="termsBody"></div>
    <div class="check-row">
      <input type="checkbox" id="agreeCheck" onchange="document.getElementById('agreeBtn').disabled=!this.checked">
      <label for="agreeCheck" data-i18n="agreeLabel">我已阅读并同意上述服务条款</label>
    </div>
    <button class="btn btn-fill" id="agreeBtn" disabled onclick="goStep(5)"><span data-i18n="btnAgree">同意并继续</span> <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 5: 完成 -->
  <div class="step" id="step5">
    <div class="hero"><span class="mat material-symbols-outlined">celebration</span></div>
    <h1 data-i18n="doneTitle">一切就绪！</h1>
    <p class="sub" data-i18n="doneSub">你可以开始使用 Tsumugi 管理数据了</p>
    <div id="rootInfo" class="danger-box" style="display:none">
      <b data-i18n="rootTitle">请保存 root 备用账号密码</b>
      <span id="rootInfoPwd"></span>
    </div>
    <button class="btn btn-fill" onclick="location.href='/dashboard'" style="margin-top:12px"><span data-i18n="btnDashboard">进入控制台</span> <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>
</div>
<script>
/*__I18N__*/
function $(id){return document.getElementById(id);}
var rootPassword='';
function applyTexts(){
  var c=curLang();
  document.documentElement.lang=c;
  document.title=t('wizPageTitle');
  document.querySelectorAll('[data-i18n]').forEach(function(el){
    var key=el.getAttribute('data-i18n');var v=I18N_TEXT[key];if(!v)return;
    var txt=v[c]||v.en||v.zh;if(txt==null)return;
    var kids=el.childNodes;for(var k=kids.length-1;k>=0;k--){if(kids[k].nodeType===3){kids[k].data=txt;break;}}
  });
  document.querySelectorAll('[data-ph]').forEach(function(el){el.setAttribute('placeholder',t(el.getAttribute('data-ph')));});
  $('agreeBtn').disabled=!$('agreeCheck').checked;
  renderTerms();
}
var ringCur=0;
function getRingIndex(code){for(var i=0;i<(I18N_LANGS||[]).length;i++){if(I18N_LANGS[i].code===(code||curLang()))return i;}return 0;}
function renderLangRing(){
  var stage=$('langRing');if(!stage)return;
  ringCur=getRingIndex();
  stage.innerHTML='';
  I18N_LANGS.forEach(function(l,idx){
    var o=document.createElement('div');
    o.className='apple-orb';
    o.id='orb'+idx;
    o.innerHTML='<span class="n"></span><span class="s">'+l.en+'</span>';
    o.querySelector('.n').textContent=l.name;
    o.addEventListener('click',function(){selectLang(l.code);});
    o.style.transform='translate(-50%,-50%) scale(.06)';
    o.style.opacity='0';
    stage.appendChild(o);
  });
  requestAnimationFrame(function(){appleLayout();});
}
function appleLayout(){
  if(!(I18N_LANGS&&I18N_LANGS.length))return;
  var n=I18N_LANGS.length;
  for(var i=0;i<n;i++){
    var el=document.getElementById('orb'+i);
    if(!el)continue;
    var d=i-ringCur;if(d> n/2)d-=n;if(d< -n/2)d+=n;
    var ad=Math.abs(d);
    var size=ad===0?112:ad===1?56:ad===2?38:8;
    var scale=size/112;
    var x=d*72;
    var y=-(ad*ad)*4;
    el.style.transform='translate('+x+'px,'+y+'px) translate(-50%,-50%) scale('+scale+')';
    el.style.opacity=ad>3?0:1-(ad*0.24);
    if(ad>3)el.style.transform='translate('+x+'px,'+y+'px) translate(-50%,-50%) scale(0)';
    el.classList.toggle('hidden',ad>3);
    el.classList.toggle('active',ad===0);
    el.classList.toggle('min',ad>0&&ad<=2);
  }
  $('btnLang').disabled=false;
}
function selectLang(code){
  localStorage.setItem('tsumugi_lang',code);
  ringCur=getRingIndex(code);
  appleLayout();
  applyTexts();
}
function renderTerms(){
  var html='';
  for(var i=1;i<=8;i++){html+='<h4>'+t('term'+i)+'</h4><p>'+t('term'+i+'_body')+'</p>';}
  $('termsBody').innerHTML=html;
}
function goStep(n){
  document.querySelectorAll('.step').forEach(function(s){s.classList.remove('active');});
  $('step'+n).classList.add('active');
  for(var i=1;i<=5;i++){
    var dot=$('pd'+i);dot.className='dot';
    if(i<n)dot.classList.add('done');
    if(i===n)dot.classList.add('current');
  }
  if(n===5&&rootPassword){$('rootInfo').style.display='';$('rootInfoPwd').textContent='root / '+rootPassword;}
  window.scrollTo({top:0,behavior:'smooth'});
}
async function doSetup(){
  var u=$('newUser').value.trim(),p=$('newPass').value,p2=$('newPass2').value;
  var err=$('step2Err');
  if(!u){err.textContent=t('eNeedUser');err.style.display='';return;}
  if(p.length<6){err.textContent=t('ePass');err.style.display='';return;}
  if(p!==p2){err.textContent=t('ePassMismatch');err.style.display='';return;}
  err.style.display='none';
  try{
    var r=await fetch('/api/setup/complete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:p})});
    var d=await r.json();
    if(!d.ok){err.textContent=d.error||t('eSetupFail');err.style.display='';return;}
    localStorage.setItem('tsumugi_token',d.token);
    if(d.root_password)rootPassword=d.root_password;
    goStep(4);
  }catch(e){err.textContent=t('eNetErr');err.style.display='';}
}
(document.getElementById('agreeCheck')).addEventListener('change',function(){document.getElementById('agreeBtn').disabled=!this.checked;});
window.addEventListener('keydown',function(e){if(e.key==='Enter'&&document.getElementById('step3').classList.contains('active')){doSetup();}});
renderLangRing();
applyTexts();
</script>
</body>
</html>`

const loginPageHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Tsumugi · 登录</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto+Flex:opsz,wght,wdth@8..144,300..800,75..125&display=swap">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0">
<style>
:root{
  --pri:#E8607C;--on-pri:#FFFFFF;--pri-ctn:#FFF0F3;--on-pri-ctn:#8B1A3A;
  --sec-ctn:#F3E5E8;--on-sec-ctn:#4A3538;
  --ter:#F48FB1;--ter-ctn:#FCE4EC;--on-ter-ctn:#8B1A3A;
  --err:#B3261E;--on-err:#FFFFFF;
  --sf:#FFFAF9;--on-sf:#2D2028;
  --sf-var:#F0E0E6;--on-sf-var:#6D5D63;
  --ol:#9E8E96;--ol-var:#E0D4D8;
  --sc0:#FFFFFF;--sc1:#FFF5F7;
  --inv-sf:#3D2832;--on-inv-sf:#F5E6EA;--inv-pri:#F48FB1;
  --pri-20:#7B1A38;--pri-40:#E8607C;--ter-40:#D84A6A;--pri-90:#FCCDD6;
  --dur-m2:300ms;--dur-s4:200ms;
  --ease-emph:cubic-bezier(0.2,0,0,1);
  --ease-emph-dec:cubic-bezier(0.05,0.7,0.1,1);
  --r-md:12px;--r-lg:16px;--r-xl:28px;--r-full:9999px;
}
@media(prefers-color-scheme:dark){:root{
  --pri:#F8A4B8;--on-pri:#5C1228;--pri-ctn:#B33060;--on-pri-ctn:#FFE4EB;
  --sec-ctn:#6D5060;--on-sec-ctn:#F3E5E8;
  --ter:#F8A4B8;--ter-ctn:#9C3060;--on-ter-ctn:#FFE4EB;
  --err:#F2B8B5;--on-err:#601410;
  --sf:#1A1215;--on-sf:#F0E0E6;
  --sf-var:#3D2D35;--on-sf-var:#D4B8C0;
  --ol:#9E8E96;--ol-var:#3D2D35;
  --sc0:#120D10;--sc1:#1E1619;
  --inv-sf:#F0E0E6;--on-inv-sf:#3D2832;--inv-pri:#E8607C;
  --pri-20:#7B1A38;--pri-40:#E8607C;--ter-40:#D84A6A;--pri-90:#FCCDD6;
}}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Roboto Flex','Roboto',system-ui,sans-serif;background:var(--sf);color:var(--on-sf);min-height:100vh;display:flex;align-items:center;justify-content:center;font-optical-sizing:auto;-webkit-font-smoothing:antialiased}
.login{width:min(420px,92vw);padding:40px}
.hero{width:88px;height:88px;border-radius:var(--r-xl);display:grid;place-items:center;background:linear-gradient(135deg,var(--pri-20),var(--pri) 50%,var(--ter-40));color:var(--on-pri);margin:0 auto 28px;box-shadow:0 4px 16px rgba(232,96,124,.25),0 8px 32px rgba(232,96,124,.12);transition:transform var(--dur-m2) var(--ease-emph)}
.hero:hover{transform:scale(1.06) rotate(-3deg)}
.hero .mat{font-size:40px}
h1{font-size:28px;font-weight:800;text-align:center;margin-bottom:8px;letter-spacing:-.5px;font-variation-settings:'wght' 800,'wdth' 100}
.sub{text-align:center;color:var(--on-sf-var);font-size:14px;margin-bottom:32px}
.label{font-size:12px;font-weight:600;color:var(--on-sf-var);margin-bottom:6px;display:block;letter-spacing:.4px}
.txt{width:100%;padding:14px 16px;border-radius:var(--r-md);border:1px solid var(--ol);font-size:14px;background:var(--sc0);color:var(--on-sf);transition:all var(--dur-s4) var(--ease-emph);font-family:inherit}
.txt:focus{outline:none;border-color:var(--pri);box-shadow:0 0 0 2px var(--pri-90)}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;border:none;cursor:pointer;padding:14px 32px;border-radius:var(--r-full);font-size:15px;font-weight:600;transition:all var(--dur-m2) var(--ease-emph);width:100%;font-family:inherit;position:relative;overflow:hidden;background:var(--pri);color:var(--on-pri);box-shadow:0 1px 3px rgba(0,0,0,.15),0 2px 6px rgba(232,96,124,.15)}
.btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--dur-s4) cubic-bezier(0.2,0,0,1)}
.btn:hover::before{opacity:.08}
.btn:hover{box-shadow:0 2px 6px rgba(0,0,0,.2),0 4px 12px rgba(232,96,124,.25);transform:translateY(-1px)}
.btn:active{transform:translateY(0);box-shadow:0 1px 2px rgba(0,0,0,.15)}
.btn:disabled{opacity:.38;cursor:not-allowed;transform:none;box-shadow:none}
.err{color:var(--err);font-size:13px;margin-top:12px;text-align:center}
.footer{margin-top:24px;text-align:center;color:var(--ol);font-size:12px}
</style>
</head>
<body>
<div class="login">
  <div class="hero"><span class="mat material-symbols-outlined">database</span></div>
  <h1 data-i18n="loginPageTitle">Tsumugi 控制台</h1>
  <p class="sub" data-i18n="loginPageSub">请输入账号密码以继续</p>
  <label class="label" data-i18n="lblUName">用户名</label>
  <input class="txt" id="user" value="" style="margin-bottom:14px" autocomplete="username" placeholder="用户名">
  <label class="label" data-i18n="lblPsd">密码</label>
  <input class="txt" id="pass" type="password" style="margin-bottom:24px" autocomplete="current-password" placeholder="密码">
  <button class="btn" onclick="doLogin()" id="loginBtn"><span class="material-symbols-outlined" style="font-size:18px">login</span><span data-i18n="btnLogin">登录</span></button>
  <div id="msg" class="err"></div>
  <p class="footer">Tsumugi In-memory Database</p>
</div>
<script>
/*__I18N__*/
function $(id){return document.getElementById(id);}
(function(){
  var c=curLang();
  document.documentElement.lang=c;
  document.title=t('loginPageTitle');
  document.querySelectorAll('[data-i18n]').forEach(function(el){
    var key=el.getAttribute('data-i18n');var v=I18N_TEXT[key];if(!v)return;
    var txt=v[c]||v.en||v.zh;if(txt==null)return;
    var kids=el.childNodes;for(var k=kids.length-1;k>=0;k--){if(kids[k].nodeType===3){kids[k].data=txt;break;}}
  });
  $('user').placeholder=t('lblUName');
  $('pass').placeholder=t('lblPsd');
})();
async function doLogin(){
  var u=$('user').value.trim(),p=$('pass').value;
  $('msg').textContent='';
  if(!u||!p){$('msg').textContent=t('eNeedBoth');return;}
  $('loginBtn').disabled=true;
  try{
    var r=await fetch('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:u,password:p})});
    var d=await r.json();
    if(d.ok){localStorage.setItem('tsumugi_token',d.token);location.href='/dashboard';}
    else{$('msg').textContent=d.error||t('loginErr');}
  }catch(e){$('msg').textContent=t('netErr');}
  $('loginBtn').disabled=false;
}
document.getElementById('pass').addEventListener('keydown',function(e){if(e.key==='Enter')doLogin();});
</script>
</body>
</html>`
