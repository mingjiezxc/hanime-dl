package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hanime-dl/chrome"
	"hanime-dl/downloader"
	"hanime-dl/scraper"
)

// TaskStatus represents the status of a download task
type TaskStatus struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"` // "single" or "list"
	VideoID      string     `json:"video_id"`
	ListID       string     `json:"list_id,omitempty"`
	Status       string     `json:"status"` // "pending", "downloading", "completed", "failed"
	Title        string     `json:"title,omitempty"`
	Progress     int        `json:"progress"` // 0-100
	TotalSize    int64      `json:"total_size,omitempty"`
	Downloaded   int64      `json:"downloaded,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Options 用于初始化 Web 服务器的配置
type Options struct {
	Addr                string
	CacheDir            string
	DownDir             string
	MaxWorkers          int
	ChromeRemoteURL     string
	WSURL               string
	HTTPProxy           string
	DirectDownloadFirst bool
	VideoResolution     string
	ClearCache          bool
	// SingleCode / ListCode 来自 config.yaml，启动时预加载为待下载任务，
	// 让页面打开即可看到"要下载的列表"。
	SingleCode []string
	ListCode   []string
	// EmbedFS 内嵌的静态资源文件系统（来自 //go:embed web/*），
	// 为 nil 时回退到本地 ./web 目录（开发场景）。
	EmbedFS fs.FS
}

// WebServer manages the web interface and the download pipeline
type WebServer struct {
	server     *http.Server
	tasks      map[string]*TaskStatus
	tasksMutex sync.RWMutex

	scraper    *scraper.Scraper
	downloader *downloader.Downloader

	cacheDir        string
	downDir         string
	chromeRemoteURL string
	clearCache      bool

	wsURL   string
	wsMutex sync.RWMutex

	queue      chan *TaskStatus
	maxWorkers int

	embedFS fs.FS

	seen      map[string]bool
	seenMutex sync.Mutex
}

// NewWebServer creates a new web server instance
func NewWebServer(opts Options) *WebServer {
	maxWorkers := opts.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 3
	}
	return &WebServer{
		tasks:           make(map[string]*TaskStatus),
		scraper:         scraper.NewScraper(opts.CacheDir, opts.DownDir, opts.VideoResolution),
		downloader:      downloader.NewDownloader(opts.HTTPProxy, opts.DirectDownloadFirst),
		cacheDir:        opts.CacheDir,
		downDir:         opts.DownDir,
		chromeRemoteURL: opts.ChromeRemoteURL,
		clearCache:      opts.ClearCache,
		wsURL:           opts.WSURL,
		queue:           make(chan *TaskStatus, 1000),
		maxWorkers:      maxWorkers,
		embedFS:         opts.EmbedFS,
		seen:            make(map[string]bool),
	}
}

// AddTask adds a new task to the server
func (ws *WebServer) AddTask(task *TaskStatus) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	ws.tasks[task.ID] = task
}

// mutateTask 在持有写锁的情况下修改任务字段，避免与快照读取产生数据竞争
func (ws *WebServer) mutateTask(task *TaskStatus, fn func(*TaskStatus)) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	fn(task)
}

// snapshotTasks 返回所有任务的值拷贝，供 JSON 序列化使用（无需在编码期间持锁）
func (ws *WebServer) snapshotTasks() []TaskStatus {
	ws.tasksMutex.RLock()
	defer ws.tasksMutex.RUnlock()
	out := make([]TaskStatus, 0, len(ws.tasks))
	for _, t := range ws.tasks {
		out = append(out, *t)
	}
	return out
}

// snapshotTask 返回单个任务的值拷贝
func (ws *WebServer) snapshotTask(id string) (TaskStatus, bool) {
	ws.tasksMutex.RLock()
	defer ws.tasksMutex.RUnlock()
	t, ok := ws.tasks[id]
	if !ok {
		return TaskStatus{}, false
	}
	return *t, true
}

// markSeen 标记视频是否已入队，返回 true 表示首次出现（需要处理）
func (ws *WebServer) markSeen(videoID string) bool {
	ws.seenMutex.Lock()
	defer ws.seenMutex.Unlock()
	if ws.seen[videoID] {
		return false
	}
	ws.seen[videoID] = true
	return true
}

// enqueue 将任务加入下载队列，队列满时用协程避免阻塞 HTTP 处理
func (ws *WebServer) enqueue(task *TaskStatus) {
	select {
	case ws.queue <- task:
	default:
		go func() { ws.queue <- task }()
	}
}

func (ws *WebServer) getWSURL() string {
	ws.wsMutex.RLock()
	defer ws.wsMutex.RUnlock()
	return ws.wsURL
}

// refreshWSURL 在 Chrome 连接失效时重新建立 CDP 连接
func (ws *WebServer) refreshWSURL() (string, error) {
	newURL, err := chrome.EnsureChromeConnection(ws.chromeRemoteURL, chrome.StartupCDPTimeout)
	if err != nil {
		return "", err
	}
	ws.wsMutex.Lock()
	ws.wsURL = newURL
	ws.wsMutex.Unlock()
	log.Printf("Chrome CDP connection refreshed: %s", newURL)
	return newURL, nil
}

// --- HTTP 路由 ---

// SetupRoutes sets up the HTTP routes for the web server
func (ws *WebServer) SetupRoutes() error {
	http.HandleFunc("/api/tasks", ws.handleTasks)
	http.HandleFunc("/api/tasks/", ws.handleTaskByID)
	http.HandleFunc("/api/submit", ws.handleSubmit)
	http.HandleFunc("/api/status/", ws.handleStatus)
	http.HandleFunc("/api/progress", ws.handleProgress)

	// 静态资源：优先使用内嵌文件系统，未提供时回退到本地 ./web 目录（开发场景）
	var staticFS fs.FS
	if ws.embedFS != nil {
		sub, err := fs.Sub(ws.embedFS, "web")
		if err != nil {
			return fmt.Errorf("build embedded web fs: %w", err)
		}
		staticFS = sub
	} else {
		staticFS = os.DirFS("./web")
	}
	http.Handle("/", http.FileServer(http.FS(staticFS)))
	return nil
}

func writeJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func (ws *WebServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(ws.snapshotTasks())
	case "POST":
		var taskReq struct {
			VideoID string `json:"video_id"`
			ListID  string `json:"list_id"`
			Type    string `json:"type"`
			Mode    string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		task := ws.createAndDispatchTask(taskReq.VideoID, taskReq.ListID, taskReq.Type, taskReq.Mode)
		if task == nil {
			http.Error(w, "video_id or list_id is required", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(task)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (ws *WebServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	id := r.URL.Path[len("/api/tasks/"):]

	task, ok := ws.snapshotTask(id)
	if !ok {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(task)
}

func (ws *WebServer) handleSubmit(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		VideoID string `json:"video_id"`
		ListID  string `json:"list_id"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task := ws.createAndDispatchTask(req.VideoID, req.ListID, req.Type, req.Mode)
	if task == nil {
		http.Error(w, "video_id or list_id is required", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

// createAndDispatchTask 创建任务、加入任务表并推入下载队列
func (ws *WebServer) createAndDispatchTask(videoID, listID, typeField, modeField string) *TaskStatus {
	taskType := modeField
	if taskType == "" {
		taskType = typeField
	}
	if taskType == "" {
		if listID != "" {
			taskType = "list"
		} else {
			taskType = "single"
		}
	}

	// 归一化标识：list 任务允许 video_id 承载列表 ID
	if taskType == "list" && listID == "" {
		listID = videoID
	}
	if videoID == "" {
		videoID = listID
	}
	if videoID == "" && listID == "" {
		return nil
	}

	task := &TaskStatus{
		ID:        fmt.Sprintf("%s_%d", videoID, time.Now().UnixNano()),
		Type:      taskType,
		VideoID:   videoID,
		ListID:    listID,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	ws.AddTask(task)
	ws.enqueue(task)
	log.Printf("Task queued: id=%s type=%s video=%s list=%s", task.ID, task.Type, task.VideoID, task.ListID)
	return task
}

func (ws *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	id := r.URL.Path[len("/api/status/"):]

	task, ok := ws.snapshotTask(id)
	if !ok {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(task)
}

func (ws *WebServer) handleProgress(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	json.NewEncoder(w).Encode(ws.snapshotTasks())
}

// --- 下载流水线 ---

// startWorkers 启动下载 worker 池
func (ws *WebServer) startWorkers() {
	for i := 0; i < ws.maxWorkers; i++ {
		go ws.worker(i)
	}
	log.Printf("Started %d download worker(s)", ws.maxWorkers)
}

func (ws *WebServer) worker(id int) {
	for task := range ws.queue {
		ws.runTaskSafely(id, task)
	}
}

func (ws *WebServer) runTaskSafely(workerID int, task *TaskStatus) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Worker %d] recovered from panic: %v", workerID, r)
			ws.mutateTask(task, func(t *TaskStatus) {
				t.Status = "failed"
				t.ErrorMessage = fmt.Sprintf("internal error: %v", r)
			})
		}
	}()
	ws.processTask(workerID, task)
}

