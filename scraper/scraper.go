package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	chromeDownURL       = "https://hanime1.me/download?v=%s"
	downloadTitleSelector = `h3`
	downloadImageSelector = `img.download-image`
)

// Result 视频解析结果
type Result struct {
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	DataURL  string `json:"data_url"`
}

// VideoMetadata 视频元数据
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

// Scraper 网页抓取器
type Scraper struct {
	cacheDir   string
	downDir    string
	resolution string
}

// NewScraper 创建新的抓取器
func NewScraper(cacheDir, downDir, resolution string) *Scraper {
	return &Scraper{
		cacheDir:   cacheDir,
		downDir:    downDir,
		resolution: resolution,
	}
}

// GetPlaylist 获取播放列表（带缓存）
func (s *Scraper) GetPlaylist(wsURL, videoID string) ([]string, error) {
	// 检查缓存
	cacheFile := filepath.Join(s.cacheDir, fmt.Sprintf("list_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedList []string
		if err := json.Unmarshal(data, &cachedList); err == nil {
			log.Printf("Loaded playlist %s from cache", videoID)
			return cachedList, nil
		}
	}

	// 从网页获取
	ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx := chromedp.NewContext(allocatorContext)
	defer cancelCtx()

	var links []map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf("https://hanime1.me/watch?v=%s", videoID)),
		chromedp.Sleep(5*time.Second),
		chromedp.AttributesAll(`#video-playlist-wrapper a`, &links, chromedp.ByQueryAll),
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

	// 保存到缓存
	if cacheData, err := json.Marshal(result); err == nil {
		os.WriteFile(cacheFile, cacheData, 0644)
	}

	return result, nil
}

// ResolveVideoInfo 解析视频信息（带缓存）
func (s *Scraper) ResolveVideoInfo(wsURL, videoID, listID string) (VideoMetadata, error) {
	// 检查缓存
	cacheFile := filepath.Join(s.cacheDir, fmt.Sprintf("info_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedMeta VideoMetadata
		if err := json.Unmarshal(data, &cachedMeta); err == nil {
			log.Printf("Loaded info for %s from cache", videoID)
			cachedMeta.ListID = listID
			return cachedMeta, nil
		}
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3000*time.Second)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx := chromedp.NewContext(allocatorContext)
	defer cancelCtx()

	var res Result
	var downloadLinks []map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf(chromeDownURL, videoID)),
		chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.Text(downloadTitleSelector, &res.Title, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.AttributeValue(downloadImageSelector, "src", &res.ImageURL, nil, chromedp.ByQuery),
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

	// 选择分辨率
	selectedURL := ""
	if len(downloadLinks) > 0 {
		if s.resolution != "" {
			for _, linkInfo := range downloadLinks {
				if strings.Contains(linkInfo["resolution"], s.resolution) {
					selectedURL = linkInfo["url"]
					log.Printf("Found requested resolution %s for %s", s.resolution, videoID)
					break
				}
			}
		}
		if selectedURL == "" {
			selectedURL = downloadLinks[0]["url"]
			if s.resolution != "" {
				log.Printf("Requested resolution %s not found for %s, falling back to: %s", s.resolution, videoID, downloadLinks[0]["resolution"])
			}
		}
		res.DataURL = selectedURL
	}

	if res.Title == "" || res.DataURL == "" {
		return VideoMetadata{}, fmt.Errorf("missing title or data url")
	}

	// 尝试获取高清封面图
	searchURL := fmt.Sprintf("https://hanime1.me/search?query=%s", 
		url.QueryEscape(strings.TrimSpace(res.Title))) + "&type=&genre=%E8%A3%85%E7%95%8C&sort=&date=&duration="
	
	var searchImgUrl string
	searchCtx, searchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer searchCancel()

	searchErr := chromedp.Run(searchCtx,
		chromedp.Navigate(searchURL),
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
					if (attempts >= 25) {
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
		res.ImageURL = searchImgUrl
		log.Printf("Found high-res cover image from search for %s", videoID)
	}

	// 构建元数据
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

	fullDir := filepath.Join(s.downDir, dirName)
	os.MkdirAll(fullDir, 0755)

	fileNameBase := strings.ReplaceAll(title, "/", "_")
	fileNameBase = strings.ReplaceAll(fileNameBase, "\\", "_")
	fileNameBase = truncateFilename(fileNameBase, 200)

	meta := VideoMetadata{
		VideoID:       videoID,
		Title:         res.Title,
		ImageURL:      res.ImageURL,
		DataURL:       res.DataURL,
		TargetDir:     fullDir,
		ImageFilePath: filepath.Join(fullDir, fileNameBase+".jpg"),
		VideoFilePath: filepath.Join(fullDir, fileNameBase+".mp4"),
		ListID:        listID,
	}

	// 保存到缓存
	if cacheData, err := json.MarshalIndent(meta, "", "  "); err == nil {
		os.WriteFile(cacheFile, cacheData, 0644)
	}

	return meta, nil
}

// RefreshVideoDataURL 刷新视频下载 URL
func (s *Scraper) RefreshVideoDataURL(wsURL, videoID string) (string, error) {
	ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx := chromedp.NewContext(allocatorContext)
	defer cancelCtx()

	var res Result
	var downloadLinks []map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf(chromeDownURL, videoID)),
		chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.Text(downloadTitleSelector, &res.Title, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.AttributeValue(downloadImageSelector, "src", &res.ImageURL, nil, chromedp.ByQuery),
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
		return "", err
	}

	if len(downloadLinks) == 0 {
		return "", fmt.Errorf("no download links found")
	}

	selectedURL := ""
	if s.resolution != "" {
		for _, linkInfo := range downloadLinks {
			if strings.Contains(linkInfo["resolution"], s.resolution) {
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

	// 更新缓存
	cacheFile := filepath.Join(s.cacheDir, fmt.Sprintf("info_%s.json", videoID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedMeta VideoMetadata
		if err := json.Unmarshal(data, &cachedMeta); err == nil {
			cachedMeta.DataURL = selectedURL
			if res.Title != "" {
				cachedMeta.Title = res.Title
			}
			if res.ImageURL != "" {
				cachedMeta.ImageURL = res.ImageURL
			}
			if cacheData, err := json.MarshalIndent(cachedMeta, "", "  "); err == nil {
				os.WriteFile(cacheFile, cacheData, 0644)
			}
		}
	}

	log.Printf("Refreshed DataURL for %s (%s)", videoID, res.Title)
	return selectedURL, nil
}

// truncateFilename 截断文件名
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
