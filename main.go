package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"hanime-dl/chrome"
	"hanime-dl/config"
	"hanime-dl/downloader"
	"hanime-dl/internal/idutil"
	"hanime-dl/scraper"
	"hanime-dl/web"
)

//go:embed web/*
var embeddedWebFS embed.FS

// version 由发布脚本用 -ldflags "-X main.version=vX.Y.Z" 注入。
//
// 为什么需要它：此前六个平台的产物里没有任何版本信息，出问题时无法确认
// 用户手上跑的到底是哪一个二进制。默认值 "dev" 同时标记「本地裸 build，非发布产物」。
var version = "dev"

// commit 可选注入，用于排查时定位具体提交。
var commit = "none"

// VideoTask 下载任务
type VideoTask struct {
	VideoID string
	ListID  string
}

// ListStatus 记录一个播放列表的展开进度。
//
// 计数以「视频 ID 集合」为准，而不是整数减法：processList 可能重复看到同一个 ID
// （同一列表内重复、或同一视频分属多个列表），整数减法会把这些重复项多减一次，
// 甚至减成负数，从而让「pending == 0」提前成立、误删 list_*.json —— 整个列表的
// 续传能力就此丢失（审计 P1-1）。改成集合的幂等标记后，计数天然不会为负，
// 且「是否全部处理完」这一判断可以安全地重复执行。
type ListStatus struct {
	// Total 本次展开看到的唯一视频数。
	Total int
	Mutex sync.Mutex
	// pending 尚未成功下载完成的视频 ID 集合。
	//
	// 语义：只有**成功落盘**才把视频移出集合。解析失败或下载失败都保留在集合里，
	// 于是对应的 list_*.json 不会被删除，续传能力保住；删除缓存前再校验一次集合
	// 为空，同时排除了「提前删除」与「并发展开期间补入新成员」两种情况。
	pending map[string]struct{}
}

// newListStatus 创建空的列表进度。
func newListStatus() *ListStatus {
	return &ListStatus{pending: make(map[string]struct{})}
}

// markDone 幂等地把 videoID 标记为已成功下载，返回剩余待处理数量。
func (s *ListStatus) markDone(videoID string) int {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()
	delete(s.pending, videoID)
	return len(s.pending)
}

// isEmpty 报告集合中是否已无待处理视频（删除缓存前的二次校验点）。
func (s *ListStatus) isEmpty() bool {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()
	return len(s.pending) == 0
}

// contains 报告 videoID 是否仍在待处理集合中（诊断与测试用）。
func (s *ListStatus) contains(videoID string) bool {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()
	_, ok := s.pending[videoID]
	return ok
}

// mainResolver 抽象 CLI 路径需要的解析能力。
//
// 抽窄接口的目的：把 resolve 循环体提成可测的 resolveAndQueue。方法签名与
// *scraper.Scraper 精确对应，下面的编译期断言会在签名漂移时立刻报错。
type mainResolver interface {
	ResolveVideoInfo(wsURL, videoID, listID string) (scraper.VideoMetadata, error)
	GetPlaylist(wsURL, listID string) ([]string, error)
	RefreshVideoDataURL(wsURL, videoID string) (string, error)
}

var _ mainResolver = (*scraper.Scraper)(nil)

// taskQueueAdder 按 videoID 去重地把任务写入队列。
//
// 抽成独立类型是为了可测：原实现是发现协程里的匿名闭包，外部无法引用，于是
// 「同一视频只入队一次」这条 P1-1 的关键修复完全无法被测试锁定。
// 加锁的理由：CLI 目前只有发现协程这一个生产者，但并发写裸 map 的失败模式是
// fatal error（concurrent map writes，无法 recover），而「同一 videoID 只入队一次」
// 这条不变量不该依赖调用方的纪律；web 侧的等价实现（WebServer.markSeen）同样加锁，
// 两边保持一致。
type taskQueueAdder struct {
	mu    sync.Mutex
	seen  map[string]bool
	queue chan<- VideoTask
}

// newTaskQueueAdder 创建去重入队器。
func newTaskQueueAdder(queue chan<- VideoTask) *taskQueueAdder {
	return &taskQueueAdder{seen: make(map[string]bool), queue: queue}
}

