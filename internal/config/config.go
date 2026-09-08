// Package config 加载并校验 gaussdb-ro-mcp 的 YAML 配置文件。
// 支持在一个配置文件中声明多个 GaussDB 实例（数据源）。
package config

import (
	"fmt"
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
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
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
	// BlockedFunctions 覆盖默认的危险函数黑名单（前缀匹配，小写，如 "dblink*"）。
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
	if c.DefaultInstance == "" && len(c.Instances) > 0 {
		c.DefaultInstance = c.Instances[0].Name
	}
}

// validate 校验配置的完整性与一致性。
func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("配置中至少需要一个数据源 (instances)")
	}
	seen := map[string]bool{}
	for i, inst := range c.Instances {
		if inst.Name == "" {
			return fmt.Errorf("instances[%d].name 不能为空", i)
		}
		if seen[inst.Name] {
			return fmt.Errorf("重复的实例名: %q", inst.Name)
		}
		seen[inst.Name] = true
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

// BuildDSN 返回实例的最终连接串。
func (inst *Instance) BuildDSN() string {
	if inst.DSN != "" {
		return appendOptions(ensureSSLMode(inst.DSN, inst.SSLMode), inst.Options)
	}
	parts := []string{
		"host=" + inst.Host,
		"port=" + strconv.Itoa(inst.Port),
		"dbname=" + inst.Database,
	}
	if inst.User != "" {
		parts = append(parts, "user="+inst.User)
	}
	if inst.Password != "" {
		parts = append(parts, "password="+inst.Password)
	}
	parts = append(parts, "sslmode="+inst.SSLMode)
	return strings.Join(parts, " ") + joinOptions(inst.Options)
}

// appendOptions 把 key=value 形式的附加参数合并进连接串：
// URL 形式合并进查询串（正确转义），key=value 形式直接空格追加。
func appendOptions(dsn string, opts []string) string {
	if len(opts) == 0 {
		return dsn
	}
	if strings.Contains(dsn, "://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			for _, o := range opts {
				if i := strings.IndexByte(o, '='); i > 0 {
					q.Set(o[:i], o[i+1:])
				}
			}
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return joinOptions2(dsn, opts)
}

func joinOptions2(dsn string, opts []string) string {
	return dsn + " " + strings.Join(opts, " ")
}

// ensureSSLMode 在连接串未指定 sslmode 时追加指定值（默认 disable）。
// 同时支持 URL 形式与 key=value 形式。
func ensureSSLMode(dsn, mode string) string {
	if mode == "" {
		mode = "disable"
	}
	if strings.Contains(dsn, "sslmode=") {
		return dsn
	}
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "sslmode=" + mode
	}
	return dsn + " sslmode=" + mode
}

func joinOptions(opts []string) string {
	if len(opts) == 0 {
		return ""
	}
	return " " + strings.Join(opts, " ")
}

// Load 从 path 读取并解析配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
