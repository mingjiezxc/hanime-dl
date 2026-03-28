package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
)

const (
	maxRetries            = 5
	retryInterval         = 5 * time.Second
	hanimeWatchURL        = "https://hanime1.me/watch?v=%s"
	hanimeDownloadURL     = "https://hanime1.me/download?v=%s"
	playlistSelector      = `#video-playlist-wrapper a`
	downloadTitleSelector = `h3`
	downloadImageSelector = `img.download-image`
)

var (
	chromedpMu         sync.Mutex
	globalChromeWSURL  string
)

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

type VideoTask struct {
	VideoID string
	ListID  string
}

type Result struct {
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	DataURL  string `json:"data_url"`
}

// VideoMetadata stores resolved video info for caching
type VideoMetadata struct {
	VideoID       string `json:"video_id"`
	Title         string `json:"title"`
	ImageURL      string `json:"image_url"`
	DataURL       string `json:"data_url"`
	ImageFilePath string `json:"image_file_path"`
	VideoFilePath string `json:"video_file_path"`
	TargetDir     string `json:"target_dir"`
	ListID        string `json:"list_id"`
}

var globalConfig Config

// ListStatus tracks the progress of a playlist
type ListStatus struct {
	Total   int
	Pending int
	Mutex   sync.Mutex
}

var (
	listProgress = make(map[string]*ListStatus)
	progressMu   sync.Mutex
)