// add 在该 videoID 首次出现时入队并返回 true；重复出现返回 false 且不入队。
func (a *taskQueueAdder) add(task VideoTask) bool {
	a.mu.Lock()
	if a.seen[task.VideoID] {
		a.mu.Unlock()
		return false
	}
	a.seen[task.VideoID] = true
	a.mu.Unlock()

	// 入队放在锁外：队列写入可能阻塞，不该把去重表锁一起拖住。
	a.queue <- task
	return true
}

// has 报告该 videoID 是否已经被入队过。
func (a *taskQueueAdder) has(videoID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[videoID]
}

// validID 校验 ID 形状，不合法时打 WARN 并返回 false。
//
// 为什么 CLI 侧也要判（web 侧已有同一道校验）：这些 ID 会被拼进磁盘路径
// —— cache/info_<id>.json、cache/list_<id>.json —— 而未校验的值经
// filepath.Join 的 Clean 可以逃出缓存目录，把「写缓存」与「清缓存」变成
// 任意路径写/删（复现见 internal/idutil 的测试）。
//
// 关键是播放列表里的视频 ID 是**从网页抓回来的**，属于不可信输入；
// config.yaml 里的 ID 虽然是用户手写，也可能打错成带斜杠的形状。
// scraper 侧同样有这道校验作为纵深防御，这里先挡住可以避免无谓的浏览器往返。
func validID(kind, id string) bool {
	if err := idutil.Validate(id); err != nil {
		log.Printf("WARN: 跳过非法 %s ID：%v", kind, err)
		return false
	}
	return true
}

// resolveAndQueue 解析一个任务并把结果送入下载队列。
//
// 返回 false 表示解析失败。**失败路径不触碰任何列表进度**：视频留在待处理集合里，
// 于是 list_*.json 不会被删除、续传能力保留 —— 语义是「只有成功落盘才移出集合」。
// 注意解析**成功**同样不会移出集合：只有 downloadOne 真正下载成功才会调用
// markVideoDone，因此「解析成功但下载失败」的视频也留在集合里。
//
// 旧实现在解析失败的 continue 之前无条件执行 Pending--，与 processList 的不去重
// 发送叠加后会把计数减成负数，让「pending == 0」提前成立并误删整个列表的缓存
// （P1-1；反向复现见 .dbg/probe）。
func resolveAndQueue(res mainResolver, wsURL string, task VideoTask, out chan<- scraper.VideoMetadata) bool {
	meta, err := res.ResolveVideoInfo(wsURL, task.VideoID, task.ListID)
	if err != nil {
		log.Printf("Skipping %s due to resolution failure: %v", task.VideoID, err)
		return false
	}
	out <- meta
	return true
}

var (
	progressMu sync.Mutex
	// listProgress 播放列表 ID → 展开进度。
	listProgress = make(map[string]*ListStatus)
	// videoLists 反向索引：视频 ID → 包含它的播放列表 ID（去重）。
	//
	// 需要它的原因：taskQueueAdder 的去重是全局的（同一视频只入队一次，任务里的 ListID
	// 只记第一个列表），所以「某个视频下载完成」必须把它在**所有**包含它的列表里
	// 都标记完成，否则其余列表的 pending 永远清不空，它们的 list_*.json 也永远
	// 不会被回收。
	videoLists = make(map[string][]string)
	// videoDone 已成功下载完成的视频 ID，用于让重复展开保持幂等。
	videoDone = make(map[string]bool)
)

// registerListMembers 把一个播放列表的成员登记进进度集合并建立反向索引。
func registerListMembers(listID string, members []string) {
	progressMu.Lock()
	defer progressMu.Unlock()

	status, ok := listProgress[listID]
	if !ok {
		status = newListStatus()
		listProgress[listID] = status
	}

	status.Mutex.Lock()
	defer status.Mutex.Unlock()
	for _, videoID := range members {
		// 已成功下载过的视频不再计入待处理：否则重复展开会把它加回集合，而它的
		// 任务已被 taskQueueAdder 去重、不会再跑，集合就永远清不空。
		if videoDone[videoID] {
			continue
		}
		if _, dup := status.pending[videoID]; !dup {
			status.pending[videoID] = struct{}{}
			status.Total++
		}
		if !slices.Contains(videoLists[videoID], listID) {
			videoLists[videoID] = append(videoLists[videoID], listID)
		}
	}
}