func (ws *WebServer) processTask(workerID int, task *TaskStatus) {
	if task.Type == "list" {
		ws.processListTask(workerID, task)
		return
	}
	ws.processSingleTask(workerID, task)
}

func (ws *WebServer) processListTask(workerID int, task *TaskStatus) {
	listID := task.ListID
	if listID == "" {
		listID = task.VideoID
	}

	ws.mutateTask(task, func(t *TaskStatus) { t.Status = "downloading" })
	log.Printf("[Worker %d] Fetching playlist: %s", workerID, listID)

	videos, err := ws.getPlaylist(listID)
	if err != nil {
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Status = "failed"
			t.ErrorMessage = "获取播放列表失败: " + err.Error()
		})
		log.Printf("[Worker %d] Playlist %s failed: %v", workerID, listID, err)
		return
	}

	count := 0
	for _, vid := range videos {
		if !ws.markSeen(vid) {
			continue
		}
		child := &TaskStatus{
			ID:        fmt.Sprintf("%s_%d", vid, time.Now().UnixNano()),
			Type:      "single",
			VideoID:   vid,
			ListID:    listID,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		ws.AddTask(child)
		ws.enqueue(child)
		count++
	}

	now := time.Now()
	ws.mutateTask(task, func(t *TaskStatus) {
		t.Status = "completed"
		t.Progress = 100
		t.Title = fmt.Sprintf("播放列表 %s（%d 个视频，新增 %d 个任务）", listID, len(videos), count)
		t.CompletedAt = &now
	})
	log.Printf("[Worker %d] Playlist %s expanded: %d videos, %d new tasks", workerID, listID, len(videos), count)
}