// ProgressWriter tracks write progress
type ProgressWriter struct {
	Total       int64
	Downloaded  int64
	LastPrinted time.Time
	FileName    string
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.Downloaded += int64(n)
	return n, nil
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Default configuration
	globalConfig.ClearCache = true

	// Load configuration
	if err := loadConfig(*configPath); err != nil {
		log.Printf("Warning: Failed to load config from %s: %v. Using defaults/command line args may be required.", *configPath, err)
	}

	// Ensure directories exist
	if globalConfig.CacheDir == "" {
		globalConfig.CacheDir = "."
	}
	if globalConfig.DownDir == "" {
		globalConfig.DownDir = "."
	}
	os.MkdirAll(globalConfig.CacheDir, 0755)
	os.MkdirAll(globalConfig.DownDir, 0755)

	if globalConfig.ChromeRemoteURL == "" {
		globalConfig.ChromeRemoteURL = "http://localhost:9222/json/version"
	}
	if globalConfig.MaxDownloadWorkers <= 0 {
		globalConfig.MaxDownloadWorkers = 3 // Default
	}

	log.Printf("Configuration loaded: RemoteURL=%s, DownDir=%s, CacheDir=%s, Workers=%d",
		globalConfig.ChromeRemoteURL, globalConfig.DownDir, globalConfig.CacheDir, globalConfig.MaxDownloadWorkers)

	var croUrl string
	var err error
	for i := 0; i < maxRetries; i++ {
		croUrl, err = GetWebSocketDebuggerURL(globalConfig.ChromeRemoteURL)

		if err == nil {
			log.Printf("Successfully got WebSocket debugger URL: %s", croUrl)
			break
		}
		log.Printf("Failed to get WebSocket debugger URL (attempt %d/%d): %v", i+1, maxRetries, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	if err != nil {
		log.Fatalf("Failed to get WebSocket debugger URL after %d attempts: %v", maxRetries, err)
	}
	globalChromeWSURL = croUrl

	// Channel for fully resolved metadata ready for download
	downloadQueue := make(chan VideoMetadata, 100)
	var downloadWg sync.WaitGroup

	// Start Download Workers (Consumer)
	for i := 0; i < globalConfig.MaxDownloadWorkers; i++ {
		downloadWg.Add(1)
		go func(workerId int) {
			defer downloadWg.Done()
			for meta := range downloadQueue {
				log.Printf("[Worker %d] Starting download for: %s", workerId, meta.Title)
				if meta.ImageURL != "" && meta.ImageFilePath != "" {
					if _, err := os.Stat(meta.ImageFilePath); os.IsNotExist(err) {
						DownloadFileWithRetry(meta.ImageURL, meta.ImageFilePath)
					} else {
						log.Printf("[Worker %d] Image exists: %s", workerId, meta.ImageFilePath)
					}
				}

				time.Sleep(3 * time.Second)
				if meta.DataURL != "" && meta.VideoFilePath != "" {
					if _, err := os.Stat(meta.VideoFilePath); os.IsNotExist(err) {
						DownloadVideoWithRetry(meta)
					} else {
						log.Printf("[Worker %d] Video exists: %s", workerId, meta.VideoFilePath)
					}
				}

				// Check download success and clear cache if enabled
				// We consider success if the Video file exists. Image is secondary.
				videoSuccess := false
				if meta.DataURL != "" && meta.VideoFilePath != "" {
					if _, err := os.Stat(meta.VideoFilePath); err == nil {
						videoSuccess = true
					}
				}

				if videoSuccess && globalConfig.ClearCache {
					cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("info_%s.json", meta.VideoID))
					if err := os.Remove(cacheFile); err == nil {
						log.Printf("[Worker %d] Cleared cache for %s (Video downloaded)", workerId, meta.VideoID)
					} else if !os.IsNotExist(err) {
						log.Printf("[Worker %d] Failed to clear cache for %s: %v", workerId, meta.VideoID, err)
					}
				}

				// Update List Progress
				if videoSuccess && meta.ListID != "" {
					progressMu.Lock()
					if status, ok := listProgress[meta.ListID]; ok {
						status.Mutex.Lock()
						status.Pending--
						pending := status.Pending
						status.Mutex.Unlock()

						if pending == 0 && globalConfig.ClearCache {
							cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", meta.ListID))
							if err := os.Remove(cacheFile); err == nil {
								log.Printf("[Worker %d] Cleared list cache for %s (All videos finished)", workerId, meta.ListID)
							}
						}
					}
					progressMu.Unlock()
				}
			}
		}(i)
	}

	// Channel for incoming tasks (Discovery -> Resolver)
	taskQueue := make(chan VideoTask, 1000)

	// Start Discovery Goroutine (Producer 1)
	go func() {
		defer close(taskQueue)

		// Keep track of tasks to avoid duplicates
		taskMap := make(map[string]bool)

		// Helper to safely add tasks
		addTask := func(task VideoTask) {
			if !taskMap[task.VideoID] {
				taskQueue <- task
				taskMap[task.VideoID] = true
			}
		}

		// 1. Single Codes
		for _, id := range globalConfig.SingleCode {
			addTask(VideoTask{VideoID: id, ListID: ""})
		}

		// 2. Identify Cached vs Network Lists
		var cachedLists []string
		var networkLists []string

		for _, listID := range globalConfig.ListCode {
			cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", listID))
			if _, err := os.Stat(cacheFile); err == nil {
				cachedLists = append(cachedLists, listID)
			} else {
				networkLists = append(networkLists, listID)
			}
		}

		// Helper to process a list
		processList := func(listID string) {
			log.Printf("Fetching playlist for ID: %s", listID)
			list := ChromedpGetList(croUrl, listID)
			log.Printf("Playlist %s acquired: %d videos found", listID, len(list))

			// Initialize list progress
			progressMu.Lock()
			// Only initialize if not exists (preserve existing progress if resuming?)
			if _, ok := listProgress[listID]; !ok {
				listProgress[listID] = &ListStatus{
					Total:   len(list),
					Pending: len(list),
				}
			}
			progressMu.Unlock()

			for _, vid := range list {
				// We construct the task directly here.
				taskQueue <- VideoTask{VideoID: vid, ListID: listID}
				taskMap[vid] = true // Mark as seen for "cache scan info" later
			}
		}

		// 3. Process Cached Lists (Fast)
		for _, listID := range cachedLists {
			processList(listID)
		}

		// 4. Scan Cache for pending Lists (Resume logic for lists NOT in config)
		listFiles, _ := filepath.Glob(filepath.Join(globalConfig.CacheDir, "list_*.json"))
		for _, f := range listFiles {
			base := filepath.Base(f)
			if strings.HasPrefix(base, "list_") && strings.HasSuffix(base, ".json") {
				listID := base[5 : len(base)-5]

				// Check if already processed (is in globalConfig.ListCode)
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

		// 5. Scan Cache for pending Videos (info_*.json)
		infoFiles, _ := filepath.Glob(filepath.Join(globalConfig.CacheDir, "info_*.json"))
		for _, f := range infoFiles {
			base := filepath.Base(f)
			if strings.HasPrefix(base, "info_") && strings.HasSuffix(base, ".json") {
				vid := base[5 : len(base)-5]
				if !taskMap[vid] {
					log.Printf("Found cached video info for %s, resuming...", vid)
					addTask(VideoTask{VideoID: vid, ListID: ""})
				}
			}
		}

		// 6. Process Network Lists (Slow)
		for _, listID := range networkLists {
			processList(listID)
		}

		log.Println("Discovery completed, closing task queue.")
	}()

	// Main Thread / Resolver Logic (Consumer 1 / Producer 2)
	// Consumes taskQueue, Resolves Info, Produces to downloadQueue
	for task := range taskQueue {
		// This block runs in the main thread (sequentially) as requested
		// "ChromedpGetList, ChromedpDown 为同一线程操作"
		meta, err := ResolveVideoInfo(croUrl, task.VideoID, task.ListID)
		if err != nil {
			log.Printf("Skipping %s due to resolution failure: %v", task.VideoID, err)

			// If resolution fails, we should probably decrement the pending count for the list
			// because this video will never reach the worker.
			if task.ListID != "" {
				progressMu.Lock()
				if status, ok := listProgress[task.ListID]; ok {
					status.Mutex.Lock()
					status.Pending--
					pending := status.Pending
					status.Mutex.Unlock()

					// Check if list is complete (even if this one failed)
					if pending == 0 && globalConfig.ClearCache {
						cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", task.ListID))
						if err := os.Remove(cacheFile); err == nil {
							log.Printf("Cleared list cache for %s (All videos processed/failed)", task.ListID)
						}
					}
				}
				progressMu.Unlock()
			}
			continue
		}

		// Push to download queue
		downloadQueue <- meta
	}

	// Close queue to signal workers to finish
	close(downloadQueue)

	// Wait for downloads to finish
	downloadWg.Wait()

	log.Println("All tasks completed.")
}

func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, &globalConfig)
}

func ChromedpGetList(croUrl, videoID string) []string {
	// Check cache first
	cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedList []string
		if err := json.Unmarshal(data, &cachedList); err == nil {
			log.Printf("Loaded playlist %s from cache", videoID)
			return cachedList
		}
	}

	var idList []string
	var err error

	chromedpMu.Lock()
	defer chromedpMu.Unlock()

	for i := 0; i < maxRetries; i++ {
		// Encapsulate attempt in a closure to ensure defers run immediately after attempt
		idList, err = func() ([]string, error) {
			ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
			defer cancel1()
			allocatorContext, cancel := chromedp.NewRemoteAllocator(ctx1, croUrl, chromedp.NoModifyURL)
			defer cancel()

			ctx, cancelCtx := chromedp.NewContext(allocatorContext)
			defer cancelCtx()

			var links []map[string]string
			err := chromedp.Run(ctx,
				chromedp.Navigate(fmt.Sprintf(hanimeWatchURL, videoID)),
				chromedp.Sleep(5*time.Second),
				chromedp.AttributesAll(playlistSelector, &links, chromedp.ByQueryAll),
			)

			if err != nil {
				return nil, err
			}

			var result []string
			idMap := make(map[string]int)
			for _, link := range links {
				if v, ok := link["class"]; ok && v == "overlay" {
					if href, okHref := link["href"]; okHref {
						parts := strings.Split(href, "=")
						if len(parts) > 1 {
							idMap[parts[1]] = 0
						}
					}
				}
			}
			for k := range idMap {
				result = append(result, k)
			}
			return result, nil
		}()

		if err == nil {
			// Save to cache
			if cacheData, err := json.Marshal(idList); err == nil {
				os.WriteFile(cacheFile, cacheData, 0644)
			}
			return idList
		}

		log.Printf("Failed to get playlist (attempt %d/%d) for video ID %s: %v", i+1, maxRetries, videoID, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	log.Printf("Failed to get playlist for video ID %s after %d attempts.", videoID, maxRetries)
	return idList
}

// ResolveVideoInfo replaces ChromedpDown's logic for fetching metadata.
// It checks cache first, then uses ChromeDP, then caches result.
func ResolveVideoInfo(croUrl, videoID, listID string) (VideoMetadata, error) {
	// Check cache
	cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("info_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedMeta VideoMetadata
		if err := json.Unmarshal(data, &cachedMeta); err == nil {
			log.Printf("Loaded info for %s from cache", videoID)
			// Update ListID from current context
			cachedMeta.ListID = listID
			return cachedMeta, nil
		}
	}

	var res Result
	var meta VideoMetadata
	var err error

	for i := 0; i < maxRetries; i++ {
		// Encapsulate attempt in a closure to ensure defers run immediately after attempt
		meta, err = func() (VideoMetadata, error) {
			chromedpMu.Lock()
			defer chromedpMu.Unlock()

			ctx1, cancel1 := context.WithTimeout(context.Background(), 3000*time.Second)
			defer cancel1()

			allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, croUrl, chromedp.NoModifyURL)
			defer cancelAllocator()

			ctx, cancelCtx := chromedp.NewContext(allocatorContext)
			defer cancelCtx()

			var currentRes Result
			var downloadLinks []map[string]string
			err := chromedp.Run(ctx,
				chromedp.Navigate(fmt.Sprintf(hanimeDownloadURL, videoID)),
				chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
				chromedp.Sleep(2*time.Second),
				chromedp.Text(downloadTitleSelector, &currentRes.Title, chromedp.NodeVisible, chromedp.ByQuery),
				chromedp.AttributeValue(downloadImageSelector, "src", &currentRes.ImageURL, nil, chromedp.ByQuery),
				chromedp.Evaluate(`
					Array.from(document.querySelectorAll('table.download-table tr')).slice(1).map(tr => {
						let resTd = tr.querySelector('td:nth-child(2)');
						let linkA = tr.querySelector('td:nth-child(5) a');
						if (resTd && linkA) {
							return {
								resolution: resTd.innerText.trim(),
								url: linkA.getAttribute('data-url')
							};
						}
						return null;
					}).filter(x => x !== null)
				`, &downloadLinks),
			)

			if err != nil {
				return VideoMetadata{}, err
			}

			// Select the appropriate resolution
			if len(downloadLinks) > 0 {
				selectedURL := ""
				// Try to find the configured resolution
				if globalConfig.VideoResolution != "" {
					for _, linkInfo := range downloadLinks {
						if strings.Contains(linkInfo["resolution"], globalConfig.VideoResolution) {
							selectedURL = linkInfo["url"]
							log.Printf("Found requested resolution %s for %s", globalConfig.VideoResolution, videoID)
							break
						}
					}
				}
				// Fallback to the highest resolution (first in the list)
				if selectedURL == "" {
					selectedURL = downloadLinks[0]["url"]
					if globalConfig.VideoResolution != "" {
						log.Printf("Requested resolution %s not found for %s, falling back to highest available: %s", globalConfig.VideoResolution, videoID, downloadLinks[0]["resolution"])
					}
				}
				currentRes.DataURL = selectedURL
			}

			if currentRes.Title == "" || currentRes.DataURL == "" {
				return VideoMetadata{}, fmt.Errorf("missing title or data url")
			}

			// Try to get high-res cover image from search page
			searchURL := fmt.Sprintf("https://hanime1.me/search?query=%s", url.QueryEscape(strings.TrimSpace(currentRes.Title))) + "&type=&genre=%E8%A3%8F%E7%95%AA&sort=&date=&duration="
			var searchImgUrl string
			log.Printf("VideoID: %s, Title: %s, searchURL: %s,  ", videoID, currentRes.Title, searchURL)

			searchCtx, searchCancel := context.WithTimeout(ctx, 15*time.Second)
			defer searchCancel()

			searchErr := chromedp.Run(searchCtx,
				chromedp.Navigate(searchURL),
				// sleep for a bit to allow the page to load
				chromedp.Sleep(2*time.Second),
				chromedp.Evaluate(fmt.Sprintf(`
					new Promise((resolve) => {
						let attempts = 0;
						let check = () => {
							attempts++;
							let link = document.querySelector("a[href*='watch?v=%s']");
							if (link) {
								let img = link.querySelector("img");
								if (img && img.src) {
									resolve(img.src);
									return;
								}
							}
							if (attempts >= 25) { // wait up to 5 seconds (25 * 200ms)
								resolve("");
								return;
							}
							setTimeout(check, 200);
						};
						check();
					});
				`, videoID), &searchImgUrl, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
					return p.WithAwaitPromise(true)
				}),
			)

			if searchErr == nil && searchImgUrl != "" {
				currentRes.ImageURL = searchImgUrl
				log.Printf("Found high-res cover image from search for %s", videoID)
			} else {
				log.Printf("Search for high-res cover failed for %s, falling back to download page image", videoID)
			}

			// Copy resolved data to local res variable if needed or construct meta directly
			res = currentRes

			fmt.Printf("Resolved Info for ID %s: %s\n", videoID, res.Title)

			// Sanitize directory name
			title := strings.TrimSpace(res.Title)
			var dirName string

			if strings.HasPrefix(title, "(") {
				if idx := strings.Index(title, ")"); idx != -1 {
					dirName = title[:idx+1]
				}
			} else if strings.HasPrefix(title, "[") {
				if idx := strings.Index(title, "]"); idx != -1 {
					dirName = title[:idx+1]
				}
			}

			if dirName == "" {
				dirName = strings.Split(title, " ")[0]
			}

			dirName = strings.ReplaceAll(dirName, "/", "_")
			dirName = strings.ReplaceAll(dirName, "\\", "_")

			// Use configured DownDir
			fullDir := filepath.Join(globalConfig.DownDir, dirName)

			if errMkdir := os.MkdirAll(fullDir, 0755); errMkdir != nil {
				return VideoMetadata{}, fmt.Errorf("failed to create dir: %w", errMkdir)
			}

			fileNameBase := strings.ReplaceAll(strings.TrimSpace(res.Title), "/", "_")
			fileNameBase = strings.ReplaceAll(fileNameBase, "\\", "_")
			fileNameBase = truncateFilename(fileNameBase, 200)

			return VideoMetadata{
				VideoID:       videoID,
				Title:         res.Title,
				ImageURL:      res.ImageURL,
				DataURL:       res.DataURL,
				TargetDir:     fullDir,
				ImageFilePath: filepath.Join(fullDir, fileNameBase+".jpg"),
				VideoFilePath: filepath.Join(fullDir, fileNameBase+".mp4"),
				ListID:        listID,
			}, nil
		}()

		if err == nil {
			// Save to cache
			if cacheData, err := json.MarshalIndent(meta, "", "  "); err == nil {
				os.WriteFile(cacheFile, cacheData, 0644)
			}
			return meta, nil
		}

		log.Printf("Failed to fetch download info (attempt %d/%d) for %s: %v", i+1, maxRetries, videoID, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}

	return meta, fmt.Errorf("failed after retries")
}

func GetWebSocketDebuggerURL(uri string) (string, error) {
	var lastErr error
	if strings.HasPrefix(uri, "ws://") || strings.HasPrefix(uri, "wss://") {
		return uri, nil
	}
	parsedURL, parseErr := url.Parse(uri)
	baseHost := ""
	if parseErr == nil {
		baseHost = parsedURL.Host
	}
	for i := 0; i < maxRetries; i++ {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{
			Transport: tr,
			Timeout:   10 * time.Second,
		}
		req, err := http.NewRequest("GET", uri, nil)
		if err != nil {
			lastErr = fmt.Errorf("creating request failed: %w", err)
			time.Sleep(retryInterval)
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "curl/7.81.0")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http client.Do failed: %w", err)
			time.Sleep(retryInterval)
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("reading response body failed: %w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			bodyText := strings.TrimSpace(string(body))
			if bodyText == "" {
				lastErr = fmt.Errorf("server returned non-200 status: %s", resp.Status)
			} else {
				lastErr = fmt.Errorf("server returned non-200 status: %s, body: %s", resp.Status, bodyText)
			}
			continue
		}

		var WsInfo struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if err := json.Unmarshal(body, &WsInfo); err != nil {
			lastErr = fmt.Errorf("json unmarshal failed: %w", err)
			continue
		}

		if WsInfo.WebSocketDebuggerURL == "" {
			lastErr = fmt.Errorf("WebSocketDebuggerURL is empty")
			continue
		}
		if baseHost != "" {
			if wsURL, err := url.Parse(WsInfo.WebSocketDebuggerURL); err == nil && wsURL.Host != "" {
				if strings.HasPrefix(wsURL.Host, "0.0.0.0") || strings.HasPrefix(wsURL.Host, "127.0.0.1") {
					wsURL.Host = baseHost

					// Append the token from the original URI if it exists
					if parseErr == nil && parsedURL.RawQuery != "" {
						if wsURL.RawQuery == "" {
							wsURL.RawQuery = parsedURL.RawQuery
						} else {
							wsURL.RawQuery = wsURL.RawQuery + "&" + parsedURL.RawQuery
						}
					}

					return wsURL.String(), nil
				}
			}
		}
		return WsInfo.WebSocketDebuggerURL, nil
	}
	return "", fmt.Errorf("failed after %d attempts, last error: %w", maxRetries, lastErr)
}

func DownloadFileWithRetry(url, filePath string) {
	if globalConfig.DirectDownloadFirst {
		log.Printf("Attempting direct download first for %s", url)
		err := DownloadFile(url, filePath, "")
		if err == nil {
			log.Printf("Successfully downloaded %s to %s (Direct)", url, filePath)
			return
		}
		log.Printf("Direct download failed: %v. Switching to proxy...", err)
	}

	for i := 0; i < maxRetries; i++ {
		err := DownloadFile(url, filePath, globalConfig.HttpProxy)
		if err == nil {
			log.Printf("Successfully downloaded %s to %s", url, filePath)
			return
		}
		log.Printf("Failed to download %s (attempt %d/%d): %v", url, i+1, maxRetries, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	log.Printf("Failed to download %s to %s after %d attempts.", url, filePath, maxRetries)
}

func DownloadFile(urlStr, filePath, proxyURL string) error {
	tempFilePath := filePath + ".tmp"
	var file *os.File
	var err error

	fileInfo, err := os.Stat(tempFilePath)
	var startOffset int64 = 0
	if err == nil {
		startOffset = fileInfo.Size()
		file, err = os.OpenFile(tempFilePath, os.O_APPEND|os.O_WRONLY, 0644)
	} else {
		file, err = os.Create(tempFilePath)
	}

	if err != nil {
		return fmt.Errorf("failed to open/create temp file %s: %w", tempFilePath, err)
	}
	defer file.Close()

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
		log.Printf("Resuming download for %s from byte %d", filePath, startOffset)
	}

	// Proxy configuration
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment, // Default
	}
	if proxyURL != "" {
		pUrl, err := url.Parse(proxyURL)
		if err == nil {
			tr.Proxy = http.ProxyURL(pUrl)
			log.Printf("Using proxy: %s", proxyURL)
		} else {
			log.Printf("Invalid proxy URL: %s, ignoring.", proxyURL)
		}
	}

	client := http.Client{
		Transport: tr,
		Timeout:   360000 * time.Second, // Increased timeout for large files
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return &HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: urlStr}
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return &HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: urlStr}
	}

	if startOffset > 0 && resp.StatusCode == http.StatusOK {
		log.Printf("Server does not support resume (got 200 OK), restarting download.")
		file.Truncate(0)
		file.Seek(0, 0)
		startOffset = 0 // Reset offset since we are restarting
	}

	// Total size logic
	contentLength := resp.ContentLength
	totalSize := contentLength + startOffset
	if contentLength == -1 {
		totalSize = -1 // Unknown size
	}

	pw := &ProgressWriter{
		Total:       totalSize,
		Downloaded:  startOffset,
		LastPrinted: time.Now(),
		FileName:    filepath.Base(filePath),
	}

	// Create a Ticker to print progress periodically
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Channel to signal download complete
	done := make(chan bool)

	// Progress printing goroutine
	go func() {
		for {
			select {
			case <-ticker.C:
				var progressStr string
				if pw.Total > 0 {
					percent := float64(pw.Downloaded) / float64(pw.Total) * 100
					progressStr = fmt.Sprintf("%.2f%% (%d/%d bytes)", percent, pw.Downloaded, pw.Total)
				} else {
					progressStr = fmt.Sprintf("%d bytes (Total unknown)", pw.Downloaded)
				}
				log.Printf("[%s] Download Progress: %s", pw.FileName, progressStr)
			case <-done:
				return
			}
		}
	}()

	// Copy data using io.TeeReader or just wrap the writer?
	// io.Copy uses Writer.Write, so we can just pass pw
	// However, we need to write to the file AND update progress.
	// Let's create a custom writer that writes to file and updates counters.

	// We need a writer that writes to 'file' and updates 'pw'
	mw := io.MultiWriter(file, pw)

	n, err := io.Copy(mw, resp.Body)

	// Stop the progress ticker
	done <- true

	if err != nil {
		return fmt.Errorf("failed to write to file: %w", err)
	}

	file.Close()

	log.Printf("[%s] Downloaded %d bytes (Total: %d)", pw.FileName, n, pw.Downloaded)

	if err := os.Rename(tempFilePath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

type HTTPStatusError struct {
	StatusCode int
	Status     string
	URL        string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "http status error"
	}
	if e.URL == "" {
		return fmt.Sprintf("server returned status %s", e.Status)
	}
	return fmt.Sprintf("server returned status %s for %s", e.Status, e.URL)
}

