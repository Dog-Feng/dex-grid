// Package config 负责加载 config.yaml。
//
// 这里只承载【密钥与运维参数】。网格参数可写在 strategy_file，启动时加载。
// 交易对、网格区间、格数、杠杆等策略参数也可以走 SQLite（API 下发）。
package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持 "28d" 写法的时长。
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Config 是配置文件的根结构。
type Config struct {
	App       App        `yaml:"app"`
	Server    Server     `yaml:"server"`
	Proxy     Proxy      `yaml:"proxy"`
	Exchanges []Exchange `yaml:"exchanges"`
}

type App struct {
	LogLevel          string   `yaml:"log_level"`
	LogFormat         string   `yaml:"log_format"`
	LogFile           string   `yaml:"log_file"`
	LogBufferSize     int      `yaml:"log_buffer_size"`
	DataDir           string   `yaml:"data_dir"`
	TickInterval      Duration `yaml:"tick_interval"`
	ReconcileInterval Duration `yaml:"reconcile_interval"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	HTTPTimeout       Duration `yaml:"http_timeout"`
}

type Server struct {
	Addr           string      `yaml:"addr"`
	Auth           Auth        `yaml:"auth"`
	IPWhitelist    IPWhitelist `yaml:"ip_whitelist"`
	MetricsEnabled bool        `yaml:"metrics_enabled"`
	CORSOrigins    []string    `yaml:"cors_origins"`
	// TrustedProxies 是可信反向代理的 IP/CIDR。只有 RemoteAddr 命中时才采信 X-Forwarded-For。
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type Auth struct {
	Enabled bool   `yaml:"enabled"`
	Token   string `yaml:"token"`
}

// IPWhitelist 限制可访问 HTTP API 的来源 IP。关闭时不检查。
type IPWhitelist struct {
	Enabled bool     `yaml:"enabled"`
	Allow   []string `yaml:"allow"` // 单个 IP 或 CIDR，如 1.2.3.4、10.0.0.0/8
}

type Proxy struct {
	Enabled        bool     `yaml:"enabled"`
	URL            string   `yaml:"url"`
	Username       string   `yaml:"username"`
	Password       string   `yaml:"password"`
	NoProxy        []string `yaml:"no_proxy"`
	HealthInterval Duration `yaml:"health_interval"`
}

// Exchange 是单个交易所的连接配置。
//
// 一个 name 只能出现一次：一个 DEX 只运行一个网格实例。
type Exchange struct {
	Name        string      `yaml:"name"`
	Enabled     bool        `yaml:"enabled"`
	Network     string      `yaml:"network"`
	BaseURL     string      `yaml:"base_url"`
	WSURL       string      `yaml:"ws_url"`
	Credentials Credentials `yaml:"credentials"`
	// Options 是交易所专有配置，由各适配器自己解码，
	// 这样新增交易所不需要改动本包。
	Options    yaml.Node `yaml:"options"`
	RateLimit  RateLimit `yaml:"rate_limit"`
	Timeout    Duration  `yaml:"timeout"`
	MaxRetries int       `yaml:"max_retries"`
	Reconnect  Reconnect `yaml:"reconnect"`
	// StrategyFile 指向网格参数 YAML（如 config/lighter-sol.yaml）。
	// 启动时写入 SQLite；Autostart 为 true 时接着启动该交易所的网格。
	StrategyFile string `yaml:"strategy_file"`
	Autostart    bool   `yaml:"autostart"`
}

type Credentials struct {
	AccountIndex int64 `yaml:"account_index"`
	// AccountID 是 SODEx 等用数字账户号的别名；未填时用 AccountIndex。
	AccountID        int64  `yaml:"account_id"`
	APIKeyIndex      uint8  `yaml:"api_key_index"`
	APIKeyName       string `yaml:"api_key_name"`
	APIKeyPrivateKey string `yaml:"api_key_private_key"`
	// AccountAddress 是主钱包地址（SODEx 账户查询 / WS 用户频道）。
	AccountAddress string `yaml:"account_address"`
}

// AccountIDOrIndex 返回交易所账户号：优先 account_id，否则 account_index。
func (c Credentials) AccountIDOrIndex() int64 {
	if c.AccountID > 0 {
		return c.AccountID
	}
	return c.AccountIndex
}

type RateLimit struct {
	RPS   int `yaml:"rps"`
	Burst int `yaml:"burst"`
}

type Reconnect struct {
	Initial Duration `yaml:"initial"`
	Max     Duration `yaml:"max"`
}

// DecodeOptions 把交易所专有配置解到 out。未配置 options 时不做任何事。
func (e Exchange) DecodeOptions(out any) error {
	if e.Options.IsZero() {
		return nil
	}
	if err := e.Options.Decode(out); err != nil {
		return fmt.Errorf("exchange %s: invalid options: %w", e.Name, err)
	}
	return nil
}

// Find 按名称返回交易所配置。
func (c *Config) Find(name string) (Exchange, bool) {
	for _, e := range c.Exchanges {
		if e.Name == name {
			return e, true
		}
	}
	return Exchange{}, false
}

// EnabledExchanges 返回启用的交易所配置。
func (c *Config) EnabledExchanges() []Exchange {
	var out []Exchange
	for _, e := range c.Exchanges {
		if e.Enabled {
			out = append(out, e)
		}
	}
	return out
}

func (c *Config) applyDefaults() {
	if c.App.LogLevel == "" {
		c.App.LogLevel = "info"
	}
	if c.App.LogFormat == "" {
		c.App.LogFormat = "json"
	}
	if c.App.LogBufferSize <= 0 {
		c.App.LogBufferSize = 2000
	}
	if c.App.DataDir == "" {
		c.App.DataDir = "./data"
	}
	if c.App.TickInterval == 0 {
		c.App.TickInterval = Duration(time.Second)
	}
	if c.App.ShutdownTimeout == 0 {
		c.App.ShutdownTimeout = Duration(30 * time.Second)
	}
	if c.App.HTTPTimeout == 0 {
		c.App.HTTPTimeout = Duration(15 * time.Second)
	}
	if c.Server.Addr == "" {
		c.Server.Addr = "127.0.0.1:8080"
	}
	if c.Proxy.HealthInterval == 0 {
		c.Proxy.HealthInterval = Duration(time.Minute)
	}

	for i := range c.Exchanges {
		e := &c.Exchanges[i]
		if e.Network == "" {
			e.Network = "mainnet"
		}
		if e.RateLimit.RPS <= 0 {
			e.RateLimit.RPS = 10
		}
		if e.RateLimit.Burst <= 0 {
			e.RateLimit.Burst = e.RateLimit.RPS * 2
		}
		if e.Timeout == 0 {
			e.Timeout = Duration(10 * time.Second)
		}
		if e.MaxRetries <= 0 {
			e.MaxRetries = 3
		}
		if e.Reconnect.Initial == 0 {
			e.Reconnect.Initial = Duration(time.Second)
		}
		if e.Reconnect.Max == 0 {
			e.Reconnect.Max = Duration(30 * time.Second)
		}
	}
}

// Validate 检查配置是否可用。
func (c *Config) Validate() error {
	switch c.App.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("app.log_level: 未知的日志级别 %q", c.App.LogLevel)
	}
	switch c.App.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("app.log_format: 未知的日志格式 %q", c.App.LogFormat)
	}

	if c.Server.Auth.Enabled && strings.TrimSpace(c.Server.Auth.Token) == "" {
		return fmt.Errorf("server.auth.enabled 为 true 时必须提供 server.auth.token（建议用环境变量注入）")
	}
	if c.Server.IPWhitelist.Enabled && len(c.Server.IPWhitelist.Allow) == 0 {
		return fmt.Errorf("server.ip_whitelist.enabled 为 true 时必须至少配置一条 allow")
	}
	if !c.Server.Auth.Enabled && !c.Server.IPWhitelist.Enabled && isPublicAddr(c.Server.Addr) {
		return fmt.Errorf("server.addr 为 %s（公网可达）时必须启用 server.auth 或 server.ip_whitelist；只本机访问请改成 127.0.0.1:8080", c.Server.Addr)
	}

	if c.Proxy.Enabled {
		if c.Proxy.URL == "" {
			return fmt.Errorf("proxy.enabled 为 true 时必须提供 proxy.url")
		}
		if _, err := url.Parse(c.Proxy.URL); err != nil {
			return fmt.Errorf("proxy.url 无法解析: %w", err)
		}
	}

	seen := map[string]bool{}
	enabled := 0
	for i, e := range c.Exchanges {
		if e.Name == "" {
			return fmt.Errorf("exchanges[%d].name 不能为空", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("交易所 %q 重复配置：一个 DEX 只能有一个实例", e.Name)
		}
		seen[e.Name] = true

		if !e.Enabled {
			continue
		}
		enabled++
		switch e.Network {
		case "mainnet", "testnet":
		default:
			return fmt.Errorf("exchanges[%s].network: 未知网络 %q", e.Name, e.Network)
		}
		if e.Credentials.AccountIDOrIndex() <= 0 {
			return fmt.Errorf("exchanges[%s].credentials.account_index / account_id 缺失", e.Name)
		}
		if strings.TrimSpace(e.Credentials.APIKeyPrivateKey) == "" {
			return fmt.Errorf(
				"exchanges[%s].credentials.api_key_private_key 为空（环境变量未设置或 .env 未加载？）", e.Name)
		}
	}
	if enabled == 0 {
		return fmt.Errorf("没有启用任何交易所")
	}
	return nil
}

func isPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && !ip.IsLoopback()
}
