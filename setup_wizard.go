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
  --md-primary:#6750A4;--md-on-primary:#FFFFFF;--md-primary-container:#EADDFF;--md-on-primary-container:#21005D;
  --md-secondary:#625B71;--md-on-secondary:#FFFFFF;--md-secondary-container:#E8DEF8;--md-on-secondary-container:#1D192B;
  --md-tertiary:#7D5260;--md-on-tertiary:#FFFFFF;--md-tertiary-container:#FFD8E4;--md-on-tertiary-container:#31111D;
  --md-error:#B3261E;--md-on-error:#FFFFFF;--md-error-container:#F9DEDC;--md-on-error-container:#410E0B;
  --md-surface:#FEF7FF;--md-on-surface:#1D1B20;--md-surface-dim:#DED8E1;--md-surface-bright:#FEF7FF;
  --md-surface-variant:#E7E0EC;--md-on-surface-variant:#49454F;
  --md-outline:#79747E;--md-outline-variant:#CAC4D0;
  --md-surface-container-lowest:#FFFFFF;--md-surface-container-low:#F7F2FA;--md-surface-container:#F3EDF7;
  --md-surface-container-high:#ECE6F0;--md-surface-container-highest:#E6E0E9;
  --md-inverse-surface:#322F35;--md-inverse-on-surface:#F5EFF7;--md-inverse-primary:#D0BCFF;
  --md-primary-10:#21005D;--md-primary-20:#381E72;--md-primary-30:#4F378B;--md-primary-40:#6750A4;
  --md-primary-80:#D0BCFF;--md-primary-90:#EADDFF;--md-primary-95:#F6EDFF;--md-primary-99:#FFFBFE;
  --md-duration-medium2:300ms;--md-duration-short4:200ms;--md-duration-long4:600ms;
  --md-easing-emphasized:cubic-bezier(0.2,0,0,1);
  --md-easing-emphasized-decelerate:cubic-bezier(0.05,0.7,0.1,1);
  --md-shape-xs:4px;--md-shape-sm:8px;--md-shape-md:12px;
  --md-shape-lg:16px;--md-shape-xl:28px;--md-shape-full:9999px;
}
@media(prefers-color-scheme:dark){:root{
  --md-primary:#D0BCFF;--md-on-primary:#381E72;--md-primary-container:#4F378B;--md-on-primary-container:#EADDFF;
  --md-secondary:#CCC2DC;--md-on-secondary:#332D41;--md-secondary-container:#4A4458;--md-on-secondary-container:#E8DEF8;
  --md-tertiary:#EFB8C8;--md-on-tertiary:#492532;--md-tertiary-container:#633B48;--md-on-tertiary-container:#FFD8E4;
  --md-error:#F2B8B5;--md-on-error:#601410;--md-error-container:#8C1D18;--md-on-error-container:#F9DEDC;
  --md-surface:#141218;--md-on-surface:#E6E0E9;--md-surface-dim:#141218;--md-surface-bright:#3B383E;
  --md-surface-variant:#49454F;--md-on-surface-variant:#CAC4D0;
  --md-outline:#938F99;--md-outline-variant:#49454F;
  --md-surface-container-lowest:#0F0D13;--md-surface-container-low:#1D1B20;--md-surface-container:#211F26;
  --md-surface-container-high:#2B2930;--md-surface-container-highest:#36343B;
  --md-inverse-surface:#E6E0E9;--md-inverse-on-surface:#322F35;--md-inverse-primary:#6750A4;
}}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Roboto Flex','Roboto',system-ui,sans-serif;background:var(--md-surface);color:var(--md-on-surface);min-height:100vh;display:flex;align-items:center;justify-content:center;font-optical-sizing:auto;-webkit-font-smoothing:antialiased}
.wizard{width:min(560px,92vw);padding:48px 40px}
.step{display:none;animation:stepIn var(--md-duration-medium2) var(--md-easing-emphasized-decelerate)}
.step.active{display:block}
@keyframes stepIn{from{opacity:0;transform:translateY(16px)}to{opacity:1;transform:translateY(0)}}
.hero{width:96px;height:96px;border-radius:var(--md-shape-xl);display:grid;place-items:center;background:linear-gradient(135deg,var(--md-primary-20),var(--md-primary) 50%,var(--md-tertiary));color:var(--md-on-primary);margin:0 auto 28px;box-shadow:0 4px 16px rgba(103,80,164,.3),0 8px 32px rgba(103,80,164,.15);transition:transform var(--md-duration-medium2) var(--md-easing-emphasized)}
.hero:hover{transform:scale(1.06) rotate(-3deg)}
.hero .mat{font-size:44px}
h1{font-size:28px;font-weight:800;text-align:center;margin-bottom:8px;letter-spacing:-.5px;font-variation-settings:'wght' 800,'wdth' 100}
.sub{text-align:center;color:var(--md-on-surface-variant);font-size:14px;margin-bottom:32px;line-height:1.6}
.label{font-size:12px;font-weight:600;color:var(--md-on-surface-variant);margin-bottom:6px;display:block;letter-spacing:.4px}
.txt{width:100%;padding:14px 16px;border-radius:var(--md-shape-md);border:1px solid var(--md-outline);font-size:14px;background:var(--md-surface-container-lowest);color:var(--md-on-surface);transition:all var(--md-duration-short4) var(--md-easing-emphasized);font-family:inherit}
.txt:focus{outline:none;border-color:var(--md-primary);box-shadow:0 0 0 2px var(--md-primary-90)}
.pwd-box{background:var(--md-surface-container);border-radius:var(--md-shape-lg);padding:24px;text-align:center;margin:20px 0;border:none;position:relative;overflow:hidden}
.pwd-box::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--md-outline-variant);pointer-events:none}
.pwd-val{font-size:22px;font-weight:800;letter-spacing:3px;color:var(--md-primary);font-family:'Fira Code',Consolas,monospace;margin:10px 0;font-variation-settings:'wght' 800}
.pwd-hint{font-size:12px;color:var(--md-outline);font-weight:500}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;border:none;cursor:pointer;padding:14px 32px;border-radius:var(--md-shape-full);font-size:15px;font-weight:600;transition:all var(--md-duration-medium2) var(--md-easing-emphasized);width:100%;font-family:inherit;position:relative;overflow:hidden}
.btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--md-duration-short4) cubic-bezier(0.2,0,0,1)}
.btn:hover::before{opacity:.08}
.btn:active::before{opacity:.12}
.btn-fill{background:var(--md-primary);color:var(--md-on-primary);box-shadow:0 1px 3px rgba(0,0,0,.15),0 2px 6px rgba(103,80,164,.2)}
.btn-fill:hover{box-shadow:0 2px 6px rgba(0,0,0,.2),0 4px 12px rgba(103,80,164,.3);transform:translateY(-1px)}
.btn-fill:active{transform:translateY(0);box-shadow:0 1px 2px rgba(0,0,0,.15)}
.btn-fill:disabled{opacity:.38;cursor:not-allowed;transform:none;box-shadow:none}
.btn-tonal{background:var(--md-secondary-container);color:var(--md-on-secondary-container);box-shadow:0 1px 2px rgba(0,0,0,.1)}
.btn-tonal:hover{box-shadow:0 2px 6px rgba(0,0,0,.15);transform:translateY(-1px)}
.progress{display:flex;gap:8px;justify-content:center;margin-bottom:36px}
.progress .dot{width:8px;height:8px;border-radius:50%;background:var(--md-outline-variant);transition:all var(--md-duration-medium2) var(--md-easing-emphasized)}
.progress .dot.done{background:var(--md-primary)}
.progress .dot.current{background:var(--md-primary);width:24px;border-radius:4px}
.terms{background:var(--md-surface-container-low);border-radius:var(--md-shape-lg);padding:20px;max-height:240px;overflow-y:auto;margin:20px 0;font-size:13px;line-height:1.8;color:var(--md-on-surface);border:none;position:relative;overflow:hidden}
.terms::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--md-outline-variant);pointer-events:none}
.terms h4{font-size:14px;font-weight:700;margin:12px 0 6px;color:var(--md-primary)}
.terms p{margin-bottom:8px}
.check-row{display:flex;align-items:center;gap:10px;margin:16px 0;cursor:pointer}
.check-row input{width:18px;height:18px;accent-color:var(--md-primary)}
.check-row label{font-size:14px;cursor:pointer}
.danger-box{background:var(--md-error-container);color:var(--md-on-error-container);border-radius:var(--md-shape-lg);padding:16px 20px;margin:16px 0;font-size:13px;line-height:1.6}
.danger-box b{display:block;margin-bottom:4px;font-size:14px}
.err-msg{color:var(--md-error);font-size:13px;margin-top:8px;text-align:center}
.feature-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin:20px 0}
.feature{display:flex;align-items:center;gap:10px;padding:12px 14px;border-radius:var(--md-shape-md);background:var(--md-surface-container-low);font-size:13px;font-weight:500;color:var(--md-on-surface-variant)}
.feature .mat{font-size:20px;color:var(--md-primary)}
</style>
</head>
<body>
<div class="wizard">
  <div class="progress" id="progress">
    <div class="dot current" id="pd1"></div>
    <div class="dot" id="pd2"></div>
    <div class="dot" id="pd3"></div>
    <div class="dot" id="pd4"></div>
  </div>

  <!-- Step 1: 欢迎 -->
  <div class="step active" id="step1">
    <div class="hero"><span class="mat material-symbols-outlined">database</span></div>
    <h1>欢迎使用 Tsumugi</h1>
    <p class="sub">轻量级高性能内存数据库<br>支持原生二进制协议与 MySQL 兼容协议</p>
    <div class="feature-grid">
      <div class="feature"><span class="mat material-symbols-outlined">bolt</span>亚毫秒级延迟</div>
      <div class="feature"><span class="mat material-symbols-outlined">save</span>WAL 持久化</div>
      <div class="feature"><span class="mat material-symbols-outlined">dns</span>MySQL 兼容</div>
      <div class="feature"><span class="mat material-symbols-outlined">security</span>多用户权限</div>
    </div>
    <button class="btn btn-fill" onclick="goStep(2)">开始设置 <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 2: 设置管理员账号 -->
  <div class="step" id="step2">
    <div class="hero"><span class="mat material-symbols-outlined">person_add</span></div>
    <h1>创建管理员账号</h1>
    <p class="sub">设置你的管理员用户名和密码<br>此账号将拥有全部管理权限</p>
    <label class="label">用户名</label>
    <input class="txt" id="newUser" placeholder="admin" style="margin-bottom:14px" autocomplete="off">
    <label class="label">密码</label>
    <input class="txt" id="newPass" type="password" placeholder="至少 6 位" style="margin-bottom:14px" autocomplete="new-password">
    <label class="label">确认密码</label>
    <input class="txt" id="newPass2" type="password" placeholder="再次输入密码" style="margin-bottom:20px" autocomplete="new-password">
    <div id="step2Err" class="err-msg" style="display:none"></div>
    <button class="btn btn-fill" onclick="doSetup()">创建账号并继续 <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 3: 服务条款 -->
  <div class="step" id="step3">
    <div class="hero"><span class="mat material-symbols-outlined">gavel</span></div>
    <h1>服务条款</h1>
    <p class="sub">使用前请阅读以下条款</p>
    <div class="terms">
      <h4>一、服务说明</h4>
      <p>Tsumugi（つむぎ）是一款用 Go 标准库从零实现的轻量级内存数据库，提供原生二进制协议与 MySQL 兼容协议。本软件按"现状"提供，不附带任何明示或暗示的保证。</p>
      <h4>二、开源协议</h4>
      <p>本项目基于 <b>MIT License</b> 开源。你可以在遵守 MIT 许可条款的前提下自由使用、修改、复制、分发与商用。完整协议文本见项目根目录 LICENSE 文件。</p>
      <h4>三、数据责任</h4>
      <p>用户应自行负责数据的备份与安全。Tsumugi 提供 WAL（预写日志）持久化机制，但不承诺在极端场景（如断电、硬件故障、进程被强制终止）下数据的完整性。建议定期通过备份功能保留关键数据快照。</p>
      <h4>四、安全义务</h4>
      <p>用户应妥善保管账号密码，不得将管理权限授予未经授权的第三方。因用户自身密码泄露、弱口令或服务器暴露于公网等原因导致的安全问题，Tsumugi 及其开发者不承担责任。</p>
      <h4>五、使用限制</h4>
      <p>用户不得利用 Tsumugi 进行任何违反法律法规的活动，包括但不限于：存储非法内容、发起网络攻击、未经授权访问他人系统或数据。</p>
      <h4>六、性能与限制</h4>
      <p>Tsumugi 为内存数据库，数据主存于内存中，服务重启后从 WAL 日志恢复。请根据实际硬件资源合理规划数据规模与写入频率；fsync 模式下写入吞吐受磁盘同步速度影响。</p>
      <h4>七、隐私保护</h4>
      <p>Tsumugi 不会主动收集、上传或向第三方披露任何用户数据。所有数据均存储在用户自有服务器上，监控指标与日志仅包含运行状态与操作审计信息，不包含业务数据内容。</p>
      <h4>八、免责条款</h4>
      <p>在任何情况下，Tsumugi 的开发者均不对因使用或无法使用本软件而导致的任何直接、间接、附带、特殊、惩罚性或后果性损害承担责任，即使已被告知此类损害的可能性。</p>
    </div>
    <div class="check-row">
      <input type="checkbox" id="agreeCheck" onchange="document.getElementById('agreeBtn').disabled=!this.checked">
      <label for="agreeCheck">我已阅读并同意上述服务条款</label>
    </div>
    <button class="btn btn-fill" id="agreeBtn" disabled onclick="goStep(4)">同意并继续 <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>

  <!-- Step 4: 完成 -->
  <div class="step" id="step4">
    <div class="hero"><span class="mat material-symbols-outlined">celebration</span></div>
    <h1>一切就绪！</h1>
    <p class="sub">你可以开始使用 Tsumugi 管理数据了</p>
    <div id="rootInfo" class="danger-box" style="display:none">
      <b>请保存 root 备用账号密码</b>
      <span id="rootInfoPwd"></span>
    </div>
    <button class="btn btn-fill" onclick="location.href='/dashboard'" style="margin-top:12px">进入控制台 <span class="material-symbols-outlined" style="font-size:18px">arrow_forward</span></button>
  </div>