func DownloadVideoWithRetry(meta VideoMetadata) {
	urlStr := meta.DataURL

	for i := 0; i < maxRetries; i++ {
		err := downloadOnceWithOptionalDirectFirst(urlStr, meta.VideoFilePath)
		if err == nil {
			log.Printf("Successfully downloaded %s to %s", urlStr, meta.VideoFilePath)
			return
		}

		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) {
			if statusErr.StatusCode == http.StatusGone {
				newURL, refreshErr := RefreshVideoDataURL(meta.VideoID, meta.Title)
				if refreshErr != nil {
					log.Printf("Failed to refresh download URL for %s after 410 Gone: %v", meta.VideoID, refreshErr)
				} else {
					urlStr = newURL
					log.Printf("Refreshed download URL for %s, retrying...", meta.VideoID)
					continue
				}
			}

			if statusErr.StatusCode == http.StatusRequestedRangeNotSatisfiable {
				tempFilePath := meta.VideoFilePath + ".tmp"
				if rmErr := os.Remove(tempFilePath); rmErr == nil {
					log.Printf("Deleted temp file after 416 for %s, restarting download", meta.VideoID)
					continue
				}
			}
		}

		log.Printf("Failed to download %s (attempt %d/%d): %v", urlStr, i+1, maxRetries, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	log.Printf("Failed to download %s to %s after %d attempts.", urlStr, meta.VideoFilePath, maxRetries)
}

func downloadOnceWithOptionalDirectFirst(urlStr, filePath string) error {
	if globalConfig.DirectDownloadFirst {
		log.Printf("Attempting direct download first for %s", urlStr)
		err := DownloadFile(urlStr, filePath, "")
		if err == nil {
			return nil
		}
		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusGone || statusErr.StatusCode == http.StatusRequestedRangeNotSatisfiable) {
			return err
		}
		log.Printf("Direct download failed: %v. Trying proxy...", err)
	}
	return DownloadFile(urlStr, filePath, globalConfig.HttpProxy)
}

