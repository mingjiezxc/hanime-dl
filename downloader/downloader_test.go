package downloader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// 下载器会大量 log.Printf，测试输出保持干净；失败信息仍由 t.Errorf/t.Fatalf 给出。
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// newTestDownloader 构造一个测试用下载器：默认 client（无代理、无总超时）、毫秒级重试间隔。
func newTestDownloader(t *testing.T, client *http.Client) *Downloader {
	t.Helper()
	if client == nil {
		client = &http.Client{Transport: &http.Transport{}}
	}
	d := NewDownloaderWithClient("", client)
	d.RetryInterval = time.Millisecond
	d.IdleTimeout = 5 * time.Second
	return d
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file %s should not exist (stat err=%v)", path, err)
	}
}

func assertExists(t *testing.T, path string, size int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file %s should exist: %v", path, err)
	}
	if size >= 0 && fi.Size() != int64(size) {
		t.Fatalf("file %s size = %d, want %d", path, fi.Size(), size)
	}
}

func testBody(n int) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	return body
}

// --- 基础：200 正常下载 ---------------------------------------------------

func TestDownloadFileSuccess(t *testing.T) {
	body := testBody(5000)

	var ranges atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)

	var lastDownloaded, lastTotal int64
	var calls atomic.Int64
	if err := d.DownloadFileCB(srv.URL, path, func(downloaded, total int64) {
		calls.Add(1)
		lastDownloaded, lastTotal = downloaded, total
	}); err != nil {
		t.Fatalf("DownloadFileCB: %v", err)
	}

	if got := readFile(t, path); !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes", len(got), len(body))
	}
	assertNoFile(t, path+".tmp")
	if ranges.Load() != 0 {
		t.Fatalf("fresh download must not send Range, got %d such requests", ranges.Load())
	}
	if calls.Load() == 0 {
		t.Fatal("progress callback was never invoked")
	}
	if lastDownloaded != int64(len(body)) || lastTotal != int64(len(body)) {
		t.Fatalf("final progress = (%d, %d), want (%d, %d)", lastDownloaded, lastTotal, len(body), len(body))
	}
	if percent, ok := Percent(lastDownloaded, lastTotal); !ok || percent != 100 {
		t.Fatalf("Percent(%d,%d) = (%d,%v), want (100,true)", lastDownloaded, lastTotal, percent, ok)
	}
}

// --- 断点续传：206 -------------------------------------------------------

func TestDownloadFileResume206(t *testing.T) {
	body := testBody(36)
	const half = 18

	var requests atomic.Int64
	var rangeHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		rangeHeader.Store(r.Header.Get("Range"))
		http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path+".tmp", body[:half], 0644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t, nil)
	if err := d.DownloadFile(srv.URL, path); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}

	if got := readFile(t, path); !bytes.Equal(got, body) {
		t.Fatalf("resumed content mismatch: got %q, want %q", got, body)
	}
	assertNoFile(t, path+".tmp")
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
	if got, _ := rangeHeader.Load().(string); got != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("Range header = %q, want %q", got, fmt.Sprintf("bytes=%d-", half))
	}
}

// --- 服务端不支持 Range（对 Range 请求回 200 全量） ------------------------

func TestDownloadFileServerIgnoresRange(t *testing.T) {
	body := testBody(4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 无论是否带 Range 都回 200 + 全量
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path+".tmp", body[:1000], 0644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t, nil)
	if err := d.DownloadFile(srv.URL, path); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}

	got := readFile(t, path)
	if !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes (duplicated resume?)", len(got), len(body))
	}
	assertNoFile(t, path+".tmp")
}

// --- P0-1：服务端提前断流（真实 HTTP 传输） --------------------------------

func TestDownloadFileTruncatedBody(t *testing.T) {
	body := testBody(8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:len(body)/2])
		// 直接返回：声明了 8192 字节却只写了 4096
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)
	d.MaxRetries = 2

	err := d.DownloadWithRetry(srv.URL, path)
	if err == nil {
		t.Fatal("expected error for truncated body, got nil")
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v, want errors.Is(err, ErrIncomplete)", err)
	}

	// 核心断言：残缺内容绝不能被改名成成品
	assertNoFile(t, path)
	// .tmp 保留，供后续续传
	assertExists(t, path+".tmp", -1)
}

