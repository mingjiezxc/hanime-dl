// Package downloader 提供支持断点续传、进度回调与按路径串行化的 HTTP 文件下载能力。
//
// 下载流程约定：
//   - 数据先写入 "<目标路径>.tmp"，字节完整校验通过、Sync 落盘后才 Rename 为成品；
//   - 任何失败路径都保留 .tmp，以便下一次调用断点续传；
//   - 同一目标路径的下载在包内按路径互斥，避免并发写坏同一个临时文件。
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxRetries 默认最大尝试次数（含首次）。可被 Downloader.MaxRetries 覆盖。
	MaxRetries = 5
	// RetryInterval 默认两次尝试之间的退避间隔。可被 Downloader.RetryInterval 覆盖。
	RetryInterval = 5 * time.Second

	// DefaultDialTimeout 建立 TCP 连接的超时。
	DefaultDialTimeout = 30 * time.Second
	// DefaultTLSHandshakeTimeout TLS 握手超时。
	DefaultTLSHandshakeTimeout = 30 * time.Second
	// DefaultResponseHeaderTimeout 等待响应头的超时。
	DefaultResponseHeaderTimeout = 30 * time.Second
	// DefaultIdleTimeout 响应体「长时间没有任何新字节到达」的空闲读超时。
	DefaultIdleTimeout = 30 * time.Second
)

// UnknownSize 表示响应总长度未知（例如服务端未返回 Content-Length 的分块传输）。
// 进度回调收到该值时应呈现「不确定」状态，而不是把它当成 0% 或某个具体百分比。
const UnknownSize int64 = -1

var (
	// ErrIdleTimeout 表示响应体在空闲超时时间内没有任何新字节到达，连接被主动中止。
	ErrIdleTimeout = errors.New("download stalled: no data received within idle timeout")
	// ErrIncomplete 表示服务端提前结束响应，实际落盘字节数少于声明的长度。
	ErrIncomplete = errors.New("incomplete download: fewer bytes received than declared")
	// ErrRangeMismatch 表示 206 响应的内容区间与请求的续传偏移不一致，续传结果不可信。
	ErrRangeMismatch = errors.New("server returned a content range that does not match the requested offset")
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

// ProgressCallback 下载进度回调函数。
//
// total <= 0（即 UnknownSize）表示总长度未知；percent 语义请使用 Percent 计算，
// 它会把结果钳制到 [0,100] 并在总长未知时返回 ok=false。
type ProgressCallback func(downloaded, total int64)

// Percent 计算下载百分比，结果钳制到 [0,100]。
//
// total <= 0（UnknownSize）表示总长未知，此时返回 ok=false，调用方应呈现不确定进度，
// 而不是显示 0%。downloaded 超过 total（例如服务端给出的长度偏小）时钳制为 100。
func Percent(downloaded, total int64) (percent int, ok bool) {
	if total <= 0 {
		return 0, false
	}
	if downloaded < 0 {
		downloaded = 0
	}
	p := downloaded * 100 / total
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return int(p), true
}

// ProgressWriter 进度追踪器。
//
// Downloaded 可能被 io.Copy 所在协程写入、被进度打印协程与回调读取，故使用原子计数。
type ProgressWriter struct {
	Total      int64
	Downloaded atomic.Int64
	FileName   string
	Callback   ProgressCallback
	lastCB     time.Time
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	downloaded := pw.Downloaded.Add(int64(n))
	// 限流回调，避免频繁加锁（最多每 500ms 一次）
	if pw.Callback != nil {
		now := time.Now()
		if now.Sub(pw.lastCB) >= 500*time.Millisecond {
			pw.lastCB = now
			pw.Callback(downloaded, pw.Total)
		}
	}
	return n, nil
}

// pathLock 是单个路径的互斥量及其引用计数。
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// keyedMutex 是按 key 串行化的互斥量集合；引用归零后条目会被回收，避免无界增长。
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*pathLock)}
}

// Lock 获取 key 的互斥量，返回释放函数（必须被调用，且必须由同一协程调用）。
func (km *keyedMutex) Lock(key string) func() {
	km.mu.Lock()
	l, ok := km.locks[key]
	if !ok {
		l = &pathLock{}
		km.locks[key] = l
	}
	l.refs++
	km.mu.Unlock()

	l.mu.Lock()

	return func() {
		l.mu.Unlock()
		km.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(km.locks, key)
		}
		km.mu.Unlock()
	}
}

