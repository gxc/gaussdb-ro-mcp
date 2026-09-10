// Package dbtest 提供极简的 GaussDB 线协议 mock 服务端，供 internal/db、
// internal/tools、cmd 等包的测试使用，无需真实数据库。
//
// 支持：
//   - 启动包解析（可断言 runtime 参数）与握手（免认证）；
//   - 简单协议（Q）与扩展协议（Parse/Describe/Bind/Execute/Sync）；
//   - SHOW transaction_read_only 依序返回可配置的值（模拟只读参数未生效/回退）；
//   - 其余查询按内置通用结果集应答（列覆盖元数据查询所需的全部列名），
//     可通过 QueryHook 按查询文本覆盖应答或返回错误。
package dbtest

import (
	"bytes"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"gaussdb-ro-mcp/internal/config"
)

// Col 是结果集的一列：名称、类型 OID（0 视为 text）与文本值。
type Col struct {
	Name string
	OID  uint32
	Val  string
}

// Text/Int8/BoolCol 构造常用类型的列。
func Text(name, val string) Col { return Col{Name: name, OID: 25, Val: val} }
func Int8(name string, val int64) Col {
	return Col{Name: name, OID: 20, Val: strconv.FormatInt(val, 10)}
}
func BoolCol(name string, val bool) Col {
	v := "f"
	if val {
		v = "t"
	}
	return Col{Name: name, OID: 16, Val: v}
}

// Result 是一次查询的应答：正常结果集或错误。
type Result struct {
	cols   []Col
	rows   [][]string
	errMsg string
	tag    string
}

// Rows 构造结果集；rows 为每行按列顺序的文本值，可为空（0 行）。
func Rows(cols []Col, rows ...[]string) *Result { return &Result{cols: cols, rows: rows} }

// ErrorResult 构造一条 ErrorResponse 应答。
func ErrorResult(msg string) *Result { return &Result{errMsg: msg} }

// Server 是 mock 服务端。
type Server struct {
	ln            net.Listener
	startupParams chan map[string]string
	showValues    []string
	partition     bool
	hook          func(query string) *Result
}

// Option 定制 mock 行为。
type Option func(*Server)

// WithShowValues 让 SHOW transaction_read_only 依序返回给定值（超出后重复最后一个）。
// 默认仅 "on"。
func WithShowValues(vals ...string) Option {
	return func(s *Server) {
		if len(vals) > 0 { // 防御空/越界入参，避免取值时索引越界
			s.showValues = vals
		}
	}
}

// WithPartitionSupport 模拟 openGauss/GaussDB（pg_class 含 parttype 列）。
func WithPartitionSupport() Option { return func(s *Server) { s.partition = true } }

// WithQueryHook 按查询文本覆盖默认应答；返回 nil 时使用内置默认应答。
func WithQueryHook(hook func(query string) *Result) Option {
	return func(s *Server) { s.hook = hook }
}

// Start 启动 mock 服务并注册测试结束时的清理。
func Start(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("启动 mock 服务失败: %v", err)
	}
	s := &Server{ln: ln, startupParams: make(chan map[string]string, 8), showValues: []string{"on"}}
	for _, opt := range opts {
		opt(s)
	}
	go s.accept()
	tb.Cleanup(func() { ln.Close() })
	return s
}

// Addr 返回监听地址（host:port）。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// DSN 返回指向 mock 的连接串。
func (s *Server) DSN() string {
	return "gaussdb://ro_user:secret@" + s.Addr() + "/postgres?sslmode=disable"
}

// StartupParams 返回启动包参数通道（每个新连接投递一次）。
func (s *Server) StartupParams() <-chan map[string]string { return s.startupParams }

// NewConfig 构造指向 mock 的单实例配置（实例名 "mock"）。
func (s *Server) NewConfig() *config.Config {
	host, portStr, _ := net.SplitHostPort(s.Addr())
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
			MaxRows:          500,
			StatementTimeout: config.Duration(5 * time.Second),
		}},
		DefaultInstance: "mock",
	}
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// countPlaceholders 统计查询中的 $n 占位符个数（取最大编号）。
// pgx 以 nil 参数 OID 发起 Prepare，参数个数由服务端从查询文本推断。
func countPlaceholders(q string) int {
	max := 0
	for i := 0; i < len(q); i++ {
		if q[i] != '$' || i+1 >= len(q) || q[i+1] < '0' || q[i+1] > '9' {
			continue
		}
		num := 0
		for j := i + 1; j < len(q) && q[j] >= '0' && q[j] <= '9'; j++ {
			num = num*10 + int(q[j]-'0')
		}
		if num > max {
			max = num
		}
	}
	return max
}