func (ws *WebServer) processSingleTask(workerID int, task *TaskStatus) {
	ws.mutateTask(task, func(t *TaskStatus) { t.Status = "downloading" })

	meta, err := ws.resolveVideoInfo(task.VideoID, task.ListID)
	if err != nil {
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Status = "failed"
			t.ErrorMessage = "解析视频信息失败: " + err.Error()
		})
		log.Printf("[Worker %d] Resolve %s failed: %v", workerID, task.VideoID, err)
		return
	}

	ws.mutateTask(task, func(t *TaskStatus) { t.Title = meta.Title })
	log.Printf("[Worker %d] Downloading: %s", workerID, meta.Title)

	// 下载封面图（失败不阻断视频下载）
	if meta.ImageURL != "" && meta.ImageFilePath != "" {
		if _, e := os.Stat(meta.ImageFilePath); os.IsNotExist(e) {
			if e := ws.downloader.DownloadWithRetry(meta.ImageURL, meta.ImageFilePath); e != nil {
				log.Printf("[Worker %d] Image download failed for %s: %v", workerID, task.VideoID, e)
			}
		}
	}

	// 下载视频
	if meta.DataURL == "" || meta.VideoFilePath == "" {
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Status = "failed"
			t.ErrorMessage = "未找到视频下载地址"
		})
		return
	}

	if _, e := os.Stat(meta.VideoFilePath); e == nil {
		// 文件已存在，直接标记完成
		ws.finishTask(task)
		log.Printf("[Worker %d] Video already exists: %s", workerID, meta.VideoFilePath)
		return
	}

	progressCB := func(downloaded, total int64) {
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Downloaded = downloaded
			if total > 0 {
				t.TotalSize = total
				t.Progress = int(float64(downloaded) / float64(total) * 100)
			}
		})
	}

	dlErr := ws.downloader.DownloadWithRetryCB(meta.DataURL, meta.VideoFilePath, progressCB)
	if dlErr != nil {
		// 下载地址可能已过期，刷新后重试一次
		log.Printf("[Worker %d] Download failed, refreshing data URL for %s", workerID, task.VideoID)
		if newURL, rerr := ws.refreshDataURL(task.VideoID); rerr == nil && newURL != "" {
			dlErr = ws.downloader.DownloadWithRetryCB(newURL, meta.VideoFilePath, progressCB)
		}
	}

	if dlErr != nil {
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Status = "failed"
			t.ErrorMessage = "视频下载失败: " + dlErr.Error()
		})
		log.Printf("[Worker %d] Video download failed for %s: %v", workerID, task.VideoID, dlErr)
		return
	}

	ws.finishTask(task)
	if ws.clearCache {
		os.Remove(filepath.Join(ws.cacheDir, fmt.Sprintf("info_%s.json", task.VideoID)))
	}
	log.Printf("[Worker %d] Completed: %s", workerID, meta.Title)
}