// Downloader 文件下载器。请通过 NewDownloader / NewDownloaderWithClient 构造。
type Downloader struct {
	proxyURL string
	client   *http.Client

	// MaxRetries 最大尝试次数（含首次）；<=0 时使用包级默认值 MaxRetries。
	MaxRetries int
	// RetryInterval 两次尝试之间的退避间隔；<=0 时使用包级默认值 RetryInterval。
	RetryInterval time.Duration
	// IdleTimeout 响应体无字节增长的空闲读超时；<=0 表示不启用空闲超时。
	IdleTimeout time.Duration

	initOnce sync.Once
	locks    *keyedMutex
}

// NewDownloader 创建新的下载器。
func NewDownloader(proxyURL string) *Downloader {
	return newDownloader(proxyURL, nil)
}

// NewDownloaderWithClient 使用调用方提供的 *http.Client 创建下载器（client 为 nil 时按
// proxyURL 构造默认 client）。注入 client 便于测试与自定义传输层；注入的 client 会被复用，
// 从而共享连接池。请勿设置 client.Timeout 为总超时——本包使用分段超时 + 空闲读超时。
func NewDownloaderWithClient(proxyURL string, client *http.Client) *Downloader {
	return newDownloader(proxyURL, client)
}

func newDownloader(proxyURL string, client *http.Client) *Downloader {
	d := &Downloader{
		proxyURL:      proxyURL,
		client:        client,
		MaxRetries:    MaxRetries,
		RetryInterval: RetryInterval,
		IdleTimeout:   DefaultIdleTimeout,
		locks:         newKeyedMutex(),
	}
	return d
}

// newHTTPClient 按代理配置构造共享 client。
//
// 不使用 client.Timeout（总超时无法覆盖「读 body 全程不可取消」的问题），改为
// 连接/握手/响应头各自超时，响应体的停滞由空闲读超时负责。
func newHTTPClient(proxyURL string) *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   DefaultDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   DefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: DefaultResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}
	if proxyURL != "" {
		if pURL, err := url.Parse(proxyURL); err == nil && pURL.Host != "" {
			tr.Proxy = http.ProxyURL(pURL)
			log.Printf("Using proxy: %s", proxyURL)
		} else {
			log.Printf("Invalid proxy URL: %s, ignoring.", proxyURL)
		}
	}
	return &http.Client{Transport: tr}
}

// init 懒初始化，使零值 Downloader 也能安全工作。
func (d *Downloader) init() {
	d.initOnce.Do(func() {
		if d.client == nil {
			d.client = newHTTPClient(d.proxyURL)
		}
		if d.locks == nil {
			d.locks = newKeyedMutex()
		}
	})
}

func (d *Downloader) maxRetries() int {
	if d.MaxRetries > 0 {
		return d.MaxRetries
	}
	return MaxRetries
}

func (d *Downloader) retryInterval() time.Duration {
	if d.RetryInterval > 0 {
		return d.RetryInterval
	}
	return RetryInterval
}

// DownloadFile 下载文件（支持断点续传）。
//
// 同一 filePath 的并发调用会被串行化：进入时获取按路径互斥量并持有到本次下载结束。
func (d *Downloader) DownloadFile(urlStr, filePath string) error {
	d.init()
	unlock := d.locks.Lock(filePath)
	defer unlock()
	return d.downloadFile(urlStr, filePath, nil)
}

// DownloadFileCB 下载文件并回调进度（支持断点续传）。
func (d *Downloader) DownloadFileCB(urlStr, filePath string, cb ProgressCallback) error {
	d.init()
	unlock := d.locks.Lock(filePath)
	defer unlock()
	return d.downloadFile(urlStr, filePath, cb)
}

