package main

// ==================== C1：复制（物理 WAL 传送） ====================
// 设计取舍：不实现 MySQL 的 binlog wire protocol（COM_BINLOG_DUMP + ROW/GTID
// 事件是数千行的独立协议栈），而是复用已有的**唯一真相源**——WAL。
//
// 模型：
//   - 主库 WAL 本身就是有序变更流，偏移量即 LSN；
//   - 从库用主库 WAL 的一份**前缀副本**做初始状态（Backup/RestorePointInTime
//     已经能生成），因此两边 LSN 天然对齐；
//   - 从库按偏移轮询主库，拉取 [offset, masterEnd) 的原始分帧字节，
//     校验每一帧的 CRC 后原地追加到自己的 WAL，再用现有重放器增量应用。
//
// 这个模型的关键性质：
//   - 不引入第二套变更表示，主库写什么从库就应用什么，语义必然一致；
//   - 断点续传天然成立：offset 就是本地 WAL 的字节长度；
//   - 崩溃安全：先应用后追加时，崩溃后本地 WAL 只是主库 WAL 的前缀，
//     重启后从同一 offset 重新拉取即可（幂等）；
//   - 提升（failover）只是停止拉取并解除只读，不需要额外的一致性协议。
//
// 明确的限制（README/RACE 之外单独说明）：
//   - 主库一旦 Compact 重写 WAL，旧偏移全部失效。启用复制的主库必须关闭
//     自动压缩（Compact 会显式拒绝，见 Compact 里的检查）。
//   - 只支持 Tsumugi→Tsumugi，不兼容 MySQL 原生从库。

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errReadOnly 从库未提升时拒绝写语句，映射 MySQL errno 1290。
var errReadOnly = errors.New("the MySQL server is running with the --read-only option so it cannot execute this statement")

// replicationMasterEnabled 主库模式：配置了 replication_token 即视为对外提供
// 复制流。此时禁止 Compact——它会重写 WAL，让所有从库的偏移失效。
func (db *DB) replicationMasterEnabled() bool {
	return db.configSnapshot().ReplicationToken != ""
}

// walOffsetsPinned 表示 WAL 偏移必须保持稳定，不能压缩重写：
//   - 主库对外提供复制流时，从库持有偏移；
//   - 从库自身在提升之前，本地 WAL 必须与主库保持前缀关系。
func (db *DB) walOffsetsPinned() bool {
	cfg := db.configSnapshot()
	if cfg.ReplicationToken != "" {
		return true
	}
	return cfg.ReplicaOf != "" && !db.promoted.Load()
}

// replicaStatusRunning 等状态字符串（SHOW SLAVE STATUS / /api/stats 用）。
const (
	replicaRunning = "running"
	replicaStopped = "stopped"
	replicaError   = "error"
)

func (db *DB) setReplicaStatus(status string, err error) {
	s := status
	db.replicaStatus.Store(&s)
	if err != nil {
		msg := err.Error()
		db.replicaErr.Store(&msg)
	} else {
		db.replicaErr.Store(nil)
	}
}

// ReplicaStatus 返回复制状态供监控使用。
func (db *DB) ReplicaStatus() map[string]interface{} {
	out := map[string]interface{}{
		"offset": db.replicaOffset.Load(),
		"lag_ms": db.replicaLagMS.Load(),
	}
	if p := db.replicaStatus.Load(); p != nil {
		out["status"] = *p
	} else {
		out["status"] = replicaStopped
	}
	if p := db.replicaErr.Load(); p != nil {
		out["error"] = *p
	}
	cfg := db.configSnapshot()
	out["master"] = cfg.ReplicaOf
	out["read_only"] = db.replicaReadOnly()
	out["promoted"] = db.promoted.Load()
	return out
}

// replicaReadOnly 从库在未提升前只读。
func (db *DB) replicaReadOnly() bool {
	cfg := db.configSnapshot()
	return cfg.ReplicaOf != "" && !db.promoted.Load()
}