</div>
<script>
function $(id){return document.getElementById(id);}
var rootPassword='';
function goStep(n){
  document.querySelectorAll('.step').forEach(function(s){s.classList.remove('active');});
  $('step'+n).classList.add('active');
  for(var i=1;i<=4;i++){
    var dot=$('pd'+i);
    dot.className='dot';
    if(i<n)dot.classList.add('done');
    if(i===n)dot.classList.add('current');
  }
  if(n===4&&rootPassword){
    $('rootInfo').style.display='';
    $('rootInfoPwd').textContent='root / '+rootPassword;
  }
}
async function doSetup(){
  var u=$('newUser').value.trim(),p=$('newPass').value,p2=$('newPass2').value;
  var err=$('step2Err');
  if(!u){err.textContent='请输入用户名';err.style.display='';return;}
  if(p.length<6){err.textContent='密码至少 6 位';err.style.display='';return;}
  if(p!==p2){err.textContent='两次密码不一致';err.style.display='';return;}
  err.style.display='none';
  try{
    var r=await fetch('/api/setup/complete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:p})});
    var d=await r.json();
    if(!d.ok){err.textContent=d.error||'设置失败';err.style.display='';return;}
    localStorage.setItem('tsumugi_token',d.token);
    if(d.root_password)rootPassword=d.root_password;
    goStep(3);
  }catch(e){err.textContent='网络错误';err.style.display='';}
}
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
  --md-primary:#6750A4;--md-on-primary:#FFFFFF;--md-primary-container:#EADDFF;--md-on-primary-container:#21005D;
  --md-secondary:#625B71;--md-on-secondary:#FFFFFF;--md-secondary-container:#E8DEF8;--md-on-secondary-container:#1D192B;
  --md-tertiary:#7D5260;--md-on-tertiary:#FFFFFF;--md-tertiary-container:#FFD8E4;--md-on-tertiary-container:#31111D;
  --md-error:#B3261E;--md-on-error:#FFFFFF;--md-error-container:#F9DEDC;--md-on-error-container:#410E0B;
  --md-surface:#FEF7FF;--md-on-surface:#1D1B20;--md-surface-dim:#DED8E1;--md-surface-bright:#FEF7FF;
  --md-surface-variant:#E7E0EC;--md-on-surface-variant:#49454F;
  --md-outline:#79747E;--md-outline-variant:#CAC4D0;
  --md-surface-container-lowest:#FFFFFF;--md-surface-container-low:#F7F2FA;--md-surface-container:#F3EDF7;
  --md-surface-container-high:#ECE6F0;--md-surface-container-highest:#E6E0E9;
  --md-inverse-surface:#322F35;--md-inverse-on-surface:#F5EFF7;--md-inverse-primary:#D0BCFF;
  --md-primary-10:#21005D;--md-primary-20:#381E72;--md-primary-30:#4F378B;--md-primary-40:#6750A4;
  --md-primary-80:#D0BCFF;--md-primary-90:#EADDFF;--md-primary-95:#F6EDFF;--md-primary-99:#FFFBFE;
  --md-duration-medium2:300ms;--md-duration-short4:200ms;
  --md-easing-emphasized:cubic-bezier(0.2,0,0,1);
  --md-easing-emphasized-decelerate:cubic-bezier(0.05,0.7,0.1,1);
  --md-shape-xs:4px;--md-shape-sm:8px;--md-shape-md:12px;
  --md-shape-lg:16px;--md-shape-xl:28px;--md-shape-full:9999px;
}
@media(prefers-color-scheme:dark){:root{
  --md-primary:#D0BCFF;--md-on-primary:#381E72;--md-primary-container:#4F378B;--md-on-primary-container:#EADDFF;
  --md-secondary:#CCC2DC;--md-on-secondary:#332D41;--md-secondary-container:#4A4458;--md-on-secondary-container:#E8DEF8;
  --md-tertiary:#EFB8C8;--md-on-tertiary:#492532;--md-tertiary-container:#633B48;--md-on-tertiary-container:#FFD8E4;
  --md-error:#F2B8B5;--md-on-error:#601410;--md-error-container:#8C1D18;--md-on-error-container:#F9DEDC;
  --md-surface:#141218;--md-on-surface:#E6E0E9;--md-surface-dim:#141218;--md-surface-bright:#3B383E;
  --md-surface-variant:#49454F;--md-on-surface-variant:#CAC4D0;
  --md-outline:#938F99;--md-outline-variant:#49454F;
  --md-surface-container-lowest:#0F0D13;--md-surface-container-low:#1D1B20;--md-surface-container:#211F26;
  --md-surface-container-high:#2B2930;--md-surface-container-highest:#36343B;
  --md-inverse-surface:#E6E0E9;--md-inverse-on-surface:#322F35;--md-inverse-primary:#6750A4;
}}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Roboto Flex','Roboto',system-ui,sans-serif;background:var(--md-surface);color:var(--md-on-surface);min-height:100vh;display:flex;align-items:center;justify-content:center;font-optical-sizing:auto;-webkit-font-smoothing:antialiased}
.login{width:min(420px,92vw);padding:40px}
.hero{width:88px;height:88px;border-radius:var(--md-shape-xl);display:grid;place-items:center;background:linear-gradient(135deg,var(--md-primary-20),var(--md-primary) 50%,var(--md-tertiary));color:var(--md-on-primary);margin:0 auto 28px;box-shadow:0 4px 16px rgba(103,80,164,.3),0 8px 32px rgba(103,80,164,.15);transition:transform var(--md-duration-medium2) var(--md-easing-emphasized)}
.hero:hover{transform:scale(1.06) rotate(-3deg)}
.hero .mat{font-size:40px}
h1{font-size:28px;font-weight:800;text-align:center;margin-bottom:8px;letter-spacing:-.5px;font-variation-settings:'wght' 800,'wdth' 100}
.sub{text-align:center;color:var(--md-on-surface-variant);font-size:14px;margin-bottom:32px}
.label{font-size:12px;font-weight:600;color:var(--md-on-surface-variant);margin-bottom:6px;display:block;letter-spacing:.4px}
.txt{width:100%;padding:14px 16px;border-radius:var(--md-shape-md);border:1px solid var(--md-outline);font-size:14px;background:var(--md-surface-container-lowest);color:var(--md-on-surface);transition:all var(--md-duration-short4) var(--md-easing-emphasized);font-family:inherit}
.txt:focus{outline:none;border-color:var(--md-primary);box-shadow:0 0 0 2px var(--md-primary-90)}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;border:none;cursor:pointer;padding:14px 32px;border-radius:var(--md-shape-full);font-size:15px;font-weight:600;transition:all var(--md-duration-medium2) var(--md-easing-emphasized);width:100%;font-family:inherit;position:relative;overflow:hidden;background:var(--md-primary);color:var(--md-on-primary);box-shadow:0 1px 3px rgba(0,0,0,.15),0 2px 6px rgba(103,80,164,.2)}
.btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--md-duration-short4) cubic-bezier(0.2,0,0,1)}
.btn:hover::before{opacity:.08}
.btn:hover{box-shadow:0 2px 6px rgba(0,0,0,.2),0 4px 12px rgba(103,80,164,.3);transform:translateY(-1px)}
.btn:active{transform:translateY(0);box-shadow:0 1px 2px rgba(0,0,0,.15)}
.btn:disabled{opacity:.38;cursor:not-allowed;transform:none;box-shadow:none}
.err{color:var(--md-error);font-size:13px;margin-top:12px;text-align:center}
.footer{margin-top:24px;text-align:center;color:var(--md-outline);font-size:12px}
</style>
</head>
<body>
<div class="login">
  <div class="hero"><span class="mat material-symbols-outlined">database</span></div>
  <h1>Tsumugi 控制台</h1>
  <p class="sub">请输入账号密码以继续</p>
  <label class="label">用户名</label>
  <input class="txt" id="user" value="" style="margin-bottom:14px" autocomplete="username" placeholder="用户名">
  <label class="label">密码</label>
  <input class="txt" id="pass" type="password" style="margin-bottom:24px" autocomplete="current-password" placeholder="密码">
  <button class="btn" onclick="doLogin()" id="loginBtn"><span class="material-symbols-outlined" style="font-size:18px">login</span>登录</button>
  <div id="msg" class="err"></div>
  <p class="footer">Tsumugi In-memory Database</p>
</div>
<script>
function $(id){return document.getElementById(id);}
async function doLogin(){
  var u=$('user').value.trim(),p=$('pass').value;
  $('msg').textContent='';
  if(!u||!p){$('msg').textContent='请输入用户名和密码';return;}
  $('loginBtn').disabled=true;
  try{
    var r=await fetch('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:u,password:p})});
    var d=await r.json();
    if(d.ok){localStorage.setItem('tsumugi_token',d.token);location.href='/dashboard';}
    else{$('msg').textContent=d.error||'登录失败';}
  }catch(e){$('msg').textContent='网络错误';}
  $('loginBtn').disabled=false;
}
document.getElementById('pass').addEventListener('keydown',function(e){if(e.key==='Enter')doLogin();});
</script>
</body>
</html>`
