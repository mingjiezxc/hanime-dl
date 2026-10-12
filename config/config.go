package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// 各配置项的权威默认值。这些常量是默认值的唯一来源：
// DefaultConfigYAML 由它们拼装（不会漂移），其他包（如 web.RunWebServer 的兜底值）
// 需要默认值时也应引用这里，而不是各自写一份字面量。
const (
	// DefaultChromeRemoteURL Chrome 远程调试地址。
	DefaultChromeRemoteURL = "http://localhost:9222/json/version"
	// DefaultCacheDir 缓存目录。
	DefaultCacheDir = "./cache"
	// DefaultDownDir 下载目录。
	// 注意：这是全仓唯一权威默认值。历史上的不一致是
	// web/web_server.go 的 RunWebServer 用了 "./download"，与本值不同，
	// 该处应改为引用 config.DefaultDownDir。
	DefaultDownDir = "./downloads"
	// DefaultHTTPProxy 代理占位值（通用示例地址，非真实可用代理）。
	DefaultHTTPProxy = "http://proxy.example.com:8080"
	// DefaultMaxDownloadWorkers 并发下载线程数。
	DefaultMaxDownloadWorkers = 3
	// DefaultClearCache 下载完成后是否清除缓存。
	// 默认 true：与历史行为一致（main.go 曾无条件覆盖为 true）。
	DefaultClearCache = true
	// DefaultVideoResolution 目标分辨率；未配置或未命中时由下载侧回落最高分辨率。
	DefaultVideoResolution = "1080p"
)

// DefaultConfigYAML 默认配置模板（对齐 README 与 config.example.yaml 的通用占位值）。
// 当 config.yaml 缺失时，LoadOrCreate 会将本内容落盘为默认配置文件。
// 内容由上面的默认常量拼装，因此常量与模板不会各自漂移；不含任何私有网络地址。
// 修改本模板时，请同步更新仓库根目录的 config.example.yaml（其正文应与本模板一致）。
var DefaultConfigYAML = fmt.Sprintf(`# Chrome 远程调试 URL
chromeRemoteURL: %s

# 缓存目录
CacheDir: %s

# 下载目录
DownDir: %s

# HTTP 代理（可选，留空表示直连）
HttpProxy: %s

# 最大并发下载线程数
MaxDownloadWorkers: %d

# 播放列表 ID 列表（字符串数组，可留空；写法：- "playlist-id-1"）
ListCode: []

# 单个视频 ID 列表（字符串数组，可留空；写法：- "video-id-1"）
SingleCode: []

# 下载后清除缓存
ClearCache: %t

# 视频分辨率（如：1080p, 720p；未配置或未命中时回落最高分辨率）
VideoResolution: %s
`, DefaultChromeRemoteURL, DefaultCacheDir, DefaultDownDir, DefaultHTTPProxy,
	DefaultMaxDownloadWorkers, DefaultClearCache, DefaultVideoResolution)

// Config 应用程序配置。
// 字段的 yaml tag 使用与 config.example.yaml / DefaultConfigYAML 完全一致的大小写写法，
// 请勿改动其中任一处，否则解析会失效。
type Config struct {
	ChromeRemoteURL    string   `yaml:"chromeRemoteURL"`
	CacheDir           string   `yaml:"CacheDir"`
	DownDir            string   `yaml:"DownDir"`
	HttpProxy          string   `yaml:"HttpProxy"`
	MaxDownloadWorkers int      `yaml:"MaxDownloadWorkers"`
	ListCode           []string `yaml:"ListCode"`
	SingleCode         []string `yaml:"SingleCode"`
	ClearCache         bool     `yaml:"ClearCache"`
	VideoResolution    string   `yaml:"VideoResolution"`
}

// Load 从 YAML 文件加载配置。
//
// 这是底层读取函数，由 LoadOrCreate 调用，因此并非死代码（审计曾将其列为未使用）。
// 需要「缺失即自动生成默认配置」的语义请用 LoadOrCreate / MustLoadOrCreate。
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