// Promote 把从库提升为主库：停止拉取、解除只读。返回原主库地址。
func (db *DB) Promote() string {
	// 等待在途增量应用完成后再解除只读：否则 PROMOTE 返回后用户写入
	// 可能与仍在应用的重放写并发操作同一棵树。
	db.replicaApplyMu.Lock()
	defer db.replicaApplyMu.Unlock()
	cfg := db.configSnapshot()
	db.promoted.Store(true)
	db.setReplicaStatus(replicaStopped, nil)
	return cfg.ReplicaOf
}

// appendRawWALFrame 把已分帧的原始字节原样交给 WAL 写协程（先 sync 落盘）。
func (db *DB) appendRawWALFrame(raw []byte) error {
	done := make(chan error, 1)
	select {
	case walCh <- walEntry{raw: raw, sync: true, done: done}:
	case <-db.walClose:
		return errWALClosed
	}
	return <-done
}

// WALSince 返回 [offset, min(end, offset+maxBytes)) 的原始 WAL 字节以及当前
// 末尾偏移。offset 必须落在帧边界上（由上一次返回的 end 保证）。
func (db *DB) WALSince(offset, maxBytes int64) ([]byte, int64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("wal offset must be >= 0")
	}
	// flush 之后文件末尾就是逻辑 LSN 的上界；之后的追加只会写到 end 之后。
	if err := db.flushWAL(); err != nil {
		return nil, 0, err
	}
	st, err := db.walFile.Stat()
	if err != nil {
		return nil, 0, err
	}
	end := st.Size()
	headerLen := int64(len(WALMagic) + 4 + 4)
	if offset == 0 {
		offset = headerLen // 从库不应重复拉取文件头
	}
	if offset > end {
		return nil, end, fmt.Errorf("wal offset %d is beyond current end %d", offset, end)
	}
	n := end - offset
	if maxBytes > 0 && n > maxBytes {
		n = maxBytes
	}
	if n == 0 {
		return nil, end, nil
	}
	buf := make([]byte, n)
	if _, err := db.walFile.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, end, err
	}
	return buf, end, nil
}

// ApplyWALDelta 校验并应用一段主库 WAL 增量，然后把它原样追加到本地 WAL，
// 使本地 LSN 与主库对齐。verifyCRCFrames 控制外层批量帧校验（内层 catalog
// 记录是否带 CRC 由主库的 enable_checksum 决定，必须与主库一致）。
//
// 顺序是"先应用、后追加"：若应用失败，本地 WAL 仍是主库 WAL 的干净前缀，
// 重启后从同一 offset 重拉即可；若追加失败，返回错误并停止复制，由运维处理。
func (db *DB) ApplyWALDelta(delta []byte, innerCRC bool) error {
	if len(delta) == 0 {
		return nil
	}
	// 与 Promote 互斥：提升必须等当前增量应用完成，避免用户写入与
	// 重放写并发操作同一棵树。
	db.replicaApplyMu.Lock()
	defer db.replicaApplyMu.Unlock()
	frames, err := parseWALBatchFrames(delta)
	if err != nil {
		return err
	}
	ctx := &walReplayCtx{
		db:          db,
		enableCRC:   innerCRC,
		cowApply:    true,
		pending:     make(map[string][]walOp),
		pendingTxns: make(map[uint64][]walOp),
	}
	for _, f := range frames {
		ctx.buf = f
		ctx.atEOF = false // 中间帧残缺是真实损坏，绝不允许截断本地 WAL
		ctx.baseOffset = 0
		if err := ctx.parseV2(); err != nil {
			return fmt.Errorf("apply replication frame: %w", err)
		}
	}
	ctx.flushPending()
	if err := db.appendRawWALFrame(delta); err != nil {
		return fmt.Errorf("append replicated WAL: %w", err)
	}
	return nil
}