// markVideoDone 把一个视频的成功完成同步到所有包含它的列表，并回收因此清空的
// list_*.json 缓存。
func markVideoDone(workerID int, cacheDir, videoID string) {
	progressMu.Lock()
	videoDone[videoID] = true
	lists := append([]string(nil), videoLists[videoID]...)
	emptied := make([]string, 0, len(lists))
	for _, listID := range lists {
		status, ok := listProgress[listID]
		if !ok {
			continue
		}
		if status.markDone(videoID) == 0 {
			emptied = append(emptied, listID)
		}
	}
	progressMu.Unlock()

	for _, listID := range emptied {
		clearListCacheIfEmpty(workerID, cacheDir, listID)
	}
}

// clearListCacheIfEmpty 在二次校验「进度集合为空」之后回收 list_*.json。
//
// 二次校验不可省略：第一次判断与真正删除之间存在窗口，并发的列表展开可能在这期间
// 补入新的待处理成员；重新持锁确认集合为空才允许删除。
func clearListCacheIfEmpty(workerID int, cacheDir, listID string) {
	progressMu.Lock()
	status, ok := listProgress[listID]
	empty := ok && status.isEmpty()
	progressMu.Unlock()
	if !empty {
		return
	}

	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("list_%s.json", listID))
	if err := os.Remove(cacheFile); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[Worker %d] Failed to clear list cache for %s: %v", workerID, listID, err)
		}
		return
	}
	log.Printf("[Worker %d] Cleared list cache for %s", workerID, listID)
}

func main() {
	// 只在 run() 返回之后才退出进程：此时 run() 内的 defer（含 Chrome 进程回收）
	// 已经执行完毕。若在 run() 内部用 log.Fatal，defer 会被 os.Exit 跳过，
	// 拉起的 Chrome 又会变成孤儿进程（gocritic exitAfterDefer）。
	if err := run(); err != nil {
		log.Fatalf("Fatal: %v", err)
	}
}

// run 承载真正的启动逻辑，让所有 defer 都能在失败路径上执行。
func run() error {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	webMode := flag.Bool("web", false, "Start web server instead of CLI mode")
	webAddr := flag.String("web-addr", ":8080", "Web server address")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	// -version 必须在读配置之前处理：查版本不该要求工作目录里有一份可用配置，
	// 也不该顺带创建 config.yaml。
	if *versionFlag {
		fmt.Printf("hanime-dl %s (commit %s)\n", version, commit)
		return nil
	}

	// 进程退出时回收本次启动过程中拉起的本地 Chrome（进程组 + 临时 profile 目录）。
	// 少了这一步，StartLocalChrome / refreshWSURL 拉起的浏览器进程会一直留在宿主上
	// （审计 P2-13）。
	defer chrome.CleanupLaunchedChrome()

	cfg, created := config.MustLoadOrCreate(*configPath)
	if created {
		log.Printf("Config not found at %s, generated default config. Please review and edit it as needed.", *configPath)
	}
	log.Printf("hanime-dl %s (commit %s) starting", version, commit)
	// 注意：不再无条件覆盖 cfg.ClearCache —— 配置文件里显式写 false 必须生效
	// （审计 P3-15）。默认值语义由 config 侧决定。

	if *webMode {
		return runWebMode(*webAddr, cfg)
	}
	return runCLIMode(cfg)
}

// logChromeHelp 打印无法连接 Chrome 时的排查选项。
func logChromeHelp() {
	log.Println("\n=== Chrome Connection Options ===")
	log.Println("Option 1: Start Chrome manually with remote debugging:")
	log.Println("  google-chrome --remote-debugging-port=9222")
	log.Println("Option 2: Use Docker desktop environment:")
	log.Println("  cd ubuntu-desktop && docker compose up -d")
	log.Println("Option 3: Configure remote Chrome in config.yaml:")
	log.Println("  chromeRemoteURL: http://your-chrome-host:9222/json/version")
}

