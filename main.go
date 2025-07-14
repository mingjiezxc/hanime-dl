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
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	maxRetries    = 3
	retryInterval = 5 * time.Second
	// chromeRemoteURL       = "http://192.168.188.103:9222/json/version"
	hanimeWatchURL        = "https://hanime1.me/watch?v=%s"
	hanimeDownloadURL     = "https://hanime1.me/download?v=%s"
	playlistSelector      = `#video-playlist-wrapper a`
	downloadTitleSelector = `h3`
	downloadImageSelector = `img.download-image`
	downloadLinkSelector  = `table.download-table tr:nth-child(2) td:nth-child(5) a`
)

type Result struct {
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	DataURL  string `json:"data_url"`
}

func main() {
	mode := flag.String("mode", "list", "Download mode: 'single' or 'list'")
	chromeRemoteURL := flag.String("chromeRemoteURL", "http://localhost:9222/json/version", "Chrome Remote Debugging URL")
	flag.Parse()

	if len(flag.Args()) < 1 {
		log.Fatal("Usage: go run main.go -mode=[single|list] -chromeRemoteURL=<url> <videoID>")
	}
	videoID := flag.Args()[0]

	if videoID == "" {
		log.Panicf("videoID is null")
	}

	var croUrl string
	var err error
	for i := 0; i < maxRetries; i++ {
		croUrl, err = GetWebSocketDebuggerURL(*chromeRemoteURL)
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

	switch *mode {
	case "single":
		log.Printf("Starting single download for video ID: %s", videoID)
		ChromedpDown(croUrl, videoID)
	case "list":
		log.Printf("Starting list download for video ID: %s", videoID)
		list := ChromedpGetList(croUrl, videoID)
		if len(list) == 0 {
			log.Printf("No videos found in the playlist for ID: %s", videoID)
			return
		}
		log.Printf("Playlist acquired: %d videos found", len(list))
		for i, id := range list {
			log.Printf("Downloading video %d/%d from playlist: %s", i+1, len(list), id)
			ChromedpDown(croUrl, id)
		}
	default:
		log.Fatalf("Invalid mode: %s. Use 'single' or 'list'.", *mode)
	}

	log.Println("All downloads processed.")
}

func ChromedpGetList(croUrl, videoID string) []string {
	var links []map[string]string
	var idList []string

	for i := 0; i < maxRetries; i++ {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second) // Increased timeout
		defer cancel1()
		allocatorContext, cancel := chromedp.NewRemoteAllocator(ctx1, croUrl)
		defer cancel()

		ctx, cancelCtx := chromedp.NewContext(allocatorContext)
		defer cancelCtx()

		var screenshot []byte
		err := chromedp.Run(ctx,
			chromedp.Navigate(fmt.Sprintf(hanimeWatchURL, videoID)),
			chromedp.Sleep(5*time.Second), // Consider using WaitVisible if a reliable selector exists
			chromedp.AttributesAll(playlistSelector, &links, chromedp.ByQueryAll),
			chromedp.FullScreenshot(&screenshot, 70),
		)

		if err == nil {
			if err := os.WriteFile(fmt.Sprintf("./playlist_screenshot_%s.png", videoID), screenshot, 0o644); err != nil {
				log.Printf("Failed to save playlist screenshot: %s", err)
			}
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
			return idList
		}

		log.Printf("Failed to get playlist (attempt %d/%d) for video ID %s: %v", i+1, maxRetries, videoID, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	log.Printf("Failed to get playlist for video ID %s after %d attempts.", videoID, maxRetries)
	return idList // Return empty list on persistent failure
}

func ChromedpDown(croUrl, videoID string) {
	var res Result

	for i := 0; i < maxRetries; i++ {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 3000*time.Second) // Increased timeout
		defer cancel1()

		allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, croUrl)
		defer cancelAllocator()

		ctx, cancelCtx := chromedp.NewContext(allocatorContext)
		defer cancelCtx()

		var screenshot []byte
		err := chromedp.Run(ctx,
			chromedp.Navigate(fmt.Sprintf(hanimeDownloadURL, videoID)),
			chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
			chromedp.Sleep(2*time.Second), // Allow page to fully render after visible
			chromedp.Text(downloadTitleSelector, &res.Title, chromedp.NodeVisible, chromedp.ByQuery),
			chromedp.AttributeValue(downloadImageSelector, "src", &res.ImageURL, nil, chromedp.ByQuery),
			chromedp.AttributeValue(downloadLinkSelector, "data-url", &res.DataURL, nil, chromedp.ByQueryAll), // ByQueryAll in case of multiple, though we expect one
			chromedp.FullScreenshot(&screenshot, 70),
		)

		if err == nil {
			if res.Title == "" || res.DataURL == "" {
				log.Printf("Failed to extract all necessary data (attempt %d/%d) for video ID %s. Title: '%s', DataURL: '%s'", i+1, maxRetries, videoID, res.Title, res.DataURL)
				if i < maxRetries-1 {
					time.Sleep(retryInterval)
					continue
				}
				log.Printf("Skipping download for video ID %s due to missing title or data URL after %d attempts.", videoID, maxRetries)
				return
			}

			//if err := os.WriteFile(fmt.Sprintf("./download_screenshot_%s.png", videoID), screenshot, 0o644); err != nil {
			//	log.Printf("Failed to save download page screenshot for %s: %s", videoID, err)
			//}

			fmt.Printf("Download Info for ID %s:\nTitle: %s\nImage: %s\nDownload URL: %s\n",
				videoID, res.Title, res.ImageURL, res.DataURL)

			dir := strings.Split(strings.TrimSpace(res.Title), " ")[0]
			dir = strings.ReplaceAll(dir, "/", "_") // Sanitize directory name
			dir = strings.ReplaceAll(dir, "\\", "_")
			if _, errStat := os.Stat(dir); os.IsNotExist(errStat) {
				if errMkdir := os.Mkdir(dir, 0o755); errMkdir != nil {
					log.Printf("Failed to create directory %s: %v", dir, errMkdir)
					return // Cannot proceed without directory
				}
			}

			fileNameBase := strings.ReplaceAll(strings.TrimSpace(res.Title), "/", "_")
			fileNameBase = strings.ReplaceAll(fileNameBase, "\\", "_")

			imageFilePath := fmt.Sprintf("%s/%s.jpg", dir, fileNameBase)
			videoFilePath := fmt.Sprintf("%s/%s.mp4", dir, fileNameBase)

			if res.ImageURL != "" {
				if _, err := os.Stat(imageFilePath); os.IsNotExist(err) {
					DownloadFileWithRetry(res.ImageURL, imageFilePath)
				} else {
					log.Printf("Image file already exists, skipping: %s", imageFilePath)
				}
			}
			if res.DataURL != "" {
				if _, err := os.Stat(videoFilePath); os.IsNotExist(err) {
					DownloadFileWithRetry(res.DataURL, videoFilePath)
				} else {
					log.Printf("Video file already exists, skipping: %s", videoFilePath)
				}
			} else {
				log.Printf("No data URL found for video ID %s, title: %s", videoID, res.Title)
			}
			return // Success
		}

		log.Printf("Failed to fetch download info (attempt %d/%d) for video ID %s: %v", i+1, maxRetries, videoID, err)
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	log.Printf("Failed to process download for video ID %s after %d attempts.", videoID, maxRetries)
}

func GetWebSocketDebuggerURL(uri string) (string, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{
			Transport: tr,
			Timeout:   10 * time.Second, // Increased timeout
		}
		req, err := http.NewRequest("GET", uri, nil)
		if err != nil {
			lastErr = fmt.Errorf("creating request failed: %w", err)
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http client.Do failed: %w", err)
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("reading response body failed: %w", err)
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("server returned non-200 status: %s, body: %s", resp.Status, string(body))
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		var WsInfo struct { // Anonymous struct for this specific unmarshal
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if err := json.Unmarshal(body, &WsInfo); err != nil {
			lastErr = fmt.Errorf("json unmarshal failed: %w, body: %s", err, string(body))
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		if WsInfo.WebSocketDebuggerURL == "" {
			lastErr = fmt.Errorf("WebSocketDebuggerURL is empty in response. Body: %s", string(body))
			log.Printf("Attempt %d/%d: %v", i+1, maxRetries, lastErr)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}
		return WsInfo.WebSocketDebuggerURL, nil // Success
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

func DownloadFile(url, filePath string) error {
	tempFilePath := filePath + ".tmp"
	var file *os.File
	var err error

	// Check if a temporary file already exists and get its size for resuming
	fileInfo, err := os.Stat(tempFilePath)
	var startOffset int64 = 0
	if err == nil {
		startOffset = fileInfo.Size()
		file, err = os.OpenFile(tempFilePath, os.O_APPEND|os.O_WRONLY, 0644)
	} else {
		file, err = os.Create(tempFilePath)
	}

	if err != nil {
		return fmt.Errorf("failed to open or create temp file %s: %w", tempFilePath, err)
	}
	defer file.Close()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
		log.Printf("Resuming download for %s from byte %d", filePath, startOffset)
	}

	// Create a transport with proxy explicitly set to nil
	tr := &http.Transport{
		// use env proxy
		// Proxy: http.ProxyFromEnvironment,
		Proxy: nil,
	}
	// Create a client with the custom transport and a timeout
	client := http.Client{
		Transport: tr,
		Timeout:   36000 * time.Second, //  timeout for download
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed for %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK: // 200
		// Standard download from the beginning
	case http.StatusPartialContent: // 206
		// Resuming download
	default:
		return fmt.Errorf("server returned error %s for %s", resp.Status, url)
	}

	n, err := io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write to temp file %s (wrote %d bytes): %w", tempFilePath, n, err)
	}

	// Rename the temporary file to the final file path upon successful download
	if err := os.Rename(tempFilePath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file %s to %s: %w", tempFilePath, filePath, err)
	}

	return nil
}