// downloadFile 执行一次下载尝试。
//
// 约束：调用方必须已持有 filePath 的按路径互斥量（downloadFile 自身不加锁）。
func (d *Downloader) downloadFile(urlStr, filePath string, cb ProgressCallback) error {
	tempFilePath := filePath + ".tmp"

	// 确保目标文件的父目录存在，否则创建临时文件会失败
	if dir := filepath.Dir(tempFilePath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// 检查是否已存在临时文件，决定是否续传
	var startOffset int64
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

	// 单一明确的关闭点：closeFile 幂等，defer 仅作为提前返回时的兜底。
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	defer func() { _ = closeFile() }()

	// 请求上下文：空闲读超时通过取消它来中止「卡住」的 body 读取。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
		log.Printf("Resuming download for %s from byte %d", filePath, startOffset)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusRequestedRangeNotSatisfiable:
		return &HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: urlStr}
	case http.StatusOK, http.StatusPartialContent:
	default:
		return &HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: urlStr}
	}

	if startOffset > 0 && resp.StatusCode == http.StatusOK {
		log.Printf("Server does not support resume (got 200 OK), restarting download.")
		if err := truncateAndRewind(file); err != nil {
			return err
		}
		startOffset = 0
	}

	// 206 必须从请求的偏移开始，否则续传结果不可信（可能把全量内容拼到已有前缀后面）。
	if startOffset > 0 && resp.StatusCode == http.StatusPartialContent {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if rangeStart, ok := contentRangeStart(cr); ok && rangeStart != startOffset {
				return fmt.Errorf("%w: requested bytes=%d-, got %q for %s", ErrRangeMismatch, startOffset, cr, urlStr)
			}
		}
	}

	// 进度追踪：Content-Length 缺失时显式标记为「未知」，而不是拼出一个假的长度。
	totalSize := UnknownSize
	if resp.ContentLength >= 0 {
		totalSize = resp.ContentLength + startOffset
	}

	pw := &ProgressWriter{
		Total:    totalSize,
		FileName: filepath.Base(filePath),
		Callback: cb,
	}
	pw.Downloaded.Store(startOffset)

	// 进度打印协程
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-ticker.C:
				downloaded := pw.Downloaded.Load()
				if percent, ok := Percent(downloaded, pw.Total); ok {
					log.Printf("[%s] Download Progress: %d%% (%d/%d bytes)", pw.FileName, percent, downloaded, pw.Total)
				} else {
					log.Printf("[%s] Download Progress: %d bytes (total unknown)", pw.FileName, downloaded)
				}
			case <-done:
				return
			}
		}
	}()

	// 空闲读超时：单次 Read 长时间无字节返回时取消请求上下文。
	var timedOut atomic.Bool
	var body io.Reader = resp.Body
	var idle *idleTimeoutReader
	if d.IdleTimeout > 0 {
		idle = newIdleTimeoutReader(resp.Body, d.IdleTimeout, func() {
			timedOut.Store(true)
			cancel()
		})
		body = idle
	}

	// 下载
	mw := io.MultiWriter(file, pw)
	n, copyErr := io.Copy(mw, body)
	if idle != nil {
		idle.Stop()
	}

	// P0：落盘完整性校验。服务端声明了长度却提前结束（io.Copy 可能返回 nil）时，
	// 必须保留 .tmp 并报错，绝不 Rename 成成品。
	if copyErr != nil {
		if timedOut.Load() {
			return fmt.Errorf("failed to download %s: %w", filePath, ErrIdleTimeout)
		}
		if resp.ContentLength > 0 && n+startOffset != totalSize {
			return fmt.Errorf("%w: %s (declared %d bytes, received %d, offset %d, copy error: %v)",
				ErrIncomplete, filePath, resp.ContentLength, n, startOffset, copyErr)
		}
		return fmt.Errorf("failed to write to file: %w", copyErr)
	}
	if resp.ContentLength > 0 && n+startOffset != totalSize {
		return fmt.Errorf("%w: %s (declared %d bytes, received %d, offset %d)",
			ErrIncomplete, filePath, resp.ContentLength, n, startOffset)
	}

	// 只有完整落盘后才算成功
	downloaded := pw.Downloaded.Load()
	log.Printf("[%s] Downloaded %d bytes (Total: %d)", pw.FileName, n, downloaded)

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file %s: %w", tempFilePath, err)
	}
	if err := closeFile(); err != nil {
		return fmt.Errorf("failed to close temp file %s: %w", tempFilePath, err)
	}

	if err := os.Rename(tempFilePath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// 下载完成，回调最终进度
	if cb != nil {
		cb(downloaded, pw.Total)
	}

	return nil
}

