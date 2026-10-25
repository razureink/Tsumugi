# assets

面板静态资源目录（可选）。

## material-symbols-outlined.woff2

管理面板的图标字体（Material Symbols Outlined，即 M3E 图标集）。仓库不内置该文件。

页面的取用顺序：**Google Fonts → 镜像 CDN（fonts.googleapis.cn / fonts.loli.net）→ 本文件 → 单色字符**。

- **放了这个文件** → 面板优先通过 `/assets/material-symbols-outlined.woff2` 本地加载，断网也有一致的图标外观
- **没放** → 先走 CDN（含镜像）；全部不可用时才降级为单色字符符号（`add → ＋`、`chevron_left → ‹`，不使用 emoji）
- 想彻底禁用字符降级（保持原 CDN 行为）：启动时设置 `TSUMUGI_ICON_FALLBACK=off`

获取方式（任意一种）：

1. 浏览器打开 <https://fonts.google.com/icons>，下载 Material Symbols Outlined 的 woff2；
2. 打开 `https://fonts.googleapis.cn/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@48,400,0,0`
   （国内可直连），从返回 CSS 的 `src: url(...woff2)` 里下载字体文件；
3. 直接下载上面 CSS 里给出的 `fonts.gstatic.cn` woff2 地址（约 316KB）；
4. 从其它项目/机器的浏览器缓存里复制已知可用的同名 woff2。

放置后无需重启服务（每次请求按需读取），刷新页面即可生效。

> 单二进制/Docker 部署时，`assets/` 需要和数据目录一样随部署目录一起分发；
> 容器镜像默认只拷贝二进制，因此容器内依赖 CDN 或字符降级。
