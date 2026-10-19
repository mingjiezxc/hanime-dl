package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hanime-dl/chrome"
	"hanime-dl/config"
	"hanime-dl/downloader"
	"hanime-dl/internal/idutil"
	"hanime-dl/scraper"
)

const (
	// TaskEnqueueTimeout 限制 HTTP handler 在「队列已满」时最多等待多久。
	//
	// 这是背压的取舍点：不设上限会让请求无限期挂住（客户端与服务器都不知情），
	// 用协程把任务丢进队列则会派生不可回收的 goroutine。这里给一次有界等待，
	// 超时就返回 503，让调用方明确感知并自行重试。
	TaskEnqueueTimeout = 5 * time.Second
	// shutdownTimeout 优雅停机的宽限时间：等待 HTTP 连接与 in-flight 任务收尾。
	shutdownTimeout = 5 * time.Second
	// taskQueueSize 单个视频任务的队列长度。
	taskQueueSize = 1000
	// listQueueSize 播放列表展开任务的队列长度。
	listQueueSize = 64
	// seenWarnStep 每认领这么多个唯一视频就打一条提示日志，让长期驻留部署能观察到
	// seen 集合的增长。
	seenWarnStep = 10000
	// maxSubmitBodyBytes 限制 /api/tasks、/api/submit 请求体大小。
	//
	// 请求体是 {"video_id":"...","list_id":"..."} 这样的小对象，合法输入远小于 1 MiB。
	// 不设上限时 json.Decoder 会把整个 body 读进内存，而这个服务默认无鉴权、
	// 默认绑定 :8080（整个局域网可达），一个大 body 就是一次内存压力攻击。
	maxSubmitBodyBytes = 1 << 20
)

// 流水线错误。handler 通过 statusForError 把它们映射为 HTTP 状态码。
var (
	// ErrMissingTarget video_id 与 list_id 同时为空。
	ErrMissingTarget = errors.New("video_id or list_id is required")
	// ErrInvalidTarget video_id / list_id 形状不合法。
	//
	// 这不是「输入不好看」的问题：这两个 ID 会被拼进磁盘路径
	// （cache/info_<id>.json、cache/list_<id>.json），未校验的 ID 经 filepath.Join
	// 的 Clean 之后可以逃出缓存目录，把缓存写入与清理变成任意路径写/删。
	// 详见 internal/idutil 的包注释。这里刻意复用同一个哨兵，
	// 使 errors.Is(err, idutil.ErrInvalidID) 与 errors.Is(err, ErrInvalidTarget) 等价。
	ErrInvalidTarget = idutil.ErrInvalidID
	// ErrDuplicateVideo 该视频已被认领（在队列中或已成功处理过）。
	ErrDuplicateVideo = errors.New("video already queued")
	// ErrQueueFull 在入队预算内队列始终没有空位（显式背压）。
	ErrQueueFull = errors.New("download queue is full, please retry later")
	// ErrServerStopped 服务器正在停机，不再接受新任务。
	ErrServerStopped = errors.New("web server is stopping")
	// ErrEnqueueCanceled 等待入队期间请求被取消（通常是客户端断开）。
	ErrEnqueueCanceled = errors.New("request canceled while waiting for queue space")
	// ErrUnauthorized 请求缺少或携带了错误的 API token。
	ErrUnauthorized = errors.New("missing or invalid API token")
)

// videoResolver 抽象视频信息与播放列表的解析能力。
//
// 方法签名与 *scraper.Scraper 的公开方法逐字对应（下载侧由 task-1 冻结签名），
// 因此下方的编译期断言能在 alice/carol 改动 scraper 签名时立刻报错。
type videoResolver interface {
	ResolveVideoInfo(wsURL, videoID, listID string) (scraper.VideoMetadata, error)
	GetPlaylist(wsURL, listID string) ([]string, error)
	RefreshVideoDataURL(wsURL, videoID string) (string, error)
}

// fileDownloader 抽象下载器（*downloader.Downloader 天然满足该接口）。
type fileDownloader interface {
	DownloadWithRetry(url, path string) error
	DownloadWithRetryCB(url, path string, cb downloader.ProgressCallback) error
}

// 编译期断言：具体类型必须满足窄接口。若 scraper/downloader 的公开签名发生漂移，
// 这里会立刻编译失败，而不是等到测试才暴露。
var (
	_ videoResolver  = (*scraper.Scraper)(nil)
	_ fileDownloader = (*downloader.Downloader)(nil)
)

