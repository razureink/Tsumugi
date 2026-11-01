package main

// ==================== 真实第三方客户端：MySQL Connector/J ====================
// 第三个独立实现。PyMySQL 走文本协议，mysql2 走二进制预编译，Connector/J
// 又有一套完全不同的握手/元数据/预编译处理（例如它会在连接时发一串
// 初始化查询）。用它验证协议实现没有"只对某一家驱动有效"。
//
// 驱动从 Maven Central 下载并缓存在系统临时目录；离线时自动跳过。
// 需要 JDK（javac + java）。

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const javaClientSrc = `
import java.sql.*;

public class Main {
    static void ok(boolean c, String m) {
        if (!c) { System.err.println("ASSERT: " + m); System.exit(1); }
    }

    public static void main(String[] args) throws Exception {
        int port = Integer.parseInt(args[0]);
        String url = "jdbc:mysql://127.0.0.1:" + port
            + "/?sslMode=DISABLED&allowPublicKeyRetrieval=true"
            + "&serverTimezone=UTC&useUnicode=true&characterEncoding=UTF-8"
            + "&connectTimeout=5000&socketTimeout=15000";

        try (Connection c = DriverManager.getConnection(url, "root", "password")) {
            // 1) 文本结果集
            try (Statement st = c.createStatement();
                 ResultSet rs = st.executeQuery("SELECT id, name, n FROM n_t ORDER BY id")) {
                ok(rs.next() && rs.getInt("id") == 1
                    && "alice".equals(rs.getString("name")) && rs.getInt("n") == 10, "text row1");
                ok(rs.next() && rs.getInt("id") == 2
                    && "\u4e2d\u6587".equals(rs.getString("name")) && rs.getInt("n") == 20, "text row2");
                ok(!rs.next(), "text row count");
            }

            // 2) 服务端预编译（Connector/J 默认 useServerPrepStmts=false，
            //    需要显式打开才走 COM_STMT；这里两种都要能过）
            try (PreparedStatement ps = c.prepareStatement("SELECT id, name, n FROM n_t WHERE id = ?")) {
                ps.setInt(1, 2);
                try (ResultSet rs = ps.executeQuery()) {
                    ok(rs.next() && rs.getInt("n") == 20
                        && "\u4e2d\u6587".equals(rs.getString("name")), "client-prepared select");
                    ok(!rs.next(), "client-prepared row count");
                }
            }

            // 3) 插入 + affected rows
            try (PreparedStatement ps = c.prepareStatement(
                    "INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)")) {
                ps.setInt(1, 3); ps.setString(2, "bob"); ps.setInt(3, 30);
                ok(ps.executeUpdate() == 1, "insert affected");
            }
            try (Statement st = c.createStatement();
                 ResultSet rs = st.executeQuery("SELECT name FROM n_t WHERE id = 3")) {
                ok(rs.next() && "bob".equals(rs.getString(1)), "inserted row");
            }

            // 4) 错误码
            boolean dup = false; int code = 0;
            try (PreparedStatement ps = c.prepareStatement(
                    "INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)")) {
                ps.setInt(1, 3); ps.setString(2, "dup"); ps.setInt(3, 0);
                ps.executeUpdate();
            } catch (SQLException e) { dup = true; code = e.getErrorCode(); }
            ok(dup && code == 1062, "dup errno=" + code);

            boolean missing = false; int mcode = 0;
            try (Statement st = c.createStatement()) {
                st.executeQuery("SELECT * FROM n_no_such");
            } catch (SQLException e) { missing = true; mcode = e.getErrorCode(); }
            ok(missing && mcode == 1146, "table errno=" + mcode);

            // 5) 事务
            c.setAutoCommit(false);
            try (PreparedStatement ps = c.prepareStatement(
                    "INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)")) {
                ps.setInt(1, 4); ps.setString(2, "rb"); ps.setInt(3, 40);
                ps.executeUpdate();
            }
            c.rollback();
            try (Statement st = c.createStatement();
                 ResultSet rs = st.executeQuery("SELECT COUNT(*) FROM n_t WHERE id = 4")) {
                rs.next(); ok(rs.getInt(1) == 0, "rollback count");
            }
            try (PreparedStatement ps = c.prepareStatement(
                    "INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)")) {
                ps.setInt(1, 5); ps.setString(2, "cm"); ps.setInt(3, 50);
                ps.executeUpdate();
            }
            c.commit();
            try (Statement st = c.createStatement();
                 ResultSet rs = st.executeQuery("SELECT n FROM n_t WHERE id = 5")) {
                rs.next(); ok(rs.getInt(1) == 50, "commit");
            }
            c.setAutoCommit(true);

            System.out.println("ALL_OK");
        }
    }
}
`

// javaConnectorJar 下载（或复用缓存）Connector/J，返回 jar 路径。
func javaConnectorJar(t *testing.T) string {
	t.Helper()
	jar := filepath.Join(os.TempDir(), "tsumugi-mysql-connector-j-8.4.0.jar")
	if st, err := os.Stat(jar); err == nil && st.Size() > 1<<20 {
		return jar
	}
	const url = "https://repo1.maven.org/maven2/com/mysql/mysql-connector-j/8.4.0/mysql-connector-j-8.4.0.jar"
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Skipf("download mysql-connector-j failed (offline?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("download mysql-connector-j: HTTP %d", resp.StatusCode)
	}
	tmp := jar + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Skipf("write jar: %v", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		t.Skipf("save jar: %v", err)
	}
	f.Close()
	if err := os.Rename(tmp, jar); err != nil {
		os.Remove(tmp)
		t.Skipf("rename jar: %v", err)
	}
	return jar
}

// TestJavaConnectorJClient 用 MySQL 官方 JDBC 驱动跑完整链路。
func TestJavaConnectorJClient(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short")
	}
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not found")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not found")
	}
	jar := javaConnectorJar(t)

	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE n_t (id INT PRIMARY KEY, name VARCHAR(64), n INT)")
	mustRun(t, db, "INSERT INTO n_t VALUES (1, 'alice', 10), (2, '\u4e2d\u6587', 20)")

	work := t.TempDir()
	srcPath := filepath.Join(work, "Main.java")
	if err := os.WriteFile(srcPath, []byte(javaClientSrc), 0600); err != nil {
		t.Fatalf("write Main.java: %v", err)
	}
	build := exec.Command(javac, "-encoding", "UTF-8", "-cp", jar, "Main.java")
	build.Dir = work
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn, autocommit: true}).handle()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	classpath := jar + string(os.PathListSeparator) + work
	cmd := exec.Command(java, "-cp", classpath, "Main", strconv.Itoa(port))
	cmd.Dir = work
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		t.Fatal("Connector/J 测试超时（120s）")
	}
	if runErr != nil {
		t.Fatalf("Connector/J 客户端退出异常: %v\n输出:\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "ALL_OK") {
		t.Fatalf("Connector/J 断言失败:\n%s", out)
	}
	if testing.Verbose() {
		fmt.Println("Connector/J: 连接/查询/预编译/错误码/事务/UTF-8 全部通过")
	}
}