type connState struct {
	showIdx    int
	lastQuery  string
	lastParams int
	prepared   map[string]string // 预备语句名 → 查询文本（语句缓存命中时不重发 Parse）
	pending    map[string]bool   // 刚 Parse 完、等待 Bind 的语句：Bind 时复用 Parse 时计算的结果
	res        *Result
	errMode    bool
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	params, err := s.readStartup(conn)
	if err != nil {
		return
	}
	select {
	case s.startupParams <- params:
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

	st := &connState{prepared: map[string]string{}, pending: map[string]bool{}}
	pkt := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		if len(pkt) >= 5 {
			// 长度字段不含类型字节：线上消息占 total+1 字节。
			total := int(binary.BigEndian.Uint32(pkt[1:5]))
			if total >= 4 && len(pkt) >= total+1 {
				if resp := s.response(pkt[0], pkt[5:total+1], st); len(resp) > 0 {
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
func (s *Server) readStartup(conn net.Conn) (map[string]string, error) {
	head := make([]byte, 8)
	if _, err := readFull(conn, head); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(head[4:8]) == 80877103 { // SSLRequest
		if _, err := conn.Write([]byte{'N'}); err != nil {
			return nil, err
		}
		return s.readStartup(conn)
	}
	rest := make([]byte, int(binary.BigEndian.Uint32(head[0:4]))-8)
	if _, err := readFull(conn, rest); err != nil {
		return nil, err
	}
	fields := bytes.Split(rest, []byte{0})
	params := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		params[string(fields[i])] = string(fields[i+1])
	}
	return params, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Server) response(typ byte, payload []byte, st *connState) []byte {
	switch typ {
	case 'Q': // 简单查询：描述 + 数据 + 完成 + 就绪
		st.lastQuery = cstr(payload)
		st.res = s.computeResult(st.lastQuery, st)
		if st.res.errMsg != "" {
			st.errMode = true
			return append(errorResponse(st.res.errMsg), msg('Z', []byte{'I'})...)
		}
		out := append(resDescription(st.res), resData(st.res)...)
		out = append(out, msg('C', cbytes(resTag(st.res)))...)
		return append(out, msg('Z', []byte{'I'})...)

	case 'P': // Parse：name\0 query\0 int16 参数个数 + 参数 OID…
		parts := bytes.SplitN(payload, []byte{0}, 3)
		name := ""
		if len(parts) >= 1 {
			name = string(parts[0])
		}
		if len(parts) >= 2 {
			st.lastQuery = string(parts[1])
			st.prepared[name] = st.lastQuery
			st.pending[name] = true
		}
		st.lastParams = countPlaceholders(st.lastQuery)
		st.res = s.computeResult(st.lastQuery, st)
		if st.res.errMsg != "" {
			st.errMode = true
			return errorResponse(st.res.errMsg)
		}
		return msg('1', nil) // ParseComplete

	case 'B': // Bind：portal\0 stmt\0 …
		// 语句缓存命中（本连接上未见对应 Parse）时按名字重算结果集，
		// 保证 SHOW 序列等按逻辑执行次数推进；本次 Parse 对应的 Bind 复用原结果。
		if parts := bytes.SplitN(payload, []byte{0}, 3); len(parts) >= 2 {
			name := string(parts[1])
			if st.pending[name] {
				delete(st.pending, name)
			} else if q, ok := st.prepared[name]; ok {
				st.lastQuery = q
				st.res = s.computeResult(q, st)
			}
		}
		return msg('2', nil) // BindComplete

	case 'D': // Describe：语句级应答 ParameterDescription + 描述；游标级仅描述
		if st.errMode {
			return nil
		}
		var out []byte
		if len(payload) > 0 && payload[0] == 'S' {
			// ParameterDescription：参数个数 + 全 0 的参数 OID。
			pb := i16(int16(st.lastParams))
			for i := 0; i < st.lastParams; i++ {
				pb = append(pb, i32(0)...)
			}
			out = append(out, msg('t', pb)...)
		}
		return append(out, resDescription(st.res)...)

	case 'E': // Execute
		if st.errMode {
			return nil
		}
		return append(resData(st.res), msg('C', cbytes(resTag(st.res)))...)

	case 'S': // Sync
		st.errMode = false
		return msg('Z', []byte{'I'})

	case 'C': // Close
		return msg('3', nil) // CloseComplete

	default: // H(Flush)/X(Terminate) 等无需应答
		return nil
	}
}

// computeResult 依查询文本给出应答；hook 优先。
func (s *Server) computeResult(query string, st *connState) *Result {
	if s.hook != nil {
		if r := s.hook(query); r != nil {
			return r
		}
	}
	u := strings.ToUpper(query)
	switch {
	case strings.Contains(u, "TRANSACTION_READ_ONLY"):
		idx := st.showIdx
		if idx >= len(s.showValues) {
			idx = len(s.showValues) - 1
		}
		st.showIdx++
		val := s.showValues[idx]
		return Rows([]Col{Text("transaction_read_only", val)}, []string{val})
	case strings.HasPrefix(u, "SET"), strings.HasPrefix(u, "ROLLBACK"):
		return &Result{tag: "SET"}
	case strings.HasPrefix(u, "BEGIN"), strings.HasPrefix(u, "START"):
		return &Result{tag: "BEGIN"}
	case strings.Contains(query, "pg_attribute") && strings.Contains(query, "parttype"):
		n := int64(0)
		if s.partition {
			n = 5
		}
		return Rows([]Col{Int8("n", n)}, []string{strconv.FormatInt(n, 10)})
	default:
		return genericResult()
	}
}

func genericResult() *Result {
	cols := []Col{
		Int8("oid", 16384),
		Text("schema_name", "public"),
		Text("table_name", "mock_table"),
		Text("kind", "table"),
		Text("comment", "mock comment"),
		Int8("estimated_rows", 42),
		BoolCol("in_current", true),
		Text("column_name", "id"),
		Text("data_type", "integer"),
		BoolCol("nullable", true),
		Text("default_value", ""),
		Int8("position", 1),
		Text("constraint_name", "mock_pkey"),
		Text("constraint_type", "primary key"),
		Text("definition", "PRIMARY KEY (id)"),
		Text("index_name", "mock_pkey"),
		BoolCol("is_primary", true),
		BoolCol("is_unique", true),
		BoolCol("is_valid", true),
		Text("viewdef", "SELECT 1"),
		Int8("est_rows", 42),
		Text("partition_name", "p1"),
		Text("part_strategy", "r"),
		Text("boundaries", "{0,100}"),
		Text("version", "mock GaussDB Kernel 503.1.0"),
		Text("database", "mockdb"),
		Text("user", "ro_user"),
		Text("server_start_time", "2026-09-10 00:00:00"),
		Int8("n", 1),
		Int8("relations", 3),
	}
	vals := make([]string, len(cols))
	for i, c := range cols {
		vals[i] = c.Val
	}
	return Rows(cols, vals, vals) // 两行相同数据，便于覆盖多行/歧义/截断路径
}

func resDescription(r *Result) []byte {
	if len(r.cols) == 0 {
		return msg('n', nil) // NoData
	}
	b := i16(int16(len(r.cols)))
	for _, c := range r.cols {
		oid := c.OID
		if oid == 0 {
			oid = 25
		}
		b = append(b, cbytes(c.Name)...)
		b = append(b, i32(0)...) // 表 OID
		b = append(b, i16(0)...) // 列号
		b = append(b, i32(int32(oid))...)
		b = append(b, i16(-2)...) // typlen（变长）
		b = append(b, i32(-1)...) // typmod
		b = append(b, i16(0)...)  // 文本格式
	}
	return msg('T', b)
}

func resData(r *Result) []byte {
	var out []byte
	for _, row := range r.rows {
		b := i16(int16(len(row)))
		for _, v := range row {
			b = append(b, i32(int32(len(v)))...)
			b = append(b, v...)
		}
		out = append(out, msg('D', b)...)
	}
	return out
}

func resTag(r *Result) string {
	if r.tag != "" {
		return r.tag
	}
	return "SELECT " + strconv.Itoa(len(r.rows))
}

func errorResponse(msgText string) []byte {
	b := cbytes("SERROR")
	b = append(b, cbytes("CXX999")...)
	b = append(b, cbytes("M"+msgText)...)
	b = append(b, 0)
	return msg('E', b)
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

// cstr 取载荷中第一个 \0 结尾字符串。
func cstr(payload []byte) string {
	if i := bytes.IndexByte(payload, 0); i >= 0 {
		return string(payload[:i])
	}
	return string(payload)
}

func paramStatus(k, v string) []byte {
	return msg('S', append(cbytes(k), cbytes(v)...))
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
