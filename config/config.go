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
	// DefaultChromeInsecureTLS 是否允许 CDP 端点使用自签名证书。
	// 默认 false：跳过证书校验会让中间人伪造「Chrome 调试端点」并接管 DevTools，
	// 只有确实使用自签名端点时才应显式打开。
	DefaultChromeInsecureTLS = false
	// DefaultAPIToken Web API 的访问令牌。默认空 = 不启用鉴权（本机单人使用的形态）。
	// 一旦这个服务可能被同网段其它设备访问到，就应该设置一个非空 token。
	DefaultAPIToken = ""
	// DefaultAPIAllowOrigin 允许跨源调用 API 的来源。默认空 = 不设 CORS 头（同源策略）。
	// 历史实现无条件返回 `Access-Control-Allow-Origin: *`，
	// 于是用户浏览器里任意网页都能跨源写这个服务的任务队列。
	DefaultAPIAllowOrigin = ""
)

// DefaultConfigYAML 默认配置模板（对齐 README 与 config.example.yaml 的通用占位值）。
// 当 config.yaml 缺失时，LoadOrCreate 会将本内容落盘为默认配置文件。
// 内容由上面的默认常量拼装，因此常量与模板不会各自漂移；不含任何私有网络地址。
// 修改本模板时，请同步更新仓库根目录的 config.example.yaml（其正文应与本模板一致）。
//
// 新增的三个安全相关项默认都是「最保守」的取值：不跳过证书校验、不启用跨源、
// 不鉴权（鉴权默认关闭是为了不破坏本机既有用法，而不是因为它安全——
// README 的「安全与暴露方式」一节说明了什么时候必须打开）。
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

# 允许 CDP 端点使用自签名证书（跳过证书校验）。默认 false。
# 仅在确实使用自签名远程 Chrome 时打开；打开后中间人可伪造调试端点。
chromeInsecureTLS: %t

# Web API 访问令牌。留空 = 不启用鉴权（仅适合本机单人使用）。
# 非空时所有 /api/* 请求必须带 Authorization: Bearer <token> 头。
apiToken: "%s"

# 允许跨源调用 API 的来源（如 http://localhost:3000）。留空 = 同源策略，不发 CORS 头。
apiAllowOrigin: "%s"
`, DefaultChromeRemoteURL, DefaultCacheDir, DefaultDownDir, DefaultHTTPProxy,
	DefaultMaxDownloadWorkers, DefaultClearCache, DefaultVideoResolution,
	DefaultChromeInsecureTLS, DefaultAPIToken, DefaultAPIAllowOrigin)

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
	// ChromeInsecureTLS 允许 CDP 端点使用自签名证书（跳过证书校验）。默认 false。
	ChromeInsecureTLS bool `yaml:"chromeInsecureTLS"`
	// APIToken 非空时，Web API 要求 `Authorization: Bearer <token>`。默认空 = 不鉴权。
	APIToken string `yaml:"apiToken"`
	// APIAllowOrigin 需要跨源调用 API 时的显式来源。默认空 = 不发 CORS 头。
	APIAllowOrigin string `yaml:"apiAllowOrigin"`
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
