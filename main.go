package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hanime-dl/chrome"
	"hanime-dl/config"
	"hanime-dl/downloader"
	"hanime-dl/scraper"
	"hanime-dl/web"
)

var globalConfig *config.Config

// VideoTask 下载任务
type VideoTask struct {
	VideoID string
	ListID  string
}

// ListStatus 播放列表进度
type ListStatus struct {
	Total   int
	Pending int
	Mutex   sync.Mutex
}

var (
	listProgress = make(map[string]*ListStatus)
	progressMu   sync.Mutex
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	webMode := flag.Bool("web", false, "Start web server instead of CLI mode")
	webAddr := flag.String("web-addr", ":8080", "Web server address")
	flag.Parse()

	// Web 模式
	if *webMode {
		cfg := config.MustLoad(*configPath)
		cfg.ClearCache = true
		wsURL, launchedLocalChrome, err := chrome.EnsureChromeConnectionDetailed(cfg.ChromeRemoteURL, chrome.StartupCDPTimeout)
		if err != nil {
			log.Printf("Failed to prepare Chrome CDP for web mode: %v", err)
			log.Println("\n=== Chrome Connection Options ===")
			log.Println("Option 1: Start Chrome manually with remote debugging:")
			log.Println("  google-chrome --remote-debugging-port=9222")
			log.Println("Option 2: Use Docker desktop environment:")
			log.Println("  cd ubuntu-desktop && docker compose up -d")
			log.Println("Option 3: Configure remote Chrome in config.yaml:")
			log.Println("  chromeRemoteURL: http://your-chrome-host:9222/json/version")
			log.Fatal("\nUnable to connect to Chrome. Please set up Chrome and try again.")
		}

		if launchedLocalChrome {
			webURL := webInterfaceURL(*webAddr)
			go func() {
				waitForHTTPReady(webURL, 10*time.Second)
				if err := chrome.OpenURL(wsURL, webURL, chrome.OpenPageTimeout); err != nil {
					log.Printf("Failed to open web interface in local Chrome: %v", err)
					return
				}
				log.Printf("Opened web interface in local Chrome: %s", webURL)
			}()
		}

		web.RunWebServer(web.Options{
			Addr:                *webAddr,
			CacheDir:            cfg.CacheDir,
			DownDir:             cfg.DownDir,
			MaxWorkers:          cfg.MaxDownloadWorkers,
			ChromeRemoteURL:     cfg.ChromeRemoteURL,
			WSURL:               wsURL,
			HTTPProxy:           cfg.HttpProxy,
			DirectDownloadFirst: cfg.DirectDownloadFirst,
			VideoResolution:     cfg.VideoResolution,
			ClearCache:          cfg.ClearCache,
			SingleCode:          cfg.SingleCode,
			ListCode:           cfg.ListCode,
		})
		return
	}

	// 加载配置
	globalConfig = config.MustLoad(*configPath)
	globalConfig.ClearCache = true

	// 确保目录存在
	os.MkdirAll(globalConfig.CacheDir, 0755)
	os.MkdirAll(globalConfig.DownDir, 0755)

	// 设置默认值
	if globalConfig.MaxDownloadWorkers <= 0 {
		globalConfig.MaxDownloadWorkers = 3
	}

	log.Printf("Configuration loaded: RemoteURL=%s, DownDir=%s, CacheDir=%s, Workers=%d",
		globalConfig.ChromeRemoteURL, globalConfig.DownDir, globalConfig.CacheDir, globalConfig.MaxDownloadWorkers)

	// 连接 Chrome
	wsURL, err := chrome.EnsureChromeConnection(globalConfig.ChromeRemoteURL, chrome.StartupCDPTimeout)
	if err != nil {
		log.Printf("Failed to prepare Chrome CDP: %v", err)
		log.Println("\n=== Chrome Connection Options ===")
		log.Println("Option 1: Start Chrome manually with remote debugging:")
		log.Println("  google-chrome --remote-debugging-port=9222")
		log.Println("Option 2: Use Docker desktop environment:")
		log.Println("  cd ubuntu-desktop && docker compose up -d")
		log.Println("Option 3: Configure remote Chrome in config.yaml:")
		log.Println("  chromeRemoteURL: http://your-chrome-host:9222/json/version")
		log.Fatal("\nUnable to connect to Chrome. Please set up Chrome and try again.")
	}

	// 注册清理
	defer func() {
		log.Println("Cleaning up...")
	}()

	// 创建抓取器和下载器
	s := scraper.NewScraper(globalConfig.CacheDir, globalConfig.DownDir, globalConfig.VideoResolution)
	d := downloader.NewDownloader(globalConfig.HttpProxy, globalConfig.DirectDownloadFirst)

	// 下载队列
	downloadQueue := make(chan scraper.VideoMetadata, 100)
	var downloadWg sync.WaitGroup

	// 启动下载 Worker
	for i := 0; i < globalConfig.MaxDownloadWorkers; i++ {
		downloadWg.Add(1)
		go func(workerId int) {
			defer downloadWg.Done()
			for meta := range downloadQueue {
				log.Printf("[Worker %d] Starting download for: %s", workerId, meta.Title)

				// 下载图片
				if meta.ImageURL != "" && meta.ImageFilePath != "" {
					if _, err := os.Stat(meta.ImageFilePath); os.IsNotExist(err) {
						d.DownloadWithRetry(meta.ImageURL, meta.ImageFilePath)
					} else {
						log.Printf("[Worker %d] Image exists: %s", workerId, meta.ImageFilePath)
					}
				}

				time.Sleep(3 * time.Second)

				// 下载视频
				if meta.DataURL != "" && meta.VideoFilePath != "" {
					if _, err := os.Stat(meta.VideoFilePath); os.IsNotExist(err) {
						d.DownloadWithRetry(meta.DataURL, meta.VideoFilePath)
					} else {
						log.Printf("[Worker %d] Video exists: %s", workerId, meta.VideoFilePath)
					}
				}

				// 检查下载成功并清理缓存
				videoSuccess := false
				if meta.DataURL != "" && meta.VideoFilePath != "" {
					if _, err := os.Stat(meta.VideoFilePath); err == nil {
						videoSuccess = true
					}
				}

				if videoSuccess && globalConfig.ClearCache {
					cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("info_%s.json", meta.VideoID))
					os.Remove(cacheFile)
					log.Printf("[Worker %d] Cleared cache for %s", workerId, meta.VideoID)
				}

				// 更新播放列表进度
				if videoSuccess && meta.ListID != "" {
					progressMu.Lock()
					if status, ok := listProgress[meta.ListID]; ok {
						status.Mutex.Lock()
						status.Pending--
						pending := status.Pending
						status.Mutex.Unlock()

						if pending == 0 && globalConfig.ClearCache {
							cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", meta.ListID))
							os.Remove(cacheFile)
							log.Printf("[Worker %d] Cleared list cache for %s", workerId, meta.ListID)
						}
					}
					progressMu.Unlock()
				}
			}
		}(i)
	}

	// 任务队列
	taskQueue := make(chan VideoTask, 1000)

	// 启动发现协程
	go func() {
		defer close(taskQueue)

		taskMap := make(map[string]bool)
		addTask := func(task VideoTask) {
			if !taskMap[task.VideoID] {
				taskQueue <- task
				taskMap[task.VideoID] = true
			}
		}

		// 1. 单个视频
		for _, id := range globalConfig.SingleCode {
			addTask(VideoTask{VideoID: id, ListID: ""})
		}

		// 2. 区分缓存和网络的播放列表
		var cachedLists, networkLists []string
		for _, listID := range globalConfig.ListCode {
			cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", listID))
			if _, err := os.Stat(cacheFile); err == nil {
				cachedLists = append(cachedLists, listID)
			} else {
				networkLists = append(networkLists, listID)
			}
		}

		processList := func(listID string) {
			log.Printf("Fetching playlist for ID: %s", listID)
			list, err := s.GetPlaylist(wsURL, listID)
			if err != nil {
				log.Printf("Failed to get playlist %s: %v", listID, err)
				return
			}
			log.Printf("Playlist %s acquired: %d videos found", listID, len(list))

			progressMu.Lock()
			if _, ok := listProgress[listID]; !ok {
				listProgress[listID] = &ListStatus{
					Total:   len(list),
					Pending: len(list),
				}
			}
			progressMu.Unlock()

			for _, vid := range list {
				taskQueue <- VideoTask{VideoID: vid, ListID: listID}
				taskMap[vid] = true
			}
		}

		// 3. 处理缓存的播放列表
		for _, listID := range cachedLists {
			processList(listID)
		}

		// 4. 扫描缓存中待处理的播放列表
		listFiles, _ := filepath.Glob(filepath.Join(globalConfig.CacheDir, "list_*.json"))
		for _, f := range listFiles {
			base := filepath.Base(f)
			if filepath.Ext(base) == ".json" && len(base) > 5 {
				listID := base[5 : len(base)-5]
				alreadyProcessed := false
				for _, cfgID := range globalConfig.ListCode {
					if cfgID == listID {
						alreadyProcessed = true
						break
					}
				}
				if !alreadyProcessed {
					log.Printf("Found cached playlist %s, resuming...", listID)
					processList(listID)
				}
			}
		}

		// 5. 扫描缓存中待处理的视频
		infoFiles, _ := filepath.Glob(filepath.Join(globalConfig.CacheDir, "info_*.json"))
		for _, f := range infoFiles {
			base := filepath.Base(f)
			if filepath.Ext(base) == ".json" && len(base) > 5 {
				vid := base[5 : len(base)-5]
				if !taskMap[vid] {
					log.Printf("Found cached video info for %s, resuming...", vid)
					addTask(VideoTask{VideoID: vid, ListID: ""})
				}
			}
		}

		// 6. 处理网络的播放列表
		for _, listID := range networkLists {
			processList(listID)
		}

		log.Println("Discovery completed, closing task queue.")
	}()

	// 主线程：解析视频信息
	for task := range taskQueue {
		meta, err := s.ResolveVideoInfo(wsURL, task.VideoID, task.ListID)
		if err != nil {
			log.Printf("Skipping %s due to resolution failure: %v", task.VideoID, err)

			if task.ListID != "" {
				progressMu.Lock()
				if status, ok := listProgress[task.ListID]; ok {
					status.Mutex.Lock()
					status.Pending--
					status.Mutex.Unlock()
				}
				progressMu.Unlock()
			}
			continue
		}

		downloadQueue <- meta
	}

	close(downloadQueue)
	downloadWg.Wait()

	log.Println("All tasks completed.")
}

// jsonMarshal 辅助函数
func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func probeHTTPReady(target string) bool {
	u := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	hostPort := u
	if slash := strings.Index(hostPort, "/"); slash >= 0 {
		hostPort = hostPort[:slash]
	}
	conn, err := net.DialTimeout("tcp", hostPort, 500*time.Millisecond)
	if err == nil {
		conn.Close()
	}
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

func waitForHTTPReady(target string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probeHTTPReady(target) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func webInterfaceURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if len(addr) > 0 && addr[0] == ':' {
			return "http://localhost" + addr
		}
		return "http://" + addr
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}

	return fmt.Sprintf("http://%s:%s", host, port)
}
