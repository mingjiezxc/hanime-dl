package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
)

const (
	maxRetries            = 3
	retryInterval         = 5 * time.Second
	hanimeWatchURL        = "https://hanime1.me/watch?v=%s"
	hanimeDownloadURL     = "https://hanime1.me/download?v=%s"
	playlistSelector      = `#video-playlist-wrapper a`
	downloadTitleSelector = `h3`
	downloadImageSelector = `img.download-image`
	downloadLinkSelector  = `table.download-table tr:nth-child(2) td:nth-child(5) a`
)

type Config struct {
	ChromeRemoteURL    string   `yaml:"chromeRemoteURL"`
	CacheDir           string   `yaml:"CacheDir"`
	DownDir            string   `yaml:"DownDir"`
	HttpProxy          string   `yaml:"HttpProxy"`
	MaxDownloadWorkers int      `yaml:"MaxDownloadWorkers"`
	ListCode           []string `yaml:"ListCode"`
	SingleCode         []string `yaml:"SingleCode"`
	ClearCache         bool     `yaml:"ClearCache"`
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
}

var globalConfig Config

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
				if meta.DataURL != "" && meta.VideoFilePath != "" {
					if _, err := os.Stat(meta.VideoFilePath); os.IsNotExist(err) {
						DownloadFileWithRetry(meta.DataURL, meta.VideoFilePath)
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
			}
		}(i)
	}

	// Main Thread / Producer Logic
	// This runs sequentially (or controlled) to resolve links using ChromeDP

	// 1. Collect all target Video IDs
	var allVideoIDs []string

	// Add Single Codes
	for _, id := range globalConfig.SingleCode {
		allVideoIDs = append(allVideoIDs, id)
	}

	// Process Lists to get IDs
	for _, listID := range globalConfig.ListCode {
		log.Printf("Fetching playlist for ID: %s", listID)
		list := ChromedpGetList(croUrl, listID)
		log.Printf("Playlist %s acquired: %d videos found", listID, len(list))
		allVideoIDs = append(allVideoIDs, list...)

		// Clear list cache if enabled
		if globalConfig.ClearCache {
			cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("list_%s.json", listID))
			if err := os.Remove(cacheFile); err == nil {
				log.Printf("Cleared list cache for %s", listID)
			} else if !os.IsNotExist(err) {
				log.Printf("Failed to clear list cache for %s: %v", listID, err)
			}
		}
	}

	// Remove duplicates if needed? For now, we process all.

	// 2. Resolve Metadata for each ID and push to download queue
	for _, videoID := range allVideoIDs {
		// This block runs in the main thread (sequentially) as requested
		// "ChromedpGetList, ChromedpDown 为同一线程操作"
		meta, err := ResolveVideoInfo(croUrl, videoID)
		if err != nil {
			log.Printf("Skipping %s due to resolution failure: %v", videoID, err)
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

	var links []map[string]string
	var idList []string

	for i := 0; i < maxRetries; i++ {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel1()
		allocatorContext, cancel := chromedp.NewRemoteAllocator(ctx1, croUrl)
		defer cancel()

		ctx, cancelCtx := chromedp.NewContext(allocatorContext)
		defer cancelCtx()

		err := chromedp.Run(ctx,
			chromedp.Navigate(fmt.Sprintf(hanimeWatchURL, videoID)),
			chromedp.Sleep(5*time.Second),
			chromedp.AttributesAll(playlistSelector, &links, chromedp.ByQueryAll),
		)

		if err == nil {
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
				idList = append(idList, k)
			}

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
func ResolveVideoInfo(croUrl, videoID string) (VideoMetadata, error) {
	// Check cache
	cacheFile := filepath.Join(globalConfig.CacheDir, fmt.Sprintf("info_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedMeta VideoMetadata
		if err := json.Unmarshal(data, &cachedMeta); err == nil {
			log.Printf("Loaded info for %s from cache", videoID)
			// Verify if cached paths are still valid relative to current config?
			// The user requirement says "cache the... target address".
			// We should respect the cached target dir/paths if we want "continue downloading when restarting".
			return cachedMeta, nil
		}
	}

	var res Result
	var meta VideoMetadata

	for i := 0; i < maxRetries; i++ {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 3000*time.Second)
		defer cancel1()

		allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, croUrl)
		defer cancelAllocator()

		ctx, cancelCtx := chromedp.NewContext(allocatorContext)
		defer cancelCtx()

		err := chromedp.Run(ctx,
			chromedp.Navigate(fmt.Sprintf(hanimeDownloadURL, videoID)),
			chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
			chromedp.Sleep(2*time.Second),
			chromedp.Text(downloadTitleSelector, &res.Title, chromedp.NodeVisible, chromedp.ByQuery),
			chromedp.AttributeValue(downloadImageSelector, "src", &res.ImageURL, nil, chromedp.ByQuery),
			chromedp.AttributeValue(downloadLinkSelector, "data-url", &res.DataURL, nil, chromedp.ByQueryAll),
		)

		if err == nil {
			if res.Title == "" || res.DataURL == "" {
				log.Printf("Failed to extract data (attempt %d/%d) for %s.", i+1, maxRetries, videoID)
				if i < maxRetries-1 {
					time.Sleep(retryInterval)
					continue
				}
				return meta, fmt.Errorf("missing title or data url")
			}

			fmt.Printf("Resolved Info for ID %s: %s\n", videoID, res.Title)

			// Sanitize directory name
			dirName := strings.Split(strings.TrimSpace(res.Title), " ")[0]
			dirName = strings.ReplaceAll(dirName, "/", "_")
			dirName = strings.ReplaceAll(dirName, "\\", "_")

			// Use configured DownDir
			fullDir := filepath.Join(globalConfig.DownDir, dirName)

			if errMkdir := os.MkdirAll(fullDir, 0755); errMkdir != nil {
				return meta, fmt.Errorf("failed to create dir: %w", errMkdir)
			}

			fileNameBase := strings.ReplaceAll(strings.TrimSpace(res.Title), "/", "_")
			fileNameBase = strings.ReplaceAll(fileNameBase, "\\", "_")

			meta = VideoMetadata{
				VideoID:       videoID,
				Title:         res.Title,
				ImageURL:      res.ImageURL,
				DataURL:       res.DataURL,
				TargetDir:     fullDir,
				ImageFilePath: filepath.Join(fullDir, fileNameBase+".jpg"),
				VideoFilePath: filepath.Join(fullDir, fileNameBase+".mp4"),
			}

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
		req.Header.Set("Content-Type", "application/json")

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
			lastErr = fmt.Errorf("server returned non-200 status: %s", resp.Status)
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
		return WsInfo.WebSocketDebuggerURL, nil
	}
	return "", fmt.Errorf("failed after %d attempts, last error: %w", maxRetries, lastErr)
}

func DownloadFileWithRetry(url, filePath string) {
	for i := 0; i < maxRetries; i++ {
		err := DownloadFile(url, filePath)
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

func DownloadFile(urlStr, filePath string) error {
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
	if globalConfig.HttpProxy != "" {
		proxyUrl, err := url.Parse(globalConfig.HttpProxy)
		if err == nil {
			tr.Proxy = http.ProxyURL(proxyUrl)
			log.Printf("Using proxy: %s", globalConfig.HttpProxy)
		} else {
			log.Printf("Invalid proxy URL: %s, ignoring.", globalConfig.HttpProxy)
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
		return fmt.Errorf("server returned 416 Range Not Satisfiable")
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned status %s", resp.Status)
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

	log.Printf("[%s] Downloaded %d bytes (Total: %d)", pw.FileName, n, pw.Downloaded)

	if err := os.Rename(tempFilePath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}