// DownloadWithRetry 带重试的下载。
//
// 同一 filePath 的并发调用会被串行化（互斥量覆盖整个重试周期，含 416/区间不一致后的重置）。
func (d *Downloader) DownloadWithRetry(urlStr, filePath string) error {
	d.init()
	unlock := d.locks.Lock(filePath)
	defer unlock()
	return d.downloadWithRetry(urlStr, filePath, nil)
}

// DownloadWithRetryCB 带重试的下载并回调进度
func (d *Downloader) DownloadWithRetryCB(urlStr, filePath string, cb ProgressCallback) error {
	d.init()
	unlock := d.locks.Lock(filePath)
	defer unlock()
	return d.downloadWithRetry(urlStr, filePath, cb)
}

// downloadWithRetry 在给定路径的互斥量保护下重试下载。
func (d *Downloader) downloadWithRetry(urlStr, filePath string, cb ProgressCallback) error {
	maxRetries := d.maxRetries()
	var lastErr error

	// 临时文件已不可信（416 / 续传区间不一致）时删除它并重置进度。
	resetAndRestart := func() {
		if rmErr := resetTempFile(filePath); rmErr != nil {
			log.Printf("Failed to remove temp file for %s: %v", filePath, rmErr)
		} else {
			log.Printf("Deleted temp file for %s, restarting download", filePath)
		}
	}

	for i := 0; i < maxRetries; i++ {
		err := d.downloadFile(urlStr, filePath, cb)
		if err == nil {
			log.Printf("Successfully downloaded %s to %s", urlStr, filePath)
			return nil
		}
		lastErr = err
		log.Printf("Failed to download %s (attempt %d/%d): %v", urlStr, i+1, maxRetries, err)

		// 按状态码分类处理（errors.As 取类型化错误，不再靠字符串匹配）
		var httpErr *HTTPStatusError
		if errors.As(err, &httpErr) {
			switch httpErr.StatusCode {
			case http.StatusGone:
				// 410 Gone：资源已永久失效，快速失败，不做无意义的重试与退避。
				return fmt.Errorf("failed to download %s: %w", urlStr, err)
			case http.StatusRequestedRangeNotSatisfiable:
				// 416：临时文件不可信，删除后重置进度并立即重试。
				resetAndRestart()
				continue
			}
		}

		// 续传区间与服务端返回不一致：同样重启下载。
		if errors.Is(err, ErrRangeMismatch) {
			resetAndRestart()
			continue
		}

		if i < maxRetries-1 {
			time.Sleep(d.retryInterval())
		}
	}

	if lastErr == nil {
		lastErr = errors.New("download failed")
	}
	return fmt.Errorf("failed to download %s after %d attempts: %w", urlStr, maxRetries, lastErr)
}

// truncateAndRewind 把临时文件清空并回到起点（服务端不支持 Range 时重启下载）。
func truncateAndRewind(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("failed to truncate temp file: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek temp file: %w", err)
	}
	return nil
}

// resetTempFile 删除指定目标的临时文件。
func resetTempFile(filePath string) error {
	err := os.Remove(filePath + ".tmp")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// contentRangeStart 解析 "bytes <start>-<end>/<total>" 形式的 Content-Range，取出 start。
func contentRangeStart(v string) (int64, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes "))
	dash := strings.IndexByte(v, '-')
	if dash <= 0 {
		return 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(v[:dash]), 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

// idleTimeoutReader 在「单次 Read 长时间没有返回任何字节」时触发 onTimeout。
//
// 每次读取前重置定时器：只要有字节流动就不会触发，从而不对慢速但健康的连接误判。
type idleTimeoutReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

func newIdleTimeoutReader(r io.Reader, idle time.Duration, onTimeout func()) *idleTimeoutReader {
	return &idleTimeoutReader{
		r:     r,
		idle:  idle,
		timer: time.AfterFunc(idle, onTimeout),
	}
}

func (ir *idleTimeoutReader) Read(p []byte) (int, error) {
	if ir.timer != nil {
		ir.timer.Reset(ir.idle)
	}
	n, err := ir.r.Read(p)
	if err != nil && ir.timer != nil {
		ir.timer.Stop()
	}
	return n, err
}

// Stop 停止空闲超时定时器。
func (ir *idleTimeoutReader) Stop() {
	if ir.timer != nil {
		ir.timer.Stop()
	}
}
