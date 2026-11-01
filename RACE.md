# 数据竞争检测（`go test -race`）

竞态检测器**必须有 cgo**，而 cgo 必须有 C 编译器。本仓库不需要 cgo 构建，
但想跑 `-race` 就必须临时提供一个 gcc。

## 一次性准备（便携工具链，不改系统环境）

Windows 下最省事的是 [w64devkit](https://github.com/skeeto/w64devkit/releases)
（单文件自解压，gcc + make + 常用工具，无需安装）：

```powershell
$dir = "$env:TEMP\w64devkit"
Invoke-WebRequest `
  -Uri "https://github.com/skeeto/w64devkit/releases/latest/download/w64devkit-x64-2.11.0.7z.exe" `
  -OutFile "$dir.exe"
& "$dir.exe" -y -o"$dir"      # 会解到 "$dir\w64devkit\"
```

Linux / WSL 只需 `sudo apt install -y gcc`。

## 跑测试

```powershell
# Windows + w64devkit
$bin = "$env:TEMP\w64devkit\w64devkit\bin"
$env:PATH = "$bin;$env:PATH"
$env:CGO_ENABLED = "1"
$env:CC = "$bin\gcc.exe"

go test -race -count=1 -timeout 2400s ./...
```

```bash
# Linux / WSL
CGO_ENABLED=1 go test -race -count=1 -timeout 2400s ./...
```

只打并发风险最高的部分：

```powershell
go test -race -run "TestStress|TestRBTreeConcurrent|TestAutoModeUpgradesOnConcurrency" ./...
```

## 当前基线

- 全量 `-race`：**通过**（`ok ~56s`，本机 32 核；无竞态告警）
- 全量 `CGO_ENABLED=0`：**通过**（`ok ~15s`）

## 由 `-race` 发现并已修复的竞争

1. **auto 模式的升级窗口**（`tree.go`）
   `enter()` 在函数开头读到 `upgraded == false` 后可能被调度出去；升级者此刻
   看到 `inflight == 0` 并完成升级。迟到的进入者不再复查 `upgraded`，拿到
   “原地写”权限，与已走 COW 无锁读的 goroutine 并发。
   修复：占用 `inflight` 之后**必须**重新检查 `upgraded`。

2. **包级 `walCloseCh`**（`db.go`）
   `Close` 的后台 goroutine join 带 10s 超时兜底，下一次 `NewDB` 可能与之
   并发读写同一个包级变量。
   修复：关闭哨兵改为每实例 `db.walClose`；包级 `appendWAL` 通过
   `atomic.Pointer` 读取当前活跃实例的哨兵。

3. **B 树 COW：根节点未克隆**（`btree.go` `setLocked`）
   旧实现只在根节点已满（需要分裂）时克隆；根不满时 `insert` 直接原地改
   共享根节点的 `keys/vals/kids`，与无锁读者并发。
   修复：`cow` 模式下根节点总是先克隆。

4. **B 树 COW：子节点快路径未克隆**（`btree.go` `ensureChild`）
   旧实现只在需要借用/合并时克隆子节点；子节点键数足够时直接返回，
   随后 `remove` 原地修改这个仍被读者共享的节点。
   修复：`cow` 模式下目标子节点在**任何**修改或递归之前就克隆。

这四处都属于“功能性测试永远测不出来、只有竞态检测器能看见”的问题：
数据结构和算法在单线程下完全正确。
