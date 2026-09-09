package db

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"

	"gaussdb-ro-mcp/internal/config"
)

// fakeGaussDBDebug 由环境变量 FAKE_GAUSSDB_DEBUG 开启，打印 mock 收发的协议消息。
var fakeGaussDBDebug = os.Getenv("FAKE_GAUSSDB_DEBUG") != ""

func dbg(format string, args ...any) {
	if fakeGaussDBDebug {
		fmt.Printf("fakeGaussDB: "+format+"\n", args...)
	}
}

// fakeGaussDB 是极简的 GaussDB 线协议 mock：解析启动包、完成握手，并以固定脚本
// 回答 enforceReadOnly 的探针语句（SHOW transaction_read_only / SET / ROLLBACK）。
// 仅支持无参语句，简单协议（Q）与扩展协议（P/B/D/E/S/C）均可应答。
//
// showValues 依次回答每次 SHOW transaction_read_only，超出后重复最后一个值：
// 传入 "on" 模拟启动参数已生效；传入 "off","on" 模拟参数未生效、回退 SET 后生效；
// 传入 "off","off" 模拟回退后仍未生效（应拒绝该连接）。
type fakeGaussDB struct {
	ln            net.Listener
	startupParams chan map[string]string
	showValues    []string
}

func startFakeGaussDB(t *testing.T, showValues ...string) *fakeGaussDB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 mock 服务失败: %v", err)
	}
	f := &fakeGaussDB{ln: ln, startupParams: make(chan map[string]string, 8), showValues: showValues}
	go f.accept()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeGaussDB) addr() string { return f.ln.Addr().String() }

func (f *fakeGaussDB) dsn() string {
	return "gaussdb://ro_user:secret@" + f.addr() + "/postgres?sslmode=disable"
}

func (f *fakeGaussDB) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeGaussDB) handle(conn net.Conn) {
	defer conn.Close()
	params, err := f.readStartup(conn)
	if err != nil {
		return
	}
	select {
	case f.startupParams <- params:
	default:
	}

	// 握手：AuthenticationOk + 若干 ParameterStatus + ReadyForQuery('I')。
	out := msg('R', i32(0))
	out = append(out, paramStatus("server_version", "9.2.4")...)
	out = append(out, paramStatus("client_encoding", "UTF8")...)
	out = append(out, paramStatus("DateStyle", "ISO, MDY")...)
	out = append(out, paramStatus("standard_conforming_strings", "on")...)
	out = append(out, msg('Z', []byte{'I'})...)
	if _, err := conn.Write(out); err != nil {
		return
	}

	shows := 0
	lastQuery := ""
	pkt := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		if len(pkt) >= 5 {
			// 长度字段不含类型字节：线上消息占 total+1 字节。
			total := int(binary.BigEndian.Uint32(pkt[1:5]))
			if total >= 4 && len(pkt) >= total+1 {
				dbg("recv %c len=%d payload=%q", pkt[0], total, pkt[5:total+1])
				if resp := f.response(pkt[0], pkt[5:total+1], &shows, &lastQuery); len(resp) > 0 {
					dbg("send % x", resp)
					if _, err := conn.Write(resp); err != nil {
						return
					}
				}
				pkt = pkt[total+1:]
				continue
			}
		}
		n, err := conn.Read(tmp)
		if err != nil {
			return
		}
		pkt = append(pkt, tmp[:n]...)
	}
}

// readStartup 读取启动包；客户端 sslmode=disable 时不发 SSLRequest，若收到则拒绝 SSL 后重读。
func (f *fakeGaussDB) readStartup(conn net.Conn) (map[string]string, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(head[4:8]) == 80877103 { // SSLRequest
		if _, err := conn.Write([]byte{'N'}); err != nil {
			return nil, err
		}
		return f.readStartup(conn)
	}
	rest := make([]byte, int(binary.BigEndian.Uint32(head[0:4]))-8)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return nil, err
	}
	fields := bytes.Split(rest, []byte{0})
	params := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		params[string(fields[i])] = string(fields[i+1])
	}
	return params, nil
}

func (f *fakeGaussDB) response(typ byte, payload []byte, shows *int, lastQuery *string) []byte {
	switch typ {
	case 'Q': // 简单查询
		return append(f.answer(cstr(payload), shows, true), msg('Z', []byte{'I'})...)
	case 'P': // Parse：name\0 query\0 …，记录查询文本（命名语句时为第二个字符串），用于 Describe/Execute 区分 SHOW 与其他
		if parts := bytes.SplitN(payload, []byte{0}, 3); len(parts) >= 2 {
			*lastQuery = string(parts[1])
		}
		return msg('1', nil)
	case 'B': // Bind
		return msg('2', nil)
	case 'D': // Describe：语句级应答 ParameterDescription + RowDescription/NoData；游标级无参数描述
		var out []byte
		if len(payload) > 0 && payload[0] == 'S' {
			out = append(out, msg('t', i16(0))...) // ParameterDescription：0 个参数
		}
		if isShowReadOnly(*lastQuery) {
			out = append(out, rowDesc()...)
		} else {
			out = append(out, msg('n', nil)...) // NoData
		}
		return out
	case 'E': // Execute
		return f.executeResult(*lastQuery, shows)
	case 'S': // Sync
		return msg('Z', []byte{'I'})
	case 'C': // Close
		return msg('3', nil)
	default: // H(Flush)/X(Terminate) 等无需应答
		return nil
	}
}

