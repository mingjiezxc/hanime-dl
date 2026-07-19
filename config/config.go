package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultConfigYAML 默认配置（对齐 README 通用占位值）
// 当 config.yaml 缺失时将其落盘为默认配置文件，不含任何私有网络地址。
const DefaultConfigYAML = `# Chrome 远程调试 URL
chromeRemoteURL: http://localhost:9222/json/version

# 缓存目录
CacheDir: ./cache

# 下载目录
DownDir: ./downloads

# HTTP 代理（可选）
HttpProxy: http://proxy.example.com:8080

# 是否优先尝试直接下载
DirectDownloadFirst: true

# 最大并发下载线程数
MaxDownloadWorkers: 3

# 播放列表 ID 列表
ListCode: []

# 单个视频 ID 列表
SingleCode: []

# 下载后清除缓存
ClearCache: true

# 视频分辨率（如：1080p, 720p）
VideoResolution: 1080p
`

// Config 应用程序配置
type Config struct {
	ChromeRemoteURL     string   `yaml:"chromeRemoteURL"`
	CacheDir            string   `yaml:"CacheDir"`
	DownDir             string   `yaml:"DownDir"`
	HttpProxy           string   `yaml:"HttpProxy"`
	DirectDownloadFirst bool     `yaml:"DirectDownloadFirst"`
	MaxDownloadWorkers  int      `yaml:"MaxDownloadWorkers"`
	ListCode            []string `yaml:"ListCode"`
	SingleCode          []string `yaml:"SingleCode"`
	ClearCache          bool     `yaml:"ClearCache"`
	VideoResolution     string   `yaml:"VideoResolution"`
}

// Load 从 YAML 文件加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// MustLoad 加载配置，失败则 panic
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}
	return cfg
}

// LoadOrCreate 存在则加载配置；缺失则写入默认配置后再加载。
// 返回值 created 表示本次是否新建了默认配置文件。
func LoadOrCreate(path string) (cfg *Config, created bool, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, false, statErr
		}
		if wErr := os.WriteFile(path, []byte(DefaultConfigYAML), 0644); wErr != nil {
			return nil, false, fmt.Errorf("generate default config %s: %w", path, wErr)
		}
		created = true
	}
	cfg, err = Load(path)
	return cfg, created, err
}

// MustLoadOrCreate 包裹 LoadOrCreate，仅在非缺失类严重错误时 panic。
// 文件缺失属于预期自愈场景，不会 panic。
func MustLoadOrCreate(path string) (*Config, bool) {
	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		panic(err)
	}
	return cfg, created
}
