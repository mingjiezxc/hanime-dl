package downloader

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxRetries    = 5
	RetryInterval = 5 * time.Second
)

// HTTPStatusError HTTP 状态错误
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

// ProgressWriter 进度追踪器
type ProgressWriter struct {
	Total      int64
	Downloaded int64
	FileName   string
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.Downloaded += int64(n)
	return n, nil
}

// Downloader 文件下载器
type Downloader struct {
	proxyURL           string
	directDownloadFirst bool
}

// NewDownloader 创建新的下载器
func NewDownloader(proxyURL string, directFirst bool) *Downloader {
	return &Downloader{
		proxyURL:            proxyURL,
		directDownloadFirst: directFirst,
	}
}

// DownloadFile 下载文件（支持断点续传）
func (d *Downloader) DownloadFile(urlStr, filePath string) error {
	tempFilePath := filePath + ".tmp"

	// 检查是否已存在临时文件
	var startOffset int64 = 0
	fileInfo, err := os.Stat(tempFilePath)
	var file *os.File
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

	// 配置代理
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if d.proxyURL != "" {
		pUrl, err := url.Parse(d.proxyURL)
		if err == nil {
			tr.Proxy = http.ProxyURL(pUrl)
			log.Printf("Using proxy: %s", d.proxyURL)
		} else {
			log.Printf("Invalid proxy URL: %s, ignoring.", d.proxyURL)
		}
	}

	client := http.Client{
		Transport: tr,
		Timeout:   360000 * time.Second,
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
		startOffset = 0
	}

	// 进度追踪
	contentLength := resp.ContentLength
	totalSize := contentLength + startOffset
	if contentLength == -1 {
		totalSize = -1
	}

	pw := &ProgressWriter{
		Total:    totalSize,
		Downloaded: startOffset,
		FileName: filepath.Base(filePath),
	}

	// 进度打印协程
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	done := make(chan bool)

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

	// 下载
	mw := io.MultiWriter(file, pw)
	n, err := io.Copy(mw, resp.Body)
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

// DownloadWithRetry 带重试的下载
func (d *Downloader) DownloadWithRetry(urlStr, filePath string) error {
	for i := 0; i < MaxRetries; i++ {
		// 如果配置了直接下载优先，先尝试直接下载
		if d.directDownloadFirst {
			log.Printf("Attempting direct download first for %s", urlStr)
			err := d.DownloadFile(urlStr, filePath)
			if err == nil {
				log.Printf("Successfully downloaded %s to %s (Direct)", urlStr, filePath)
				return nil
			}
			log.Printf("Direct download failed: %v. Switching to proxy...", err)
		}

		err := d.DownloadFile(urlStr, filePath)
		if err == nil {
			log.Printf("Successfully downloaded %s to %s", urlStr, filePath)
			return nil
		}

		log.Printf("Failed to download %s (attempt %d/%d): %v", urlStr, i+1, MaxRetries, err)
		
		// 处理特定错误
		if strings.Contains(err.Error(), "410") || strings.Contains(err.Error(), "416") {
			if strings.Contains(err.Error(), "416") {
				// 删除临时文件后重试
				tempFilePath := filePath + ".tmp"
				os.Remove(tempFilePath)
				log.Printf("Deleted temp file after 416, restarting download")
				continue
			}
		}

		if i < MaxRetries-1 {
			time.Sleep(RetryInterval)
		}
	}

	return fmt.Errorf("failed to download %s after %d attempts", urlStr, MaxRetries)
}