func (f *fakeGaussDB) executeResult(query string, shows *int) []byte {
	if isShowReadOnly(query) {
		out := dataRow(f.nextShowValue(shows))
		return append(out, msg('C', cbytes("SHOW"))...)
	}
	return msg('C', cbytes("SET"))
}

// answer 应答简单查询；withRowDesc 时先带 RowDescription（扩展协议下 RowDescription 已由 Describe 发出）。
func (f *fakeGaussDB) answer(query string, shows *int, withRowDesc bool) []byte {
	if isShowReadOnly(query) {
		var out []byte
		if withRowDesc {
			out = append(out, rowDesc()...)
		}
		out = append(out, dataRow(f.nextShowValue(shows))...)
		return append(out, msg('C', cbytes("SHOW"))...)
	}
	return msg('C', cbytes("SET"))
}

func (f *fakeGaussDB) nextShowValue(shows *int) string {
	*shows++
	if *shows-1 < len(f.showValues) {
		return f.showValues[*shows-1]
	}
	return f.showValues[len(f.showValues)-1]
}

func isShowReadOnly(query string) bool {
	return strings.Contains(strings.ToLower(query), "transaction_read_only")
}

// ---- 线协议编码辅助 ----

// msg 编码一条后端消息：1 字节类型 + 4 字节长度（含自身、不含类型字节）+ 载荷。
func msg(typ byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(4+len(payload)))
	copy(b[5:], payload)
	return b
}

// cbytes 返回以 \0 结尾的字符串（协议中的 C 字符串）。
func cbytes(s string) []byte { return append([]byte(s), 0) }

// cstr 取载荷中第一个 \0 结尾字符串（Parse/Q 载荷中的语句文本）。
func cstr(payload []byte) string {
	if i := bytes.IndexByte(payload, 0); i >= 0 {
		return string(payload[:i])
	}
	return string(payload)
}

func paramStatus(k, v string) []byte {
	return msg('S', append(cbytes(k), cbytes(v)...))
}

// rowDesc 返回单文本列 transaction_read_only 的 RowDescription。
func rowDesc() []byte {
	b := i16(1)
	b = append(b, cbytes("transaction_read_only")...)
	b = append(b, i32(0)...)  // 表 OID
	b = append(b, i16(0)...)  // 列号
	b = append(b, i32(25)...) // text OID
	b = append(b, i16(-2)...) // typlen（变长）
	b = append(b, i32(-1)...) // typmod
	b = append(b, i16(0)...)  // 文本格式
	return msg('T', b)
}

func dataRow(val string) []byte {
	b := i16(1)
	b = append(b, i32(int32(len(val)))...)
	b = append(b, val...)
	return msg('D', b)
}

func i16(v int16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(v))
	return b
}

func i32(v int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// ---- 测试 ----

// mockInstanceConfig 构造指向 mock 地址的实例配置。
func mockInstanceConfig(t *testing.T, addr string) *config.Config {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("拆分 mock 地址失败: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	return &config.Config{
		Server: config.Server{
			MaxRows:        500,
			MaxRowsCap:     10000,
			ConnectTimeout: config.Duration(5 * time.Second),
		},
		Instances: []*config.Instance{{
			Name:             "mock",
			Host:             host,
			Port:             port,
			Database:         "postgres",
			User:             "ro_user",
			Password:         "secret",
			SSLMode:          "disable",
			PoolMaxConns:     2,
			StatementTimeout: config.Duration(5 * time.Second),
		}},
		DefaultInstance: "mock",
	}
}

// TestStartupParamCarriesReadOnly 是 55P02 修复的回归测试：default_transaction_read_only=on
// 必须随启动包在会话初始化时下发（先于任何事务生效），而不能依赖建连后再 SET。
func TestStartupParamCarriesReadOnly(t *testing.T) {
	f := startFakeGaussDB(t, "on")
	cfg := mockInstanceConfig(t, f.addr())

	inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
	if err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	defer inst.pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := inst.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取连接失败（AfterConnect 只读校验未通过？）: %v", err)
	}
	c.Release()

	select {
	case got := <-f.startupParams:
		if got["default_transaction_read_only"] != "on" {
			t.Fatalf("启动包缺少 default_transaction_read_only=on，实际参数: %v", got)
		}
		if got["user"] == "" {
			t.Fatalf("启动包缺少 user，实际参数: %v", got)
		}
	case <-ctx.Done():
		t.Fatal("未收到启动包")
	}
}

// TestEnforceReadOnlyFallback 验证回退分支：启动参数未生效（首次 SHOW 为 off）时，
// 先 ROLLBACK 清理再会话级 SET 重新校验；复查仍 off 则拒绝该连接。
func TestEnforceReadOnlyFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("回退后生效", func(t *testing.T) {
		f := startFakeGaussDB(t, "off", "on")
		conn, err := gaussdbgo.Connect(ctx, f.dsn())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		if err := enforceReadOnly(ctx, conn, 5*time.Second); err != nil {
			t.Fatalf("回退路径应成功: %v", err)
		}
	})

	t.Run("复查仍off则拒绝", func(t *testing.T) {
		f := startFakeGaussDB(t, "off", "off")
		conn, err := gaussdbgo.Connect(ctx, f.dsn())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		err = enforceReadOnly(ctx, conn, 0)
		if err == nil || !strings.Contains(err.Error(), "拒绝该连接") {
			t.Fatalf("复查仍 off 应拒绝该连接，实际: %v", err)
		}
	})
}
