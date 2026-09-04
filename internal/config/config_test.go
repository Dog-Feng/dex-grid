package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const minimalYAML = `
app:
  log_level: debug
  data_dir: ./data
  tick_interval: 2s
server:
  addr: "127.0.0.1:9000"
exchanges:
  - name: lighter
    enabled: true
    network: mainnet
    credentials:
      account_index: 411813
      api_key_index: 4
      api_key_private_key: ${TEST_LIGHTER_KEY}
    options:
      tx_send_channel: ws
      order_expiry: 28d
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("TEST_LIGHTER_KEY", "deadbeef")

	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := cfg.Find("lighter")
	if !ok {
		t.Fatal("未找到 lighter 交易所配置")
	}
	if ex.Credentials.APIKeyPrivateKey != "deadbeef" {
		t.Fatalf("私钥展开结果 = %q，期望 deadbeef", ex.Credentials.APIKeyPrivateKey)
	}
	if cfg.App.TickInterval.Std() != 2*time.Second {
		t.Fatalf("tick_interval = %s，期望 2s", cfg.App.TickInterval.Std())
	}
}

// 环境变量没设置时要给出明确提示，而不是让空私钥一路跑到下单时才失败。
func TestLoadReportsMissingEnv(t *testing.T) {
	_, err := Load(writeConfig(t, minimalYAML))
	if err == nil {
		t.Fatal("私钥为空时应当报错")
	}
	if got := err.Error(); !contains(got, "TEST_LIGHTER_KEY") {
		t.Fatalf("错误信息应当点名缺失的环境变量，实际为: %s", got)
	}
}

// 公网监听且未开鉴权时拒绝启动。
func TestValidateRejectsPublicAddrWithoutAuth(t *testing.T) {
	cfg := &Config{}
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Exchanges = []Exchange{{
		Name: "lighter", Enabled: true, Network: "mainnet",
		Credentials: Credentials{AccountIndex: 1, APIKeyPrivateKey: "x"},
	}}
	cfg.applyDefaults()
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Server.Auth.Enabled = false

	if err := cfg.Validate(); err == nil {
		t.Fatal("公网监听且无鉴权应当拒绝启动")
	}

	cfg.Server.Auth.Enabled = true
	cfg.Server.Auth.Token = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("开启鉴权但未提供 token 时应当拒绝")
	}
	cfg.Server.Auth.Token = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("开启鉴权并提供 token 后应当通过: %v", err)
	}
}

// 一个 DEX 只能有一个实例。
func TestValidateRejectsDuplicateExchange(t *testing.T) {
	cfg := &Config{Exchanges: []Exchange{
		{Name: "lighter", Enabled: true, Credentials: Credentials{AccountIndex: 1, APIKeyPrivateKey: "x"}},
		{Name: "lighter", Enabled: false},
	}}
	cfg.applyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("重复配置同一个交易所时应当报错")
	}
}

func TestLoadAcceptsSodexAccountID(t *testing.T) {
	body := `
app:
  log_level: info
  data_dir: ./data
server:
  addr: "127.0.0.1:9000"
exchanges:
  - name: sodex
    enabled: true
    network: mainnet
    credentials:
      account_id: 6222
      account_address: "0x1111111111111111111111111111111111111111"
      api_key_name: boom
      api_key_private_key: dummy
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := cfg.Find("sodex")
	if !ok {
		t.Fatal("未找到 sodex")
	}
	if ex.Credentials.AccountIDOrIndex() != 6222 {
		t.Fatalf("AccountIDOrIndex = %d，期望 6222", ex.Credentials.AccountIDOrIndex())
	}
	if ex.Credentials.APIKeyName != "boom" {
		t.Fatalf("APIKeyName = %q", ex.Credentials.APIKeyName)
	}
	if ex.Credentials.AccountAddress != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("AccountAddress = %q", ex.Credentials.AccountAddress)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	t.Setenv("TEST_LIGHTER_KEY", "deadbeef")
	body := minimalYAML + "\nunknown_section:\n  foo: bar\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("配置项写错名字时应当报错，而不是被静默忽略")
	}
}

func TestParseDurationSupportsDays(t *testing.T) {
	got, err := ParseDuration("28d")
	if err != nil {
		t.Fatal(err)
	}
	if want := 28 * 24 * time.Hour; got.Std() != want {
		t.Fatalf("28d = %s，期望 %s", got.Std(), want)
	}
}

func TestLoadDotEnvDoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# 注释\nFOO=from_file\nexport BAR=\"quoted\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOO", "from_shell")

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO"); got != "from_shell" {
		t.Fatalf("FOO = %q，系统环境变量应当优先于 .env", got)
	}
	if got := os.Getenv("BAR"); got != "quoted" {
		t.Fatalf("BAR = %q，期望 quoted", got)
	}
	os.Unsetenv("BAR")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
