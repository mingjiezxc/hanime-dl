package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

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