// chromeProbeOptions 把配置里的 TLS 开关翻译成 chrome 包的探测选项。
//
// 默认（cfg.ChromeInsecureTLS == false）保持证书校验开启。历史上这里是
// 无条件 InsecureSkipVerify: true，任何 https CDP 端点都能被中间人伪造。
func chromeProbeOptions(cfg *config.Config) chrome.ProbeOptions {
	return chrome.ProbeOptions{InsecureSkipVerify: cfg.ChromeInsecureTLS}
}

// runWebMode 启动 web 界面模式，并把 Ctrl+C / SIGTERM 接到优雅停机。
func runWebMode(addr string, cfg *config.Config) error {
	// signal.NotifyContext 接管 SIGINT/SIGTERM：ctx 被取消后 web.RunWebServer 会调用
	// WebServer.Stop()，让 HTTP 服务与 worker 有序退出（旧实现的 Stop() 无任何调用点，
	// 日志里的「Press Ctrl+C to stop the server」是空头承诺）。
	// 只在 web 模式安装：CLI 模式保留「Ctrl+C 立即终止」的默认语义。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	wsURL, launchedLocalChrome, err := chrome.EnsureChromeConnectionDetailedWith(
		cfg.ChromeRemoteURL, chrome.StartupCDPTimeout, chromeProbeOptions(cfg))
	if err != nil {
		log.Printf("Failed to prepare Chrome CDP for web mode: %v", err)
		logChromeHelp()
		return errors.New("unable to connect to Chrome, please set up Chrome and try again")
	}

	if launchedLocalChrome {
		webURL := webInterfaceURL(addr)
		go func() {
			waitForHTTPReady(webURL, 10*time.Second)
			if err := chrome.OpenURL(wsURL, webURL, chrome.OpenPageTimeout); err != nil {
				log.Printf("Failed to open web interface in local Chrome: %v", err)
				return
			}
			log.Printf("Opened web interface in local Chrome: %s", webURL)
		}()
	}

	// 配置里写了 apiToken 却同时留空，等价于不启用鉴权：这里不做额外判断，
	// 由 web 侧按「空 = 关闭」处理，保持配置语义单一。
	if cfg.APIToken == "" {
		log.Println("WARN: apiToken 为空，Web API 无鉴权；同网段设备可直接写任务队列。" +
			"如需暴露到本机之外，请在 config.yaml 设置 apiToken（详见 README「安全与暴露方式」）")
	}

	runErr := web.RunWebServer(&web.Options{
		Ctx:               ctx,
		Addr:              addr,
		CacheDir:          cfg.CacheDir,
		DownDir:           cfg.DownDir,
		MaxWorkers:        cfg.MaxDownloadWorkers,
		ChromeRemoteURL:   cfg.ChromeRemoteURL,
		WSURL:             wsURL,
		HTTPProxy:         cfg.HttpProxy,
		VideoResolution:   cfg.VideoResolution,
		ClearCache:        cfg.ClearCache,
		SingleCode:        cfg.SingleCode,
		ListCode:          cfg.ListCode,
		EmbedFS:           embeddedWebFS,
		APIToken:          cfg.APIToken,
		APIAllowOrigin:    cfg.APIAllowOrigin,
		ChromeInsecureTLS: cfg.ChromeInsecureTLS,
	})
	if runErr != nil {
		return fmt.Errorf("web server: %w", runErr)
	}
	log.Println("Web server stopped.")
	return nil
}

