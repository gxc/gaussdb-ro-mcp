// Package config 加载并校验 gaussdb-ro-mcp 的 YAML 配置文件。
// 支持在一个配置文件中声明多个 GaussDB 实例（数据源）。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 是支持 YAML 反序列化的 time.Duration，
// 接受 "30s"、"5m" 等字符串，或纯数字（单位：秒）。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	s := strings.TrimSpace(node.Value)
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		// 拒绝 NaN/±Inf/负数/溢出：这些值会以平台相关的方式被静默重置或损坏配置。
		// 边界用 >= 而非 >：float64(MaxInt64) 恰等于 2^63，乘积达到该值时
		// 转换即回绕为负数（实证：9223372036.8547758 秒 → MinInt64）。
		if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 ||
			secs*float64(time.Second) >= math.MaxInt64 {
			return fmt.Errorf("非法时长 %q", node.Value)
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("非法时长 %q: %w", node.Value, err)
	}
	if v < 0 {
		// 与纯数字路径保持一致：负时长一律显式报错，而非被静默换成默认值。
		return fmt.Errorf("非法时长 %q：不允许负值", node.Value)
	}
	*d = Duration(v)
	return nil
}

// Server 是服务级配置。
type Server struct {
	// Name 是 MCP 服务器名称，默认 "gaussdb-readonly"。
	Name string `yaml:"name"`
	// MaxRows 是 execute_select 默认返回的最大行数。
	MaxRows int `yaml:"max_rows"`
	// MaxRowsCap 是单次调用可通过参数放宽到的行数上限。
	MaxRowsCap int `yaml:"max_rows_cap"`
	// StatementTimeout 是服务端语句超时（GaussDB statement_timeout）。
	StatementTimeout Duration `yaml:"statement_timeout"`
	// ConnectTimeout 是建立 TCP 连接的超时。
	ConnectTimeout Duration `yaml:"connect_timeout"`
	// BlockedFunctions 覆盖默认的危险函数黑名单（小写）。
	// 匹配规则：条目以 * 结尾时对函数名做前缀匹配（如 "dblink*"）；
	// 无 * 时按函数尾段精确匹配（如 "set_config"）；含 schema 限定的条目
	// （如 "pg_catalog.dblink"）按全名匹配。
	// 注意：非空时【整体替换】默认黑名单（见 guard.DefaultBlockedFunctions），
	// 追加条目请先复制默认清单。
	BlockedFunctions []string `yaml:"blocked_functions"`
}

// Instance 是一个 GaussDB 数据源。
// 既可以直接给 DSN，也可以拆分成 host/port/database/user/password 字段。
type Instance struct {
	Name string `yaml:"name"`
	// DSN 是完整连接串（gaussdb://user:pass@host:port/db 或 k=v 空格分隔）。
	// 未显式指定 sslmode 时强制追加 sslmode=disable（内网非 SSL 场景）。
	DSN string `yaml:"dsn"`
	// 以下是拆分字段方式（与 DSN 二选一，DSN 优先）。
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// SSLMode 默认 disable（内网非 SSL）。可选 disable/require/verify-ca/verify-full 等。
	SSLMode string `yaml:"sslmode"`
	// Options 追加到连接串的额外参数，如 ["application_name=mcp", "connect_timeout=5"]。
	Options []string `yaml:"options"`
	// ConnectTimeout 覆盖建立 TCP 连接的超时；未设置时用服务级
	// server.connect_timeout，DSN/options 中显式给出的 connect_timeout 优先级最高。
	ConnectTimeout Duration `yaml:"connect_timeout"`

	// MaxRows 覆盖服务级 MaxRows。
	MaxRows int `yaml:"max_rows"`
	// StatementTimeout 覆盖服务级 StatementTimeout。
	StatementTimeout Duration `yaml:"statement_timeout"`
	// PoolMaxConns 连接池上限，默认 4。
	PoolMaxConns int `yaml:"pool_max_conns"`
}

// Config 是配置文件根结构。
type Config struct {
	Server Server `yaml:"server"`
	// Instances 是多个 GaussDB 数据源，至少声明一个。
	Instances []*Instance `yaml:"instances"`
	// DefaultInstance 是未在工具调用中指定 instance 时使用的实例，
	// 默认取 Instances[0].Name。
	DefaultInstance string `yaml:"default_instance"`
}