func RefreshVideoDataURL(videoID, titleForLog string) (string, error) {
	if globalChromeWSURL == "" {
		return "", fmt.Errorf("chrome websocket url is empty; cannot refresh")
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		dataURL, err := func() (string, error) {
			chromedpMu.Lock()
			defer chromedpMu.Unlock()

			ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
			defer cancel1()

			allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, globalChromeWSURL, chromedp.NoModifyURL)
			defer cancelAllocator()

			ctx, cancelCtx := chromedp.NewContext(allocatorContext)
			defer cancelCtx()

			var currentRes Result
			var downloadLinks []map[string]string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(fmt.Sprintf(hanimeDownloadURL, videoID)),
				chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
				chromedp.Sleep(2*time.Second),
				chromedp.Text(downloadTitleSelector, &currentRes.Title, chromedp.NodeVisible, chromedp.ByQuery),
				chromedp.AttributeValue(downloadImageSelector, "src", &currentRes.ImageURL, nil, chromedp.ByQuery),
				chromedp.Evaluate(`
					Array.from(document.querySelectorAll('table.download-table tr')).slice(1).map(tr => {
						let resTd = tr.querySelector('td:nth-child(2)');
						let linkA = tr.querySelector('td:nth-child(5) a');
						if (resTd && linkA) {
							return {
								resolution: resTd.innerText.trim(),
								url: linkA.getAttribute('data-url')
							};
						}
						return null;
					}).filter(x => x !== null)
				`, &downloadLinks),
			); err != nil {
				return "", err
			}

			if len(downloadLinks) == 0 {
				return "", fmt.Errorf("no download links found")
			}

			selectedURL := ""
			if globalConfig.VideoResolution != "" {
				for _, linkInfo := range downloadLinks {
					if strings.Contains(linkInfo["resolution"], globalConfig.VideoResolution) {
						selectedURL = linkInfo["url"]
						break
					}
				}
			}
			if selectedURL == "" {
				selectedURL = downloadLinks[0]["url"]
			}
			if selectedURL == "" {
				return "", fmt.Errorf("resolved data url is empty")
			}

			cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("info_%s.json", videoID))
			if data, err := os.ReadFile(cacheFile); err == nil {
				var cachedMeta VideoMetadata
				if err := json.Unmarshal(data, &cachedMeta); err == nil {
					cachedMeta.DataURL = selectedURL
					if currentRes.Title != "" {
						cachedMeta.Title = currentRes.Title
					}
					if currentRes.ImageURL != "" {
						cachedMeta.ImageURL = currentRes.ImageURL
					}
					if cacheData, err := json.MarshalIndent(cachedMeta, "", "  "); err == nil {
						os.WriteFile(cacheFile, cacheData, 0644)
					}
				}
			}

			if currentRes.Title != "" {
				log.Printf("Refreshed DataURL for %s (%s)", videoID, currentRes.Title)
			} else if titleForLog != "" {
				log.Printf("Refreshed DataURL for %s (%s)", videoID, titleForLog)
			} else {
				log.Printf("Refreshed DataURL for %s", videoID)
			}
			return selectedURL, nil
		}()

		if err == nil {
			return dataURL, nil
		}
		lastErr = err
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}

	return "", fmt.Errorf("failed to refresh data url after retries: %w", lastErr)
}

func truncateFilename(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			s = s[:len(s)-1]
		} else {
			break
		}
	}
	return s
}
