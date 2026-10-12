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
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"hanime-dl/chrome"
)

const (
	chromeDownURL         = "https://hanime1.me/download?v=%s"
	chromeWatchURL        = "https://hanime1.me/watch?v=%s"
	chromeSearchURL       = "https://hanime1.me/search?query=%s"
	downloadTitleSelector = `h3`
	downloadImageSelector = `img.download-image`
)

// 超时预算。取值均为「单次页面操作」量级：
// 列表页/下载页需要真正的页面加载 + 少量固定等待，因此是分钟级；
// 高清封面搜索纯属增强项，失败可安全回落，因此只有十几秒。
const (
	// playlistFetchTimeout 播放列表页抓取预算。
	playlistFetchTimeout = 120 * time.Second
	// downloadPageTimeout 下载页解析预算。原实现为 3000s（50 分钟），
	// 与同文件同类操作（300s）相比明显是笔误，此处统一为 300s。
	downloadPageTimeout = 300 * time.Second
	// refreshDataURLTimeout 刷新下载直链的预算（与下载页解析同量级）。
	refreshDataURLTimeout = 300 * time.Second
	// coverSearchTimeout 高清封面搜索的预算（尽力而为，失败回落低清封面）。
	coverSearchTimeout = 15 * time.Second
	// tabOpenTimeout 打开新标签页的预算。
	tabOpenTimeout = 15 * time.Second
)

// 文件路径长度预算。Windows 传统 MAX_PATH 为 260，这里留出余量，
// 并把「目录 + 分隔符 + 基础名 + 扩展名」整体纳入预算，
// 避免只截断基础名而全路径仍然超限。
const (
	// maxPathBytes 全路径（目录 + 分隔符 + 文件名）的字节预算。
	maxPathBytes = 240
	// maxBaseNameBytes 文件名基础名（不含扩展名）的字节上限。
	maxBaseNameBytes = 180
	// minBaseNameBytes 截断下限，避免目录过深时基础名被截成空串。
	minBaseNameBytes = 16
)

// newTabContext 在已有浏览器中打开一个新「标签页」（而非新窗口）并返回附着其上的 chromedp context。
// 说明：chromedp v0.13.x 在远程 allocator 下会强制以 newWindow=true 创建新窗口，
// 这里改为先通过 DevTools 创建标签页拿到 target id，再用 WithTargetID 附着，
// 取消返回的 cancel 时 chromedp 会自动关闭该标签页。
func newTabContext(allocatorContext context.Context, wsURL string) (context.Context, context.CancelFunc, error) {
	tabID, err := chrome.CreateTab(wsURL, "about:blank", tabOpenTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open new tab: %w", err)
	}
	ctx, cancel := chromedp.NewContext(allocatorContext, chromedp.WithTargetID(target.ID(tabID)))
	return ctx, cancel, nil
}

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
	ListID        string `json:"list_id,omitempty"`
}

// downloadLink 下载链接表的一行：分辨率标签与直链。
type downloadLink struct {
	Resolution string
	URL        string
}

// downloadLinksJS 解析下载页链接表的共享脚本。
// 说明：ResolveVideoInfo 与 RefreshVideoDataURL 原先各自内联了一份逐字符相同的 JS，
// 这里统一成一处，避免两处逻辑各自漂移。
const downloadLinksJS = `
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
`

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