// SetDefaults 填充未显式配置的字段。
func (c *Config) SetDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = "gaussdb-readonly"
	}
	if c.Server.MaxRows <= 0 {
		c.Server.MaxRows = 500
	}
	if c.Server.MaxRowsCap > 0 {
		// 显式配置的行数上限是硬顶：默认 max_rows 也要被钳制。
		if c.Server.MaxRowsCap < c.Server.MaxRows {
			c.Server.MaxRows = c.Server.MaxRowsCap
		}
	} else {
		c.Server.MaxRowsCap = c.Server.MaxRows
	}
	if c.Server.StatementTimeout <= 0 {
		c.Server.StatementTimeout = Duration(30 * time.Second)
	}
	if c.Server.ConnectTimeout <= 0 {
		c.Server.ConnectTimeout = Duration(10 * time.Second)
	}
	for _, inst := range c.Instances {
		if inst == nil {
			continue // 空列表项由 validate 报错
		}
		if inst.SSLMode == "" {
			inst.SSLMode = "disable"
		}
		if inst.MaxRows <= 0 {
			inst.MaxRows = c.Server.MaxRows
		}
		if inst.MaxRows > c.Server.MaxRowsCap {
			inst.MaxRows = c.Server.MaxRowsCap
		}
		if inst.StatementTimeout <= 0 {
			inst.StatementTimeout = c.Server.StatementTimeout
		}
		if inst.PoolMaxConns <= 0 {
			inst.PoolMaxConns = 4
		}
	}
	if c.DefaultInstance == "" {
		for _, inst := range c.Instances {
			if inst != nil && inst.Name != "" {
				c.DefaultInstance = inst.Name
				break
			}
		}
	}
}

// validate 校验配置的完整性与一致性。
func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("配置中至少需要一个数据源 (instances)")
	}
	seen := map[string]bool{}
	for i, inst := range c.Instances {
		if inst == nil {
			return fmt.Errorf("instances[%d] 不能为空项", i)
		}
		if inst.Name == "" {
			return fmt.Errorf("instances[%d].name 不能为空", i)
		}
		if seen[inst.Name] {
			return fmt.Errorf("重复的实例名: %q", inst.Name)
		}
		seen[inst.Name] = true
		// 上限校验：pool_max_conns 最终写入 int32 的池配置，超界值会被静默
		// 回绕成小值（如 4294967300 → 4），必须显式报错。
		if inst.PoolMaxConns > 1024 {
			return fmt.Errorf("实例 %q 的 pool_max_conns 过大（%d，上限 1024）", inst.Name, inst.PoolMaxConns)
		}
		if inst.DSN == "" {
			if inst.Host == "" {
				return fmt.Errorf("实例 %q 缺少 host（或直接提供 dsn）", inst.Name)
			}
			if inst.Database == "" {
				return fmt.Errorf("实例 %q 缺少 database", inst.Name)
			}
			if inst.Port <= 0 {
				inst.Port = 5432
			}
		}
	}
	if c.DefaultInstance != "" && !seen[c.DefaultInstance] {
		return fmt.Errorf("default_instance %q 未在 instances 中定义", c.DefaultInstance)
	}
	return nil
}

// BuildDSN 返回实例的最终连接串（URL 形式，密码等特殊字符自动转义）。
func (inst *Instance) BuildDSN() string {
	if inst.DSN != "" {
		return appendOptions(ensureSSLMode(inst.DSN, inst.SSLMode), inst.Options)
	}
	host := inst.Host
	if inst.Port > 0 {
		host = net.JoinHostPort(inst.Host, strconv.Itoa(inst.Port))
	}
	u := url.URL{Scheme: "gaussdb", Host: host, Path: "/" + inst.Database}
	if inst.User != "" {
		if inst.Password != "" {
			u.User = url.UserPassword(inst.User, inst.Password)
		} else {
			u.User = url.User(inst.User)
		}
	}
	q := url.Values{}
	sslmode := inst.SSLMode
	if sslmode == "" {
		sslmode = "disable" // 与 SetDefaults 的默认值一致，直接构造时同样生效
	}
	q.Set("sslmode", sslmode)
	mergeOptionsIntoQuery(q, inst.Options)
	u.RawQuery = q.Encode()
	return u.String()
}

