package main

import (
	"net/http"
	"os"
	"strings"
)

// ==================== 面板图标字体（网页优先 + 本地兜底） ====================
// 面板图标是 Material Symbols（M3E 图标集）的连字字形，默认仍从 Google Fonts
// 拉取（三个页面里的 <link> 保持不变）。
//
// 这里的脚本只做「补救」，不改默认行为：
//  1. 主 CDN 可用 → 什么都不做，图标就是 Google Fonts 的原始字形；
//  2. 主 CDN 不可用（内网/被墙）→ 依次追加镜像 CSS（fonts.googleapis.cn、
//     fonts.loli.net），成功即恢复原图标；
//  3. 镜像也不可用 → 若 assets/material-symbols-outlined.woff2 存在则本地加载；
//  4. 全部不可用 → 替换成单色字符符号（不使用 emoji），保证界面不出现
//     "database" 这类连字文字。可通过 TSUMUGI_ICON_FALLBACK=off 关闭降级。
//
// 字体是否就位由服务端 os.Stat 判定后写进页面：document.fonts.check 对
// 加载失败的字族同样返回 true，不能用来判断。

// panelIconFontPath 自托管图标字体的磁盘路径（相对进程工作目录）。
const panelIconFontPath = "assets/material-symbols-outlined.woff2"

// panelIconFontURL 前端 @font-face 引用的地址。
const panelIconFontURL = "/assets/material-symbols-outlined.woff2"

// panelAssetsMarker 三个内嵌页面里的注入锚点（位于 </body> 之前）。
const panelAssetsMarker = "<!--__PANEL_ASSETS__-->"

// handlePanelIconFont 提供自托管图标字体。文件不存在时返回 404。
func handlePanelIconFont(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(panelIconFontPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "material-symbols-outlined.woff2", st.ModTime(), f)
}

// panelIconFontPresent 判断自托管字体是否已放置。
func panelIconFontPresent() bool {
	st, err := os.Stat(panelIconFontPath)
	return err == nil && !st.IsDir()
}

// panelIconFallbackEnabled 是否允许最后的字符降级（默认开启）。
// TSUMUGI_ICON_FALLBACK=off/0/false/no 时关闭，完全保持原来的 CDN 行为。
func panelIconFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TSUMUGI_ICON_FALLBACK"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// injectPanelAssets 把图标样式与补救脚本插入页面。
func injectPanelAssets(html string) string {
	if !strings.Contains(html, panelAssetsMarker) {
		return html
	}
	local := "false"
	if panelIconFontPresent() {
		local = "true"
	}
	fallback := "false"
	if panelIconFallbackEnabled() {
		fallback = "true"
	}
	block := strings.ReplaceAll(panelAssetsHTML, "__PANEL_ICON_FONT_LOCAL__", local)
	block = strings.ReplaceAll(block, "__PANEL_ICON_FALLBACK__", fallback)
	return strings.ReplaceAll(html, panelAssetsMarker, block)
}

// panelAssetsHTML 图标字体声明 + CDN 镜像补救 + 最后的单色字符降级。
const panelAssetsHTML = `<style>
@font-face{font-family:'Tsumugi Icons';font-style:normal;font-weight:100 700;font-display:swap;src:url('/assets/material-symbols-outlined.woff2') format('woff2')}
.material-symbols-outlined{font-family:'Tsumugi Icons','Material Symbols Outlined',sans-serif;font-weight:normal;font-style:normal;font-size:24px;line-height:1;letter-spacing:normal;text-transform:none;display:inline-block;white-space:nowrap;word-wrap:normal;direction:ltr;-webkit-font-feature-settings:'liga';font-feature-settings:'liga';-webkit-font-smoothing:antialiased}
.no-icon-font .material-symbols-outlined{font-family:inherit;font-feature-settings:normal;font-size:1em;font-style:normal}
</style>
<script>
(function(){
  var HAS_LOCAL=__PANEL_ICON_FONT_LOCAL__;
  var ALLOW_FALLBACK=__PANEL_ICON_FALLBACK__;
  var MIRRORS=['https://fonts.googleapis.cn/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0','https://fonts.loli.net/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0'];
  var MAP={add:'＋',add_table:'⊞',arrow_forward:'→',backspace:'⌫',bolt:'ϟ',celebration:'★',check:'✓',chevron_left:'‹',chevron_right:'›',close:'✕',compress:'⊟',computer:'▭',database:'▤',delete:'⊖',dns:'⊙',edit:'✎',gavel:'§',grid_on:'▦',group:'众',info:'ⓘ',list:'☰',local_fire_department:'热',lock:'锁',login:'↵',memory:'▣',memory_alt:'▤',monitoring:'∿',palette:'◑',people:'人',person_add:'人＋',play_arrow:'▸',refresh:'⟳',restart_alt:'⟲',save:'存',schedule:'◷',security:'盾',settings:'设',show_chart:'∿',storage:'▥',sync_alt:'⇄',table_chart:'▦',table_view:'▦',terminal:'❯',translate:'文',warning:'警'};
  var active=false;
  function rep(el){
    if(!el||el.nodeType!==1)return;
    if(el.classList&&el.classList.contains('material-symbols-outlined')){
      var k=(el.textContent||'').trim();
      if(MAP[k]&&el.textContent!==MAP[k])el.textContent=MAP[k];
    }
    if(el.querySelectorAll){
      var list=el.querySelectorAll('.material-symbols-outlined');
      for(var i=0;i<list.length;i++){
        var k2=(list[i].textContent||'').trim();
        if(MAP[k2]&&list[i].textContent!==MAP[k2])list[i].textContent=MAP[k2];
      }
    }
  }
  function charFallback(){
    if(active||!ALLOW_FALLBACK)return;
    active=true;
    document.documentElement.classList.add('no-icon-font');
    if(document.body)rep(document.body);
    if(window.MutationObserver){
      new MutationObserver(function(ms){
        for(var i=0;i<ms.length;i++){
          var ns=ms[i].addedNodes;
          for(var j=0;j<ns.length;j++)rep(ns[j]);
        }
      }).observe(document.body,{childList:true,subtree:true});
    }
  }
  function familyReady(){
    if(!document.fonts||!document.fonts.load)return Promise.resolve(false);
    return document.fonts.load('24px "Material Symbols Outlined"').then(function(faces){
      return !!(faces&&faces.length);
    }).catch(function(){return false;});
  }
  function addCss(href){
    return new Promise(function(done){
      var l=document.createElement('link');
      l.rel='stylesheet';l.href=href;
      l.onload=function(){done(true);};
      l.onerror=function(){done(false);};
      document.head.appendChild(l);
    });
  }
  function tryMirror(i){
    if(i>=MIRRORS.length){charFallback();return;}
    addCss(MIRRORS[i]).then(function(ok){
      if(!ok){tryMirror(i+1);return;}
      familyReady().then(function(ready){if(!ready)tryMirror(i+1);});
    });
  }
  // localFont: 本地已放置字体文件 → @font-face 会直接生效，无需再补救
  function boot(){
    if(HAS_LOCAL)return;
    familyReady().then(function(ready){if(!ready)tryMirror(0);});
  }
  // 等 load 之后再判断：此时主 CDN 的 <link> 已确定成败，避免刚打开就误判
  if(document.readyState==='complete')boot();
  else window.addEventListener('load',boot);
})();
</script>
`