// --- P0-1：io.Copy 返回 nil 但字节不足时的字节数校验（直接命中审计描述） -----

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadFileShortBodyWithNilCopyError(t *testing.T) {
	const declared = 1000
	body := testBody(100)

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// 模拟「连接正常结束、io.Copy 返回 nil，但字节数少于 Content-Length」的响应
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			ContentLength: declared,
			Body:          io.NopCloser(bytes.NewReader(body)),
			Request:       r,
		}, nil
	})}

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, client)

	err := d.DownloadFile("http://example.invalid/video.mp4", path)
	if err == nil {
		t.Fatal("expected error for short body, got nil")
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v, want errors.Is(err, ErrIncomplete)", err)
	}
	if !strings.Contains(err.Error(), "declared 1000") {
		t.Fatalf("error should mention declared length, got: %v", err)
	}

	assertNoFile(t, path)
	assertExists(t, path+".tmp", len(body))
}

// --- 进度语义：无 Content-Length（chunked） -------------------------------

func chunkedHandler(t *testing.T, body []byte, chunk int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 强制分块传输：客户端看到 ContentLength == -1
		}
		for i := 0; i < len(body); i += chunk {
			end := i + chunk
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[i:end])
		}
	}
}

func TestDownloadFileUnknownLengthProgress(t *testing.T) {
	body := testBody(4096)
	srv := httptest.NewServer(chunkedHandler(t, body, 512))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)

	var lastDownloaded, lastTotal int64
	if err := d.DownloadFileCB(srv.URL, path, func(downloaded, total int64) {
		lastDownloaded, lastTotal = downloaded, total
	}); err != nil {
		t.Fatalf("DownloadFileCB: %v", err)
	}

	if !bytes.Equal(readFile(t, path), body) {
		t.Fatal("content mismatch for chunked response")
	}
	if lastTotal != UnknownSize {
		t.Fatalf("total = %d, want UnknownSize(%d)", lastTotal, UnknownSize)
	}
	if lastDownloaded != int64(len(body)) {
		t.Fatalf("downloaded = %d, want %d", lastDownloaded, len(body))
	}
	// 下游必须能识别「未知」而不是 0% 卡死
	if percent, ok := Percent(lastDownloaded, lastTotal); ok {
		t.Fatalf("Percent with unknown total must report ok=false, got (%d,true)", percent)
	}
}

// 分块传输 + 已有部分 .tmp（服务端仍回 206）：总长必须为「未知」，
// 不能是 -1 + startOffset 拼出来的假长度（旧实现会得到 startOffset-1，百分比 >100%）。
func TestDownloadFileUnknownLengthWithResumeOffset(t *testing.T) {
	body := testBody(2048)
	const half = 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
			return
		}
		start, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"), 10, 64)
		if err != nil || start < 0 || start > int64(len(body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 206 且不带 Content-Length -> 分块
		}
		_, _ = w.Write(body[start:])
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path+".tmp", body[:half], 0644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t, nil)
	var lastTotal int64
	if err := d.DownloadFileCB(srv.URL, path, func(downloaded, total int64) {
		lastTotal = total
	}); err != nil {
		t.Fatalf("DownloadFileCB: %v", err)
	}

	if !bytes.Equal(readFile(t, path), body) {
		t.Fatal("resumed content mismatch")
	}
	if lastTotal != UnknownSize {
		t.Fatalf("total = %d, want UnknownSize(%d) (旧实现会得到 %d)", lastTotal, UnknownSize, half-1)
	}
}

// --- 416 → 删除 .tmp 后重试成功 -------------------------------------------

func TestDownloadWithRetry416RemovesTempAndRetries(t *testing.T) {
	body := testBody(3000)

	var requests, rangeRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Range") != "" {
			rangeRequests.Add(1)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	// 预置一个「过期/不合法」的部分文件，使首次尝试带上 Range 并收到 416
	if err := os.WriteFile(path+".tmp", []byte("stale-partial-data"), 0644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t, nil)
	if err := d.DownloadWithRetry(srv.URL, path); err != nil {
		t.Fatalf("DownloadWithRetry: %v", err)
	}

	if got := readFile(t, path); !bytes.Equal(got, body) {
		t.Fatalf("content mismatch after 416 retry: got %d bytes, want %d bytes", len(got), len(body))
	}
	assertNoFile(t, path+".tmp")
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (416 后应删除 .tmp 并立即重试一次)", requests.Load())
	}
	if rangeRequests.Load() != 1 {
		t.Fatalf("range requests = %d, want 1", rangeRequests.Load())
	}
}

// --- 410 → 快速失败，不空转 -------------------------------------------------

