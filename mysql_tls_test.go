package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ==================== MySQL 协议 TLS ====================
// 线上 MySQL 流量当前是明文（密码只做 native scramble，仍然可被离线爆破 +
// 会话内容完全可见）。这里验证 CLIENT_SSL 升级路径：客户端发 32 字节
// SSLRequest，服务器在裸 TCP 上做 TLS 握手，之后所有 packet 走加密通道。

// makeTestTLSFiles 生成自签证书并写入临时目录。
func makeTestTLSFiles(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tsumugi-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// startTLSMySQL 起一个带 TLS 配置的 MySQL 监听器。
func startTLSMySQL(t *testing.T, db *DB, tlsConf *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	caps := uint32(serverCapabilities)
	if tlsConf != nil {
		caps |= clientSSL
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mc := &mysqlConn{db: db, conn: conn, autocommit: true,
				tlsConf: tlsConf, serverCaps: caps}
			go mc.handle()
		}
	}()
	return ln.Addr().String()
}

func dialRaw(t *testing.T, addr string) *protoClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	t.Cleanup(func() { conn.Close() })
	return &protoClient{t: t, conn: conn, br: bufio.NewReaderSize(conn, 32*1024)}
}

// authPayload 构造认证响应（与 dialProto 相同，便于测试复用）。
func authPayload(c *protoClient, scramble []byte) []byte {
	auth := nativeAuthResp("password", scramble)
	var resp []byte
	resp = protoU32(resp, c.caps)
	resp = protoU32(resp, 1<<24)
	resp = append(resp, defaultCollation)
	resp = append(resp, make([]byte, 23)...)
	resp = append(resp, []byte("root")...)
	resp = append(resp, 0)
	if c.caps&clientPluginAuthLenEncClientData != 0 {
		resp = appendLenEncBytes(resp, auth)
	} else {
		resp = append(resp, byte(len(auth)))
		resp = append(resp, auth...)
	}
	if c.caps&clientPluginAuth != 0 {
		resp = append(resp, []byte("mysql_native_password")...)
		resp = append(resp, 0)
	}
	return resp
}

// TestMySQLTLSUpgrade 完整走一遍 SSLRequest + TLS 握手 + 认证 + 查询。
func TestMySQLTLSUpgrade(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	certPath, keyPath := makeTestTLSFiles(t)
	tlsConf := mysqlTLSConfig(Config{TLSCert: certPath, TLSKey: keyPath})
	if tlsConf == nil {
		t.Fatal("mysqlTLSConfig 返回 nil，证书应有效")
	}
	addr := startTLSMySQL(t, db, tlsConf)

	c := dialRaw(t, addr)
	caps, scramble := parseServerHandshake(t, c.readRawPacket())
	if caps&clientSSL == 0 {
		t.Fatal("服务端配置了证书却没有广播 CLIENT_SSL")
	}
	c.caps = (protoBaseCaps | clientSSL) & caps

	// SSLRequest：恰好 32 字节，序号 1
	req := make([]byte, 0, 32)
	req = protoU32(req, c.caps)
	req = protoU32(req, 1<<24)
	req = append(req, defaultCollation)
	req = append(req, make([]byte, 23)...)
	if len(req) != 32 {
		t.Fatalf("SSLRequest 长度=%d want 32", len(req))
	}
	c.writePacket(req)

	// 之后立刻在裸 TCP 上做 TLS 握手
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client tls handshake: %v", err)
	}
	c.conn = tc
	c.br = bufio.NewReaderSize(tc, 32*1024)
	c.seq = 1 // SSLRequest 用了序号 1

	// 认证响应走加密通道，序号 2
	c.writePacket(authPayload(c, scramble))
	ok := c.readRawPacket()
	if len(ok) == 0 || ok[0] != 0x00 {
		t.Fatalf("TLS 认证失败: %x", ok)
	}

	// 加密通道上的真正查询
	c.seq = 0
	c.writePacket([]byte{comPing})
	pong := c.readRawPacket()
	if len(pong) == 0 || pong[0] != 0x00 {
		t.Fatalf("TLS ping 失败: %x", pong)
	}
}

// TestMySQLPlaintextStillWorksWhenTLSConfigured 未请求 TLS 的客户端必须仍能连接
// （TLS 是可选项；只在客户端显式置 CLIENT_SSL 时才升级）。
func TestMySQLPlaintextStillWorksWhenTLSConfigured(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	certPath, keyPath := makeTestTLSFiles(t)
	tlsConf := mysqlTLSConfig(Config{TLSCert: certPath, TLSKey: keyPath})
	addr := startTLSMySQL(t, db, tlsConf)

	c := dialRaw(t, addr)
	caps, scramble := parseServerHandshake(t, c.readRawPacket())
	c.caps = protoBaseCaps & caps // 不带 CLIENT_SSL
	c.writePacket(authPayload(c, scramble))
	ok := c.readRawPacket()
	if len(ok) == 0 || ok[0] != 0x00 {
		t.Fatalf("明文认证失败: %x", ok)
	}
	c.seq = 0
	c.writePacket([]byte{comPing})
	if pong := c.readRawPacket(); len(pong) == 0 || pong[0] != 0x00 {
		t.Fatalf("明文 ping 失败: %x", pong)
	}
}

// TestMySQLTLSConfigInvalid 证书配置不完整时必须返回 nil（保持明文），
// 而不是 panic 或假装已加密。
func TestMySQLTLSConfigInvalid(t *testing.T) {
	certPath, keyPath := makeTestTLSFiles(t)
	if got := mysqlTLSConfig(Config{TLSCert: certPath}); got != nil {
		t.Fatal("只有 cert 没有 key 时应返回 nil")
	}
	if got := mysqlTLSConfig(Config{}); got != nil {
		t.Fatal("未配置证书时应返回 nil")
	}
	if got := mysqlTLSConfig(Config{TLSCert: filepath.Join(t.TempDir(), "nope.pem"),
		TLSKey: filepath.Join(t.TempDir(), "nope.key")}); got != nil {
		t.Fatal("文件不存在时应返回 nil")
	}
	// 完整的自签证书对应该成功
	if got := mysqlTLSConfig(Config{TLSCert: certPath, TLSKey: keyPath}); got == nil {
		t.Fatal("有效证书应返回配置")
	}
}