// runCLIMode 按配置里的 SingleCode / ListCode 下载。
func runCLIMode(cfg *config.Config) error {
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %s: %w", cfg.CacheDir, err)
	}
	if err := os.MkdirAll(cfg.DownDir, 0o755); err != nil {
		return fmt.Errorf("create download dir %s: %w", cfg.DownDir, err)
	}
	if cfg.MaxDownloadWorkers <= 0 {
		cfg.MaxDownloadWorkers = 3
	}

	log.Printf("Configuration loaded: RemoteURL=%s, DownDir=%s, CacheDir=%s, Workers=%d",
		cfg.ChromeRemoteURL, cfg.DownDir, cfg.CacheDir, cfg.MaxDownloadWorkers)

	wsURL, err := chrome.EnsureChromeConnectionWith(cfg.ChromeRemoteURL, chrome.StartupCDPTimeout, chromeProbeOptions(cfg))
	if err != nil {
		log.Printf("Failed to prepare Chrome CDP: %v", err)
		logChromeHelp()
		return errors.New("unable to connect to Chrome, please set up Chrome and try again")
	}

	s := scraper.NewScraper(cfg.CacheDir, cfg.DownDir, cfg.VideoResolution)
	d := downloader.NewDownloader(cfg.HttpProxy)

	// 下载队列：worker 只消费、不生产，因此不存在「生产者与消费者互相阻塞」的死锁。
	downloadQueue := make(chan scraper.VideoMetadata, 100)
	var downloadWg sync.WaitGroup

	for i := 0; i < cfg.MaxDownloadWorkers; i++ {
		downloadWg.Add(1)
		go func(workerID int) {
			defer downloadWg.Done()
			for meta := range downloadQueue {
				downloadOne(workerID, cfg, d, &meta)
			}
		}(i)
	}

	// 任务队列
	taskQueue := make(chan VideoTask, 1000)

	// 启动发现协程
	go func() {
		defer close(taskQueue)

		// 按 videoID 去重入队（见 taskQueueAdder：重复 ID 只入队一次）。
		adder := newTaskQueueAdder(taskQueue)

		// 1. 单个视频
		for _, id := range cfg.SingleCode {
			if !validID("video", id) {
				continue
			}
			adder.add(VideoTask{VideoID: id, ListID: ""})
		}

		// 2. 区分缓存和网络的播放列表
		var cachedLists, networkLists []string
		for _, listID := range cfg.ListCode {
			if !validID("playlist", listID) {
				continue
			}
			cacheFile := filepath.Join(cfg.CacheDir, fmt.Sprintf("list_%s.json", listID))
			if _, err := os.Stat(cacheFile); err == nil {
				cachedLists = append(cachedLists, listID)
			} else {
				networkLists = append(networkLists, listID)
			}
		}

		// processList 复用 adder（而不是直接往 taskQueue 发送）：同一列表内的重复
		// 视频 ID 在这里就被去掉，列表进度集合也只会登记唯一成员，从源头消除
		// 「重复项被多减一次」的可能（审计 P1-1）。
		processList := func(listID string) {
			log.Printf("Fetching playlist for ID: %s", listID)
			list, err := s.GetPlaylist(wsURL, listID)
			if err != nil {
				log.Printf("Failed to get playlist %s: %v", listID, err)
				return
			}
			// 列表成员来自网页，是不可信输入：非法 ID 直接丢弃，
			// 不进入后续的信息解析与缓存写入。
			valid := list[:0]
			for _, vid := range list {
				if validID("video", vid) {
					valid = append(valid, vid)
				}
			}
			if len(valid) != len(list) {
				log.Printf("WARN: playlist %s 中有 %d 个视频 ID 形状非法，已跳过", listID, len(list)-len(valid))
			}
			log.Printf("Playlist %s acquired: %d videos found", listID, len(valid))

			registerListMembers(listID, valid)
			for _, vid := range valid {
				adder.add(VideoTask{VideoID: vid, ListID: listID})
			}
		}

		// 3. 处理缓存的播放列表
		for _, listID := range cachedLists {
			processList(listID)
		}

		// 4. 扫描缓存中待处理的播放列表
		listFiles, err := filepath.Glob(filepath.Join(cfg.CacheDir, "list_*.json"))
		if err != nil {
			log.Printf("Failed to scan cached playlists: %v", err)
		}
		for _, f := range listFiles {
			base := filepath.Base(f)
			if filepath.Ext(base) == ".json" && len(base) > 5 {
				listID := base[5 : len(base)-5]
				if !slices.Contains(cfg.ListCode, listID) {
					log.Printf("Found cached playlist %s, resuming...", listID)
					processList(listID)
				}
			}
		}

		// 5. 扫描缓存中待处理的视频
		infoFiles, err := filepath.Glob(filepath.Join(cfg.CacheDir, "info_*.json"))
		if err != nil {
			log.Printf("Failed to scan cached video info: %v", err)
		}
		for _, f := range infoFiles {
			base := filepath.Base(f)
			if filepath.Ext(base) == ".json" && len(base) > 5 {
				vid := base[5 : len(base)-5]
				if !adder.has(vid) {
					log.Printf("Found cached video info for %s, resuming...", vid)
					adder.add(VideoTask{VideoID: vid, ListID: ""})
				}
			}
		}

		// 6. 处理网络的播放列表
		for _, listID := range networkLists {
			processList(listID)
		}

		log.Println("Discovery completed, closing task queue.")
	}()

	// 主线程：解析视频信息。失败时 resolveAndQueue 返回 false 且不触碰列表进度，
	// 该视频留在待处理集合里，list_*.json 因而不会被删除，下次运行仍可续传（审计 P1-1）。
	for task := range taskQueue {
		resolveAndQueue(s, wsURL, task, downloadQueue)
	}

	close(downloadQueue)
	downloadWg.Wait()

	log.Println("All tasks completed.")
	return nil
}