// GetPlaylist 获取播放列表（带缓存）。
// listID 是列表/合集标识（播放列表页里的视频 ID），缓存文件为 list_<listID>.json。
func (s *Scraper) GetPlaylist(wsURL, listID string) ([]string, error) {
	// 检查缓存
	cacheFile := filepath.Join(s.cacheDir, fmt.Sprintf("list_%s.json", listID))
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cachedList []string
		if err := json.Unmarshal(data, &cachedList); err == nil && len(cachedList) > 0 {
			log.Printf("Loaded playlist %s from cache (%d items)", listID, len(cachedList))
			return cachedList, nil
		}
	}

	// 从网页获取
	ctx1, cancel1 := context.WithTimeout(context.Background(), playlistFetchTimeout)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx, err := newTabContext(allocatorContext, wsURL)
	if err != nil {
		return nil, err
	}
	defer cancelCtx()

	// 用 JS 直接提取播放列表里的所有视频 ID。
	// 真实页面结构：播放列表容器 id 为 #playlist-scroll，每个视频项外层是
	// <div class="playlist-hover-wrap clickable-row" data-href="https://hanime1.me/watch?v=xxx">，
	// data-href 即视频地址，最精确。退而求其次取容器内 a[href*="watch?v="]。
	extractJS := `(function(){
		var wrap = document.querySelector('#playlist-scroll');
		if(!wrap){ return []; }
		var nodes = wrap.querySelectorAll('.playlist-hover-wrap');
		if(nodes.length === 0){
			nodes = wrap.querySelectorAll('a[href*="watch?v="]');
		}
		var seen = {};
		var out = [];
		nodes.forEach(function(n){
			var href = n.getAttribute('data-href') || n.getAttribute('href');
			if(!href){ return; }
			try {
				var u = new URL(href, location.href);
				var v = u.searchParams.get('v');
				if(v && !seen[v]){ seen[v] = true; out.push(v); }
			} catch(e){}
		});
		return out;
	})()`

	var raw []interface{}
	err = chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf(chromeWatchURL, listID)),
		chromedp.Sleep(5*time.Second),
		chromedp.Evaluate(extractJS, &raw),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to fetch playlist %s: %w", listID, err)
	}

	result := make([]string, 0, len(raw))
	seen := make(map[string]bool)
	for _, item := range raw {
		id, ok := item.(string)
		if !ok || id == "" {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("playlist %s returned no video ids (page structure may have changed)", listID)
	}

	// 保存到缓存（写失败不应静默，但也不该中断已经成功的抓取）
	writeJSONFile(cacheFile, result)

	log.Printf("Fetched playlist %s: %d videos", listID, len(result))
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
			// 缓存语义：info_<id>.json 只保存与列表无关的字段，ListID 不属于缓存内容
			// （见写入处的同款说明）。归属完全由调用方通过入参决定，
			// 这样同一视频被多个列表复用时，归属不会随缓存内容而漂移。
			cachedMeta.ListID = listID
			return cachedMeta, nil
		}
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), downloadPageTimeout)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx, err := newTabContext(allocatorContext, wsURL)
	if err != nil {
		return VideoMetadata{}, err
	}
	defer cancelCtx()

	res, links, err := fetchDownloadLinks(ctx, videoID)
	if err != nil {
		return VideoMetadata{}, err
	}

	res.DataURL = s.selectDataURL(links, videoID)

	if res.Title == "" || res.DataURL == "" {
		return VideoMetadata{}, fmt.Errorf("missing title or data url")
	}

	// 尝试获取高清封面图（尽力而为：失败只回落并打 WARN，不影响主流程）
	res.ImageURL = s.fetchHighResCover(ctx, videoID, res.Title, res.ImageURL)

	// 构建元数据
	title := strings.TrimSpace(res.Title)
	dirName := dirNameFromTitle(title)

	fullDir := filepath.Join(s.downDir, dirName)
	if err := os.MkdirAll(fullDir, 0755); err != nil {
		return VideoMetadata{}, fmt.Errorf("failed to create target dir %s: %w", fullDir, err)
	}

	imagePath, videoPath := buildMediaFilePaths(fullDir, title)

	meta := VideoMetadata{
		VideoID:       videoID,
		Title:         res.Title,
		ImageURL:      res.ImageURL,
		DataURL:       res.DataURL,
		TargetDir:     fullDir,
		ImageFilePath: imagePath,
		VideoFilePath: videoPath,
		ListID:        listID,
	}

	// 保存到缓存：缓存里不落 ListID（只存与列表无关的字段）。
	// 判定依据：同一视频可以出现在多个列表里，把 ListID 写入缓存会让归属随最后
	// 一次写入而漂移；归属完全由调用方入参决定更可预测。
	cached := meta
	cached.ListID = ""
	writeJSONFile(cacheFile, cached)

	return meta, nil
}

// RefreshVideoDataURL 刷新视频下载 URL
func (s *Scraper) RefreshVideoDataURL(wsURL, videoID string) (string, error) {
	ctx1, cancel1 := context.WithTimeout(context.Background(), refreshDataURLTimeout)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx, err := newTabContext(allocatorContext, wsURL)
	if err != nil {
		return "", err
	}
	defer cancelCtx()

	res, links, err := fetchDownloadLinks(ctx, videoID)
	if err != nil {
		return "", err
	}

	if len(links) == 0 {
		return "", fmt.Errorf("no download links found")
	}

	selectedURL := s.selectDataURL(links, videoID)
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
			// 与 ResolveVideoInfo 写入处一致：缓存不保存 ListID。
			cachedMeta.ListID = ""
			writeJSONFile(cacheFile, cachedMeta)
		}
	}

	log.Printf("Refreshed DataURL for %s (%s)", videoID, res.Title)
	return selectedURL, nil
}

// fetchDownloadLinks 在给定标签页上下文中打开下载页，抓取标题、封面与下载链接表。
// ResolveVideoInfo 与 RefreshVideoDataURL 共用同一实现（原先是两套近乎重复的流程）。
func fetchDownloadLinks(ctx context.Context, videoID string) (Result, []downloadLink, error) {
	var res Result
	var rawLinks []map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf(chromeDownURL, videoID)),
		chromedp.WaitVisible(downloadTitleSelector, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.Text(downloadTitleSelector, &res.Title, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.AttributeValue(downloadImageSelector, "src", &res.ImageURL, nil, chromedp.ByQuery),
		chromedp.Evaluate(downloadLinksJS, &rawLinks),
	)
	if err != nil {
		return Result{}, nil, err
	}
	return res, toDownloadLinks(rawLinks), nil
}