// parseWALBatchFrames 严格切分 [varint len][u32 crc][payload] 帧并逐帧校验。
// 任何截断/越界/CRC 不符都直接报错——复制流不允许"尽力而为"。
func parseWALBatchFrames(delta []byte) ([][]byte, error) {
	var out [][]byte
	pos := 0
	for pos < len(delta) {
		start := pos
		n, nb := binary.Uvarint(delta[pos:])
		if nb <= 0 {
			return nil, fmt.Errorf("replication frame: invalid length prefix at %d", start)
		}
		pos += nb
		if pos+4 > len(delta) {
			return nil, fmt.Errorf("replication frame at %d: truncated crc", start)
		}
		want := binary.BigEndian.Uint32(delta[pos : pos+4])
		pos += 4
		if n > uint64(len(delta)-pos) {
			return nil, fmt.Errorf("replication frame at %d: payload %d exceeds remaining %d",
				start, n, len(delta)-pos)
		}
		payload := delta[pos : pos+int(n)]
		if crc32.ChecksumIEEE(payload) != want {
			return nil, fmt.Errorf("replication frame at %d: crc mismatch", start)
		}
		out = append(out, payload)
		pos += int(n)
	}
	return out, nil
}

// ---- 主库侧 HTTP：给从库提供 WAL 增量 ----

// replicationAuthorized 校验从库的拉取凭据。
// 优先 replication_token；未配置时退回 metrics_token/管理员会话。
func (db *DB) replicationAuthorized(r *http.Request) bool {
	cfg := db.configSnapshot()
	tok := bearerToken(r)
	if cfg.ReplicationToken != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.ReplicationToken)) == 1
	}
	return db.metricsAuthorized(r)
}

func (db *DB) handleReplicationWAL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if !db.replicationAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	maxBytes := int64(32 << 20)
	if v := r.URL.Query().Get("max"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 && n <= 256<<20 {
			maxBytes = n
		}
	}
	data, end, err := db.WALSince(offset, maxBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	cfg := db.configSnapshot()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-WAL-End", strconv.FormatInt(end, 10))
	w.Header().Set("X-WAL-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("X-WAL-Checksum", strconv.FormatBool(cfg.EnableChecksum))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// ---- 从库侧：轮询拉取并应用 ----

func (db *DB) startReplicaLoop() {
	cfg := db.configSnapshot()
	if cfg.ReplicaOf == "" {
		return
	}
	interval := time.Duration(cfg.ReplicaPollMS) * time.Millisecond
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	db.setReplicaStatus(replicaRunning, nil)
	db.wg.Add(1)
	go func() {
		defer db.wg.Done()
		base := strings.TrimRight(cfg.ReplicaOf, "/")
		client := &http.Client{Timeout: 30 * time.Second}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if db.promoted.Load() {
				return
			}
			db.fetchAndApplyOnce(client, base)
			select {
			case <-db.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (db *DB) fetchAndApplyOnce(client *http.Client, base string) {
	offset := db.replicaOffset.Load()
	url := base + "/replication/wal?offset=" + strconv.FormatInt(offset, 10)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		db.setReplicaStatus(replicaError, err)
		return
	}
	cfg := db.configSnapshot()
	if cfg.ReplicaToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.ReplicaToken)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		db.setReplicaStatus(replicaError, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		db.setReplicaStatus(replicaError, fmt.Errorf("master returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	end, _ := strconv.ParseInt(resp.Header.Get("X-WAL-End"), 10, 64)
	innerCRC := resp.Header.Get("X-WAL-Checksum") != "false"
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		db.setReplicaStatus(replicaError, err)
		return
	}
	if len(body) == 0 {
		// 已追平；用主库末尾与本地偏移差作为延迟估计的粗粒度代理
		if end > 0 {
			db.replicaOffset.Store(offset)
		}
		db.replicaLagMS.Store(0)
		db.setReplicaStatus(replicaRunning, nil)
		return
	}
	if err := db.ApplyWALDelta(body, innerCRC); err != nil {
		db.setReplicaStatus(replicaError, err)
		return
	}
	// 本地 WAL 长度即新的 LSN
	if pos, err := db.WALPosition(); err == nil {
		db.replicaOffset.Store(pos)
	} else {
		db.replicaOffset.Store(offset + int64(len(body)))
	}
	db.replicaLagMS.Store(time.Since(start).Milliseconds())
	db.setReplicaStatus(replicaRunning, nil)
}