func TestDownloadWithRetry410FailsFast(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)
	d.MaxRetries = 5
	d.RetryInterval = 50 * time.Millisecond

	start := time.Now()
	err := d.DownloadWithRetry(srv.URL, path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for 410, got nil")
	}
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusGone {
		t.Fatalf("error = %v, want wrapped HTTPStatusError{410}", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (410 必须快速失败，不得空转 %d 轮)", requests.Load(), d.MaxRetries)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("410 took %v, expected fast failure", elapsed)
	}
	assertNoFile(t, path)
}

// --- 重试次数与退避 --------------------------------------------------------

func TestDownloadWithRetryRetriesThenSucceeds(t *testing.T) {
	body := testBody(2048)
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)
	d.MaxRetries = 4

	if err := d.DownloadWithRetry(srv.URL, path); err != nil {
		t.Fatalf("DownloadWithRetry: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (2 次失败后成功)", attempts.Load())
	}
	if !bytes.Equal(readFile(t, path), body) {
		t.Fatal("content mismatch after retries")
	}
}

func TestDownloadWithRetryExhaustsAndBacksOff(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	const interval = 30 * time.Millisecond
	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)
	d.MaxRetries = 3
	d.RetryInterval = interval

	start := time.Now()
	err := d.DownloadWithRetry(srv.URL, path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if attempts.Load() != int64(d.MaxRetries) {
		t.Fatalf("attempts = %d, want %d", attempts.Load(), d.MaxRetries)
	}
	// 3 次尝试之间应发生 2 次退避
	if minElapsed := 2 * interval; elapsed < minElapsed {
		t.Fatalf("elapsed = %v, want >= %v (退避未生效)", elapsed, minElapsed)
	}
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("final error should wrap the last HTTPStatusError, got %v", err)
	}
	assertNoFile(t, path)
}

// --- P1-6：同一路径并发下载不得损坏文件 ------------------------------------

func TestDownloadConcurrentSamePath(t *testing.T) {
	body := testBody(64 * 1024)
	const chunk = 2048

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			if err != nil || v < 0 || v > len(body) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = v
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		// 分块慢速写入，制造并发窗口
		for i := start; i < len(body); i += chunk {
			end := i + chunk
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)

	const workers = 2
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = d.DownloadWithRetry(srv.URL, path)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", i, err)
		}
	}

	got := readFile(t, path)
	if len(got) != len(body) {
		t.Fatalf("concurrent download corrupted size: got %d bytes, want %d", len(got), len(body))
	}
	if !bytes.Equal(got, body) {
		t.Fatal("concurrent download corrupted content")
	}
	assertNoFile(t, path+".tmp")
}

// P1-6 强断言版本：走 DownloadFile（单次尝试，没有重试兜底），并发损坏无法被自愈。
func TestDownloadConcurrentSamePathSingleAttempt(t *testing.T) {
	body := testBody(64 * 1024)
	const chunk = 2048

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			if err != nil || v < 0 || v > len(body) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = v
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		for i := start; i < len(body); i += chunk {
			end := i + chunk
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)

	const workers = 3
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = d.DownloadFile(srv.URL, path)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", i, err)
		}
	}
	got := readFile(t, path)
	if !bytes.Equal(got, body) {
		t.Fatalf("并发单次下载损坏：got %d bytes, want %d bytes", len(got), len(body))
	}
	assertNoFile(t, path+".tmp")
}

// --- P0-2：空闲读超时（字节长时间无增长） ----------------------------------

func TestDownloadFileIdleTimeout(t *testing.T) {
	body := testBody(8192)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 停滞：直到客户端取消请求（或测试收尾）
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}))
	// defer 为 LIFO：close(release) 会先于 srv.Close() 执行，避免 Close 等待挂起的 handler
	defer srv.Close()
	defer close(release)

	path := filepath.Join(t.TempDir(), "video.mp4")
	d := newTestDownloader(t, nil)
	d.IdleTimeout = 100 * time.Millisecond
	d.MaxRetries = 2

	started := time.Now()
	err := d.DownloadWithRetry(srv.URL, path)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("expected idle-timeout error, got nil")
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("error = %v, want errors.Is(err, ErrIdleTimeout)", err)
	}
	// 两次尝试 * 100ms + 余量
	if elapsed > 3*time.Second {
		t.Fatalf("idle timeout did not fire: elapsed %v", elapsed)
	}
	assertNoFile(t, path)
}

// --- 206 区间与服务端偏移不一致：不得把全量内容拼到已有前缀后面 --------------