// TaskStatus represents the status of a download task
type TaskStatus struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"` // "single" or "list"
	VideoID       string     `json:"video_id"`
	ListID        string     `json:"list_id,omitempty"`
	Status        string     `json:"status"` // "pending", "downloading", "completed", "failed"
	Title         string     `json:"title,omitempty"`
	Progress      int        `json:"progress"` // 0-100
	ProgressKnown bool       `json:"progress_known"`
	TotalSize     int64      `json:"total_size,omitempty"`
	Downloaded    int64      `json:"downloaded,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// Options 用于初始化 Web 服务器的配置
type Options struct {
	Addr            string
	CacheDir        string
	DownDir         string
	MaxWorkers      int
	ChromeRemoteURL string
	WSURL           string
	HTTPProxy       string
	VideoResolution string
	ClearCache      bool
	// SingleCode / ListCode 来自 config.yaml，启动时预加载为待下载任务，
	// 让页面打开即可看到"要下载的列表"。
	SingleCode []string
	ListCode   []string
	// EmbedFS 内嵌的静态资源文件系统（来自 //go:embed web/*），
	// 为 nil 时回退到本地 ./web 目录（开发场景）。
	EmbedFS fs.FS
	// Ctx 外部生命周期上下文（通常是 signal.NotifyContext 的产物）。
	// 它被取消时，服务器会走优雅停机：停止接受请求、唤醒阻塞的入队、结束 worker。
	// 为 nil 时表示不响应外部停机信号。
	Ctx context.Context
	// APIToken 非空时，所有 /api/* 请求必须携带 `Authorization: Bearer <token>`
	// （常数时间比较）。为空表示不启用鉴权——本机单人使用的默认形态。
	//
	// 为什么要有它：这个服务能按请求写盘与删文件，默认又绑定 :8080（整个局域网可达），
	// 裸暴露等于把文件系统的一个角落交给同网段的任何设备。
	APIToken string
	// APIAllowOrigin 需要跨源访问 API 时的显式来源（例如 "http://localhost:3000"）。
	// 为空表示**不设** Access-Control-Allow-Origin——即默认同源策略。
	//
	// 旧实现对每个响应无条件写 `*`，于是用户浏览器里访问的任意网页都能对本服务
	// 发起跨源 POST（提交下载任务、读取任务列表）。默认关闭跨源是更安全的缺省。
	APIAllowOrigin string
	// ChromeInsecureTLS 允许 CDP 端点使用自签名证书（跳过证书校验）。默认 false。
	//
	// 旧实现无条件 InsecureSkipVerify: true，对所有 https CDP 探测关闭证书校验，
	// 中间人可任意伪造「Chrome 调试端点」。现在这变成一个显式的、默认关闭的开关。
	ChromeInsecureTLS bool
}

// WebServer manages the web interface and the download pipeline
type WebServer struct {
	server *http.Server
	mux    *http.ServeMux

	tasks      map[string]*TaskStatus
	tasksMutex sync.RWMutex

	// resolver / downloader 持有窄接口而不是具体类型：测试可以注入纯内存假实现，
	// 从而完全不启动 Chrome、不访问外网。生产路径由 NewWebServer 装配具体实现。
	resolver   videoResolver
	downloader fileDownloader

	cacheDir        string
	downDir         string
	chromeRemoteURL string
	clearCache      bool

	wsURL   string
	wsMutex sync.RWMutex

	// connectChrome 获取可用的 CDP WebSocket 地址（默认 chrome.EnsureChromeConnection）。
	//
	// 之所以要做成可注入的函数字段：resolver 报错时 refreshWSURL 会调用它，而
	// chrome.EnsureChromeConnection 在找不到远端端点时会**自动拉起本地 Chrome**。
	// 测试必须能证明「绝不启动浏览器」，因此需要一个注入点把这条路径变成必然失败；
	// 生产路径仍装配 chrome.EnsureChromeConnection，行为与之前完全一致。
	connectChrome func(remoteURL string, timeout time.Duration) (string, error)

	// ctx 是服务器生命周期上下文：Stop 时取消，用于唤醒阻塞式入队与结束 worker 循环。
	ctx    context.Context
	cancel context.CancelFunc

	// queue 承载单个视频任务，由 maxWorkers 个 worker 消费。
	queue chan *TaskStatus
	// listQueue 承载播放列表展开任务，由独立的 expander 消费。
	//
	// 为什么与 queue 分开：展开列表需要把子任务写回任务队列，如果队列已满就会阻塞在
	// 发送上；若展开逻辑跑在 worker 里，全部 worker 同时卡在发送上时就没有任何协程
	// 消费队列 —— 直接死锁。把生产者（expander）与消费者（worker）分离后，expander
	// 的阻塞只是等待 worker 排空队列，进度始终向前。
	listQueue  chan *TaskStatus
	maxWorkers int
	workers    sync.WaitGroup

	// seq 生成进程内唯一、单调递增的任务序号。
	seq atomic.Uint64

	embedFS fs.FS

	// apiToken / apiAllowOrigin 来自 Options，见各自的注释。
	// apiToken 为空表示不启用鉴权；apiAllowOrigin 为空表示不设 CORS 头。
	apiToken       string
	apiAllowOrigin string

	// seen 记录本进程已认领（已入队或已展开）的视频 ID。
	//
	// 语义：认领是**进程生命周期**的，不随任务完成释放 —— 这是「重复展开同一列表
	// 保持幂等」的前提（第二次展开必须得到 0 个新子任务）。所以它的规模上界是本次
	// 运行出现过的唯一视频 ID 数量：单条记录是一个 ID 字符串加 map 开销（约
	// 50-100 字节），10^4 个视频约 1MB，可接受。每 seenWarnStep 条打一条提示日志，
	// 让长期驻留部署能观察到增长；若将来需要数天不重启地运行，应换成带 TTL 的 LRU
	// 或落盘索引。
	seen      map[string]struct{}
	seenMutex sync.Mutex

	stopOnce sync.Once
	stopErr  error
}

// NewWebServer creates a new web server instance.
//
// opts 按指针传入：Options 有 200+ 字节（gocritic hugeParam / performance 标签），
// 且本函数只读取它，不修改调用方的结构体。
func NewWebServer(opts *Options) *WebServer {
	if opts == nil {
		opts = &Options{}
	}
	maxWorkers := opts.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = config.DefaultMaxDownloadWorkers
	}
	parent := opts.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	// connectChrome 默认与历史行为一致（chrome.EnsureChromeConnection）。
	// 只有显式打开 ChromeInsecureTLS 时才换成带跳过校验的探测——并且仍然保持
	// 字段签名不变，测试里的 connectChrome 注入点不受影响。
	connect := chrome.EnsureChromeConnection
	if opts.ChromeInsecureTLS {
		connect = func(remoteURL string, timeout time.Duration) (string, error) {
			return chrome.EnsureChromeConnectionWith(remoteURL, timeout, chrome.ProbeOptions{
				InsecureSkipVerify: true,
			})
		}
	}

	return &WebServer{
		tasks:           make(map[string]*TaskStatus),
		resolver:        scraper.NewScraper(opts.CacheDir, opts.DownDir, opts.VideoResolution),
		downloader:      downloader.NewDownloader(opts.HTTPProxy),
		cacheDir:        opts.CacheDir,
		downDir:         opts.DownDir,
		chromeRemoteURL: opts.ChromeRemoteURL,
		clearCache:      opts.ClearCache,
		wsURL:           opts.WSURL,
		connectChrome:   connect,
		ctx:             ctx,
		cancel:          cancel,
		queue:           make(chan *TaskStatus, taskQueueSize),
		listQueue:       make(chan *TaskStatus, listQueueSize),
		maxWorkers:      maxWorkers,
		embedFS:         opts.EmbedFS,
		apiToken:        opts.APIToken,
		apiAllowOrigin:  opts.APIAllowOrigin,
		seen:            make(map[string]struct{}),
	}
}

// AddTask adds a new task to the server
func (ws *WebServer) AddTask(task *TaskStatus) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	ws.tasks[task.ID] = task
}

// untrackTask 从任务表移除任务（仅用于入队失败时回滚）。
func (ws *WebServer) untrackTask(id string) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	delete(ws.tasks, id)
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

// markSeen 认领一个视频 ID，返回 true 表示本次调用是首次认领。
//
// 同时作用于列表子任务与直接提交的单视频任务：旧实现只在展开列表时调用，因此同一
// 个视频重复提交会产生两个任务、并发写同一个 .tmp（审计 P1-3）。
func (ws *WebServer) markSeen(videoID string) bool {
	ws.seenMutex.Lock()
	defer ws.seenMutex.Unlock()
	if _, ok := ws.seen[videoID]; ok {
		return false
	}
	ws.seen[videoID] = struct{}{}
	if n := len(ws.seen); n%seenWarnStep == 0 {
		log.Printf("note: %d distinct videos claimed in this process; the claim set is kept for the process lifetime to keep list re-expansion idempotent", n)
	}
	return true
}

// unmarkSeen 释放一次认领（仅用于「已认领但最终没能入队」的回滚）。
func (ws *WebServer) unmarkSeen(videoID string) {
	ws.seenMutex.Lock()
	defer ws.seenMutex.Unlock()
	delete(ws.seen, videoID)
}

// enqueue 以显式背压把任务送入队列。
//
// 取舍：旧实现在队列满时执行 go func() { ch <- task }()，会派生永久阻塞的 goroutine
// （无上限增长、Stop 时也不退出，若将来 close(queue) 更会 panic）。这里改为
// 「可取消的阻塞式入队」：
//   - 先做一次非阻塞尝试，队列有空位立即返回（绝大多数情况）；
//   - 队列满则在 ctx 上等待，服务器停机（ws.ctx）→ ErrServerStopped；
//     等待预算耗尽（HTTP handler 传入带超时的 ctx）→ ErrQueueFull，由 handler
//     返回 503 让调用方感知，而不是无限期挂住连接；
//   - 请求被取消（客户端断开）→ ErrEnqueueCanceled。
//
// 内部调用方（列表展开、配置预加载）传服务器生命周期 ctx，因此享有真正的背压：
// 队列满时它们等待而不是丢任务或派生 goroutine。
func (ws *WebServer) enqueue(ctx context.Context, task *TaskStatus) error {
	ch := ws.queue
	if task.Type == "list" {
		ch = ws.listQueue
	}

	// 快路径：有空间就立刻入队，避免 ctx 已过期时把本可成功的情况判成失败。
	select {
	case ch <- task:
		return nil
	default:
	}

	select {
	case ch <- task:
		return nil
	case <-ws.ctx.Done():
		return ErrServerStopped
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrQueueFull
		}
		return ErrEnqueueCanceled
	}
}

func (ws *WebServer) getWSURL() string {
	ws.wsMutex.RLock()
	defer ws.wsMutex.RUnlock()
	return ws.wsURL
}

// refreshWSURL 在 Chrome 连接失效时重新建立 CDP 连接
func (ws *WebServer) refreshWSURL() (string, error) {
	connect := ws.connectChrome
	if connect == nil {
		connect = chrome.EnsureChromeConnection
	}
	newURL, err := connect(ws.chromeRemoteURL, chrome.StartupCDPTimeout)
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

// SetupRoutes 构建一个**局部** mux 并记录到 ws.mux，供 Start 与测试共用。
//
// 旧实现用包级 http.HandleFunc（http.DefaultServeMux）：第二次调用即 panic
// （重复注册），且任何测试都无法起一个隔离的 server。改为 http.NewServeMux() 后
// SetupRoutes 可重复调用，测试也可以直接 httptest.NewServer(ws.Handler())。
func (ws *WebServer) SetupRoutes() error {
	mux := http.NewServeMux()
	// API 路由统一过 token 守卫（apiToken 为空时守卫是直通，行为与历史一致）。
	// 静态资源不在守卫内：面板 HTML 本身不含敏感数据，加了守卫页面就无法加载。
	mux.Handle("/api/tasks", ws.requireToken(http.HandlerFunc(ws.handleTasks)))
	mux.Handle("/api/tasks/", ws.requireToken(http.HandlerFunc(ws.handleTaskByID)))
	mux.Handle("/api/submit", ws.requireToken(http.HandlerFunc(ws.handleSubmit)))
	mux.Handle("/api/status/", ws.requireToken(http.HandlerFunc(ws.handleStatus)))
	mux.Handle("/api/progress", ws.requireToken(http.HandlerFunc(ws.handleProgress)))

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
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	ws.mux = mux
	return nil
}

// Handler 返回路由 handler（需先调用 SetupRoutes），供测试起隔离 server。
func (ws *WebServer) Handler() http.Handler {
	return ws.mux
}

// writeJSONHeaders 写响应头。
//
// CORS 只在显式配置了 APIAllowOrigin 时才设置：旧实现对每个响应无条件写
// `Access-Control-Allow-Origin: *`，等于允许用户浏览器里任意网页对本服务发起
// 跨源读写（提交任务、读取任务列表）。默认回到同源策略。
func (ws *WebServer) writeJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if ws.apiAllowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", ws.apiAllowOrigin)
	}
}

// writeJSON 写 JSON 响应，记录编码失败。
//
// 编码失败时响应头已经写出，无法再改状态码，只能记日志（审计 P2-8 的 errcheck 项）。
func (ws *WebServer) writeJSON(w http.ResponseWriter, status int, v any) {
	ws.writeJSONHeaders(w)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("Failed to encode JSON response: %v", err)
	}
}

// apiError 统一错误响应体。
//
// 同时提供 error 与 message 两个键：error 是 API 契约里的规范字段；message 是
// web/index.html 既有前端读取的字段 —— 它已经按 JSON 解析错误体，而旧实现先设
// application/json 再用 http.Error 改写为 text/plain，导致前端 response.json()
// 直接抛错，用户只能看到「请求失败：Unexpected token...」。保持两个键就不必改前端。
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeJSONError 统一错误响应为 JSON，并保留正确状态码。
func (ws *WebServer) writeJSONError(w http.ResponseWriter, status int, msg string) {
	ws.writeJSONHeaders(w)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(apiError{Error: msg, Message: msg}); err != nil {
		log.Printf("Failed to encode JSON error response: %v", err)
	}
}

// statusForError 把流水线错误映射为 HTTP 状态码。
func statusForError(err error) int {
	switch {
	case errors.Is(err, ErrMissingTarget):
		return http.StatusBadRequest
	case errors.Is(err, ErrInvalidTarget):
		return http.StatusBadRequest
	case errors.Is(err, ErrDuplicateVideo):
		return http.StatusConflict
	case errors.Is(err, ErrQueueFull), errors.Is(err, ErrServerStopped), errors.Is(err, ErrEnqueueCanceled):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// requireToken 在 apiToken 非空时守卫 /api/* 路由；未启用时原样返回内层 handler。
//
// 比较走 crypto/subtle 的常数时间路径，避免「按前缀匹配成功即提前返回」
// 这类可被计时侧信道利用的比较方式。静态资源不受该守卫影响。
func (ws *WebServer) requireToken(next http.Handler) http.Handler {
	if ws.apiToken == "" {
		return next
	}
	want := []byte(ws.apiToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(provided), want) != 1 {
			ws.writeJSONError(w, statusForError(ErrUnauthorized), ErrUnauthorized.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken 取 Authorization 头里的 Bearer token。
func bearerToken(r *http.Request) (string, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(auth[len(prefix):]), true
}

// submitRequest 是 /api/tasks 与 /api/submit 的请求体。
type submitRequest struct {
	VideoID string `json:"video_id"`
	ListID  string `json:"list_id"`
	Type    string `json:"type"`
	Mode    string `json:"mode"`
}

// dispatchRequest 解析请求体并按背压提交任务，返回给 handler 的 (task, err)。
// ctx 用于限制入队等待时间。
func (ws *WebServer) dispatchRequest(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：合法 body 是几十字节的小对象，超过 maxSubmitBodyBytes 一律拒绝。
	// MaxBytesReader 在超限时让 Decode 返回错误，从而复用下面这条已有的 400 路径。
	r.Body = http.MaxBytesReader(w, r.Body, maxSubmitBodyBytes)

	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ws.writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TaskEnqueueTimeout)
	defer cancel()

	task, err := ws.createAndDispatchTask(ctx, req.VideoID, req.ListID, req.Type, req.Mode)
	if err != nil {
		ws.writeJSONError(w, statusForError(err), err.Error())
		return
	}
	ws.writeJSON(w, http.StatusCreated, task)
}

func (ws *WebServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ws.writeJSON(w, http.StatusOK, ws.snapshotTasks())
	case http.MethodPost:
		ws.dispatchRequest(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		ws.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method)
	}
}

func (ws *WebServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		ws.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")

	task, ok := ws.snapshotTask(id)
	if !ok {
		ws.writeJSONError(w, http.StatusNotFound, "task not found: "+id)
		return
	}
	ws.writeJSON(w, http.StatusOK, task)
}

func (ws *WebServer) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		ws.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method)
		return
	}
	ws.dispatchRequest(w, r)
}

// createAndDispatchTask 创建任务、加入任务表并按背压推入队列。
//
// 返回 (task, nil) 表示已入队；其余情况 task 为 nil，err 为 ErrMissingTarget /
// ErrInvalidTarget / ErrDuplicateVideo / ErrQueueFull / ErrServerStopped /
// ErrEnqueueCanceled，由 handler 用 statusForError 映射状态码。
// ctx 只约束入队等待，不参与任务执行。
func (ws *WebServer) createAndDispatchTask(ctx context.Context, videoID, listID, typeField, modeField string) (*TaskStatus, error) {
	videoID = strings.TrimSpace(videoID)
	listID = strings.TrimSpace(listID)

	// 先判空再归一化：归一化会把两个字段互相回填，若放在归一化之后判断则恒为假
	// （审计 P2-6 的死判断）。
	if videoID == "" && listID == "" {
		return nil, ErrMissingTarget
	}

	// 形状校验必须在**认领与建任务之前**：这两个 ID 之后会被拼进磁盘路径
	// （cache/info_<id>.json、cache/list_<id>.json），未校验的值可以逃出缓存目录，
	// 把「写缓存」与「清缓存」变成任意路径写/删。放在 markSeen 之前还保证
	// 恶意输入不会占用任何视频的认领名额。
	if videoID != "" {
		if err := idutil.Validate(videoID); err != nil {
			return nil, err
		}
	}
	if listID != "" {
		if err := idutil.Validate(listID); err != nil {
			return nil, err
		}
	}

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

	// 单视频任务按 video_id 认领，重复提交不再产生第二个任务。列表任务**不**在
	// 这一步去重：重复展开本身必须是幂等的（子任务按 video_id 认领），第二次展开
	// 应返回 0 个新任务而不是报错。
	claim := taskType != "list"
	if claim && !ws.markSeen(videoID) {
		return nil, ErrDuplicateVideo
	}

	task := &TaskStatus{
		ID:        ws.nextTaskID(videoID),
		Type:      taskType,
		VideoID:   videoID,
		ListID:    listID,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	ws.AddTask(task)
	if err := ws.enqueue(ctx, task); err != nil {
		// 回滚：既不算已入队，也不该占用 video_id 的认领名额。
		ws.untrackTask(task.ID)
		if claim {
			ws.unmarkSeen(videoID)
		}
		return nil, err
	}
	log.Printf("Task queued: id=%s type=%s video=%s list=%s", task.ID, task.Type, task.VideoID, task.ListID)
	return task, nil
}

// nextTaskID 生成进程内唯一且可读的任务 ID。
//
// 旧实现是 videoID + 纳秒时间戳：同一纳秒内的并发提交（或系统时间回拨）会撞出相同
// ID，后写入者直接覆盖 tasks 映射里的前一个任务（审计 P2-7）。改用单调自增序号后
// 碰撞不可能发生，同时保留「ID 里带视频 ID」的可读性。
func (ws *WebServer) nextTaskID(videoID string) string {
	return fmt.Sprintf("%s_%d", videoID, ws.seq.Add(1))
}

func (ws *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		ws.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/status/")

	task, ok := ws.snapshotTask(id)
	if !ok {
		ws.writeJSONError(w, http.StatusNotFound, "task not found: "+id)
		return
	}
	ws.writeJSON(w, http.StatusOK, task)
}

func (ws *WebServer) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		ws.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method)
		return
	}
	ws.writeJSON(w, http.StatusOK, ws.snapshotTasks())
}

// --- 下载流水线 ---

// startWorkers 启动下载 worker 池与播放列表展开器。
func (ws *WebServer) startWorkers() {
	for i := 0; i < ws.maxWorkers; i++ {
		ws.workers.Add(1)
		go ws.worker(i)
	}
	ws.workers.Add(1)
	go ws.expander()
	log.Printf("Started %d download worker(s) plus 1 playlist expander", ws.maxWorkers)
}

// worker 消费单个视频任务。
//
// 循环以 ws.ctx 为出口，因此 Stop() 能结束 worker（旧实现的 for range queue
// 只在队列被 close 时才退出，而队列从未被 close，worker 只能随进程消失）。
func (ws *WebServer) worker(id int) {
	defer ws.workers.Done()
	who := fmt.Sprintf("Worker %d", id)
	for ws.ctx.Err() == nil {
		select {
		case <-ws.ctx.Done():
			return
		case task := <-ws.queue:
			ws.runTaskSafely(who, task)
		}
	}
}

// expander 串行展开播放列表任务（见 WebServer.listQueue 的说明）。
func (ws *WebServer) expander() {
	defer ws.workers.Done()
	for ws.ctx.Err() == nil {
		select {
		case <-ws.ctx.Done():
			return
		case task := <-ws.listQueue:
			ws.runTaskSafely("Expander", task)
		}
	}
}

// runTaskSafely 隔离单个任务的 panic：recover 后把任务标记为 failed，
// 保证 worker 协程存活并继续处理后续任务。
func (ws *WebServer) runTaskSafely(who string, task *TaskStatus) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] recovered from panic: %v", who, r)
			ws.markTaskFailed(task, fmt.Sprintf("internal error: %v", r))
		}
	}()
	ws.processTask(who, task)
}

// markTaskFailed 记录失败，并在**单视频任务**上释放认领名额。
//
// 为什么必须释放：seen 集合的语义是「本进程已认领」（见 WebServer.seen 注释），
// 它存在的理由是让**已成功**的视频不被重复下载、让列表重展开保持幂等。
// 但旧实现只在「入队失败回滚」时调用 unmarkSeen，任务真正失败后认领永不释放，
// 于是失败的视频在进程剩余生命周期内再提交一律 409 —— 用户唯一的出路是重启进程，
// 而重启又会丢掉全部任务状态。失败应当可以立即重试，成功才需要长期去重。
//
// 列表任务不做释放：列表本身从不被认领（claim = taskType != "list"），而它的
// VideoID 经归一化后等于 listID，若照此释放，恰好与某个已认领视频同名的列表
// 就会误删那个视频的认领。区分点只有任务类型，因此集中在这里判一次。
//
// 集中成一个方法而不是在各条失败出口各写一遍：漏一处的代价是「某个失败原因
// 恰好无法重试」，而这种不对称很难从日志里看出来。
func (ws *WebServer) markTaskFailed(task *TaskStatus, msg string) {
	ws.mutateTask(task, func(t *TaskStatus) {
		t.Status = "failed"
		t.ErrorMessage = msg
	})
	if task.Type != "list" {
		ws.unmarkSeen(task.VideoID)
	}
}

func (ws *WebServer) processTask(who string, task *TaskStatus) {
	if task.Type == "list" {
		ws.processListTask(who, task)
		return
	}
	ws.processSingleTask(who, task)
}

func (ws *WebServer) processListTask(who string, task *TaskStatus) {
	listID := task.ListID
	if listID == "" {
		listID = task.VideoID
	}

	ws.mutateTask(task, func(t *TaskStatus) { t.Status = "downloading" })
	log.Printf("[%s] Fetching playlist: %s", who, listID)

	videos, err := ws.getPlaylist(listID)
	if err != nil {
		ws.markTaskFailed(task, "获取播放列表失败: "+err.Error())
		log.Printf("[%s] Playlist %s failed: %v", who, listID, err)
		return
	}

	count := 0
	for _, vid := range videos {
		// 按 video_id 去重：同一列表里的重复项、以及已被单视频任务认领过的视频
		// 都不会产生第二个子任务（P1-6 的列表去重）。
		if !ws.markSeen(vid) {
			continue
		}
		child := &TaskStatus{
			ID:        ws.nextTaskID(vid),
			Type:      "single",
			VideoID:   vid,
			ListID:    listID,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		ws.AddTask(child)
		// 内部调用传服务器生命周期 ctx：队列满时阻塞等待（真背压），停机时放弃剩余子任务。
		if err := ws.enqueue(ws.ctx, child); err != nil {
			ws.untrackTask(child.ID)
			ws.unmarkSeen(vid)
			log.Printf("[%s] Playlist %s expansion stopped after %d of %d videos: %v", who, listID, count, len(videos), err)
			ws.markTaskFailed(task, fmt.Sprintf("展开播放列表时入队失败（已入队 %d/%d）: %v", count, len(videos), err))
			return
		}
		count++
	}

	now := time.Now()
	ws.mutateTask(task, func(t *TaskStatus) {
		t.Status = "completed"
		t.Progress = 100
		t.Title = fmt.Sprintf("播放列表 %s（%d 个视频，新增 %d 个任务）", listID, len(videos), count)
		t.CompletedAt = &now
	})
	log.Printf("[%s] Playlist %s expanded: %d videos, %d new tasks", who, listID, len(videos), count)
}

func (ws *WebServer) processSingleTask(who string, task *TaskStatus) {
	ws.mutateTask(task, func(t *TaskStatus) { t.Status = "downloading" })

	meta, err := ws.resolveVideoInfo(task.VideoID, task.ListID)
	if err != nil {
		ws.markTaskFailed(task, "解析视频信息失败: "+err.Error())
		log.Printf("[%s] Resolve %s failed: %v", who, task.VideoID, err)
		return
	}

	ws.mutateTask(task, func(t *TaskStatus) { t.Title = meta.Title })
	log.Printf("[%s] Downloading: %s", who, meta.Title)

	// 下载封面图（失败不阻断视频下载）
	if meta.ImageURL != "" && meta.ImageFilePath != "" {
		if !fileExists(meta.ImageFilePath) {
			if e := ws.downloader.DownloadWithRetry(meta.ImageURL, meta.ImageFilePath); e != nil {
				log.Printf("[%s] Image download failed for %s: %v", who, task.VideoID, e)
			}
		}
	}

	// 下载视频
	if meta.DataURL == "" || meta.VideoFilePath == "" {
		ws.markTaskFailed(task, "未找到视频下载地址")
		return
	}

	if fileExists(meta.VideoFilePath) {
		// 文件已存在，直接标记完成
		ws.finishTask(task)
		log.Printf("[%s] Video already exists: %s", who, meta.VideoFilePath)
		return
	}

	progressCB := func(downloaded, total int64) {
		percent, known := downloader.Percent(downloaded, total)
		ws.mutateTask(task, func(t *TaskStatus) {
			t.Downloaded = downloaded
			t.ProgressKnown = known
			if known {
				t.TotalSize = total
				t.Progress = percent
			}
			// total <= 0（总长未知，例如 chunked 响应）时故意不设置 Progress：
			// 由 ProgressKnown=false 表达「不确定进度」，而不是显示成 0% 卡死
			// （审计 P0-3 的下游契约）。
		})
	}

	dlErr := ws.downloader.DownloadWithRetryCB(meta.DataURL, meta.VideoFilePath, progressCB)
	if dlErr != nil && ws.ctx.Err() == nil {
		// 下载地址可能已过期，刷新后重试一次；停机中不再发起新的下载。
		log.Printf("[%s] Download failed, refreshing data URL for %s", who, task.VideoID)
		if newURL, rerr := ws.refreshDataURL(task.VideoID); rerr == nil && newURL != "" {
			dlErr = ws.downloader.DownloadWithRetryCB(newURL, meta.VideoFilePath, progressCB)
		}
	}

	if dlErr != nil {
		ws.markTaskFailed(task, "视频下载失败: "+dlErr.Error())
		log.Printf("[%s] Video download failed for %s: %v", who, task.VideoID, dlErr)
		return
	}

	ws.finishTask(task)
	if ws.clearCache {
		cacheFile := filepath.Join(ws.cacheDir, fmt.Sprintf("info_%s.json", task.VideoID))
		if err := os.Remove(cacheFile); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[%s] Failed to clear cache for %s: %v", who, task.VideoID, err)
			}
		}
	}
	log.Printf("[%s] Completed: %s", who, meta.Title)
}

// fileExists 报告 path 是否已存在且不是目录。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
//
// ctx 现状：scraper 的公开方法目前不接受 context，因此这里无法把请求/任务级 ctx
// 传进真正的网络调用（分阶段方案见任务报告）。本文件能做到的是把「是否继续下一步」
// 用 ws.ctx 约束 —— 停机后不再发起刷新重试，避免停机时又起一轮长任务。
func (ws *WebServer) resolveVideoInfo(videoID, listID string) (scraper.VideoMetadata, error) {
	meta, err := ws.resolver.ResolveVideoInfo(ws.getWSURL(), videoID, listID)
	if err != nil && ws.ctx.Err() == nil {
		if u, rerr := ws.refreshWSURL(); rerr == nil {
			meta, err = ws.resolver.ResolveVideoInfo(u, videoID, listID)
		}
	}
	return meta, err
}

// getPlaylist 获取播放列表，连接失效时自动重连一次
func (ws *WebServer) getPlaylist(listID string) ([]string, error) {
	list, err := ws.resolver.GetPlaylist(ws.getWSURL(), listID)
	if err != nil && ws.ctx.Err() == nil {
		if u, rerr := ws.refreshWSURL(); rerr == nil {
			list, err = ws.resolver.GetPlaylist(u, listID)
		}
	}
	return list, err
}

// refreshDataURL 刷新视频下载地址，连接失效时自动重连一次
func (ws *WebServer) refreshDataURL(videoID string) (string, error) {
	u, err := ws.resolver.RefreshVideoDataURL(ws.getWSURL(), videoID)
	if err != nil && ws.ctx.Err() == nil {
		if wu, rerr := ws.refreshWSURL(); rerr == nil {
			u, err = ws.resolver.RefreshVideoDataURL(wu, videoID)
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
		Handler:     ws.mux,
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

// Stop 优雅停机：取消生命周期上下文（唤醒阻塞的入队、结束 worker 循环）、停止
// HTTP 服务、等待 worker 收尾。
//
// 幂等：可在 signal goroutine 与 RunWebServer 的返回路径重复调用。
func (ws *WebServer) Stop() error {
	ws.stopOnce.Do(func() {
		log.Println("Stopping web server...")
		ws.cancel()
		if ws.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			ws.stopErr = ws.server.Shutdown(ctx)
		}
		if !ws.waitWorkers(shutdownTimeout) {
			log.Printf("Some workers did not exit within %s; queued tasks are abandoned (in-flight ones were allowed to finish)", shutdownTimeout)
		}
		log.Println("Web server stopped.")
	})
	return ws.stopErr
}

// waitWorkers 等待 worker 结束，最多等 timeout。
func (ws *WebServer) waitWorkers(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		ws.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		// 该 goroutine 会在 worker 最终退出时自然结束，不会永久泄漏。
		return false
	}
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
		if _, err := ws.createAndDispatchTask(ws.ctx, id, "", "single", ""); err != nil {
			log.Printf("Failed to preload single task %s: %v", id, err)
			continue
		}
		count++
	}
	for _, id := range listCodes {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := ws.createAndDispatchTask(ws.ctx, "", id, "list", ""); err != nil {
			log.Printf("Failed to preload list task %s: %v", id, err)
			continue
		}
		count++
	}
	if count > 0 {
		log.Printf("Preloaded %d task(s) from config (single=%d, list=%d)",
			count, len(singleCodes), len(listCodes))
	}
}

// RunWebServer starts the web interface server together with the download pipeline.
//
// 返回 nil 表示走的是优雅停机路径（signal 触发 Stop → ListenAndServe 返回
// http.ErrServerClosed），其余情况返回服务错误。
//
// opts 按指针传入（Options 体积大，gocritic hugeParam）；函数内部先复制一份再补
// 默认值，因此**不会**修改调用方的结构体。
func RunWebServer(opts *Options) error {
	if opts == nil {
		return errors.New("web: Options must not be nil")
	}
	resolved := *opts
	if resolved.CacheDir == "" {
		resolved.CacheDir = config.DefaultCacheDir
	}
	// 默认下载目录只有 config 包一个权威来源，避免与 config.DefaultDownDir 漂移
	// （旧实现这里写死 ./download，与配置侧的 ./downloads 不一致）。
	if resolved.DownDir == "" {
		resolved.DownDir = config.DefaultDownDir
	}
	if err := os.MkdirAll(resolved.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %s: %w", resolved.CacheDir, err)
	}
	if err := os.MkdirAll(resolved.DownDir, 0o755); err != nil {
		return fmt.Errorf("create download dir %s: %w", resolved.DownDir, err)
	}

	if resolved.MaxWorkers <= 0 {
		resolved.MaxWorkers = config.DefaultMaxDownloadWorkers
	}

	log.Printf("Web server mode enabled")
	log.Printf("Configuration: CacheDir=%s, DownDir=%s, Workers=%d, Resolution=%s",
		resolved.CacheDir, resolved.DownDir, resolved.MaxWorkers, resolved.VideoResolution)

	webSrv := NewWebServer(&resolved)
	webSrv.startWorkers()

	// 预加载 config.yaml 里配置的任务，让打开页面即可见"要下载的列表"
	webSrv.preloadConfigTasks(resolved.SingleCode, resolved.ListCode)

	// 把外部停机信号接到优雅停机：resolved.Ctx 取消 → 停止接受请求、唤醒阻塞的入队、
	// 结束 worker。旧实现的 Stop() 没有任何调用点，Ctrl+C 实际只会杀进程。
	if resolved.Ctx != nil {
		go func() {
			<-resolved.Ctx.Done()
			log.Printf("Shutdown signal received, stopping web server gracefully...")
			if err := webSrv.Stop(); err != nil {
				log.Printf("Graceful shutdown returned an error: %v", err)
			}
		}()
	}

	err := webSrv.Start(resolved.Addr)
	if errors.Is(err, http.ErrServerClosed) {
		// 停机信号路径：ListenAndServe 在 Shutdown 一开始就返回 ErrServerClosed，
		// 此时 Stop() 可能还在等 worker 收尾。再调用一次 Stop() —— stopOnce 会让
		// 并发/重复调用阻塞到第一次执行结束 —— 保证函数返回前优雅停机已彻底完成。
		if serr := webSrv.Stop(); serr != nil {
			log.Printf("Graceful shutdown returned an error: %v", serr)
		}
		return nil
	}
	return err
}