// toDownloadLinks 把内联 JS 返回的原始行（[]map[string]string）转换为 downloadLink。
// 保持与原有 map 结构逐字段一致，不过滤任何行（行数语义与原实现相同）。
func toDownloadLinks(raw []map[string]string) []downloadLink {
	if len(raw) == 0 {
		return nil
	}
	links := make([]downloadLink, 0, len(raw))
	for _, m := range raw {
		links = append(links, downloadLink{Resolution: m["resolution"], URL: m["url"]})
	}
	return links
}

// pickByResolution 按请求分辨率挑选下载链接。
//
// 约定（必须与原实现行为兼容）：
//   - links 的第一条即最高分辨率；任何未命中都回落到 links[0]；
//   - want 为空、links 为空、或没有任何分辨率包含 want 时 matched 为 false；
//   - links 为空时返回零值 downloadLink。
func pickByResolution(links []downloadLink, want string) (link downloadLink, matched bool) {
	if len(links) == 0 {
		return downloadLink{}, false
	}
	if want != "" {
		for _, l := range links {
			if strings.Contains(l.Resolution, want) {
				return l, true
			}
		}
	}
	return links[0], false
}

// selectDataURL 用 pickByResolution 挑选直链，并统一两处调用点的日志语义：
// 原 ResolveVideoInfo 有「命中/未命中回落」日志，而 RefreshVideoDataURL 两者都没有。
// links 为空或选中行没有 URL 时返回空串。
func (s *Scraper) selectDataURL(links []downloadLink, videoID string) string {
	link, matched := pickByResolution(links, s.resolution)
	if link.URL == "" {
		return ""
	}
	if matched {
		log.Printf("Found requested resolution %s for %s", s.resolution, videoID)
	} else if s.resolution != "" {
		log.Printf("Requested resolution %s not found for %s, falling back to: %s", s.resolution, videoID, link.Resolution)
	}
	return link.URL
}

// fetchHighResCover 尽力而为地通过搜索页获取高清封面。
//
// 搜索只是「更好的封面」这一增强项，失败必须安全回落下载页自带的低清封面，
// 因此本函数不返回 error —— 但每一步失败都会打 WARN 说明回落原因
// （原实现在 searchErr != nil 时完全静默，整个过程无任何痕迹）。
//
// 搜索 URL 不再携带任何分类(genre)过滤：原实现恒拼 &genre=%E8%A3%85%E7%95%8C（"装界"），
// 对不属于该分类的视频搜索结果必然为空，导致高清封面永远取不到。
// 现改为先按完整标题搜索，未命中再用放宽关键词重试；
// 因为脚本是按 watch?v=<videoID> 精确匹配的，放宽关键词不会选到别的视频。
func (s *Scraper) fetchHighResCover(ctx context.Context, videoID, title, fallback string) string {
	queries := coverSearchQueries(title)
	if len(queries) == 0 {
		log.Printf("WARN: high-res cover search skipped for %s: empty title, keeping page cover", videoID)
		return fallback
	}

	// 独立于下载页解析的短预算：派生自标签页 ctx（父级取消可传播到搜索），
	// 再用 coverSearchTimeout 限制这个「尽力而为」阶段的耗时上限。
	searchCtx, cancel := context.WithTimeout(ctx, coverSearchTimeout)
	defer cancel()

	for _, query := range queries {
		if searchCtx.Err() != nil {
			log.Printf("WARN: high-res cover search budget exhausted for %s: %v", videoID, searchCtx.Err())
			break
		}
		imgURL, err := searchCoverImage(searchCtx, videoID, query)
		switch {
		case err != nil:
			log.Printf("WARN: high-res cover search failed for %s (query=%q): %v", videoID, query, err)
		case imgURL == "":
			log.Printf("WARN: high-res cover search returned no result for %s (query=%q)", videoID, query)
		default:
			log.Printf("Found high-res cover image from search for %s (query=%q)", videoID, query)
			return imgURL
		}
	}

	log.Printf("WARN: no high-res cover found for %s, falling back to the page cover", videoID)
	return fallback
}