func TestDownloadFileRangeMismatchRestarts(t *testing.T) {
	body := testBody(3000)
	const half = 1500

	var requests, rangeRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
			return
		}
		// 缺陷服务端：声称 206，但内容区间从 0 开始（等于回全量）
		rangeRequests.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path+".tmp", body[:half], 0644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(t, nil)
	if err := d.DownloadWithRetry(srv.URL, path); err != nil {
		t.Fatalf("DownloadWithRetry: %v", err)
	}

	got := readFile(t, path)
	if !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes (重复拼接)", len(got), len(body))
	}
	if rangeRequests.Load() != 1 || requests.Load() != 2 {
		t.Fatalf("requests = %d (range %d), want 2 (range 1)", requests.Load(), rangeRequests.Load())
	}
}

// --- 单元：Percent / ProgressWriter / keyedMutex / 构造 ---------------------

func TestPercent(t *testing.T) {
	cases := []struct {
		downloaded, total int64
		wantPercent       int
		wantOK            bool
	}{
		{0, 100, 0, true},
		{50, 100, 50, true},
		{100, 100, 100, true},
		{150, 100, 100, true}, // 钳制上界
		{-5, 100, 0, true},    // 钳制下界
		{10, UnknownSize, 0, false},
		{10, 0, 0, false},
		{10, -100, 0, false},
	}
	for _, c := range cases {
		gotPercent, gotOK := Percent(c.downloaded, c.total)
		if gotPercent != c.wantPercent || gotOK != c.wantOK {
			t.Errorf("Percent(%d, %d) = (%d, %v), want (%d, %v)",
				c.downloaded, c.total, gotPercent, gotOK, c.wantPercent, c.wantOK)
		}
	}
}

func TestProgressWriterConcurrentAccess(t *testing.T) {
	pw := &ProgressWriter{Total: 1000}
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := pw.Write([]byte("x")); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				_ = pw.Downloaded.Load()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 1000; j++ {
			_ = pw.Downloaded.Load()
		}
	}()
	wg.Wait()

	if got := pw.Downloaded.Load(); got != 400 {
		t.Fatalf("Downloaded = %d, want 400", got)
	}
}

func TestKeyedMutexSerializesSameKey(t *testing.T) {
	km := newKeyedMutex()

	var inFlight, maxInFlight atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := km.Lock("same")
			defer unlock()
			cur := inFlight.Add(1)
			for {
				old := maxInFlight.Load()
				if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("same key ran %d concurrent holders, want 1", got)
	}
	if n := len(km.locks); n != 0 {
		t.Fatalf("locks map leaked %d entries after all releases", n)
	}
}

func TestKeyedMutexAllowsDifferentKeys(t *testing.T) {
	km := newKeyedMutex()

	const n = 4
	acquired := make(chan struct{}, n)
	proceed := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := km.Lock(fmt.Sprintf("key-%d", i))
			defer unlock()
			acquired <- struct{}{}
			<-proceed
		}(i)
	}

	// 若不同 key 被互相阻塞，这里会超时
	for i := 0; i < n; i++ {
		select {
		case <-acquired:
		case <-time.After(3 * time.Second):
			t.Fatalf("different keys were serialized: only %d/%d acquired", i, n)
		}
	}
	close(proceed)
	wg.Wait()
}

func TestNewDownloaderDefaults(t *testing.T) {
	d := NewDownloader("")
	if d == nil {
		t.Fatal("NewDownloader returned nil")
	}
	if d.MaxRetries != MaxRetries {
		t.Errorf("MaxRetries = %d, want %d", d.MaxRetries, MaxRetries)
	}
	if d.RetryInterval != RetryInterval {
		t.Errorf("RetryInterval = %v, want %v", d.RetryInterval, RetryInterval)
	}
	if d.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", d.IdleTimeout, DefaultIdleTimeout)
	}
	d.init()
	if d.client == nil {
		t.Error("client 未初始化")
	}
	if d.locks == nil {
		t.Error("locks 未初始化")
	}
}

func TestNewDownloaderWithClientReusesInjectedClient(t *testing.T) {
	client := &http.Client{Transport: &http.Transport{}}
	d := NewDownloaderWithClient("", client)
	if d.client != client {
		t.Fatal("injected client 未被复用")
	}

	// 同一 Downloader 的多次下载必须复用同一个 client/连接池
	d.init()
	if d.client != client {
		t.Fatal("init 覆盖了注入的 client")
	}
}

func TestContentRangeStart(t *testing.T) {
	cases := []struct {
		in     string
		want   int64
		wantOK bool
	}{
		{"bytes 0-99/1000", 0, true},
		{"bytes 500-999/1000", 500, true},
		{" bytes 42-99/1000", 42, true},
		{"bytes */1000", 0, false},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, ok := contentRangeStart(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("contentRangeStart(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
