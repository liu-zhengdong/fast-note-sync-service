package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigPreservesExplicitFalseAndEmptyDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(configPath, []byte(`
app:
  ws-parallel-enabled: false
  ws-check-utf8-enabled: false
  ws-compression-enabled: false
security:
  webgui-login-token-expiry:
  webgui-login-token-bind-ip: false
tracer:
  enabled: false
storage:
  local-fs:
    httpfs-is-enable: false
  aliyun-oss:
    is-enable: false
  aws-s3:
    is-enable:
database:
  auto-migrate: false
user-database:
  auto-migrate: false
`), 0644)
	require.NoError(t, err)

	cfg, _, err := LoadConfig(configPath)
	require.NoError(t, err)

	require.False(t, *cfg.App.WebSocketParallelEnabled)
	require.False(t, *cfg.App.WebSocketCheckUtf8Enabled)
	require.False(t, *cfg.App.WebSocketCompressionEnabled)
	require.False(t, *cfg.Security.WebGUILoginTokenBindIP)
	require.False(t, *cfg.Tracer.Enabled)
	require.False(t, *cfg.Storage.LocalFS.HttpfsIsEnable)
	require.False(t, *cfg.Storage.AliyunOSS.IsEnabled)
	require.False(t, *cfg.Database.AutoMigrate)
	require.False(t, *cfg.UserDatabase.AutoMigrate)

	require.Equal(t, "7d", cfg.Security.WebGUILoginTokenExpiry)
	require.True(t, *cfg.Storage.AwsS3.IsEnabled)
	require.True(t, *cfg.Storage.MinIO.IsEnabled)
}

func TestLoadConfigGitHubProxiesNilEmptyAndCustom(t *testing.T) {
	unset, _, err := LoadConfig(writeTestConfig(t, "app: {}\n"))
	require.NoError(t, err)
	require.Nil(t, unset.App.GitHubProxies)

	empty, _, err := LoadConfig(writeTestConfig(t, "app:\n  github-proxies: []\n"))
	require.NoError(t, err)
	require.NotNil(t, empty.App.GitHubProxies)
	require.Empty(t, *empty.App.GitHubProxies)

	custom, _, err := LoadConfig(writeTestConfig(t, "app:\n  github-proxies:\n    - https://example.test/\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.test/"}, *custom.App.GitHubProxies)

	require.NoError(t, empty.Save())
	reloaded, _, err := LoadConfig(empty.File)
	require.NoError(t, err)
	require.NotNil(t, reloaded.App.GitHubProxies)
	require.Empty(t, *reloaded.App.GitHubProxies)
}

func TestLoadConfigMCPDisableLocalhostProtection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(configPath, []byte(`
server:
  mcp-disable-localhost-protection: true
`), 0644)
	require.NoError(t, err)

	cfg, _, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.True(t, cfg.Server.MCPDisableLocalhostProtection)
}
