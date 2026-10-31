package main

import (
	"fmt"
	"net/http"
	"strings"
)

// renderApp 渲染统一应用页面（侧边栏布局，监控 + 数据管理两个视图）。
func renderApp(w http.ResponseWriter, db *DB, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板样式内嵌在 HTML 里：升级后必须回源，否则浏览器会拿缓存里的旧配色
	w.Header().Set("Cache-Control", "no-store")
	html := strings.Replace(appHTML, "__APP_PAGE__", page, 1)
	html = strings.Replace(html, "/*__I18N__*/", cachedDashDictJS(), 1)
	html = strings.Replace(html, "/*__PORTS__*/", buildPortsJS(db), 1)
	// 图标字体：本地优先 + 缺字体时降级（见 panel_icons.go）
	fmt.Fprint(w, injectPanelAssets(html))
}

// buildPortsJS 生成前端端口默认值 JS，避免与 Go 侧默认端口重复维护。
func buildPortsJS(db *DB) string {
	cfg := db.configSnapshot()
	return fmt.Sprintf("var PORTS={binary:%d,metrics:%d,mysql:%d};", cfg.Port, cfg.MetricsPort, cfg.MySQLPort)
}

// appHTML is the complete single-page application: sidebar navigation, real-time monitoring,
// data management, user management, and settings — all in one embedded HTML document.
// 数据管理：表列表、分页数据、SQL 控制台、新建表；访问需登录（token）。
const appHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Tsumugi · 控制台</title>
<script>try{if(localStorage.getItem('tsumugi_tone')==='hc')document.documentElement.classList.add('hc');}catch(e){}</script>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto+Flex:opsz,wght,wdth@8..144,300..800,75..125&display=swap">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Fira+Code:wght@400;500;600;700&display=swap">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0">
<style>
  :root{
    --pri:#E8607C;--on-pri:#FFFFFF;--pri-ctn:#FFF0F3;--on-pri-ctn:#8B1A3A;
    --sec:#8D6E73;--on-sec:#FFFFFF;--sec-ctn:#F3E5E8;--on-sec-ctn:#4A3538;
    --ter:#F48FB1;--on-ter:#FFFFFF;--ter-ctn:#FCE4EC;--on-ter-ctn:#8B1A3A;
    --err:#B3261E;--on-err:#FFFFFF;--err-ctn:#F9DEDC;--on-err-ctn:#410E0B;
    --sf:#FFFAF9;--on-sf:#2D2028;--sf-dim:#F5E6EA;--sf-bri:#FFFAF9;
    --sf-var:#F0E0E6;--on-sf-var:#6D5D63;
    --ol:#9E8E96;--ol-var:#E0D4D8;
    --sc0:#FFFFFF;--sc1:#FFF5F7;--sc2:#FFF0F3;
    --sc3:#FCE4EC;--sc4:#F8BBD0;
    --inv-sf:#3D2832;--on-inv-sf:#F5E6EA;--inv-pri:#F48FB1;
    --scrim:#000000;--shd:#000000;
    --pri-10:#5C1228;--pri-20:#7B1A38;--pri-30:#A32050;--pri-40:#E8607C;
    --pri-80:#F8A4B8;--pri-90:#FCCDD6;--pri-95:#FFE4EB;--pri-99:#FFFAF9;
    --sec-10:#2D2025;--sec-20:#3D3035;--sec-30:#5D4D53;--sec-40:#8D6E73;
    --sec-80:#C9ADA2;--sec-90:#E8D5DB;--sec-95:#F3E5E8;
    --ter-10:#5C1228;--ter-20:#7B1A38;--ter-30:#A32050;--ter-40:#D84A6A;
    --ter-80:#F8A4B8;--ter-90:#FCCDD6;--ter-95:#FFE4EB;
    --dur-s1:50ms;--dur-s2:100ms;--dur-s3:150ms;--dur-s4:200ms;
    --dur-m1:250ms;--dur-m2:300ms;--dur-m3:350ms;--dur-m4:400ms;
    --dur-l1:450ms;--dur-l2:500ms;--dur-l3:550ms;--dur-l4:600ms;
    --dur-xl:800ms;
    --ease-std:cubic-bezier(0.2,0,0,1);
    --ease-std-dec:cubic-bezier(0,0,0,1);
    --ease-std-acc:cubic-bezier(0.3,0,1,1);
    --ease-emph:cubic-bezier(0.2,0,0,1);
    --ease-emph-dec:cubic-bezier(0.05,0.7,0.1,1);
    --ease-emph-acc:cubic-bezier(0.3,0,0.8,0.15);
    --ease-spring:linear;
    --r-none:0px;--r-xs:4px;--r-sm:8px;--r-md:12px;
    --r-lg:16px;--r-xl:28px;--r-full:9999px;
    --acc:#FF6B9D;--acc-soft:rgba(255,107,157,.14);
    /* 高对比强调色：图表/指标/状态用，避免整页只有粉色系 */
    --c-red:#DC2626;--c-amber:#B45309;--c-green:#15803D;--c-cyan:#0E7490;--c-blue:#1D4ED8;--c-violet:#6D28D9;
    --c-red-line:#E5484D;--c-amber-line:#F59E0B;--c-green-line:#16A34A;--c-cyan-line:#0E9384;--c-blue-line:#2563EB;--c-violet-line:#7C3AED;
    /* 高对比色：近黑 ↔ 纯白（对 --sf 对比度 ≈ 18:1，AAA） */
    --c-hc:#121014;--c-on-hc:#FFFFFF;
    --glow:0 10px 34px rgba(232,96,124,.14);
  }
  @media(prefers-color-scheme:dark){:root{
    --pri:#F8A4B8;--on-pri:#5C1228;--pri-ctn:#B33060;--on-pri-ctn:#FFE4EB;
    --sec:#C9ADA2;--on-sec:#3D3035;--sec-ctn:#6D5060;--on-sec-ctn:#F3E5E8;
    --ter:#F8A4B8;--on-ter:#5C1228;--ter-ctn:#9C3060;--on-ter-ctn:#FFE4EB;
    --err:#F2B8B5;--on-err:#601410;--err-ctn:#8C1D18;--on-err-ctn:#F9DEDC;
    --sf:#1A1215;--on-sf:#F0E0E6;--sf-dim:#1A1215;--sf-bri:#2D2025;
    --sf-var:#3D2D35;--on-sf-var:#D4B8C0;
    --ol:#9E8E96;--ol-var:#3D2D35;
    --sc0:#120D10;--sc1:#1E1619;--sc2:#231A1E;
    --sc3:#2D2025;--sc4:#382D32;
    --inv-sf:#F0E0E6;--on-inv-sf:#3D2832;--inv-pri:#E8607C;
    --c-red:#FCA5A5;--c-amber:#FCD34D;--c-green:#86EFAC;--c-cyan:#5EEAD4;--c-blue:#93C5FD;--c-violet:#C4B5FD;
    --c-hc:#FFFFFF;--c-on-hc:#121014;
  --pri-20:#5C1228;--pri-90:#FFE4EB;
  }}
  *{margin:0;padding:0;box-sizing:border-box}
  body{font-family:'Roboto Flex','Roboto',system-ui,sans-serif;color:var(--on-sf);min-height:100vh;background:radial-gradient(1200px 620px at 78% -8%,rgba(232,96,124,.12),transparent 62%),radial-gradient(900px 560px at 8% 108%,var(--acc-soft),transparent 58%),radial-gradient(760px 460px at 96% 88%,rgba(37,99,235,.10),transparent 62%),radial-gradient(680px 420px at 46% 112%,rgba(124,58,237,.09),transparent 60%),var(--sf);font-size:14px;line-height:1.5;font-optical-sizing:auto;-webkit-font-smoothing:antialiased}
  .mono{font-family:'Fira Code','Roboto Flex',monospace;font-variant-numeric:tabular-nums}
  .layout{display:flex;min-height:100vh;gap:16px;padding:18px}
  .side{width:272px;flex-shrink:0;border-radius:var(--r-xl);padding:18px 10px;position:sticky;top:18px;height:calc(100vh - 36px);display:flex;flex-direction:column;gap:2px;background:linear-gradient(180deg,var(--sc3),var(--sc2));border:none;box-shadow:0 1px 2px rgba(0,0,0,.03),0 8px 32px rgba(0,0,0,.06);overflow:hidden}
  .side::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .brand{display:flex;align-items:center;gap:14px;padding:12px 16px 20px}
  .brand .logo{width:48px;height:48px;border-radius:var(--r-lg);display:grid;place-items:center;background:linear-gradient(135deg,var(--pri-20),var(--pri) 55%,var(--ter));color:var(--on-pri);box-shadow:0 2px 6px rgba(0,0,0,.16),0 6px 18px rgba(232,96,124,.30),inset 0 1px 0 rgba(255,255,255,.28);transition:transform var(--dur-m2) var(--ease-emph)}
  .brand .logo:hover{transform:scale(1.06) rotate(-3deg)}
  .brand .logo .mat{font-size:26px}
  .brand h1{font-size:22px;font-weight:800;letter-spacing:-.3px;line-height:1.1;color:var(--on-sf);font-variation-settings:'wght' 800,'wdth' 100}
  .brand small{display:block;font-size:11px;font-weight:600;color:var(--pri);letter-spacing:.8px;text-transform:uppercase;margin-top:2px;font-variation-settings:'wght' 600,'wdth' 90}
  .nav{padding:12px 16px;border-radius:var(--r-full);display:flex;align-items:center;gap:14px;font-size:14px;font-weight:500;color:var(--on-sf-var);cursor:pointer;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .nav::before{content:'';position:absolute;inset:0;border-radius:inherit;background:var(--on-sf);opacity:0;transition:opacity var(--dur-s4) var(--ease-std)}
  .nav:hover::before{opacity:.08}
  .nav.active{background:var(--sec-ctn);color:var(--on-sec-ctn);font-weight:600;box-shadow:0 1px 2px rgba(0,0,0,.1)}
  .nav .mat{font-size:22px;position:relative;z-index:1}
  .side .spacer{flex:1}
  .side-foot{font-size:11px;color:var(--ol);padding:12px 16px;border-top:1px solid var(--ol-var)}
  .main{flex:1;min-width:0;padding:6px 10px 40px;max-width:1440px}
  .topbar{display:flex;align-items:flex-end;justify-content:space-between;margin-bottom:22px;flex-wrap:wrap;gap:16px}
  .topbar h2{font-size:30px;font-weight:800;letter-spacing:-.6px;display:flex;align-items:center;gap:12px;color:var(--on-sf);font-variation-settings:'wght' 800,'wdth' 100}
  .topbar h2 .mat{color:var(--pri);font-size:30px}
  .topbar .sub{color:var(--on-sf-var);font-size:13.5px;margin-top:3px;line-height:1.4}
  .hero-strip{display:grid;grid-template-columns:auto 1fr 1fr 1fr;gap:14px;align-items:stretch;margin-bottom:20px;padding:18px 22px;background:linear-gradient(120deg,var(--sc1),var(--sc2));border:none;position:relative;overflow:hidden;border-radius:var(--r-xl);box-shadow:var(--glow);animation:viewIn .5s var(--ease-emph-dec)}
  .hero-strip::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .hero-strip::before{content:'';position:absolute;inset:0;background:radial-gradient(420px 200px at 92% -10%,var(--acc-soft),transparent 62%)}
  .hs-brand{display:flex;align-items:center;gap:14px;min-width:220px;position:relative;z-index:1}
  .hs-brand .hs-icon{width:52px;height:52px;border-radius:var(--r-lg);display:grid;place-items:center;background:linear-gradient(135deg,var(--pri-20),var(--pri) 60%,var(--ter));color:var(--on-pri);box-shadow:0 4px 12px rgba(232,96,124,.30)}
  .hs-brand .hs-icon .mat{font-size:26px}
  .hs-brand b{font-size:16px;font-weight:800;display:flex;align-items:center;gap:8px;color:var(--on-sf)}
  .hs-brand .dot{width:9px;height:9px;border-radius:50%;background:var(--acc);box-shadow:0 0 9px var(--acc);animation:pulse 1.8s ease-in-out infinite}
  @keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
  .hs-brand small{display:block;font-size:11.5px;color:var(--on-sf-var);margin-top:2px}
  .hs-item{position:relative;padding:10px 16px;border-radius:var(--r-lg);display:flex;flex-direction:column;justify-content:center;gap:3px;background:var(--sc0);z-index:1}
  .hs-item::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .hs-item b{font-size:19px;font-weight:800;color:var(--on-sf);font-variation-settings:'wght' 800}
  .hs-item b .u{font-size:11px;font-weight:600;color:var(--on-sf-var);margin-left:4px}
  .hs-item span{font-size:11px;font-weight:600;color:var(--on-sf-var);letter-spacing:.5px;text-transform:uppercase}
  @media(max-width:900px){.hero-strip{grid-template-columns:1fr 1fr}}
  @media(max-width:560px){.hero-strip{grid-template-columns:1fr}}
  .btn{display:inline-flex;align-items:center;gap:8px;border:none;cursor:pointer;padding:12px 28px;border-radius:var(--r-full);font-size:14px;font-weight:600;letter-spacing:.1px;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden;font-family:inherit}
  .btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--dur-s4) var(--ease-std)}
  .btn:hover::before{opacity:.08}
  .btn:active::before{opacity:.12}
  .btn-fill{background:var(--pri);color:var(--on-pri);box-shadow:0 1px 3px rgba(0,0,0,.15),0 2px 6px rgba(232,96,124,.15)}
  .btn-fill:hover{box-shadow:0 2px 6px rgba(0,0,0,.2),0 4px 12px rgba(232,96,124,.25);transform:translateY(-1px)}
  .btn-fill:active{transform:translateY(0);box-shadow:0 1px 2px rgba(0,0,0,.15)}
  .btn-tonal{background:var(--sec-ctn);color:var(--on-sec-ctn);box-shadow:0 1px 2px rgba(0,0,0,.1)}
  .btn-tonal:hover{box-shadow:0 2px 6px rgba(0,0,0,.15);transform:translateY(-1px)}
  .btn-text{background:transparent;color:var(--pri)}
  .btn-outline{background:transparent;color:var(--pri);border:1px solid var(--ol)}
  .btn-danger{background:var(--err-ctn);color:var(--on-err-ctn)}
  .btn:disabled{opacity:.38;cursor:not-allowed;box-shadow:none;transform:none}
  .card{background:linear-gradient(180deg,var(--sc1),var(--sc0));border-radius:24px;padding:24px;margin-bottom:20px;border:none;transition:transform var(--dur-m2) var(--ease-emph),box-shadow var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .card::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .card::before{content:'';position:absolute;top:0;left:10%;right:10%;height:1px;background:linear-gradient(90deg,transparent,rgba(232,96,124,.4),transparent);opacity:.5}
  .card:hover{transform:translateY(-2px);box-shadow:var(--glow)}
  .card h3{font-size:15.5px;font-weight:700;display:flex;align-items:center;gap:10px;margin-bottom:16px;color:var(--on-sf);letter-spacing:-.1px}
  .card h3 .mat{color:var(--pri);font-size:22px}
  .rings{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:14px}
  .ring-card{background:var(--sc0);border-radius:20px;padding:20px 12px;text-align:center;cursor:pointer;border:none;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .ring-card::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .ring-card:hover{transform:translateY(-5px);box-shadow:0 6px 16px rgba(232,96,124,.12),0 14px 34px rgba(232,96,124,.08)}
  .ring-card:active{transform:translateY(-1px)}
  .ring-card .ring-wrap{width:100px;height:100px;margin:0 auto;position:relative}
  .ring-wrap svg{width:100%;height:100%}
  .ring-track{fill:none;stroke:var(--sf-var);stroke-width:8;opacity:.6}
  .ring-fill{fill:none;stroke-width:8;stroke-linecap:round;stroke-dasharray:327;stroke-dashoffset:327;transform:rotate(-90deg);transform-origin:center;transition:stroke-dashoffset var(--dur-l4) var(--ease-emph)}
  .ring-center{position:absolute;inset:0;display:flex;flex-direction:column;align-items:center;justify-content:center}
  .ring-value{font-size:22px;font-weight:800;line-height:1;color:var(--on-sf);font-variation-settings:'wght' 800;font-family:'Fira Code',monospace}
  .ring-value small{font-size:11px;color:var(--on-sf-var);font-weight:600;margin-left:1px}
  .ring-unit{font-size:11px;color:var(--ol);font-weight:500;margin-top:3px;font-family:'Fira Code',monospace}
  .ring-label{font-size:13px;font-weight:600;margin-top:12px;display:flex;align-items:center;justify-content:center;gap:6px;color:var(--on-sf-var)}
  .ring-sub{font-size:11px;color:var(--ol);margin-top:4px}
  .chips{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:14px;margin-bottom:20px}
  .chip{background:var(--sc0);border-radius:18px;padding:15px 18px;display:flex;align-items:center;gap:14px;border:none;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .chip::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .chip:hover{transform:translateY(-3px);box-shadow:0 8px 22px rgba(0,0,0,.08)}
  .chip .mat{font-size:24px;color:var(--pri)}
 /* 高对比强调色：图标/指示点（.chip .mat 权重更高，需单独覆盖） */
 .ic-red{color:var(--c-red)}.ic-amber{color:var(--c-amber)}.ic-green{color:var(--c-green)}.ic-cyan{color:var(--c-cyan)}.ic-blue{color:var(--c-blue)}.ic-violet{color:var(--c-violet)}
 .chip .mat.ic-red{color:var(--c-red)}.chip .mat.ic-amber{color:var(--c-amber)}.chip .mat.ic-green{color:var(--c-green)}.chip .mat.ic-cyan{color:var(--c-cyan)}.chip .mat.ic-blue{color:var(--c-blue)}.chip .mat.ic-violet{color:var(--c-violet)}
 .dot-red{background:var(--c-red-line)}.dot-amber{background:var(--c-amber-line)}.dot-green{background:var(--c-green-line)}.dot-cyan{background:var(--c-cyan-line)}.dot-blue{background:var(--c-blue-line)}.dot-violet{background:var(--c-violet-line)}
 /* 高对比色 --c-hc：近黑/纯白对（浅底 18:1，深底 18:1），用于强调数字与状态 */
 .chip .c-value{color:var(--c-hc)}
 .chip:has(.ic-blue){box-shadow:inset 4px 0 0 var(--c-blue)}
 .chip:has(.ic-red){box-shadow:inset 4px 0 0 var(--c-hc)}
 .chip:has(.ic-amber){box-shadow:inset 4px 0 0 var(--c-amber)}
 .chip:has(.ic-cyan){box-shadow:inset 4px 0 0 var(--c-cyan)}
 .chip:has(.ic-violet){box-shadow:inset 4px 0 0 var(--c-violet)}
 .chip:has(.ic-green){box-shadow:inset 4px 0 0 var(--c-green)}
 .nav.active{box-shadow:inset 4px 0 0 var(--c-hc),0 1px 2px rgba(0,0,0,.1)}
  .chip .c-label{font-size:11px;color:var(--ol);font-weight:600;letter-spacing:.6px;text-transform:uppercase}
  .chip .c-value{font-size:18px;font-weight:800;color:var(--on-sf);font-variation-settings:'wght' 800;font-family:'Fira Code',monospace}
  .chip .c-mini{font-size:11px;color:var(--ol);font-weight:500}
  .up{color:#1E7B4C}.down{color:var(--err)}
  .data-wrap{overflow-x:auto}
  table.grid{width:100%;border-collapse:collapse;font-size:13.5px}
  table.grid th{text-align:left;padding:12px 14px;font-size:11px;font-weight:700;color:var(--on-sf-var);text-transform:uppercase;letter-spacing:.6px;border-bottom:2px solid var(--ol-var);white-space:nowrap}
  table.grid td{padding:12px 14px;border-bottom:1px solid var(--sf-var);white-space:nowrap;color:var(--on-sf)}
  table.grid tr{transition:background var(--dur-s3) var(--ease-std)}
  table.grid tr:hover td{background:var(--sc2)}
  .pk-badge{font-size:10px;font-weight:700;color:var(--pri);background:var(--pri-ctn);padding:3px 10px;border-radius:var(--r-full);margin-left:8px;letter-spacing:.3px}
  .pager{display:flex;align-items:center;gap:14px;margin-top:14px}
  .pager .info{font-size:12px;color:var(--ol);flex:1}
  .icon-btn{width:40px;height:40px;border-radius:var(--r-md);border:none;cursor:pointer;display:grid;place-items:center;background:var(--sec-ctn);color:var(--on-sec-ctn);transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .icon-btn::before{content:'';position:absolute;inset:0;border-radius:inherit;background:currentColor;opacity:0;transition:opacity var(--dur-s4) var(--ease-std)}
  .icon-btn:hover::before{opacity:.08}
  .icon-btn:hover{box-shadow:0 1px 3px rgba(0,0,0,.1)}
  .icon-btn:disabled{opacity:.38;cursor:not-allowed;box-shadow:none}
  .field-label{font-size:12px;font-weight:600;color:var(--on-sf-var);margin-bottom:6px;display:block;letter-spacing:.4px}
  .txt{width:100%;padding:14px 16px;border-radius:var(--r-md);border:1px solid var(--ol);font-size:14px;background:var(--sc0);color:var(--on-sf);transition:all var(--dur-m2) var(--ease-emph);font-family:inherit}
  .txt:focus{outline:none;border-color:var(--pri);box-shadow:0 0 0 2px var(--pri-90)}
  .sql-box{width:100%;min-height:120px;border-radius:var(--r-md);border:1px solid var(--ol);padding:14px 16px;font-size:13px;font-family:'Fira Code',Consolas,monospace;resize:vertical;background:var(--sc0);color:var(--on-sf);transition:all var(--dur-m2) var(--ease-emph);line-height:1.6}
  .sql-box:focus{outline:none;border-color:var(--pri);box-shadow:0 0 0 2px var(--pri-90)}
  .result-msg{font-size:13px;font-weight:600}.ok{color:#006D3B}.err{color:var(--err)}.err-msg{color:var(--err);font-size:13px;font-weight:600}
  .empty{text-align:center;color:var(--ol);padding:32px;font-size:14px}
  .tbl-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:12px}
  .tbl-card{background:var(--sc0);border-radius:var(--r-lg);padding:14px 16px;cursor:pointer;border:none;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .tbl-card::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .tbl-card:hover{transform:translateY(-2px);box-shadow:0 2px 8px rgba(232,96,124,.10)}
  .tbl-card:active{transform:translateY(0)}
  .tbl-card .name{font-size:14px;font-weight:700;display:flex;align-items:center;gap:8px;color:var(--on-sf)}
  .tbl-card .name .mat{color:var(--pri);font-size:18px}
  .tbl-card .meta{font-size:11px;color:var(--ol);margin-top:4px;line-height:1.5;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .tbl-card .count{font-size:14px;font-weight:700;color:var(--pri);margin-top:6px}
  .tbl-card .count small{font-size:11px;color:var(--ol);font-weight:500;margin-left:2px}
  .admin-layout{display:flex;gap:16px;align-items:flex-start}
  .admin-left{width:260px;flex-shrink:0;position:sticky;top:28px;background:var(--sc1);border-radius:var(--r-xl);overflow:hidden;border:none}
  .admin-left::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .admin-left-head{padding:14px 14px 0}
  .admin-left-head .txt{width:100%;padding:10px 12px;font-size:13px}
  .admin-table-list{padding:8px;max-height:calc(100vh - 160px);overflow-y:auto;display:flex;flex-direction:column;gap:2px}
  .admin-table-item{display:flex;align-items:center;gap:10px;padding:10px 14px;border-radius:var(--r-md);cursor:pointer;transition:all var(--dur-s4) var(--ease-std);position:relative}
  .admin-table-item:hover{background:var(--sc4)}
  .admin-table-item.active{background:var(--sec-ctn);color:var(--on-sec-ctn)}
  .admin-table-item .at-icon{font-size:20px;color:var(--pri)}
  .admin-table-item .at-info{flex:1;min-width:0}
  .admin-table-item .at-name{font-size:14px;font-weight:600;color:var(--on-sf);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .admin-table-item .at-meta{font-size:11px;color:var(--ol);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .admin-table-item .at-count{font-size:12px;font-weight:700;color:var(--pri);white-space:nowrap}
  .admin-right{flex:1;min-width:0;display:flex;flex-direction:column;gap:16px}
  @media(max-width:820px){.admin-layout{flex-direction:column}.admin-left{width:100%;position:static}}
  .trend-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:16px}
  .trend-card{background:var(--sc0);border-radius:var(--r-xl);padding:18px;border:none;transition:all var(--dur-m2) var(--ease-emph);position:relative;overflow:hidden}
  .trend-card::after{content:'';position:absolute;inset:0;border-radius:inherit;box-shadow:inset 0 0 0 1px var(--ol-var);pointer-events:none}
  .trend-card .t-name{font-size:12px;font-weight:700;color:var(--on-sf-var);display:flex;align-items:center;gap:8px;text-transform:uppercase;letter-spacing:.5px}
  .trend-card .t-name .dot{width:10px;height:10px;border-radius:var(--r-full);box-shadow:0 0 6px currentColor}
  .trend-card .t-val{font-size:16px;font-weight:800;margin-left:auto;color:var(--on-sf);font-variation-settings:'wght' 800}
  .trend-card svg{width:100%;height:80px;display:block;margin-top:8px}
  .overlay{position:fixed;inset:0;background:rgba(0,0,0,.5);display:none;align-items:center;justify-content:center;z-index:50;backdrop-filter:blur(4px)}
  .overlay.show{display:flex}
  .modal{background:var(--sc0);border-radius:var(--r-xl);padding:32px;width:min(640px,92vw);box-shadow:0 8px 32px rgba(0,0,0,.2),0 2px 8px rgba(0,0,0,.1);animation:modalIn var(--dur-m3) var(--ease-emph-dec)}
  @keyframes modalIn{from{opacity:0;transform:scale(.92) translateY(16px)}to{opacity:1;transform:scale(1) translateY(0)}}
  .modal h3{font-size:20px;font-weight:700;display:flex;align-items:center;gap:10px;margin-bottom:18px;color:var(--on-sf)}
  .modal .m-chart{width:100%;height:220px}
  .m-stats{display:flex;gap:20px;margin-top:16px;flex-wrap:wrap}
  .m-stat .v{font-size:24px;font-weight:800;color:var(--pri);font-variation-settings:'wght' 800}
  .m-stat .k{font-size:12px;color:var(--ol);font-weight:600;letter-spacing:.3px}
  .toast{position:fixed;bottom:28px;left:50%;transform:translateX(-50%) translateY(24px);background:var(--inv-sf);color:var(--on-inv-sf);padding:14px 28px;border-radius:var(--r-full);font-size:14px;font-weight:600;opacity:0;pointer-events:none;transition:all var(--dur-m2) var(--ease-emph-dec);box-shadow:0 4px 16px rgba(0,0,0,.2);z-index:99}
  .toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
  .toast.err{background:var(--err);color:var(--on-err)}
  .field-chip{display:inline-flex;align-items:center;gap:8px;background:var(--pri-ctn);color:var(--on-pri-ctn);font-size:12px;font-weight:600;padding:6px 14px;border-radius:var(--r-full)}
  .view{display:none}.view.show{display:block;animation:viewIn var(--dur-m3) var(--ease-emph-dec)}
  @keyframes viewIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
  .settings-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:20px}
  .divider{height:1px;background:var(--ol-var);margin:16px 0}
.tone-opt{width:44px;height:44px;border-radius:50%;cursor:pointer;border:3px solid transparent;transition:all .25s var(--ease-emph);position:relative;box-shadow:0 2px 8px rgba(0,0,0,.12)}
.tone-opt:hover{transform:scale(1.12);box-shadow:0 4px 14px rgba(0,0,0,.18)}
.tone-opt.active{border-color:var(--on-sf);box-shadow:0 0 0 3px var(--sf),0 0 0 5px var(--on-sf)}
.tone-label{font-size:11px;text-align:center;margin-top:6px;color:var(--on-sf-var);font-weight:500}
/* 高对比模式（设置 → 色调 → 高对比）：纯黑白、2px 实边、去渐变去柔影 */
html.hc{--sf:#FFFFFF;--on-sf:#000000;--sf-dim:#F2F2F2;--sf-bri:#FFFFFF;--sf-var:#1F1F1F;--on-sf-var:#1F1F1F;--ol:#000000;--ol-var:#000000;--sc0:#FFFFFF;--sc1:#F2F2F2;--sc2:#E6E6E6;--sc3:#D9D9D9;--sc4:#BFBFBF;--pri:#000000;--on-pri:#FFFFFF;--pri-ctn:#FFFFFF;--on-pri-ctn:#000000;--sec:#000000;--on-sec:#FFFFFF;--sec-ctn:#FFFFFF;--on-sec-ctn:#000000;--ter:#000000;--on-ter:#FFFFFF;--ter-ctn:#FFFFFF;--on-ter-ctn:#000000;--err:#B00020;--on-err:#FFFFFF;--err-ctn:#FFFFFF;--on-err-ctn:#B00020;--acc:#000000;--acc-soft:rgba(0,0,0,.10);--glow:none;--c-hc:#000000;--c-on-hc:#FFFFFF}
html.hc body{background:#FFFFFF}
html.hc .side,html.hc .card,html.hc .chip,html.hc .trend-card,html.hc .ring-card,html.hc .modal,html.hc .txt,html.hc select.txt{background:#FFFFFF!important;border:2px solid #000000!important;box-shadow:none!important}
html.hc .side::after{box-shadow:none!important}
html.hc .hero,html.hc .logo{background:#000000!important;color:#FFFFFF!important;box-shadow:none!important}
html.hc .btn-fill{background:#000000!important;color:#FFFFFF!important;box-shadow:none!important}
html.hc .btn-tonal{background:#FFFFFF!important;color:#000000!important;border:2px solid #000000;box-shadow:none!important}
html.hc .btn-text{color:#000000!important}
html.hc .field-label,html.hc .c-label,html.hc .side-foot,html.hc .c-mini,html.hc .sub{color:#1F1F1F!important}
html.hc .txt:focus{outline:3px solid #000000;outline-offset:1px;box-shadow:none!important}
html.hc .divider{background:#000000!important;height:2px}
html.hc .ring-track{stroke:#000000!important;opacity:.2}
html.hc .nav.active{background:#000000!important;color:#FFFFFF!important;box-shadow:none!important}
html.hc .nav.active .mat{color:#FFFFFF!important}
html.hc .chip .c-value{color:#000000}
html.hc .tone-opt.active{border-color:#000000;box-shadow:0 0 0 3px #FFF,0 0 0 5px #000}
html.hc .t-name .dot{box-shadow:none!important}
@media(max-width:820px){.layout{flex-direction:column;gap:0;padding:0}.side{width:100%;height:auto;position:static;padding:14px;border-radius:0;box-shadow:none}.main{padding:18px}}
</style>
</head>
<body>
<div class="layout">
  <aside class="side">
    <div class="brand">
      <div class="logo"><span class="mat material-symbols-outlined">database</span></div>
      <div><h1>Tsumugi</h1><small>DATABASE CONSOLE</small></div>
    </div>
    <div class="nav" id="navMonitor" data-t="navMonitor" onclick="switchView('monitor')"><span class="mat material-symbols-outlined">monitoring</span>实时监控</div>
    <div class="nav" id="navAdmin" data-t="navAdmin" onclick="switchView('admin')"><span class="mat material-symbols-outlined">table_view</span>数据管理</div>
    <div class="nav" id="navUsers" data-t="navUsers" onclick="switchView('users')"><span class="mat material-symbols-outlined">group</span>账号管理</div>
    <div class="nav" id="navSettings" data-t="navSettings" onclick="switchView('settings')"><span class="mat material-symbols-outlined">settings</span>设置</div>
    <div class="spacer"></div>
    <div class="side-foot" id="sideFoot" data-t="sideFoot">连接中…</div>
  </aside>

  <main class="main">
    <!-- ============ 监控视图 ============ -->
    <div class="view" id="view-monitor">
      <div class="topbar">
        <div><h2 id="monitorTitle" data-t="monitorTitle"><span class="mat material-symbols-outlined">monitoring</span>实时监控</h2><div class="sub" id="monitorSub" data-t="monitorSub">Tsumugi 数据库运行状态 · 点击环形指标查看曲线</div></div>
        <div>
          <button class="btn btn-text" onclick="fetchStats(true)"><span class="material-symbols-outlined">refresh</span><span data-t="btnRefresh">刷新</span></button>
        </div>
      </div>

      <div class="hero-strip">
        <div class="hs-brand">
          <div class="hs-icon"><span class="mat material-symbols-outlined">database</span></div>
          <div><b>Tsumugi <span class="dot"></span></b><small id="heroVersion">--</small></div>
        </div>
        <div class="hs-item"><b class="mono" id="heroUptime">--</b><span data-t="chUptime">运行时长</span></div>
        <div class="hs-item"><b class="mono" id="heroQps">--<span class="u">QPS</span></b><span data-t="ringQPS">QPS</span></div>
        <div class="hs-item"><b class="mono" id="heroMysql">--</b><span data-t="sysMysql">MySQL 兼容</span></div>
      </div>

      <div class="rings">
        <div class="ring-card" onclick="showCurve('cpu',t('ringCPU'),'#E5484D')">
          <div class="ring-wrap"><svg viewBox="0 0 120 120"><circle class="ring-track" cx="60" cy="60" r="52"/><circle class="ring-fill" id="ringCpu" cx="60" cy="60" r="52"/></svg>
            <div class="ring-center"><div class="ring-value" id="cpuVal">--</div><div class="ring-unit">%</div></div></div>
          <div class="ring-label"><span class="mat material-symbols-outlined" style="font-size:18px">memory</span><span data-t="ringCPU">CPU 使用率</span></div>
          <div class="ring-sub" id="cpuSub">--</div>
        </div>
        <div class="ring-card" onclick="showCurve('mem',t('ringMem'),'#0E9384')">
          <div class="ring-wrap"><svg viewBox="0 0 120 120"><circle class="ring-track" cx="60" cy="60" r="52"/><circle class="ring-fill" id="ringMem" cx="60" cy="60" r="52"/></svg>
            <div class="ring-center"><div class="ring-value" id="memVal">--</div><div class="ring-unit">%</div></div></div>
          <div class="ring-label"><span class="mat material-symbols-outlined" style="font-size:18px">memory_alt</span><span data-t="ringMem">内存占用</span></div>
          <div class="ring-sub" id="memSub">--</div>
        </div>
        <div class="ring-card" onclick="showCurve('qps',t('ringQPS'),'#E8607C')">
          <div class="ring-wrap"><svg viewBox="0 0 120 120"><circle class="ring-track" cx="60" cy="60" r="52"/><circle class="ring-fill" id="ringQps" cx="60" cy="60" r="52"/></svg>
            <div class="ring-center"><div class="ring-value" id="qpsVal">--</div><div class="ring-unit">/s</div></div></div>
          <div class="ring-label"><span class="mat material-symbols-outlined" style="font-size:18px">bolt</span><span data-t="ringQPS">QPS</span></div>
          <div class="ring-sub" id="qpsSub">--</div>
        </div>
        <div class="ring-card" onclick="showCurve('tps',t('ringTPS'),'#2F6DF6')">
          <div class="ring-wrap"><svg viewBox="0 0 120 120"><circle class="ring-track" cx="60" cy="60" r="52"/><circle class="ring-fill" id="ringTps" cx="60" cy="60" r="52"/></svg>
            <div class="ring-center"><div class="ring-value" id="tpsVal">--</div><div class="ring-unit">/s</div></div></div>
          <div class="ring-label"><span class="mat material-symbols-outlined" style="font-size:18px">sync_alt</span><span data-t="ringTPS">TPS</span></div>
          <div class="ring-sub" id="tpsSub">--</div>
        </div>
        <div class="ring-card" onclick="showCurve('disk',t('ringDisk'),'#00696D')">
          <div class="ring-wrap"><svg viewBox="0 0 120 120"><circle class="ring-track" cx="60" cy="60" r="52"/><circle class="ring-fill" id="ringDisk" cx="60" cy="60" r="52"/></svg>
            <div class="ring-center"><div class="ring-value" id="diskVal">--</div><div class="ring-unit">MB/s</div></div></div>
          <div class="ring-label"><span class="mat material-symbols-outlined" style="font-size:16px">storage</span><span data-t="ringDisk">写入硬盘</span></div>
          <div class="ring-sub" id="diskSub">--</div>
        </div>
      </div>

      <div class="chips">
        <div class="chip"><span class="mat material-symbols-outlined ic-blue">database</span><div><div class="c-label" data-t="chTotal">总命令</div><div class="c-value" id="totalCmds">-</div></div><div class="c-mini" id="totalCmdsRate"></div></div>
        <div class="chip"><span class="mat material-symbols-outlined ic-red">warning</span><div><div class="c-label" data-t="chErr">错误</div><div class="c-value" id="totalErrs">-</div></div><div class="c-mini" id="totalErrsRate"></div></div>
        <div class="chip"><span class="mat material-symbols-outlined ic-amber">local_fire_department</span><div><div class="c-label" data-t="chTopCmd">最热命令</div><div class="c-value" id="topCmd">-</div></div><div class="c-mini" id="topCmdCount"></div></div>
        <div class="chip"><span class="mat material-symbols-outlined ic-cyan">schedule</span><div><div class="c-label" data-t="chUptime">运行时长</div><div class="c-value" id="uptime">-</div></div><div class="c-mini" data-t="uptime">已运行</div></div>
        <div class="chip"><span class="mat material-symbols-outlined ic-violet">dns</span><div><div class="c-label" data-t="chThreads">协程 / 核</div><div class="c-value" id="goroutines">-</div></div><div class="c-mini" id="numCpu"></div></div>
        <div class="chip"><span class="mat material-symbols-outlined ic-green">save</span><div><div class="c-label" data-t="chDura">持久化</div><div class="c-value" id="durability">-</div></div><div class="c-mini" id="flushInfo"></div></div>
      </div>

      <div class="card">
        <h3 data-t="trendTitle"><span class="mat material-symbols-outlined">show_chart</span>实时趋势（最近 60s）</h3>
        <div class="trend-grid">
          <div class="trend-card"><div class="t-name"><span class="dot dot-violet"></span>QPS<div class="t-val" id="trQps">--</div></div><svg id="svgQps"></svg></div>
          <div class="trend-card"><div class="t-name"><span class="dot dot-blue"></span>TPS<div class="t-val" id="trTps">--</div></div><svg id="svgTps"></svg></div>
          <div class="trend-card"><div class="t-name"><span class="dot dot-red"></span>CPU %<div class="t-val" id="trCpu">--</div></div><svg id="svgCpu"></svg></div>
          <div class="trend-card"><div class="t-name" data-t="trendDisk"><span class="dot dot-amber"></span>磁盘 MB/s<div class="t-val" id="trDisk">--</div></div><svg id="svgDisk"></svg></div>
        </div>
      </div>

      <div class="card">
        <h3 data-t="infoTitle"><span class="mat material-symbols-outlined">info</span>系统信息</h3>
        <table class="grid" id="sysTable"></table>
      </div>

      <div class="card">
        <h3 data-t="cmdTitle"><span class="mat material-symbols-outlined">list</span>命令明细</h3>
        <table class="grid"><thead><tr><th data-t="thCmd">命令</th><th data-t="thTotal">累计</th><th data-t="thDD">QPS 增量</th></tr></thead><tbody id="cmdTable"></tbody></table>
      </div>

      <div class="card">
        <h3 data-t="stressTitle"><span class="mat material-symbols-outlined">bolt</span>压力测试</h3>
        <div style="display:flex;gap:12px;flex-wrap:wrap;align-items:center">
          <div><label class="field-label" data-t="lblWorkers">并发数</label><input class="txt" type="number" id="workers" value="4" min="1" max="100" style="width:110px"></div>
          <div><label class="field-label" data-t="lblDuration">时长(秒)</label><input class="txt" type="number" id="duration" value="10" min="1" max="300" style="width:110px"></div>
          <div><label class="field-label" data-t="lblMode">负载模式</label><select class="txt" id="stressMode" style="width:140px">
            <option value="rw" data-t="modeRW">读写 (7:3)</option>
            <option value="read" data-t="modeRead">纯读取</option>
            <option value="write" data-t="modeWrite">纯写入</option>
            <option value="point" data-t="modePoint">点查询</option>
            <option value="range" data-t="modeRange">范围查询</option>
          </select></div>
          <div><label class="field-label" data-t="lblProtocol">压测协议</label><select class="txt" id="stressProto" style="width:140px">
            <option value="native" data-t="protoNative">原生协议</option>
            <option value="mysql" data-t="protoMysql">MySQL 协议</option>
          </select></div>
          <div style="margin-top:20px"><button class="btn btn-fill" onclick="startStress()"><span class="material-symbols-outlined">play_arrow</span><span data-t="btnRunStress">开始压测</span></button></div>
          <span class="result-msg" id="stressStatus" style="margin-top:20px" data-t="stressReady">就绪</span>
        </div>
      </div>
    </div>

    <!-- ============ 数据管理视图 ============ -->
    <div class="view" id="view-admin">
      <div class="topbar">
        <div><h2 id="adminTitle" data-t="adminTitle"><span class="mat material-symbols-outlined">table_view</span>数据管理</h2><div class="sub" id="adminSub" data-t="adminSub">浏览表结构与数据，执行 SQL 语句</div></div>
        <div>
          <button class="btn btn-text" onclick="refreshTables()"><span class="material-symbols-outlined">refresh</span><span data-t="btnRefresh">刷新</span></button>
          <button class="btn btn-tonal" onclick="openDbManager()"><span class="material-symbols-outlined">database</span><span data-t="dbBtnManage">数据库管理</span></button>
          <button class="btn btn-fill" onclick="showCreate()"><span class="material-symbols-outlined">add</span><span data-t="btnNewTable">新建表</span></button>
        </div>
      </div>

      <div class="admin-layout">
        <!-- 左侧面板：数据库选择 + 表列表 -->
        <div class="admin-left">
          <div class="admin-left-head">
            <select class="txt" id="dbSelect" onchange="selectDB()"></select>
          </div>
          <div class="admin-table-list" id="tableList"><div class="empty" data-t="loading">加载中…</div></div>
        </div>

        <!-- 右侧面板：数据 + SQL -->
        <div class="admin-right">
          <div class="card" id="dataCard">
            <h3><span class="mat material-symbols-outlined">grid_on</span><span id="dataTitle">← 选择左侧表查看数据</span></h3>
            <div class="data-wrap"><table class="grid" id="dataGrid"></table></div>
            <div class="pager"><span class="info" id="pageInfo"></span>
              <button class="icon-btn" id="prevBtn" onclick="pagePrev()"><span class="material-symbols-outlined">chevron_left</span></button>
              <button class="icon-btn" id="nextBtn" onclick="pageNext()"><span class="material-symbols-outlined">chevron_right</span></button>
            </div>
          </div>

          <div class="card">
            <h3 data-t="sqlTitle"><span class="mat material-symbols-outlined">terminal</span>SQL 控制台</h3>
            <textarea class="sql-box" id="sqlBox" spellcheck="false" data-ph="sqlPlaceholder" placeholder="SELECT * FROM users WHERE id = 1;  或  SHOW TABLES;  或  INSERT INTO users (id,name,age) VALUES (10,'Tom',20);"></textarea>
            <div style="display:flex;gap:12px;margin-top:12px;align-items:center">
              <button class="btn btn-fill" onclick="runSQL()"><span class="material-symbols-outlined">play_arrow</span><span data-t="btnRun">执行</span></button>
              <button class="btn btn-text" onclick="document.getElementById('sqlBox').value=''"><span class="material-symbols-outlined">backspace</span><span data-t="btnClear">清空</span></button>
              <span class="result-msg" id="sqlMsg"></span>
            </div>
            <div class="data-wrap" style="margin-top:14px"><table class="grid" id="sqlResult"></table></div>
          </div>

          <div class="card" id="createCard" style="display:none">
            <h3 data-t="createTitle"><span class="mat material-symbols-outlined">add_table</span>新建表</h3>
            <div style="display:flex;gap:12px;margin-bottom:14px;align-items:flex-end">
              <div style="flex:1"><label class="field-label" data-t="lblTableName">表名</label><input class="txt" id="newTableName" placeholder="users"></div>
              <div style="width:180px"><label class="field-label" data-t="lblPkField">主键字段</label><input class="txt" id="newTablePk" placeholder="id"></div>
            </div>
            <div style="display:flex;gap:8px;align-items:center;margin-bottom:12px;flex-wrap:wrap">
              <span class="field-chip" data-t="lblColDef">列定义</span><span style="flex:1"></span>
              <button class="btn btn-tonal" onclick="addFieldRow()"><span class="material-symbols-outlined">add</span><span data-t="btnAddCol">加列</span></button>
            </div>
            <div id="fieldRows"></div>
            <div style="margin-top:16px;display:flex;gap:10px">
              <button class="btn btn-fill" onclick="createTable()"><span class="material-symbols-outlined">check</span><span data-t="btnCreateTable">创建表</span></button>
              <button class="btn btn-text" onclick="document.getElementById('createCard').style.display='none'"><span data-t="btnCancel">取消</span></button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- ============ 账号管理视图 ============ -->
    <div class="view" id="view-users">
      <div class="topbar">
        <div><h2><span class="mat material-symbols-outlined">group</span><span data-t="usersTitle">账号管理</span></h2><div class="sub" data-t="usersSub">管理系统用户与权限配置</div></div>
        <div>
          <button class="btn btn-text" onclick="loadUsers()"><span class="material-symbols-outlined">refresh</span><span data-t="btnRefresh">刷新</span></button>
          <button class="btn btn-fill" onclick="showAddUser()"><span class="material-symbols-outlined">person_add</span><span data-t="uAdd">添加用户</span></button>
        </div>
      </div>

      <div class="card" id="addUserCard" style="display:none">
        <h3><span class="mat material-symbols-outlined">person_add</span><span data-t="uAddTitle">添加用户</span></h3>
        <div style="display:flex;gap:14px;flex-wrap:wrap;align-items:flex-end">
          <div style="flex:1;min-width:140px"><label class="field-label" data-t="uUserName">用户名</label><input class="txt" id="auName" placeholder="username"></div>
          <div style="flex:1;min-width:140px"><label class="field-label" data-t="uPassPh">密码</label><input class="txt" id="auPass" type="password" placeholder="至少 6 位"></div>
          <div style="display:flex;gap:16px;align-items:center;padding-bottom:4px">
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="auAdmin"> <span data-t="uAdmin">管理员</span></label>
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="auStress"> <span data-t="uStress">可压测</span></label>
          </div>
          <button class="btn btn-fill" onclick="createUser()" style="width:auto;padding:12px 24px" data-t="uCreateBtn">创建</button>
          <button class="btn btn-text" onclick="$('addUserCard').style.display='none'" style="width:auto" data-t="uCancelBtn">取消</button>
        </div>
        <div class="acl-left" style="margin-top:14px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start">
          <div style="flex:1;min-width:220px">
            <span class="field-label" data-t="uDatabases">可用数据库（留空=全部）</span>
            <div id="auDbs" style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px"></div>
          </div>
          <div style="flex:1;min-width:220px">
            <label class="field-label" data-t="uTables">可用表（逗号分隔，如 users,report_stats；留空=全部）</label>
            <input class="txt" id="auTables" placeholder="users, report_stats" style="margin-top:8px">
          </div>
        </div>
        <div id="auErr" class="err-msg" style="display:none;margin-top:10px"></div>
      </div>

      <div class="card">
        <h3><span class="mat material-symbols-outlined">people</span><span data-t="uListTitle">用户列表</span></h3>
        <div class="data-wrap">
          <table class="grid">
            <thead><tr>
              <th data-t="thUname">用户名</th><th data-t="thRole">角色</th><th data-t="thPerms">权限</th><th data-t="thCreated">创建时间</th><th data-t="thLast">最后登录</th><th style="width:80px" data-t="thOps">操作</th>
            </tr></thead>
            <tbody id="userTable"><tr><td colspan="6" class="empty" data-t="loading">加载中…</td></tr></tbody>
          </table>
        </div>
      </div>

      <div class="card" id="editUserCard" style="display:none">
        <h3><span class="mat material-symbols-outlined">edit</span><span data-t="uEditTitle">编辑用户</span></h3>
        <input type="hidden" id="euName">
        <div style="display:flex;gap:14px;flex-wrap:wrap;align-items:flex-end">
          <div style="flex:1;min-width:140px"><label class="field-label" data-t="uNewPass">新密码（留空不改）</label><input class="txt" id="euPass" type="password"></div>
          <div style="display:flex;gap:16px;align-items:center;padding-bottom:4px">
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="euAdmin"> <span data-t="uAdmin">管理员</span></label>
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="euStress"> <span data-t="uStress">可压测</span></label>
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="euManage"> <span data-t="uManage">可管理</span></label>
          </div>
          <button class="btn btn-fill" onclick="updateUser()" style="width:auto;padding:12px 24px" data-t="uSaveBtn">保存</button>
          <button class="btn btn-text" onclick="$('editUserCard').style.display='none'" style="width:auto" data-t="uCancelBtn">取消</button>
        </div>
        <div class="acl-left" style="margin-top:14px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start">
          <div style="flex:1;min-width:220px">
            <span class="field-label" data-t="uDatabases">可用数据库（留空=全部）</span>
            <div id="euDbs" style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px"></div>
          </div>
          <div style="flex:1;min-width:220px">
            <label class="field-label" data-t="uTables">可用表（逗号分隔，如 users,report_stats；留空=全部）</label>
            <input class="txt" id="euTables" placeholder="users, report_stats" style="margin-top:8px">
          </div>
        </div>
      </div>
    </div>

    <!-- ============ 设置视图 ============ -->
    <div class="view" id="view-settings">
      <div class="topbar">
        <div><h2><span class="mat material-symbols-outlined">settings</span><span id="settingsTitle" data-t="settingsTitle">设置</span></h2><div class="sub" id="settingsSub" data-t="settingsSub">服务与 MySQL 兼容参数配置</div></div>
        <div>
          <button class="btn btn-text" onclick="loadSettings()"><span class="material-symbols-outlined">refresh</span><span data-t="btnRefresh">刷新</span></button>
        </div>
      </div>

      <div class="settings-grid">
        <div class="card">
          <h3><span class="mat material-symbols-outlined">translate</span><span data-t="settingsCardLang">界面语言</span></h3>
          <p style="color:var(--on-sf-var);font-size:13px;margin-bottom:14px" data-t="settingsLangSub">界面将立即切换为所选语言</p>
          <select class="txt" id="langSelect" onchange="setLang(this.value)"></select>
        </div>

        <div class="card">
          <h3><span class="mat material-symbols-outlined">palette</span><span data-t="settingsCardTone">色调</span></h3>
          <p style="color:var(--on-sf-var);font-size:13px;margin-bottom:16px" data-t="settingsToneSub">选择你喜欢的主题色调</p>
          <div id="toneGrid" style="display:flex;gap:12px;flex-wrap:wrap"></div>
        </div>

        <div class="card">
          <h3><span class="mat material-symbols-outlined">account_tree</span><span id="settingsCardTree" data-t="settingsCardTree">索引结构</span></h3>
          <p style="color:var(--on-sf-var);font-size:13px;margin-bottom:14px" data-t="settingsTreeSub">主键与二级索引使用的平衡树实现。切换后需重启：索引在启动时从 WAL 重建，已建好的树无法原地更换结构。</p>
          <select class="txt" id="sTreeKind">
            <option value="llrb" data-t="treeLLRB">LLRB 红黑树（默认，成熟稳定）</option>
            <option value="btree" data-t="treeBTree">COW B 树（写入更快，克隆层数少）</option>
          </select>
          <label class="field-label" id="lblTreeMode" data-t="lblTreeMode">并发模式</label>
          <select class="txt" id="sTreeMode">
            <option value="cow" data-t="modeCOW">COW 写时复制（默认，读完全无锁）</option>
            <option value="lock" data-t="modeLock">读写锁 + 原地更新（写入快数倍，读加锁）</option>
            <option value="auto" data-t="modeAuto">自适应（单线程快写，检测到并发自动切 COW）</option>
          </select>
          <div style="color:var(--on-sf-var);font-size:12px;margin-top:10px">
            <span data-t="treeActiveNow">当前生效</span>：<b id="treeActive">-</b> / <b id="treeModeActive">-</b>
          </div>
        </div>

        <div class="card">
          <h3><span class="mat material-symbols-outlined">computer</span><span id="settingsCardSrv" data-t="settingsCardSrv">服务器</span></h3>
          <label class="field-label" id="lblUser" data-t="lblUser">管理员用户名</label><input class="txt" id="sUser">
          <label class="field-label" id="lblPass" data-t="lblPass">管理员密码</label><input class="txt" id="sPassword" type="password">
          <label class="field-label" id="lblPort" data-t="lblPort">二进制协议端口</label><input class="txt" id="sPort" type="number">
          <label class="field-label" id="lblMetrics" data-t="lblMetrics">监控端口</label><input class="txt" id="sMetrics" type="number">
          <label class="field-label" id="lblDura" data-t="lblDura">刷盘模式</label>
          <select class="txt" id="sDurability">
            <option value="batch">batch（批量，高性能）</option>
            <option value="fsync">fsync（每写立即落盘）</option>
          </select>
          <label class="field-label" id="lblFlush" data-t="lblFlush">持久化间隔 (ms)</label><input class="txt" id="sFlush" type="number">
        </div>

        <div class="card">
          <h3><span class="mat material-symbols-outlined">dns</span><span id="settingsCardMysql" data-t="settingsCardMysql">MySQL 兼容</span></h3>
          <label class="field-chip"><input type="checkbox" id="sMysqlEnable"> <span id="lblMysqlEnable" data-t="lblMysqlEnable">启用 MySQL 协议服务</span></label>
          <label class="field-label" id="lblMysqlPort" data-t="lblMysqlPort">MySQL 端口</label><input class="txt" id="sMysqlPort" type="number">
          <label class="field-label" id="lblMysqlVersion" data-t="lblMysqlVersion">MySQL 版本</label><input class="txt" id="sMysqlVersion">
          <div class="divider"></div>
          <div style="display:flex;justify-content:space-between;align-items:center">
            <span><b id="lblMysqlVars" data-t="lblMysqlVars">MySQL 变量</b></span>
            <button class="btn btn-tonal" onclick="addVarRow()"><span class="material-symbols-outlined">add</span><span id="lblAddVar" data-t="lblAddVar">加变量</span></button>
          </div>
          <div id="varRows"></div>
        </div>

        <div class="card">
          <h3><span class="mat material-symbols-outlined">inventory_2</span><span id="settingsCardStorage" data-t="settingsCardStorage">存储优化</span></h3>
          <label class="field-chip"><input type="checkbox" id="sAutoCompact"> <span id="lblAutoCompact" data-t="lblAutoCompact">低峰期自动整理 WAL 文件</span></label>
          <label class="field-label" id="lblCompactIdle" data-t="lblCompactIdle">检测间隔 (秒)</label><input class="txt" id="sCompactIdle" type="number">
          <label class="field-label" id="lblCompactMin" data-t="lblCompactMin">整理最小 WAL (MB)</label><input class="txt" id="sCompactMin" type="number">
          <label class="field-label" id="lblCompactPeak" data-t="lblCompactPeak">低峰阈值 (QPS)</label><input class="txt" id="sCompactPeak" type="number">
          <div class="divider"></div>
          <div style="margin-top:14px;display:flex;gap:10px;flex-wrap:wrap">
            <button class="btn btn-tonal" onclick="triggerCompact()"><span class="material-symbols-outlined">compress</span><span id="lblCompactNow" data-t="lblCompactNow">立即整理 WAL</span></button>
            <button class="btn btn-outline" onclick="restartService()"><span class="material-symbols-outlined">restart_alt</span><span id="lblRestart" data-t="lblRestart">重启服务</span></button>
          </div>
        </div>
      </div>

      <div style="margin-top:18px;display:flex;gap:12px;align-items:center">
        <button class="btn btn-fill" onclick="saveSettings()"><span class="material-symbols-outlined">save</span><span id="lblSave" data-t="lblSave">保存设置</span></button>
        <span class="result-msg" id="settingsMsg"></span>
      </div>
    </div>
  </main>
</div>

<!-- 数据库管理弹窗 -->
<div class="overlay" id="dbModal" onclick="if(event.target===this)this.classList.remove('show')">
  <div class="modal" style="width:min(560px,92vw)">
    <h3><span class="mat material-symbols-outlined">database</span><span data-t="dbTitle">数据库管理</span>
      <span style="flex:1"></span>
      <button class="icon-btn" onclick="$('dbModal').classList.remove('show')"><span class="material-symbols-outlined">close</span></button>
    </h3>
    <div style="display:flex;gap:10px;margin-bottom:16px;align-items:center">
      <input class="txt" id="dbNewName" data-ph="dbNewPh" placeholder="my_database" style="flex:1;font-family:'Fira Code',monospace">
      <button class="btn btn-fill" style="width:auto" onclick="createDb()"><span class="material-symbols-outlined">add</span><span data-t="dbCreate">新建</span></button>
    </div>
    <div class="data-wrap" id="dbList"></div>
  </div>
</div>

<!-- 指标曲线弹窗 -->
<div class="overlay" id="curveModal" onclick="if(event.target===this)this.classList.remove('show')">
  <div class="modal">
    <h3><span class="mat material-symbols-outlined" id="curveIcon">show_chart</span><span id="curveTitle" data-t="curveTitle">指标曲线</span>
      <span style="flex:1"></span>
      <button class="icon-btn" onclick="document.getElementById('curveModal').classList.remove('show')"><span class="material-symbols-outlined">close</span></button>
    </h3>
    <svg class="m-chart" id="curveChart" preserveAspectRatio="none"></svg>
    <div class="m-stats" id="curveStats"></div>
  </div>
</div>

<div class="toast" id="toast"></div>

<script>
var APP_PAGE = "__APP_PAGE__";
var token = localStorage.getItem('tsumugi_token') || '';
var curTable=null, afterPK=-1, _nextPk=-1, rowCount=0;
var hist = {cpu:[], mem:[], qps:[], tps:[], disk:[]};
var maxQps=10, maxTps=10, maxDisk=1;
var prevStats=null, prevTime=Date.now();
var RING_COLOR={cpu:'#E5484D',mem:'#0E9384',qps:'#7C3AED',tps:'#2563EB',disk:'#F59E0B'};
var RING_UNIT={cpu:'%',mem:'%',qps:' /s',tps:' /s',disk:' MB/s'};

function $(id){return document.getElementById(id);}
function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');}
/* jsEsc 用于"HTML 属性内 JS 字符串字面量"上下文（如 onclick="fn('NAME')"）。
   HTML 实体在 JS 编译前就会被解码，所以 &#39; 挡不住单引号逃逸；必须用 \xNN
   形式的 JS 转义，同时处理反斜杠与换行，保证解码后的字符串无法跳出字面量。 */
function jsEsc(s){return String(s==null?'':s).replace(/\\/g,'\\\\').replace(/'/g,'\\x27').replace(/"/g,'\\x22').replace(/</g,'\\x3c').replace(/>/g,'\\x3e').replace(/\r/g,'\\r').replace(/\n/g,'\\n');}
function toast(m,isErr){var t=$('toast');t.textContent=m;t.className='toast'+(isErr?' err':'')+' show';setTimeout(function(){t.classList.remove('show');},2400);}
function fmt(n,d){return (n===null||n===undefined)?'--':Number(n).toFixed(d);}
function fmtUptime(sec){sec=sec||0;var h=Math.floor(sec/3600),m=Math.floor((sec%3600)/60),s=sec%60;var p=[];if(h>0)p.push(h+'h');if(m>0)p.push(m+'m');p.push(s+'s');return p.join(' ');}
function pctColor(p){if(p>=90)return '#E5484D';if(p>=70)return '#F59E0B';return '#16A34A';}
function setRing(id,pct,color){var r=$('ring'+id.charAt(0).toUpperCase()+id.slice(1));if(!r)return;var o=327-327*Math.max(0,Math.min(100,pct))/100;r.style.stroke=color;r.style.strokeDashoffset=o;}

/* ---- i18n ---- */
/*__I18N__*/
/*__PORTS__*/

function lang(){return curLang();}
function applyLang(){
  var c=lang();
  document.documentElement.lang=c;
  document.querySelectorAll('[data-t]').forEach(function(el){
    var key=el.getAttribute('data-t');
    var v=I18N_TEXT[key];
    if(!v)return;
    var txt=v[c]||v.en||v.zh;
    if(txt==null)return;
    var kids=el.childNodes;
    for(var k=kids.length-1;k>=0;k--){if(kids[k].nodeType===3){kids[k].data=txt;break;}}
  });
  document.querySelectorAll('[data-ph]').forEach(function(el){el.setAttribute('placeholder',t(el.getAttribute('data-ph')));});
  var o=$('sDurability');
  if(o&&o.options&&o.options.length>1){o.options[0].text=t('optBatch');o.options[1].text=t('optFsync');}
  var sm=$('stressMode');
  if(sm&&sm.options){
    var mkeys=['modeRW','modeRead','modeWrite','modePoint','modeRange'];
    for(var i=0;i<sm.options.length&&i<mkeys.length;i++){sm.options[i].text=t(mkeys[i]);}
  }
  var ls=$('langSelect');
  if(ls){
    var cur=ls.value||c;
    ls.innerHTML='';
    (I18N_LANGS||[]).forEach(function(l){
      var op=document.createElement('option');
      op.value=l.code;op.text=l.name+(l.name!==l.en?' · '+l.en:'');
      if(l.code===cur)op.selected=true;
      ls.appendChild(op);
    });
  }
  document.title=t('docTitle');
}
function setLang(code){
  if(LANG_CODES.indexOf(code)<0)code='zh';
  localStorage.setItem('tsumugi_lang',code);
  applyLang();
  refreshAll();
}
// 语言切换后刷新动态内容（重跑需要重新定位的文案）
function refreshAll(){
  if(document.querySelector('#view-monitor.show')){fetchStats(true);}
  if(document.querySelector('#view-admin.show')){if(token){refreshTables();if(curTable)loadRows();}else location.href='/';}
  if(document.querySelector('#view-users.show')){if(token)loadUsers();else location.href='/';}
  if(document.querySelector('#view-settings.show')){if(token)loadSettings();else location.href='/';}
}

/* ---- 色调主题 ---- */
var TONE_PRESETS={
  pink:{name:'粉色',pri:'#E8607C',priCtn:'#FFF0F3',onPriCtn:'#8B1A3A',ter:'#F48FB1',acc:'#FF6B9D',glow:'rgba(232,96,124,.14)'},
  blue:{name:'蓝色',pri:'#5C6BC0',priCtn:'#E8EAF6',onPriCtn:'#283593',ter:'#90CAF9',acc:'#42A5F5',glow:'rgba(92,107,192,.14)'},
  purple:{name:'紫色',pri:'#7E57C2',priCtn:'#F3E5F5',onPriCtn:'#4A148C',ter:'#CE93D8',acc:'#AB47BC',glow:'rgba(126,87,194,.14)'},
  teal:{name:'青色',pri:'#26A69A',priCtn:'#E0F2F1',onPriCtn:'#00695C',ter:'#80CBC4',acc:'#26C6DA',glow:'rgba(38,166,154,.14)'},
  orange:{name:'橙色',pri:'#FF7043',priCtn:'#FBE9E7',onPriCtn:'#BF360C',ter:'#FFAB91',acc:'#FF8A65',glow:'rgba(255,112,67,.14)'},
  green:{name:'绿色',pri:'#66BB6A',priCtn:'#E8F5E9',onPriCtn:'#1B5E20',ter:'#A5D6A7',acc:'#4CAF50',glow:'rgba(102,187,106,.14)'},
  red:{name:'红色',pri:'#EF5350',priCtn:'#FFEBEE',onPriCtn:'#B71C1C',ter:'#EF9A9A',acc:'#F44336',glow:'rgba(239,83,80,.14)'},
  hc:{name:'高对比',pri:'#000000',priCtn:'#FFFFFF',onPriCtn:'#000000',ter:'#000000',acc:'#000000',glow:'rgba(0,0,0,.18)'}
};
function hexToRgb(h){var r=parseInt(h.slice(1,3),16),g=parseInt(h.slice(3,5),16),b=parseInt(h.slice(5,7),16);return[r,g,b];}
function rgbToHex(r,g,b){return '#'+[r,g,b].map(function(x){var s=Math.max(0,Math.min(255,Math.round(x))).toString(16);return s.length<2?'0'+s:s;}).join('');}
function mixColor(a,b,t){var ra=hexToRgb(a),rb=hexToRgb(b);return rgbToHex(ra[0]+(rb[0]-ra[0])*t,ra[1]+(rb[1]-ra[1])*t,ra[2]+(rb[2]-ra[2])*t);}
function deriveSurface(hex){return mixColor(hex,'#FFFFFF',.92);}
function deriveSurfaceLow(hex){return mixColor(hex,'#FFFFFF',.88);}
var curTone=localStorage.getItem('tsumugi_tone')||'pink';
function applyTone(name){
  var p=TONE_PRESETS[name];if(!p)return;
  var r=document.documentElement.style;
  /* 高对比是"模式"而不是单纯换主色：整页切到纯黑白 + 实边风格 */
  document.documentElement.classList.toggle('hc',name==='hc');
  r.setProperty('--pri',p.pri);r.setProperty('--pri-ctn',p.priCtn);r.setProperty('--on-pri-ctn',p.onPriCtn);
  r.setProperty('--ter',p.ter);r.setProperty('--acc',p.acc);
  r.setProperty('--glow','0 10px 34px '+p.glow);
  r.setProperty('--inv-pri',p.pri);
  r.setProperty('--sc1',deriveSurfaceLow(p.priCtn));r.setProperty('--sc2',p.priCtn);
  document.querySelectorAll('.tone-opt').forEach(function(el){el.classList.toggle('active',el.dataset.tone===name);});
  localStorage.setItem('tsumugi_tone',name);curTone=name;
}
function renderTones(){
  var g=$('toneGrid');if(!g)return;g.innerHTML='';
  var keys=Object.keys(TONE_PRESETS);
  keys.forEach(function(k){
    var p=TONE_PRESETS[k];
    var wrap=document.createElement('div');wrap.style.cssText='text-align:center';
    var d=document.createElement('div');
    d.className='tone-opt'+(k===curTone?' active':'');
    d.dataset.tone=k;
    d.style.background='linear-gradient(135deg,'+p.priCtn+','+p.pri+')';
    d.onclick=function(){applyTone(k);};
    var lb=document.createElement('div');lb.className='tone-label';lb.textContent=p.name;
    wrap.appendChild(d);wrap.appendChild(lb);g.appendChild(wrap);
  });
}
applyTone(curTone);

/* ---- 视图切换 ---- */
function switchView(v){
  $('view-monitor').classList.toggle('show', v==='monitor');
  $('view-admin').classList.toggle('show', v==='admin');
  $('view-users').classList.toggle('show', v==='users');
  $('view-settings').classList.toggle('show', v==='settings');
  $('navMonitor').classList.toggle('active', v==='monitor');
  $('navAdmin').classList.toggle('active', v==='admin');
  $('navUsers').classList.toggle('active', v==='users');
  $('navSettings').classList.toggle('active', v==='settings');
  document.title = v==='monitor' ? t('docTitleMonitor') : v==='settings' ? t('docTitleSettings') : v==='users' ? t('docTitleUsers') : t('docTitleAdmin');
  if(v==='admin'){ if(!token){location.href='/';return;} refreshTables(); }
  if(v==='monitor'){ fetchStats(true); }
  if(v==='users'){ if(!token){location.href='/';return;} loadUsers(); refreshUserDbs(); }
  if(v==='settings'){ if(!token){location.href='/';return;} loadSettings(); renderTones(); }
}

/* ---- 监控 ---- */
function setRingEl(name,pct,color){var id={cpu:'ringCpu',mem:'ringMem',qps:'ringQps',tps:'ringTps',disk:'ringDisk'}[name];var r=$(id);if(!r)return;var o=327-327*Math.max(0,Math.min(100,pct))/100;r.style.stroke=color;r.style.strokeDashoffset=o;}

async function fetchStats(force){
  try{
    var resp=await fetch('/api/stats',{headers:{'Authorization':'Bearer '+token}});
    var d=await resp.json();
    var now=Date.now(), elapsed=(now-prevTime)/1000; prevTime=now;
    var cpu=d.cpu_percent||0;
    setRingEl('cpu',cpu,pctColor(cpu));
    $('cpuVal').textContent=fmt(cpu,0);
    $('cpuSub').textContent=t('cpuSubT',(d.num_cpu||'-'));
    var mem=d.mem_percent||0, heapMB=d.mem_mb||0, sysMB=mem>0?(heapMB*100/mem):0;
    setRingEl('mem',mem,pctColor(mem));
    $('memVal').textContent=fmt(mem,0);
    $('memSub').textContent=fmt(heapMB,1)+' MB / '+fmt(sysMB,0)+' MB';
    var qps=d.qps||0, tps=d.tps||0;
    if(qps>maxQps)maxQps=qps; if(tps>maxTps)maxTps=tps;
    if(maxQps<10)maxQps=10; if(maxTps<10)maxTps=10;
    setRingEl('qps',qps/maxQps*100,'#7C3AED'); $('qpsVal').textContent=fmt(qps,1); $('qpsSub').textContent=t('peak')+' '+fmt(maxQps,1)+'/s';
    setRingEl('tps',tps/maxTps*100,'#2563EB'); $('tpsVal').textContent=fmt(tps,1); $('tpsSub').textContent=t('peak')+' '+fmt(maxTps,1)+'/s';
    var disk=d.wal_write_mb_s||0;
    if(disk>maxDisk)maxDisk=disk; if(maxDisk<0.5)maxDisk=0.5;
    setRingEl('disk',disk/maxDisk*100,'#F59E0B'); $('diskVal').textContent=fmt(disk,2);
    $('diskSub').textContent=t('peak')+' '+fmt(maxDisk,2)+' · fsync '+fmt(d.fsync_per_s||0,0)+'/s';
    $('totalCmds').textContent=d.total_commands||0;
    $('totalErrs').textContent=d.total_errors||0;
    $('uptime').textContent=fmtUptime(d.uptime);
    $('goroutines').textContent=d.goroutines||0;
    $('numCpu').textContent=t('cores',(d.num_cpu||'-'));
    var dur=d.durability||'batch';
    $('durability').textContent=(dur==='fsync')?'fsync':t('durBatch');
    $('flushInfo').textContent=(dur==='fsync')?t('flushFsync'):t('flushBatch');
    // 顶部状态条
    $('heroVersion').textContent=(d.server_version||'Tsumugi')+' · :'+(d.binary_port||'-');
    $('heroUptime').textContent=fmtUptime(d.uptime);
    $('heroQps').innerHTML=fmt(qps,0)+'<span class="u">/s</span>';
    $('heroMysql').textContent=d.mysql_enabled?t('mysqlOn',d.mysql_port):t('mySqlOff');
    if(prevStats){
      var cmdDelta=(d.total_commands||0)-(prevStats.total_commands||0);
      var errDelta=(d.total_errors||0)-(prevStats.total_errors||0);
      $('totalCmdsRate').textContent=cmdDelta>0?('+'+cmdDelta+' ▲'):'';
      $('totalErrsRate').innerHTML=errDelta>0?'<span class="down">'+errDelta+' ▲</span>':'';
      var md=0,tn='-',cm=d.commands||{};
      for(var k in cm){var pd=(prevStats.commands&&prevStats.commands[k])||0;var dl=cm[k]-pd;if(dl>md){md=dl;tn=k;}}
      $('topCmd').textContent=tn; $('topCmdCount').textContent=md>0?(md+' req/s'):'';
    }else{ $('totalCmdsRate').textContent=t('firstSample'); }
    prevStats=d;
    // 历史趋势
    hist.cpu.push(cpu);hist.mem.push(mem);hist.qps.push(qps);hist.tps.push(tps);hist.disk.push(disk);
    ['cpu','mem','qps','tps','disk'].forEach(function(k){if(hist[k].length>60)hist[k].shift();});
    drawTrend('qps',hist.qps,$('trQps'),fmt(qps,1));
    drawTrend('tps',hist.tps,$('trTps'),fmt(tps,1));
    drawTrend('cpu',hist.cpu,$('trCpu'),fmt(cpu,0));
    drawTrend('disk',hist.disk,$('trDisk'),fmt(disk,2));
    // 系统信息
    renderSysInfo(d);
    // 命令表
    var cmds=d.commands||{}; var cmdHtml='';
    for(var name in cmds){
      var prev=(prevStats&&prevStats.commands&&prevStats.commands[name])||0;
      var dq=cmds[name]-prev;
      cmdHtml+='<tr><td>'+esc(name)+'</td><td>'+cmds[name]+'</td><td>'+(dq>0?'+'+dq:'0')+'</td></tr>';
    }
    $('cmdTable').innerHTML=cmdHtml||'<tr><td colspan="3" class="empty">'+t('noCmd')+'</td></tr>';
    // 侧栏
    var m=(d.mysql_enabled?t('mysqlOn',d.mysql_port):''); 
    $('sideFoot').innerHTML='<div style="display:flex;align-items:center;gap:8px"><span style="width:8px;height:8px;border-radius:var(--r-full);background:#1E7B4C;box-shadow:0 0 6px #1E7B4C"></span>'+t('running')+esc(m)+'</div>';
  }catch(e){ $('sideFoot').textContent=t('connectFail',e.message); }
}

function renderSysInfo(d){
  var rows=[
    [t('sysVer'),'<span class="pk-badge" style="background:var(--pri-ctn);color:var(--on-pri-ctn)">'+esc(d.server_version||'Tsumugi-0.1')+'</span>'],
    [t('sysPort'),esc(d.binary_port)+' · '+t('sysMetrics')+' :'+(location.port||PORTS.metrics)],
    [t('sysMysql'),d.mysql_enabled?t('mysqlOn',d.mysql_port):t('mySqlOff')],
    [t('sysTables'),'<b>'+esc(d.table_count||0)+'</b> '+t('unitTables')],
    [t('sysRows'),'<b>'+esc(d.total_rows||0)+'</b> '+t('unitRows')],
    [t('sysWal'),'<b>'+fmt(d.wal_file_mb,2)+'</b> MB'+t('cumWrite',fmt(d.wal_total_mb,2))],
    [t('sysFsync'),'<b>'+esc(d.fsync_count||0)+'</b> '+t('times')+' · '+fmt(d.fsync_per_s||0,0)+'/s'],
    [t('sysFlush'),esc((d.config&&d.config.flush_interval_ms)||100)+'ms · '+t('grpCommit')+' '+esc((d.config&&d.config.group_commit_ms)||2)+'ms'],
    [t('sysCrc'),(d.config&&d.config.checksum)?t('on'):t('off')],
  ];
  $('sysTable').innerHTML='<tbody>'+rows.map(function(r){return '<tr><th style="width:200px;border:none;padding:8px 11px">'+r[0]+'</th><td style="border:none;padding:8px 11px">'+r[1]+'</td></tr>';}).join('')+'</tbody>';
}

function drawTrend(key,data,valEl,cur){
  if(valEl)valEl.textContent=cur;
  var svg=$('svg'+key.charAt(0).toUpperCase()+key.slice(1)); if(!svg)return;
  var w=280,h=74,pad=4,color=RING_COLOR[key]||'#E8607C';
  var n=data.length; if(n<2){svg.innerHTML='';return;}
  var max=Math.max.apply(null,data), min=Math.min.apply(null,data);
  if(max-min<1e-6){max+=1;min-=1;}
  var px=function(i){return pad+(w-2*pad)*i/(n-1);};
  var py=function(v){return h-pad-(h-2*pad)*(v-min)/(max-min);};
  var pts=[]; for(var i=0;i<n;i++)pts.push(px(i).toFixed(1)+','+py(data[i]).toFixed(1));
  var area='0,'+h+' '+pts.join(' ')+' '+(w-pad)+','+h;
  svg.innerHTML='<defs><linearGradient id="g'+key+'" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="'+color+'" stop-opacity=".28"/><stop offset="100%" stop-color="'+color+'" stop-opacity="0"/></linearGradient></defs>'+
    '<polyline points="'+area+'" fill="url(#g'+key+')" stroke="none"/>'+
    '<polyline points="'+pts.join(' ')+'" fill="none" stroke="'+color+'" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>'+
    '<line x1="'+pad+'" y1="'+py(max)+'" x2="'+px(n-1)+'" y2="'+py(max)+'" stroke="'+color+'" stroke-width="1" stroke-dasharray="3,3" opacity=".5"/>';
}

function showCurve(key,title,color){
  $('curveTitle').textContent=title;
  $('curveIcon').textContent='show_chart';
  var svg=$('curveChart'), w=640, h=200, pad=10;
  var data=hist[key]||[]; var unit=RING_UNIT[key]||'';
  var n=data.length;
  var inner='<div style="flex:1;min-width:90px"><div class="v">'+(n?fmt(data[n-1],key==='cpu'||key==='mem'?0:2):'--')+unit+'</div><div class="k">'+t('mCur')+'</div></div>';
  if(n<2){ svg.innerHTML=''; $('curveStats').innerHTML=inner+'<div style="flex:1;min-width:90px"><div class="v">--</div><div class="k">'+t('mWait')+'</div></div>'; }
  else{
    var max=Math.max.apply(null,data), min=Math.min.apply(null,data);
    if(max-min<1e-6){max+=1;min-=1;}
    var px=function(i){return pad+(w-2*pad)*i/(n-1);};
    var py=function(v){return h-pad-(h-2*pad)*(v-min)/(max-min);};
    var pts=[];for(var i=0;i<n;i++)pts.push(px(i).toFixed(1)+','+py(data[i]).toFixed(1));
    var area='0,'+h+' '+pts.join(' ')+' '+w+','+h;
    var grid='';
    for(var g=0;g<=4;g++){var yy=pad+(h-2*pad)*g/4;grid+='<line x1="'+pad+'" y1="'+yy+'" x2="'+(w-pad)+'" y2="'+yy+'" stroke="var(--sf-var)" stroke-width="1" opacity=".5"/>';}
    svg.innerHTML='<defs><linearGradient id="mc" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="'+color+'" stop-opacity=".3"/><stop offset="100%" stop-color="'+color+'" stop-opacity="0"/></linearGradient></defs>'+
      grid+'<polyline points="'+area+'" fill="url(#mc)" stroke="none"/>'+
      '<polyline points="'+pts.join(' ')+'" fill="none" stroke="'+color+'" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>';
    var avg=data.reduce(function(a,b){return a+b;},0)/n;
    inner+='<div style="flex:1;min-width:90px"><div class="v">'+fmt(max,key==='cpu'||key==='mem'?0:2)+unit+'</div><div class="k">'+t('mPeak')+'</div></div>'+
      '<div style="flex:1;min-width:90px"><div class="v">'+fmt(avg,key==='cpu'||key==='mem'?0:2)+unit+'</div><div class="k">'+t('mAvg')+'</div></div>'+
      '<div style="flex:1;min-width:90px"><div class="v">'+(key==='disk'?fmt(min,2):fmt(min,key==='cpu'||key==='mem'?0:1))+unit+'</div><div class="k">'+t('mMin')+'</div></div>'+
      '<div style="flex:1;min-width:90px"><div class="v">'+(hist[key]&&hist[key].length)+'</div><div class="k">'+t('mSamples')+'</div></div>';
  }
  $('curveStats').innerHTML=inner;
  $('curveModal').classList.add('show');
}

/* ---- 压测 ---- */
async function startStress(){
  var d=$('duration').value,w=$('workers').value,m=$('stressMode').value;
  $('stressStatus').textContent=t('stressStarting');
  try{
    var p=$('stressProto')?$('stressProto').value:'native';
    var r=await api('/stress?duration='+d+'&workers='+w+'&mode='+m+'&protocol='+p);
    if(r.status===401){ $('stressStatus').textContent=t('stressUnauth'); if(!$('loginOverlay'))showLogin(); return; }
    var j=await r.json();
    $('stressStatus').textContent=t('stressRunning',w,d);
  }catch(e){ $('stressStatus').textContent=t('stressFail',e.message); }
}

/* ---- 认证 ---- */
function showLogin(){
  var body=document.createElement('div');
  body.className='overlay show'; body.style.zIndex='100';
  body.id='loginOverlay';
  body.innerHTML='<div class="modal" style="width:min(400px,92vw)">'+
    '<div style="text-align:center;margin-bottom:22px"><div style="width:64px;height:64px;margin:0 auto;border-radius:var(--r-xl);display:grid;place-items:center;background:linear-gradient(135deg,var(--pri-20),var(--pri) 60%,var(--ter));color:var(--on-pri);box-shadow:0 2px 8px rgba(232,96,124,.25)"><span class="material-symbols-outlined" style="font-size:34px">lock</span></div>'+
    '<h3 style="justify-content:center;margin-top:14px;font-size:22px;font-weight:700">'+t('loginTitle')+'</h3><div class="sub" style="color:var(--ol);font-size:14px;margin-top:4px">'+t('loginSub')+'</div></div>'+
    '<label class="field-label">'+t('lblUName')+'</label><input class="txt" id="loginUser" value="root" style="margin-bottom:14px">'+
    '<label class="field-label">'+t('lblPsd')+'</label><input class="txt" id="loginPass" type="password" placeholder="password">'+
    '<button class="btn btn-fill" style="width:100%;justify-content:center;margin-top:20px;padding:14px 28px" onclick="doLogin()"><span class="material-symbols-outlined">login</span>'+t('btnLogin')+'</button>'+
    '<div class="result-msg" id="loginMsg" style="text-align:center;margin-top:14px"></div></div>';
  document.body.appendChild(body);
  $('loginPass').addEventListener('keydown',function(e){if(e.key==='Enter')doLogin();});
  $('loginPass').focus();
}
function hideLogin(){var o=$('loginOverlay');if(o)o.remove();}
async function doLogin(){
  var u=$('loginUser').value,p=$('loginPass').value;
  $('loginMsg').className='result-msg';
  $('loginMsg').textContent=t('loginVerifying');
  try{
    var r=await fetch('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:u,password:p})});
    var j=await r.json();
    if(j.ok){ token=j.token; localStorage.setItem('tsumugi_token',token); hideLogin(); refreshTables(); toast(t('loginOk')); }
    else{ $('loginMsg').className='result-msg err'; $('loginMsg').textContent=j.error||t('loginErr'); }
  }catch(e){ $('loginMsg').className='result-msg err'; $('loginMsg').textContent=t('netErr'); }
}
function logout(){ try{navigator.sendBeacon('/api/logout');}catch(e){} token=''; localStorage.removeItem('tsumugi_token'); showLogin(); }
function api(url,opts){
  opts=opts||{}; opts.headers=opts.headers||{};
  opts.headers['Authorization']='Bearer '+token;
  opts.headers['X-Lang']=lang();
  return fetch(url,opts);
}

/* ---- 数据管理 ---- */
var curDB='tsumugi';
async function refreshTables(){
  try{
    var url='/api/admin/tables';
    if(curDB!=='tsumugi')url+='?db='+encodeURIComponent(curDB);
    var r=await api(url);
    if(r.status===401){ if(!$('loginOverlay'))showLogin(); return; }
    var d=await r.json();
    // 数据库选择器
    var sel=$('dbSelect');
    if(sel&&d.databases){
      var cur=d.cur_db||curDB;
      var opts='';
      (d.databases||[]).forEach(function(db){opts+='<option value="'+esc(db)+'"'+(db===cur?' selected':'')+'>'+esc(db)+'</option>';});
      sel.innerHTML=opts;
    }
    var h='';
    (d.tables||[]).forEach(function(tb){
      var f=tb.fields.map(function(x){return x.name;}).join(', ');
      var active=curTable===tb.name?' active':'';
      h+='<div class="admin-table-item'+active+'" onclick="openTable(\''+jsEsc(tb.name)+'\')">'+
        '<span class="mat material-symbols-outlined at-icon">table_chart</span>'+
        '<div class="at-info"><div class="at-name">'+esc(tb.name)+'</div><div class="at-meta">'+esc(f)+'</div></div>'+
        '<div class="at-count">'+tb.row_count+'</div></div>';
    });
    if(!(d.tables||[]).length)h='<div class="empty">'+t('sqlCreate2')+'</div>';
    $('tableList').innerHTML=h;
  }catch(e){ toast(t('loadFail',e.message),true); }
}

// selectDB 切换数据库后刷新表列表
function selectDB(){
  curDB=$('dbSelect').value||'tsumugi';
  $('dataCard').style.display='none';
  refreshTables();
}

/* ---- 数据库管理 ---- */
async function openDbManager(){
  $('dbModal').classList.add('show');
  refreshDbManager();
}
async function refreshDbManager(){
  try{
    var r=await api('/api/admin/tables');
    if(r.status===401){showLogin();return;}
    var d=await r.json();
    var dbs=d.databases||[], cur=d.cur_db||'tsumugi';
    var h='';
    if(!dbs.length)h='<div class="empty">'+t('dbEmpty')+'</div>';
    dbs.forEach(function(db){
      var sys=['information_schema','mysql','performance_schema','sys'].indexOf(db)>=0;
      var badge=db===cur?'<span class="field-chip" style="font-size:10px;padding:2px 10px">'+t('dbUsing')+'</span>':'';
      h+='<div style="display:flex;align-items:center;gap:10px;padding:12px 14px;border-radius:var(--r-md);background:var(--sc1);margin-bottom:8px;border:none">'+
        '<span class="mat material-symbols-outlined" style="color:var(--pri)">'+((sys)?'lock':'storage')+'</span>'+
        '<b style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-family:\'Fira Code\',monospace;font-size:13.5px">'+esc(db)+'</b>'+badge+
        '<button class="btn btn-tonal" style="width:auto;padding:8px 16px;font-size:13px" onclick="useDb(\''+jsEsc(db)+'\')"><span data-t="dbUse">使用</span></button>';
      if(!sys)h+='<button class="icon-btn" style="width:32px;height:32px;background:var(--err-ctn);color:var(--on-err-ctn)" onclick="dropDb(\''+jsEsc(db)+'\')"><span class="material-symbols-outlined" style="font-size:18px">delete</span></button>';
      h+='</div>';
    });
    $('dbList').innerHTML=h;
  }catch(e){ toast(t('loadFail',e.message),true); }
}
async function createDb(){
  var n=$('dbNewName').value.trim();
  if(!n){toast(t('dbNeedName'),true);return;}
  try{
    var r=await api('/api/admin/db/create',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:n})});
    if(r.status===401){showLogin();return;}
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    toast(t('dbCreated',n));
    $('dbNewName').value='';
    curDB=n; $('dataCard').style.display='none';
    refreshDbManager(); refreshTables();
  }catch(e){ toast(t('netErr'),true); }
}
async function dropDb(name){
  if(!confirm(t('dbDropConfirm',name)))return;
  try{
    var r=await api('/api/admin/db/drop',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name})});
    if(r.status===401){showLogin();return;}
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    curDB=d.cur_db||'tsumugi'; curTable=null;
    $('dataCard').style.display='none';
    toast(t('dbDropped',name));
    refreshDbManager(); refreshTables();
  }catch(e){ toast(t('netErr'),true); }
}
function useDb(name){
  curDB=name; $('dbModal').classList.remove('show');
  $('dataCard').style.display='none';
  refreshTables(); toast(t('dbUsed',name));
}
document.getElementById('dbNewName').addEventListener('keydown',function(e){if(e.key==='Enter')createDb();});

async function openTable(name){
  curTable=name; afterPK=-1;
  $('dataTitle').textContent=name;
  document.querySelectorAll('.admin-table-item').forEach(function(el){
    el.classList.toggle('active',el.querySelector('.at-name').textContent===name);
  });
  await loadRows();
}
async function loadRows(){
  var url='/api/admin/rows?table='+encodeURIComponent(curTable)+'&limit=50';
  if(afterPK>=0)url+='&after_pk='+afterPK;
  var r=await api(url);
  if(r.status===401){showLogin();return;}
  var d=await r.json();
  rowCount=d.row_count; _nextPk=d.next_pk;
  var head='<tr><th style="width:24px"></th>';
  (d.columns||[]).forEach(function(c){head+='<th>'+esc(c)+'</th>';});
  head+='<th style="width:64px"></th></tr>';
  var b='';
  (d.rows||[]).forEach(function(rw,idx){
    b+='<tr><td style="color:var(--ol)">'+(afterPK<0?idx+1:'')+'</td>';
    rw.forEach(function(v){var s=(v===null||v===undefined)?'<span style="color:var(--ol)">NULL</span>':esc(v);b+='<td>'+s+'</td>';});
    var rpk=(d.pks&&d.pks[idx]!==undefined)?d.pks[idx]:rw[0];
    b+='<td><button class="btn btn-text" style="padding:4px 8px;color:var(--err)" onclick="delRow('+esc(JSON.stringify(rpk))+')"><span class="material-symbols-outlined" style="font-size:18px">delete</span></button></td></tr>';
  });
  $('dataGrid').innerHTML='<thead>'+head+'</thead><tbody>'+b+'</tbody>';
  $('pageInfo').textContent=t('rowsTotal',rowCount)+(afterPK<0?t('pageText','1'):t('paging'));
  $('prevBtn').disabled=afterPK<0;
  $('nextBtn').disabled=_nextPk<0;
}
function pageNext(){if(_nextPk>=0){afterPK=_nextPk;loadRows();}}
function pagePrev(){if(afterPK<0)return;afterPK=-1;loadRows();}

async function delRow(pk){
  if(!confirm(t('delConfirm',curTable,pk)))return;
  var r=await api('/api/admin/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({table:curTable,pk:pk})});
  var d=await r.json();
  if(d.ok){toast(t('deleted'));loadRows();}else toast(d.error,true);
}

async function runSQL(){
  var sql=$('sqlBox').value.trim();
  if(!sql){toast(t('enterSQL'),true);return;}
  var r=await api('/api/admin/query',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({sql:sql})});
  if(r.status===401){showLogin();return;}
  var d=await r.json();
  var m=$('sqlMsg');
  if(!d.ok){m.className='result-msg err';m.textContent='✕ '+d.error;$('sqlResult').innerHTML='';return;}
  m.className='result-msg ok';
  m.textContent=d.message?('✓ '+d.message):('✓ '+t('rowsRet',(d.rows||[]).length));
  var h='<tr>';
  (d.columns||[]).forEach(function(c){h+='<th>'+esc(c)+'</th>';});
  h+='</tr>';
  if(d.columns&&d.columns.length){
    (d.rows||[]).forEach(function(rw){h+='<tr>';rw.forEach(function(v){h+='<td>'+esc(v)+'</td>';});h+='</tr>';});
  }
  $('sqlResult').innerHTML='<thead>'+h+'</thead><tbody></tbody>';
  refreshTables();
}

/* ---- 新建表 ---- */
var fseq=0;
function addFieldRow(){
  fseq++;
  var div=document.createElement('div');
  div.style.cssText='display:flex;gap:10px;margin-bottom:10px;align-items:center';
  div.innerHTML='<input class="txt" placeholder="'+t('fieldLabel')+'" style="flex:1.4" id="f'+fseq+'_n">'+
    '<select class="txt" style="flex:1" id="f'+fseq+'_t"><option value="INT">INT</option><option value="VARCHAR">VARCHAR</option><option value="BOOL">BOOL</option></select>'+
    '<input class="txt" placeholder="'+t('fieldLen')+'" style="flex:.7" id="f'+fseq+'_l">'+
    '<button class="icon-btn" style="background:var(--err-ctn);color:var(--on-err-ctn)" onclick="this.parentNode.remove()"><span class="material-symbols-outlined">close</span></button>';
  $('fieldRows').appendChild(div);
}
function showCreate(){
  $('createCard').style.display='';
  $('createCard').scrollIntoView({behavior:'smooth'});
  if(!document.querySelector('#f1_n')){addFieldRow();addFieldRow();}
}
async function createTable(){
  var name=$('newTableName').value.trim();
  var pk=$('newTablePk').value.trim()||'id';
  if(!name){toast(t('noTableName'),true);return;}
  var defs=[];
  document.querySelectorAll('#fieldRows > div').forEach(function(r){
    var n=r.children[0].value.trim(); if(!n)return;
    var tv=r.children[1].value, l=r.children[2].value.trim();
    defs.push(n+' '+tv+(tv==='VARCHAR'&&l?'('+l+')':'')+(n===pk?' PRIMARY KEY':''));
  });
  if(!defs.length){toast(t('needCol'),true);return;}
  var sql='CREATE TABLE '+name+' ('+defs.join(', ')+', PRIMARY KEY ('+pk+'))';
  var r=await api('/api/admin/query',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({sql:sql})});
  if(r.status===401){showLogin();return;}
  var d=await r.json();
  if(d.ok){toast(t('tableCreated',name));$('createCard').style.display='none';refreshTables();openTable(name);}
  else toast(d.error,true);
}

/* ---- 设置 ---- */
function addVarRow(k,v){
  var row=document.createElement('div');
  row.className='var-row';
  row.style.cssText='display:flex;gap:8px;margin:8px 0;align-items:center';
  row.innerHTML='<input class="txt" style="flex:1" placeholder="'+t('varName')+'" value="'+esc(k||'')+'">'+
    '<input class="txt" style="flex:1.5" placeholder="'+t('varVal')+'" value="'+esc(v==null?'':v)+'">'+
    '<button class="icon-btn" onclick="this.parentElement.remove()"><span class="material-symbols-outlined">close</span></button>';
  $('varRows').appendChild(row);
}
async function loadSettings(){
  try{
    var r=await api('/api/admin/settings');
    if(r.status===401){ if(!$('loginOverlay'))showLogin(); return; }
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    var s=d.server;
    $('sUser').value=s.user||''; $('sPassword').value=s.password||'';
    $('sPort').value=s.binary_port||''; $('sMetrics').value=s.metrics_port||'';
    $('sDurability').value=s.durability||'batch'; $('sFlush').value=s.flush_interval_ms||'';
    $('sMysqlEnable').checked=!!s.mysql_enabled; $('sMysqlPort').value=s.mysql_port||'';
    $('sMysqlVersion').value=(d.mysql&&d.mysql.version)||'';
    $('sAutoCompact').checked=!!s.auto_compact;
    $('sCompactIdle').value=s.compact_idle_seconds||60;
    $('sCompactMin').value=s.compact_min_wal_mb||64;
    $('sCompactPeak').value=s.compact_peak_rate||50;
    // 索引结构：显示"配置值"与"当前进程实际生效值"（两者可能因未重启而不同）
    $('sTreeKind').value=s.tree_kind||'llrb';
    $('sTreeMode').value=s.tree_mode||'cow';
    if($('treeActive')) $('treeActive').textContent=s.tree_active||'-';
    if($('treeModeActive')) $('treeModeActive').textContent=s.tree_mode_active||'-';
    $('varRows').innerHTML='';
    var vars=(d.mysql&&d.mysql.variables)||{};
    for(var name in vars){ addVarRow(name, vars[name]); }
  }catch(e){ toast(t('loadFail',e.message),true); }
}
async function saveSettings(){
  var vars={};
  document.querySelectorAll('#varRows > div').forEach(function(row){
    var k=row.children[0].value.trim(); if(!k)return;
    vars[k]=row.children[1].value.trim();
  });
  var body={
    user:$('sUser').value.trim(), password:$('sPassword').value,
    binary_port:parseInt($('sPort').value)||PORTS.binary,
    metrics_port:parseInt($('sMetrics').value)||PORTS.metrics,
    durability:$('sDurability').value,
    flush_interval_ms:parseInt($('sFlush').value)||100,
    mysql_enabled:$('sMysqlEnable').checked,
    mysql_port:parseInt($('sMysqlPort').value)||PORTS.mysql,
    auto_compact:$('sAutoCompact').checked,
    compact_idle_seconds:parseInt($('sCompactIdle').value)||60,
    compact_min_wal_mb:parseInt($('sCompactMin').value)||64,
    compact_peak_rate:parseInt($('sCompactPeak').value)||50,
    tree_kind:$('sTreeKind').value,
    tree_mode:$('sTreeMode').value,
    variables:vars
  };
  try{
    var r=await api('/api/admin/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(r.status===401){ showLogin(); return; }
    var d=await r.json();
    if(d.ok){ $('settingsMsg').className='result-msg'; $('settingsMsg').textContent=d.msg||t('saved'); toast(t('cfgSaved')); }
    else { $('settingsMsg').className='result-msg err'; $('settingsMsg').textContent=d.error||t('saveFail'); }
  }catch(e){ $('settingsMsg').className='result-msg err'; $('settingsMsg').textContent=t('netErr'); }
}

/* ---- 立即整理 WAL + 重启服务 ---- */
async function triggerCompact(){
  if(!confirm(t('compactConfirm')))return;
  try{
    var r=await api('/api/admin/compact',{method:'POST'});
    if(r.status===401){showLogin();return;}
    var d=await r.json();
    if(d.ok){toast(d.msg||t('compactDone'));}else toast(d.error,true);
  }catch(e){toast(t('netErr'),true);}
}
function restartService(){
  if(!confirm(t('restartConfirm')))return;
  if(!confirm(t('restartFinal')))return;
  try{ api('/api/admin/restart',{method:'POST'}).then(function(){toast(t('restarting'));}); }
  catch(e){toast(t('netErr'),true);}
}

/* ---- 账号管理 ---- */
var usersData=[];
async function loadUsers(){
  try{
    var r=await api('/api/admin/users');
    if(r.status===401){location.href='/';return;}
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    usersData=d.users||[];
    renderUsers();
  }catch(e){toast(t('loadFail',e.message),true);}
}
function renderUsers(){
  var h='';
  usersData.forEach(function(u){
    var role=u.is_admin?'<span class="pk-badge" style="background:var(--pri-ctn);color:var(--on-pri-ctn)">'+esc(t('uRoleAdmin'))+'</span>':'<span>'+esc(t('uRoleNormal'))+'</span>';
    var perms=[];
    if(u.can_stress)perms.push('<span class="field-chip" style="font-size:10px;padding:2px 8px">'+esc(t('uPermStress'))+'</span>');
    if(u.can_manage)perms.push('<span class="field-chip" style="font-size:10px;padding:2px 8px;background:var(--ter-ctn);color:var(--on-ter-ctn)">'+esc(t('uPermManage'))+'</span>');
    var created=u.created_at?new Date(u.created_at*1000).toLocaleDateString():'-';
    var lastLogin=u.last_login?new Date(u.last_login*1000).toLocaleDateString():t('uNever');
    h+='<tr><td><b>'+esc(u.username)+'</b></td><td>'+role+'</td><td>'+perms.join(' ')+'</td><td>'+created+'</td><td>'+lastLogin+'</td><td>';
    h+='<button class="icon-btn" style="width:32px;height:32px" onclick="editUser(\''+jsEsc(u.username)+'\')"><span class="material-symbols-outlined" style="font-size:18px">edit</span></button> ';
    h+='<button class="icon-btn" style="width:32px;height:32px;background:var(--err-ctn);color:var(--on-err-ctn)" onclick="deleteUser(\''+jsEsc(u.username)+'\')"><span class="material-symbols-outlined" style="font-size:18px">delete</span></button>';
    h+='</td></tr>';
  });
  if(!h)h='<tr><td colspan="6" class="empty">'+t('uEmpty')+'</td></tr>';
  $('userTable').innerHTML=h;
}
var userDbs=[]; // 数据库列表缓存（用户管理用）
async function refreshUserDbs(){
  try{
    var r=await api('/api/admin/tables');
    if(r.status===401){return;}
    var d=await r.json();
    userDbs=d.databases||[];
  }catch(e){userDbs=[];}
}
// fillDbs 把可用数据库渲染为复选框组。sel 为已勾选项数组/null。
function fillDbs(elId, sel){
  var el=$(elId); if(!el)return;
  var set={}; (sel||[]).forEach(function(d){set[d]=1;});
  var h='';
  (userDbs||[]).forEach(function(db){
    h+='<label class="acl-db" style="display:inline-flex;align-items:center;gap:5px;font-size:12.5px;cursor:pointer;border:1px solid var(--ol-var);border-radius:999px;padding:5px 11px">'+
      '<input type="checkbox" '+(set[db]?'checked':'')+' value="'+esc(db)+'"> '+esc(db)+'</label>';
  });
  if(!h)h='<span style="font-size:12.5px;color:var(--on-sf-var)">（加载中，请先刷新数据页）</span>';
  el.innerHTML=h;
}
function selDbs(elId){
  var el=$(elId); if(!el)return [];
  var out=[];
  el.querySelectorAll('input[type=checkbox]:checked').forEach(function(c){out.push(c.value);});
  return out;
}
function showAddUser(){
  fillDbs('auDbs', null);
  $('auTables').value='';
  $('addUserCard').style.display='';$('auErr').style.display='none';$('auName').focus();
}
async function createUser(){
  var u=$('auName').value.trim(),p=$('auPass').value;
  var err=$('auErr');
  if(!u){err.textContent=t('uNeedName');err.style.display='';return;}
  if(p.length<6){err.textContent=t('uNeedPass');err.style.display='';return;}
  try{
    var body={username:u,password:p,is_admin:$('auAdmin').checked,can_stress:$('auStress').checked,
      databases:selDbs('auDbs'),tables:splitTables($('auTables').value)};
    var r=await api('/api/admin/users/create',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    var d=await r.json();
    if(!d.ok){err.textContent=d.error||t('uCreateFail');err.style.display='';return;}
    $('addUserCard').style.display='none';toast(t('uCreated',u));loadUsers();
  }catch(e){err.textContent=t('netErr');err.style.display='';}
}
function splitTables(s){
  return s.split(',').map(function(x){return x.trim();}).filter(Boolean);
}
function editUser(name){
  var u=usersData.find(function(x){return x.username===name;});
  if(!u)return;
  $('euName').value=u.username;
  $('euPass').value='';
  $('euAdmin').checked=u.is_admin;
  $('euStress').checked=u.can_stress;
  $('euManage').checked=u.can_manage;
  fillDbs('euDbs', u.databases||[]);
  $('euTables').value=(u.tables||[]).join(', ');
  $('editUserCard').style.display='';
}
async function updateUser(){
  var u=$('euName').value,p=$('euPass').value;
  var body={username:u,is_admin:$('euAdmin').checked,can_stress:$('euStress').checked,can_manage:$('euManage').checked,
    databases:selDbs('euDbs'),tables:splitTables($('euTables').value)};
  if(p.length>0){
    if(p.length<6){toast(t('uNeedPass'),true);return;}
    body.password=p;
  }
  try{
    var r=await api('/api/admin/users/update',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    $('editUserCard').style.display='none';toast(t('uSaved'));loadUsers();
  }catch(e){toast(t('netErr'),true);}
}
async function deleteUser(name){
  if(!confirm(t('uDelConfirm',name)))return;
  try{
    var r=await api('/api/admin/users/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:name})});
    var d=await r.json();
    if(!d.ok){toast(d.error,true);return;}
    toast(t('uDeleted'));loadUsers();
  }catch(e){toast(t('netErr'),true);}
}

/* ---- 初始化 ---- */
(function(){
  applyLang();
  // 登录检查：未登录则跳转首页
  if(!token){location.href='/';return;}
  var v = APP_PAGE==='admin' ? 'admin' : APP_PAGE==='users' ? 'users' : 'monitor';
  switchView(v);
  if(v==='monitor'){ fetchStats(true); setInterval(function(){fetchStats();},1000); }
  if(v==='admin' && token){ refreshTables(); }
  if(v==='users' && token){ loadUsers(); refreshUserDbs(); }
})();
</script>
<!--__PANEL_ASSETS__-->
</body>
</html>`