// downloadOne 下载单个视频（含封面），并按真实下载结果更新列表进度与缓存。
//
// 成功判定用**下载返回值**，而不是下载后 os.Stat：后者会把「上次残留的半成品」和
// 「本次下载失败」都当成成功（审计 P0-1 在 CLI 路径上的放大器），也会在失败时
// 依旧递减列表进度。下载器已保证只有校验完整的文件才会被改名到最终路径。
//
// meta 按指针传递：scraper.VideoMetadata 有 128 字节，gocritic 的 hugeParam
// （performance 标签）要求大结构体按指针传；调用点传的是每轮迭代的局部变量副本，
// 本函数不保留该指针。
func downloadOne(workerID int, cfg *config.Config, d *downloader.Downloader, meta *scraper.VideoMetadata) bool {
	// 封面下载失败不阻断视频下载。
	if meta.ImageURL != "" && meta.ImageFilePath != "" {
		if fileExists(meta.ImageFilePath) {
			log.Printf("[Worker %d] Image exists: %s", workerID, meta.ImageFilePath)
		} else if err := d.DownloadWithRetry(meta.ImageURL, meta.ImageFilePath); err != nil {
			log.Printf("[Worker %d] Image download failed for %s: %v", workerID, meta.VideoID, err)
		}
	}

	var videoOK bool
	switch {
	case meta.DataURL == "" || meta.VideoFilePath == "":
		log.Printf("[Worker %d] No downloadable video URL for %s", workerID, meta.VideoID)
	case fileExists(meta.VideoFilePath):
		log.Printf("[Worker %d] Video exists: %s", workerID, meta.VideoFilePath)
		videoOK = true
	default:
		if err := d.DownloadWithRetry(meta.DataURL, meta.VideoFilePath); err != nil {
			log.Printf("[Worker %d] Video download failed for %s: %v", workerID, meta.VideoID, err)
		} else {
			log.Printf("[Worker %d] Downloaded: %s", workerID, meta.VideoFilePath)
			videoOK = true
		}
	}

	if !videoOK {
		return false
	}

	if cfg.ClearCache {
		cacheFile := filepath.Join(cfg.CacheDir, fmt.Sprintf("info_%s.json", meta.VideoID))
		if err := os.Remove(cacheFile); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[Worker %d] Failed to clear cache for %s: %v", workerID, meta.VideoID, err)
			}
		} else {
			log.Printf("[Worker %d] Cleared cache for %s", workerID, meta.VideoID)
		}
	}

	markVideoDone(workerID, cfg.CacheDir, meta.VideoID)
	return true
}

// fileExists 报告 path 是否已存在且不是目录。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func probeHTTPReady(target string) bool {
	u := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	hostPort := u
	if slash := strings.Index(hostPort, "/"); slash >= 0 {
		hostPort = hostPort[:slash]
	}
	if conn, err := net.DialTimeout("tcp", hostPort, 500*time.Millisecond); err == nil {
		if cerr := conn.Close(); cerr != nil {
			log.Printf("Failed to close readiness probe connection: %v", cerr)
		}
	}
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("Failed to close readiness probe body: %v", cerr)
		}
	}()
	if _, cerr := io.Copy(io.Discard, resp.Body); cerr != nil {
		log.Printf("Failed to drain readiness probe body: %v", cerr)
	}
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