// searchCoverImage 在搜索页中查找指向 videoID 的链接并返回其封面图 src。
func searchCoverImage(ctx context.Context, videoID, query string) (string, error) {
	var imgURL string
	err := chromedp.Run(ctx,
		chromedp.Navigate(coverSearchURL(query)),
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
		`, videoID), &imgURL, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
	)
	if err != nil {
		return "", err
	}
	return imgURL, nil
}

// coverSearchQueries 返回高清封面搜索的候选关键词，按优先级排列：
//   - 完整标题；
//   - 若标题带 (...) / [...] 标注，则剥掉标注后取第一个空格分隔的词（放宽匹配）。
//
// 返回空切片表示标题为空、无法搜索。
func coverSearchQueries(title string) []string {
	t := strings.TrimSpace(title)
	if t == "" {
		return nil
	}
	queries := []string{t}
	if short := firstSearchTerm(t); short != "" && short != t {
		queries = append(queries, short)
	}
	return queries
}

// firstSearchTerm 生成放宽后的搜索词：剥掉开头的 (...) / [...] 标注后取第一个空格分隔的词。
func firstSearchTerm(title string) string {
	t := strings.TrimSpace(title)
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}} {
		if strings.HasPrefix(t, pair[0]) {
			if idx := strings.Index(t, pair[1]); idx != -1 {
				t = strings.TrimSpace(t[idx+len(pair[1]):])
			}
			break
		}
	}
	if t == "" {
		return ""
	}
	return strings.Split(t, " ")[0]
}

// coverSearchURL 构造封面搜索 URL。注意：不做任何分类(genre)过滤。
func coverSearchURL(query string) string {
	return fmt.Sprintf(chromeSearchURL, url.QueryEscape(strings.TrimSpace(query)))
}

// dirNameFromTitle 从标题推导目录名，保持原有三分支语义：
//   - "(" 开头 → 截到第一个 ")"（含）；
//   - "[" 开头 → 截到第一个 "]"（含）；
//   - 否则回落为按空格切分的第一个词。
//
// 最后把 / 与 \ 替换为 _，避免生成子目录或非法路径。
func dirNameFromTitle(title string) string {
	t := strings.TrimSpace(title)
	var dirName string
	switch {
	case strings.HasPrefix(t, "("):
		if idx := strings.Index(t, ")"); idx != -1 {
			dirName = t[:idx+1]
		}
	case strings.HasPrefix(t, "["):
		if idx := strings.Index(t, "]"); idx != -1 {
			dirName = t[:idx+1]
		}
	}
	if dirName == "" {
		dirName = strings.Split(t, " ")[0]
	}
	dirName = strings.ReplaceAll(dirName, "/", "_")
	dirName = strings.ReplaceAll(dirName, "\\", "_")
	return dirName
}

// sanitizeFileName 把标题转成文件基础名（替换路径分隔符）。
func sanitizeFileName(title string) string {
	s := strings.ReplaceAll(title, "/", "_")
	return strings.ReplaceAll(s, "\\", "_")
}

// buildMediaFilePaths 在 fullDir 下按 title 生成图片与视频的完整路径。
// 基础名在保留扩展名的前提下按「目录 + 文件名」全路径预算截断（见 truncateFilename）。
func buildMediaFilePaths(fullDir, title string) (imagePath, videoPath string) {
	base := sanitizeFileName(title)
	imagePath = filepath.Join(fullDir, truncateFilename(fullDir, base, ".jpg", maxBaseNameBytes))
	videoPath = filepath.Join(fullDir, truncateFilename(fullDir, base, ".mp4", maxBaseNameBytes))
	return imagePath, videoPath
}

// truncateFilename 在保留扩展名的前提下截断文件名基础名，返回完整的文件名（base+ext）。
//
// 预算按「目录 + 分隔符 + 基础名 + 扩展名」的全路径计算（Windows MAX_PATH=260）：
//   - 基础名上限取 maxBytes 与 (maxPathBytes - len(fullDir) - 1 - len(ext)) 中的较小值；
//   - 预算不足以容纳 minBaseNameBytes 时保底截断并打 WARN；
//   - 截断按 UTF-8 rune 边界进行，不会截出半个汉字。
func truncateFilename(fullDir, name, ext string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = maxBaseNameBytes
	}
	if budget := maxPathBytes - len(fullDir) - 1 - len(ext); budget < maxBytes {
		maxBytes = budget
	}
	if maxBytes < minBaseNameBytes {
		log.Printf("WARN: path budget too small for %q in %q, clamping base name to %d bytes", name, fullDir, minBaseNameBytes)
		maxBytes = minBaseNameBytes
	}
	return truncateBytes(name, maxBytes) + ext
}

// truncateBytes 按 UTF-8 rune 边界把 s 截断到不超过 maxBytes 字节，
// 保证不会截出半个多字节字符（如半个汉字）。
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
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

// writeJSONFile 序列化并写入 JSON 缓存文件。
// 写缓存失败打 WARN 而不静默（原实现直接丢弃 os.WriteFile 的返回值），
// 但也不中断主流程 —— 缓存只是加速手段，抓取结果本身仍然有效。
func writeJSONFile(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("WARN: failed to marshal cache %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("WARN: failed to write cache %s: %v", path, err)
	}
}
