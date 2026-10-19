package config

// 本文件是 config 包的第一个测试文件（审计记录的「chrome/ 与 config/ 仍无测试」
// 的一半）。全部离线：只读写 t.TempDir() 与仓库内的示例文件，不触网。

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDefaultConfigYAMLMatchesExample 锁住一份靠人工维持的约定。
//
// README 与 config.example.yaml 的抬头都写着「修改其中一处时请同步另一处」——
// 这类约定一定会烂掉，除非有东西盯着。这里把两者解析成同一个结构体逐字段比对：
// 以后任何人只改一边，这条用例就红。
func TestDefaultConfigYAMLMatchesExample(t *testing.T) {
	examplePath := filepath.Join("..", "config.example.yaml")
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("读取 %s 失败（文件被移动或删除？）: %v", examplePath, err)
	}

	var fromExample Config
	if err := yaml.Unmarshal(data, &fromExample); err != nil {
		t.Fatalf("解析 config.example.yaml: %v", err)
	}
	var fromDefault Config
	if err := yaml.Unmarshal([]byte(DefaultConfigYAML), &fromDefault); err != nil {
		t.Fatalf("解析 DefaultConfigYAML: %v", err)
	}

	if !reflect.DeepEqual(fromExample, fromDefault) {
		t.Errorf("config.example.yaml 与 DefaultConfigYAML 已漂移：\n example = %+v\n default = %+v",
			fromExample, fromDefault)
	}
}

// TestGeneratedDefaultConfigRoundTrips 覆盖「缺失即自动生成」这条自愈路径，
// 并断言生成出来的内容就是默认常量，而不是另一份手写副本。
func TestGeneratedDefaultConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !created {
		t.Errorf("created = false, want true（文件本不存在）")
	}
	if cfg == nil {
		t.Fatal("cfg = nil")
	}

	want := &Config{
		ChromeRemoteURL:    DefaultChromeRemoteURL,
		CacheDir:           DefaultCacheDir,
		DownDir:            DefaultDownDir,
		HttpProxy:          DefaultHTTPProxy,
		MaxDownloadWorkers: DefaultMaxDownloadWorkers,
		// 模板里写的是 `ListCode: []`，yaml.v3 解出的是**非 nil 的空切片**；
		// 这里必须同样写空切片，否则 reflect.DeepEqual 会把 nil 与 [] 判为不等。
		ListCode:          []string{},
		SingleCode:        []string{},
		ClearCache:        DefaultClearCache,
		VideoResolution:   DefaultVideoResolution,
		ChromeInsecureTLS: DefaultChromeInsecureTLS,
		APIToken:          DefaultAPIToken,
		APIAllowOrigin:    DefaultAPIAllowOrigin,
	}
	if !reflect.DeepEqual(*cfg, *want) {
		t.Errorf("生成的默认配置与常量不一致：\n got  = %+v\n want = %+v", *cfg, *want)
	}

	// 第二次调用不得覆盖用户文件。
	if err := os.WriteFile(path, []byte("CacheDir: ./mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, created2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate(已存在): %v", err)
	}
	if created2 {
		t.Errorf("created = true, want false（已存在的文件不得被重建）")
	}
	if cfg2.CacheDir != "./mine" {
		t.Errorf("CacheDir = %q, want ./mine（用户配置被覆盖了？）", cfg2.CacheDir)
	}
}

// TestSecurityDefaultsAreConservative 是 Tranche A 的契约测试：
// 三个新配置项的默认值必须是「最保守」的一档。
//
// 注意这是契约测试而不是回归测试——它同样能通过改动前的代码（那时根本没有这些字段），
// 作用是防止以后有人把默认值改成不安全的一侧。
func TestSecurityDefaultsAreConservative(t *testing.T) {
	if DefaultChromeInsecureTLS {
		t.Errorf("DefaultChromeInsecureTLS = true, want false（默认不得跳过证书校验）")
	}
	if DefaultAPIToken != "" {
		t.Errorf("DefaultAPIToken 非空会让默认配置带上一个公开已知的令牌")
	}
	if DefaultAPIAllowOrigin != "" {
		t.Errorf("DefaultAPIAllowOrigin = %q, want 空（默认同源）", DefaultAPIAllowOrigin)
	}

	// 生成的默认配置里必须真的写着这些保守值，而不只是常量正确。
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, _, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if cfg.ChromeInsecureTLS {
		t.Errorf("生成的默认配置把 chromeInsecureTLS 写成了 true")
	}
}

// TestUnknownKeyIsIgnored 锁住一条刻意的取舍：**不启用** yaml KnownFields。
//
// v0.4.7 移除了 DirectDownloadFirst，但用户手上的旧 config.yaml 里可能还留着这一行。
// 若开启严格解码，升级后第一次运行就会直接报错，等于把一次「删掉无效字段」
// 变成一次「破坏所有旧配置」。这里用断言把该决定钉住，避免以后被当成疏漏"修好"。
func TestUnknownKeyIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "CacheDir: ./c\nDirectDownloadFirst: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("含未知键的旧配置必须能加载（升级平滑），实际: %v", err)
	}
	if cfg.CacheDir != "./c" {
		t.Errorf("CacheDir = %q, want ./c", cfg.CacheDir)
	}
}