func (ws *WebServer) finishTask(task *TaskStatus) {
	now := time.Now()
	ws.mutateTask(task, func(t *TaskStatus) {
		t.Status = "completed"
		t.Progress = 100
		t.CompletedAt = &now
	})
}

// resolveVideoInfo 解析视频信息，连接失效时自动重连一次
func (ws *WebServer) resolveVideoInfo(videoID, listID string) (scraper.VideoMetadata, error) {
	meta, err := ws.scraper.ResolveVideoInfo(ws.getWSURL(), videoID, listID)
	if err != nil {
		if u, rerr := ws.refreshWSURL(); rerr == nil {
			meta, err = ws.scraper.ResolveVideoInfo(u, videoID, listID)
		}
	}
	return meta, err
}

// getPlaylist 获取播放列表，连接失效时自动重连一次
func (ws *WebServer) getPlaylist(listID string) ([]string, error) {
	list, err := ws.scraper.GetPlaylist(ws.getWSURL(), listID)
	if err != nil {
		if u, rerr := ws.refreshWSURL(); rerr == nil {
			list, err = ws.scraper.GetPlaylist(u, listID)
		}
	}
	return list, err
}

// refreshDataURL 刷新视频下载地址，连接失效时自动重连一次
func (ws *WebServer) refreshDataURL(videoID string) (string, error) {
	u, err := ws.scraper.RefreshVideoDataURL(ws.getWSURL(), videoID)
	if err != nil {
		if wu, rerr := ws.refreshWSURL(); rerr == nil {
			u, err = ws.scraper.RefreshVideoDataURL(wu, videoID)
		}
	}
	return u, err
}

// --- 生命周期 ---

// Start starts the web server
func (ws *WebServer) Start(addr string) error {
	if err := ws.SetupRoutes(); err != nil {
		return err
	}

	ws.server = &http.Server{
		Addr:        addr,
		ReadTimeout: 30 * time.Second,
		// 注意：不设置 WriteTimeout，实际下载在后台 worker 中进行，
		// HTTP 处理仅返回 JSON，无需长写超时。
	}

	displayAddr := addr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "localhost" + displayAddr
	}

	log.Printf("Web server starting on %s", addr)
	log.Printf("Visit http://%s to access the web interface", displayAddr)
	log.Printf("Press Ctrl+C to stop the server")

	return ws.server.ListenAndServe()
}

// Stop stops the web server
func (ws *WebServer) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ws.server.Shutdown(ctx)
}

// preloadConfigTasks 把 config.yaml 里配置的单视频/播放列表预加载为待下载任务。
// 已缓存的列表（list_*.json 存在）不触发网络抓取，直接复用；网络列表与单视频
// 由后台 worker 异步解析，不会阻塞页面启动。
func (ws *WebServer) preloadConfigTasks(singleCodes, listCodes []string) {
	count := 0
	for _, id := range singleCodes {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if ws.createAndDispatchTask(id, "", "single", "") != nil {
			count++
		}
	}
	for _, id := range listCodes {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if ws.createAndDispatchTask("", id, "list", "") != nil {
			count++
		}
	}
	if count > 0 {
		log.Printf("Preloaded %d task(s) from config (single=%d, list=%d)",
			count, len(singleCodes), len(listCodes))
	}
}

// RunWebServer starts the web interface server together with the download pipeline
func RunWebServer(opts Options) {
	if opts.CacheDir == "" {
		opts.CacheDir = "./cache"
	}
	if opts.DownDir == "" {
		opts.DownDir = "./download"
	}
	os.MkdirAll(opts.CacheDir, 0755)
	os.MkdirAll(opts.DownDir, 0755)

	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 3
	}

	log.Printf("Web server mode enabled")
	log.Printf("Configuration: CacheDir=%s, DownDir=%s, Workers=%d, Resolution=%s",
		opts.CacheDir, opts.DownDir, opts.MaxWorkers, opts.VideoResolution)

	webSrv := NewWebServer(opts)
	webSrv.startWorkers()

	// 预加载 config.yaml 里配置的任务，让打开页面即可见"要下载的列表"
	webSrv.preloadConfigTasks(opts.SingleCode, opts.ListCode)

	if err := webSrv.Start(opts.Addr); err != nil {
		log.Fatalf("Web server failed: %v", err)
	}
}