// mergeOptionsIntoQuery 把 key=value 形式的附加参数合并进 URL 查询串（正确转义）。
// BuildDSN 与 appendOptions 共用，避免两份逐字节相同的合并循环各自漂移。
func mergeOptionsIntoQuery(q url.Values, opts []string) {
	for _, o := range opts {
		if i := strings.IndexByte(o, '='); i > 0 {
			q.Set(o[:i], o[i+1:])
		}
	}
}

// appendOptions 把 key=value 形式的附加参数合并进连接串：
// URL 形式合并进查询串（正确转义），key=value 形式直接空格追加。
func appendOptions(dsn string, opts []string) string {
	if len(opts) == 0 {
		return dsn
	}
	if isURLFormDSN(dsn) {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			mergeOptionsIntoQuery(q, opts)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return joinOptions2(dsn, opts)
}

func joinOptions2(dsn string, opts []string) string {
	return dsn + " " + strings.Join(opts, " ")
}

// isURLFormDSN 判断连接串是否为 URL 形式（gaussdb://user:pass@host/db?...）。
// "://" 必须出现在首个空白之前且其前缀是干净的 scheme 段：密码值中恰含
// "://" 的 keyword=value 连接串（host=h password=a://b）会被 url.Parse 拒绝，
// 不能进入 URL 处理分支（否则 sslmode 等默认参数会被整个丢掉）。
func isURLFormDSN(dsn string) bool {
	i := strings.Index(dsn, "://")
	return i > 0 && !strings.ContainsAny(dsn[:i], " \t\r\n")
}

// splitKVFields 按空白切分 keyword=value 形式的连接串；单引号值内的空白
// 不切分，避免 password='my pass sslmode=x' 一类值中的伪 key= 片段干扰判断。
func splitKVFields(dsn string) []string {
	var fields []string
	var sb strings.Builder
	inQuote := false
	for i := 0; i < len(dsn); i++ {
		c := dsn[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			sb.WriteByte(c)
		case !inQuote && (c == ' ' || c == '\t' || c == '\r' || c == '\n'):
			if sb.Len() > 0 {
				fields = append(fields, sb.String())
				sb.Reset()
			}
		default:
			sb.WriteByte(c)
		}
	}
	if sb.Len() > 0 {
		fields = append(fields, sb.String())
	}
	return fields
}

// DSNHasParam 判断连接串中是否显式给出指定键：URL 形式查查询参数，
// keyword=value 形式按参数位置的字段前缀判断（值内部的伪键不算）。
// 供 config 与 db 两包共用，保证 sslmode / connect_timeout 检测口径一致。
func DSNHasParam(dsn, key string) bool {
	if isURLFormDSN(dsn) {
		u, err := url.Parse(dsn)
		return err == nil && u.Query().Has(key)
	}
	for _, field := range splitKVFields(dsn) {
		if strings.HasPrefix(field, key+"=") {
			return true
		}
	}
	return false
}

// ensureSSLMode 在连接串未指定 sslmode 时追加指定值（默认 disable）。
// URL 形式按查询参数解析判断；keyword=value 形式按参数位置判断，
// 避免密码值中恰含 "sslmode=" 子串时误判为已设置。任何情况下都保证
// 结果串带有 sslmode（驱动缺省 sslmode=prefer 会尝试 SSL，违背内网默认）。
func ensureSSLMode(dsn, mode string) string {
	if mode == "" {
		mode = "disable"
	}
	if isURLFormDSN(dsn) {
		u, err := url.Parse(dsn)
		if err == nil {
			if u.Query().Has("sslmode") {
				return dsn
			}
			q := u.Query()
			q.Set("sslmode", mode)
			u.RawQuery = q.Encode()
			return u.String()
		}
		// 无法解析的 URL 形式串：按 keyword=value 追加兜底，绝不返回
		// 不带 sslmode 的连接串。
		return dsn + " sslmode=" + mode
	}
	if DSNHasParam(dsn, "sslmode") {
		return dsn
	}
	return dsn + " sslmode=" + mode
}

// Load 从 path 读取并解析配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// 拒绝未知键：拼错的限制项（如 max_rows 写成 max_row）应大声报错，
	// 而不是静默回退到默认值。
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// 空文件/纯注释文件：给可操作的错误而不是裸 EOF。
			return nil, fmt.Errorf("配置文件为空：至少需要一个数据源 (instances)")
		}
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
