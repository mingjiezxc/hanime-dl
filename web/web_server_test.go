package web

// 本文件的离线保证：
//   - WebServer 的 resolver / downloader / connectChrome 三个依赖在 newTestServer 里
//     全部替换为纯内存假实现，因此任何路径都不会拨号 CDP、不会拉起 Chrome、不会访问外网；
//   - handler 测试用 httptest.NewRequest + httptest.NewRecorder 直接调 Handler().ServeHTTP，
//     连 socket 都不需要；
//   - 唯一使用真实 socket 的是 TestRunWebServerGracefulShutdown，它只连
//     http://127.0.0.1:<随机端口> 上我们自己起的服务。
// 假实现被调用的事实本身也是证据：resolver 断言收到的 wsURL 必须是测试配置的值，
// connectChrome 一旦被调用就返回错误（证明「没有连接真实 Chrome」）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"hanime-dl/config"
	"hanime-dl/downloader"
	"hanime-dl/scraper"
)

const (
	// testTimeout 等待异步流水线收敛的期限。假实现没有任何真实 I/O，正常在毫秒级
	// 收敛；3s 只是给 -race + 慢 CI 的余量。
	testTimeout = 3 * time.Second
	// testWSURL 测试专用的假 CDP 地址。resolver 会校验它，确保没有走真实连接。
	testWSURL = "ws://fake-cdp:0/devtools/browser/fake"
)

// --- 假实现 ---

// fakeResolver 是 videoResolver 的纯内存实现。
type fakeResolver struct {
	mu       sync.Mutex
	resolve  func(videoID, listID string) (scraper.VideoMetadata, error)
	playlist func(listID string) ([]string, error)
	refresh  func(videoID string) (string, error)

	resolveCalls  int
	playlistCalls int
	refreshCalls  int
	seenWSURLs    []string
}

func (f *fakeResolver) ResolveVideoInfo(wsURL, videoID, listID string) (scraper.VideoMetadata, error) {
	f.mu.Lock()
	f.resolveCalls++
	f.seenWSURLs = append(f.seenWSURLs, wsURL)
	fn := f.resolve
	f.mu.Unlock()
	if wsURL != testWSURL {
		return scraper.VideoMetadata{}, fmt.Errorf("resolver 收到非测试 wsURL %q：测试不得触碰真实 CDP", wsURL)
	}
	if fn == nil {
		return scraper.VideoMetadata{}, fmt.Errorf("fakeResolver: 未设置 resolve，video=%s", videoID)
	}
	return fn(videoID, listID)
}

func (f *fakeResolver) GetPlaylist(wsURL, listID string) ([]string, error) {
	f.mu.Lock()
	f.playlistCalls++
	fn := f.playlist
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("fakeResolver: 未设置 playlist，list=%s", listID)
	}
	return fn(listID)
}

func (f *fakeResolver) RefreshVideoDataURL(wsURL, videoID string) (string, error) {
	f.mu.Lock()
	f.refreshCalls++
	fn := f.refresh
	f.mu.Unlock()
	if fn == nil {
		return "", errors.New("fakeResolver: 未设置 refresh")
	}
	return fn(videoID)
}

func (f *fakeResolver) counts() (resolve, playlist, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolveCalls, f.playlistCalls, f.refreshCalls
}

// fakeDownloader 是 fileDownloader 的纯内存实现：把内容写进目标路径，不触碰网络。
type fakeDownloader struct {
	mu       sync.Mutex
	calls    []string
	content  []byte
	failWith error
	// samples 每次带回调的下载开始前，按顺序回调的 (downloaded,total) 样本。
	samples []progressSample
	// gate 非 nil 时，在回调样本之后、写文件之前阻塞等待，用于稳定观察中间态。
	gate chan struct{}
}

type progressSample struct{ downloaded, total int64 }

func (f *fakeDownloader) DownloadWithRetry(url, path string) error {
	return f.download(path, nil)
}

func (f *fakeDownloader) DownloadWithRetryCB(url, path string, cb downloader.ProgressCallback) error {
	return f.download(path, cb)
}

func (f *fakeDownloader) download(path string, cb downloader.ProgressCallback) error {
	f.mu.Lock()
	f.calls = append(f.calls, path)
	failWith := f.failWith
	content := f.content
	samples := append([]progressSample(nil), f.samples...)
	gate := f.gate
	f.mu.Unlock()

	if failWith != nil {
		return failWith
	}
	for _, s := range samples {
		if cb != nil {
			cb(s.downloaded, s.total)
		}
	}
	if gate != nil {
		<-gate
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

func (f *fakeDownloader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- 测试脚手架 ---

// newTestServer 构造完全离线的 WebServer。worker 由调用方按需启动
// （需要「先观察 pending、再让 worker 取走」的确定性状态机测试）。
func newTestServer(t *testing.T, opts *Options) (*WebServer, *fakeResolver, *fakeDownloader) {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	if opts.WSURL == "" {
		opts.WSURL = testWSURL
	}
	ws := NewWebServer(opts)

	res := &fakeResolver{}
	dl := &fakeDownloader{}
	ws.resolver = res
	ws.downloader = dl
	// 任何 refreshWSURL 都失败，且绝不触碰真实 Chrome。
	ws.connectChrome = func(remoteURL string, timeout time.Duration) (string, error) {
		return "", errors.New("测试禁止连接或拉起真实 Chrome")
	}

	t.Cleanup(func() {
		if err := ws.Stop(); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
	})
	return ws, res, dl
}

// testMeta 为 videoID 生成一份指向临时目录的元数据（DataURL 只是占位字符串，
// 因为下载器已被替换为假实现，不会真的去请求它）。
func testMeta(dir, videoID string) scraper.VideoMetadata {
	return scraper.VideoMetadata{
		VideoID:       videoID,
		Title:         "title-" + videoID,
		DataURL:       "http://example.invalid/" + videoID + ".mp4",
		VideoFilePath: filepath.Join(dir, videoID+".mp4"),
	}
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func waitForTaskStatus(t *testing.T, ws *WebServer, id, want string) TaskStatus {
	t.Helper()
	var got TaskStatus
	waitForCond(t, fmt.Sprintf("任务 %s 进入 %s", id, want), func() bool {
		snap, ok := ws.snapshotTask(id)
		if !ok {
			return false
		}
		got = snap
		return snap.Status == want
	})
	return got
}

// waitForSettled 等待所有任务离开 pending/downloading。
func waitForSettled(t *testing.T, ws *WebServer) []TaskStatus {
	t.Helper()
	var out []TaskStatus
	waitForCond(t, "全部任务收敛（无 pending/downloading）", func() bool {
		out = ws.snapshotTasks()
		for i := range out {
			if out[i].Status == "pending" || out[i].Status == "downloading" {
				return false
			}
		}
		return true
	})
	return out
}

func submitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func countByType(tasks []TaskStatus, typ string) int {
	n := 0
	for i := range tasks {
		if tasks[i].Type == typ {
			n++
		}
	}
	return n
}

func decodeBody[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("响应体应为合法 JSON，解析失败（旧实现的 text/plain 错误体正是在这里抛错）：%v；body=%q", err, body)
	}
	return v
}

// --- 1. handler 测试 ---

func TestHandleTasksGetReturnsSnapshotArray(t *testing.T) {
	ws, _, _ := newTestServer(t, &Options{})
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// 必须是 JSON 数组而不是 null：前端 renderTasks 直接对它迭代。
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("空快照 body = %q, want []", body)
	}

	ws.AddTask(&TaskStatus{ID: "a_1", Type: "single", VideoID: "a", Status: "pending"})
	rec = httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	got := decodeBody[[]TaskStatus](t, rec.Body.Bytes())
	if len(got) != 1 || got[0].ID != "a_1" {
		t.Fatalf("快照 = %+v, want 含 a_1 的单项", got)
	}
}

func TestHandleTasksPostCreatesTask(t *testing.T) {
	ws, _, _ := newTestServer(t, &Options{})
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	body := `{"video_id":"v1","type":"single"}`
	ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	task := decodeBody[TaskStatus](t, rec.Body.Bytes())
	if task.VideoID != "v1" || task.Type != "single" || task.Status != "pending" {
		t.Errorf("task = %+v, want video=v1 type=single status=pending", task)
	}
	if _, ok := ws.snapshotTask(task.ID); !ok {
		t.Errorf("任务 %s 未进入任务表", task.ID)
	}
}

func TestHandleTasksErrorsAreJSON(t *testing.T) {
	// 这条锁死 task-3 顺带修好的前端 bug：web/index.html 用 response.json() 解析错误体，
	// 旧实现先写 application/json 再用 http.Error 改写成 text/plain，前端必然抛
	// "Unexpected token"，用户永远只看到「请求失败：...」。
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantAllow  string
	}{
		{name: "非法 JSON", method: http.MethodPost, path: "/api/tasks", body: "{not json", wantStatus: http.StatusBadRequest},
		{name: "两个 ID 都为空", method: http.MethodPost, path: "/api/tasks", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "空白 ID 视为空", method: http.MethodPost, path: "/api/tasks", body: `{"video_id":"  ","list_id":"\t"}`, wantStatus: http.StatusBadRequest},
		{name: "任务不存在", method: http.MethodGet, path: "/api/tasks/nope", wantStatus: http.StatusNotFound},
		{name: "状态任务不存在", method: http.MethodGet, path: "/api/status/nope", wantStatus: http.StatusNotFound},
		{name: "不支持的方法", method: http.MethodDelete, path: "/api/tasks", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, POST"},
		{name: "submit 非 POST", method: http.MethodGet, path: "/api/submit", wantStatus: http.StatusMethodNotAllowed, wantAllow: "POST"},
		{name: "按 ID 查询非 GET", method: http.MethodPost, path: "/api/tasks/x", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET"},
		{name: "progress 非 GET", method: http.MethodPost, path: "/api/progress", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, _, _ := newTestServer(t, &Options{})
			if err := ws.SetupRoutes(); err != nil {
				t.Fatalf("SetupRoutes: %v", err)
			}
			var reqBody io.Reader
			if tc.body != "" {
				reqBody = strings.NewReader(tc.body)
			}
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, reqBody))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json（旧实现是 text/plain）", ct)
			}
			if tc.wantAllow != "" {
				if allow := rec.Header().Get("Allow"); allow != tc.wantAllow {
					t.Errorf("Allow = %q, want %q", allow, tc.wantAllow)
				}
			}
			apiErr := decodeBody[apiError](t, rec.Body.Bytes())
			if apiErr.Error == "" || apiErr.Message == "" {
				t.Errorf("错误体 = %+v, want error 与 message 都非空（index.html 读 message）", apiErr)
			}
		})
	}
}

func TestDuplicateVideoSubmitReturns409(t *testing.T) {
	// 反向证据：修复前 markSeen 只在列表展开路径调用，重复提交同一 video_id 会得到
	// 两次 201 与两个任务，两个 worker 会并发写同一个 .tmp（P1-3）。
	ws, _, _ := newTestServer(t, &Options{})
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks",
			strings.NewReader(`{"video_id":"dup","type":"single"}`)))
		return rec
	}

	if rec := post(); rec.Code != http.StatusCreated {
		t.Fatalf("首次提交 status = %d, want 201", rec.Code)
	}
	rec := post()
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复提交 status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	apiErr := decodeBody[apiError](t, rec.Body.Bytes())
	if apiErr.Error != ErrDuplicateVideo.Error() {
		t.Errorf("error = %q, want %q", apiErr.Error, ErrDuplicateVideo.Error())
	}
	if n := len(ws.snapshotTasks()); n != 1 {
		t.Errorf("任务数 = %d, want 1（重复提交不得再建任务）", n)
	}
}

func TestTaskByIDAndStatusAndProgress(t *testing.T) {
	ws, _, _ := newTestServer(t, &Options{})
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}
	ws.AddTask(&TaskStatus{ID: "t_1", Type: "single", VideoID: "v1", Status: "pending"})

	for _, path := range []string{"/api/tasks/t_1", "/api/status/t_1"} {
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		task := decodeBody[TaskStatus](t, rec.Body.Bytes())
		if task.ID != "t_1" {
			t.Errorf("GET %s task.ID = %q, want t_1", path, task.ID)
		}
	}

	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/progress", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/progress status = %d, want 200", rec.Code)
	}
	if got := decodeBody[[]TaskStatus](t, rec.Body.Bytes()); len(got) != 1 || got[0].ID != "t_1" {
		t.Errorf("progress = %+v, want 含 t_1 的列表", got)
	}
}

func TestCreateAndDispatchTaskNormalization(t *testing.T) {
	tests := []struct {
		name                          string
		videoID, listID, typeF, modeF string
		wantType, wantVideo, wantList string
		wantErr                       error
	}{
		{name: "mode 优先于 type", videoID: "v1", typeF: "single", modeF: "list", wantType: "list", wantVideo: "v1", wantList: "v1"},
		{name: "type 回填 list_id", videoID: "v2", typeF: "list", wantType: "list", wantVideo: "v2", wantList: "v2"},
		{name: "按 list_id 猜 list", listID: "L3", wantType: "list", wantVideo: "L3", wantList: "L3"},
		{name: "按 video_id 猜 single", videoID: "v4", wantType: "single", wantVideo: "v4"},
		{name: "list 任务带 list_id", videoID: "v5", listID: "L5", typeF: "list", wantType: "list", wantVideo: "v5", wantList: "L5"},
		{name: "全空", wantErr: ErrMissingTarget},
		{name: "全空白", videoID: " ", listID: "\t", wantErr: ErrMissingTarget},
	}

	ws, _, _ := newTestServer(t, &Options{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task, err := ws.createAndDispatchTask(submitCtx(t), tc.videoID, tc.listID, tc.typeF, tc.modeF)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if task.Type != tc.wantType || task.VideoID != tc.wantVideo || task.ListID != tc.wantList {
				t.Errorf("task = {type:%s video:%s list:%s}, want {type:%s video:%s list:%s}",
					task.Type, task.VideoID, task.ListID, tc.wantType, tc.wantVideo, tc.wantList)
			}
		})
	}
}

// --- 2. 流水线测试 ---

func TestListExpansionDeduplicatesPlaylistEntries(t *testing.T) {
	// 硬性回归用例（列表展开去重）。假 GetPlaylist 返回 6 项、其中 3 个唯一 ID。
	dir := t.TempDir()
	ws, res, dl := newTestServer(t, &Options{MaxWorkers: 2})
	res.playlist = func(listID string) ([]string, error) {
		return []string{"v1", "v2", "v1", "v3", "v2", "v1"}, nil
	}
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return testMeta(dir, videoID), nil
	}
	ws.startWorkers()

	if _, err := ws.createAndDispatchTask(submitCtx(t), "L1", "", "list", ""); err != nil {
		t.Fatalf("提交列表任务: %v", err)
	}
	tasks := waitForSettled(t, ws)

	if got := countByType(tasks, "single"); got != 3 {
		t.Errorf("子任务数 = %d, want 3（6 项里只有 3 个唯一 video_id）", got)
	}
	seen := map[string]int{}
	for _, ts := range tasks {
		if ts.Type == "single" {
			seen[ts.VideoID]++
		}
	}
	for vid, n := range seen {
		if n != 1 {
			t.Errorf("video %s 出现 %d 次, want 1", vid, n)
		}
	}
	if dl.count() != 3 {
		t.Errorf("下载次数 = %d, want 3", dl.count())
	}
	for _, ts := range tasks {
		if ts.Type == "list" {
			if ts.Status != "completed" {
				t.Errorf("列表任务状态 = %s, want completed", ts.Status)
			}
			if !strings.Contains(ts.Title, "新增 3 个任务") {
				t.Errorf("列表任务 Title = %q, want 含「新增 3 个任务」", ts.Title)
			}
		}
	}
}

func TestListExpansionIsIdempotent(t *testing.T) {
	// 同一列表任务执行两次：第二次不得产生重复子任务（markSeen 已记名）。
	dir := t.TempDir()
	ws, res, dl := newTestServer(t, &Options{MaxWorkers: 2})
	res.playlist = func(listID string) ([]string, error) {
		return []string{"v1", "v2", "v3"}, nil
	}
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return testMeta(dir, videoID), nil
	}
	ws.startWorkers()

	first, err := ws.createAndDispatchTask(submitCtx(t), "L1", "", "list", "")
	if err != nil {
		t.Fatalf("第一次展开: %v", err)
	}
	waitForTaskStatus(t, ws, first.ID, "completed")
	waitForSettled(t, ws)
	afterFirst := countByType(ws.snapshotTasks(), "single")

	second, err := ws.createAndDispatchTask(submitCtx(t), "L1", "", "list", "")
	if err != nil {
		t.Fatalf("第二次展开: %v", err)
	}
	secondTask := waitForTaskStatus(t, ws, second.ID, "completed")
	waitForSettled(t, ws)

	// 注意：第二次展开本身会新增一个 list 任务，所以要比较的是**子任务（single）**数量。
	if afterFirst != 3 {
		t.Fatalf("第一次展开子任务数 = %d, want 3", afterFirst)
	}
	if got := countByType(ws.snapshotTasks(), "single"); got != afterFirst {
		t.Errorf("子任务数 = %d, want %d（第二次展开不得新增子任务）", got, afterFirst)
	}
	if !strings.Contains(secondTask.Title, "新增 0 个任务") {
		t.Errorf("第二次展开 Title = %q, want 含「新增 0 个任务」", secondTask.Title)
	}
	if dl.count() != 3 {
		t.Errorf("下载次数 = %d, want 3（去重后每个视频只下一次）", dl.count())
	}
}

func TestSingleThenListDoesNotDuplicateDownload(t *testing.T) {
	// 反向证据：修复前 markSeen 只作用于列表子任务，先提交单视频 v1 再展开包含 v1 的
	// 列表会得到两个 v1 任务（一个 single、一个列表子任务），两个 worker 并发写同一
	// 个 .tmp（P1-3/P1-6）。修复后列表展开必须跳过已认领的 v1。
	dir := t.TempDir()
	ws, res, dl := newTestServer(t, &Options{MaxWorkers: 2})
	res.playlist = func(listID string) ([]string, error) {
		return []string{"v1", "v2", "v3"}, nil
	}
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return testMeta(dir, videoID), nil
	}
	ws.startWorkers()

	// 先作为单视频提交（createAndDispatchTask 会同步认领 v1）。
	single, err := ws.createAndDispatchTask(submitCtx(t), "v1", "", "single", "")
	if err != nil {
		t.Fatalf("提交单视频: %v", err)
	}
	waitForTaskStatus(t, ws, single.ID, "completed")

	listTask, err := ws.createAndDispatchTask(submitCtx(t), "L1", "", "list", "")
	if err != nil {
		t.Fatalf("提交列表: %v", err)
	}
	waitForTaskStatus(t, ws, listTask.ID, "completed")
	waitForSettled(t, ws)

	v1Tasks := 0
	for _, ts := range ws.snapshotTasks() {
		if ts.VideoID == "v1" {
			v1Tasks++
		}
	}
	if v1Tasks != 1 {
		t.Errorf("v1 的任务数 = %d, want 1（列表展开必须跳过已被单视频任务认领的 v1）", v1Tasks)
	}
	if n := dl.count(); n != 3 {
		t.Errorf("下载次数 = %d, want 3（v1/v2/v3 各一次，v1 不得重复下载）", n)
	}

	// 反向：列表已认领 v2 之后再作为单视频提交 → 409。
	rec := httptest.NewRecorder()
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}
	ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"video_id":"v2","type":"single"}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("列表已认领 v2 后再次提交 status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskStateMachinePendingDownloadingCompleted(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	ws, res, _ := newTestServer(t, &Options{MaxWorkers: 1})
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		close(entered)
		<-release
		return testMeta(dir, videoID), nil
	}

	// worker 尚未启动 → 创建后必然稳定在 pending（确定性，无 sleep 竞态）。
	task, err := ws.createAndDispatchTask(submitCtx(t), "v1", "", "single", "")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	if snap, _ := ws.snapshotTask(task.ID); snap.Status != "pending" {
		t.Fatalf("创建后状态 = %s, want pending", snap.Status)
	}

	ws.startWorkers()
	<-entered
	snap, _ := ws.snapshotTask(task.ID)
	if snap.Status != "downloading" {
		t.Fatalf("resolver 进行中状态 = %s, want downloading", snap.Status)
	}
	if snap.ProgressKnown {
		t.Errorf("下载开始前 ProgressKnown 应为 false")
	}

	close(release)
	done := waitForTaskStatus(t, ws, task.ID, "completed")
	if done.Progress != 100 {
		t.Errorf("完成时 Progress = %d, want 100", done.Progress)
	}
	if done.CompletedAt == nil {
		t.Errorf("完成时 CompletedAt 应为非 nil")
	}
	if done.ErrorMessage != "" {
		t.Errorf("成功任务不应有 ErrorMessage：%q", done.ErrorMessage)
	}
}

func TestResolveFailureMarksTaskFailed(t *testing.T) {
	ws, res, _ := newTestServer(t, &Options{MaxWorkers: 1})
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return scraper.VideoMetadata{}, errors.New("boom: 解析失败")
	}
	ws.startWorkers()

	task, err := ws.createAndDispatchTask(submitCtx(t), "v1", "", "single", "")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	failed := waitForTaskStatus(t, ws, task.ID, "failed")
	if failed.ErrorMessage == "" {
		t.Errorf("失败任务必须有 ErrorMessage")
	}
	if !strings.Contains(failed.ErrorMessage, "boom: 解析失败") {
		t.Errorf("ErrorMessage = %q, want 含解析失败原因", failed.ErrorMessage)
	}

	// 离线证据：resolver 报错会触发 refreshWSURL，而注入的 connectChrome 必然失败，
	// 因此这条路径既不探测也不拉起真实 Chrome。
	if _, _, refresh := res.counts(); refresh != 0 {
		t.Errorf("refresh 调用数 = %d, want 0（解析失败分支不调用 refreshDataURL）", refresh)
	}
}

func TestPanicIsolationKeepsWorkerAlive(t *testing.T) {
	// 反向证据：修复前没有 recover，worker 协程里的 panic 会直接终止整个测试进程
	// （未捕获的 goroutine panic 会让程序 abort），后续任务不可能被处理。
	dir := t.TempDir()
	ws, res, _ := newTestServer(t, &Options{MaxWorkers: 1})
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		if videoID == "boom" {
			panic("resolver exploded")
		}
		return testMeta(dir, videoID), nil
	}
	ws.startWorkers()

	boom, err := ws.createAndDispatchTask(submitCtx(t), "boom", "", "single", "")
	if err != nil {
		t.Fatalf("提交 boom: %v", err)
	}
	survivor, err := ws.createAndDispatchTask(submitCtx(t), "survivor", "", "single", "")
	if err != nil {
		t.Fatalf("提交 survivor: %v", err)
	}

	boomTask := waitForTaskStatus(t, ws, boom.ID, "failed")
	if !strings.Contains(boomTask.ErrorMessage, "internal error") {
		t.Errorf("panic 任务 ErrorMessage = %q, want 含 internal error", boomTask.ErrorMessage)
	}
	// worker 存活 → 后续任务仍然被完成
	waitForTaskStatus(t, ws, survivor.ID, "completed")
}

func TestExistingFileSkipsDownload(t *testing.T) {
	dir := t.TempDir()
	ws, res, dl := newTestServer(t, &Options{MaxWorkers: 1})
	meta := testMeta(dir, "v1")
	if err := os.WriteFile(meta.VideoFilePath, []byte("already on disk"), 0o644); err != nil {
		t.Fatalf("预置文件: %v", err)
	}
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return meta, nil
	}
	ws.startWorkers()

	task, err := ws.createAndDispatchTask(submitCtx(t), "v1", "", "single", "")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	done := waitForTaskStatus(t, ws, task.ID, "completed")
	if done.Progress != 100 {
		t.Errorf("Progress = %d, want 100", done.Progress)
	}
	if dl.count() != 0 {
		t.Errorf("下载次数 = %d, want 0（文件已存在时不得重新下载）", dl.count())
	}
	if b, err := os.ReadFile(meta.VideoFilePath); err != nil || string(b) != "already on disk" {
		t.Errorf("已存在文件被改写：content=%q err=%v", b, err)
	}
}

func TestProgressUnknownTotalIsNotReportedAsZeroPercent(t *testing.T) {
	// 反向证据：修复前 web 侧 `if total > 0 { ... }` 使总长未知时 Progress 恒为 0，
	// 且没有任何字段能让前端区分「0%」与「进度未知」。
	//
	// 每个子用例用**独立的临时目录**：handleTasks 路径上 processSingleTask 会先
	// fileExists(videoPath) 命中短路径直接完成，共用目录会让第二个子用例跳过下载、
	// 永远等不到进度回调。
	tests := []struct {
		name         string
		sample       progressSample
		wantKnown    bool
		wantProgress int
		wantTotal    int64
	}{
		{name: "总长未知", sample: progressSample{downloaded: 10, total: -1}, wantKnown: false, wantProgress: 0, wantTotal: 0},
		{name: "总长已知", sample: progressSample{downloaded: 50, total: 100}, wantKnown: true, wantProgress: 50, wantTotal: 100},
		{name: "下载量超过总长被钳制", sample: progressSample{downloaded: 150, total: 100}, wantKnown: true, wantProgress: 100, wantTotal: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gate := make(chan struct{})
			ws, res, dl := newTestServer(t, &Options{MaxWorkers: 1})
			res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
				return testMeta(dir, videoID), nil
			}
			dl.samples = []progressSample{tc.sample}
			dl.gate = gate
			ws.startWorkers()

			task, err := ws.createAndDispatchTask(submitCtx(t), "v1", "", "single", "")
			if err != nil {
				t.Fatalf("提交任务: %v", err)
			}
			// 回调已发生但下载被 gate 卡住 → 稳定观察中间态。
			var snap TaskStatus
			waitForCond(t, "进度回调生效", func() bool {
				snap, _ = ws.snapshotTask(task.ID)
				return snap.Downloaded == tc.sample.downloaded
			})
			if snap.ProgressKnown != tc.wantKnown {
				t.Errorf("ProgressKnown = %v, want %v", snap.ProgressKnown, tc.wantKnown)
			}
			if snap.Progress != tc.wantProgress {
				t.Errorf("Progress = %d, want %d", snap.Progress, tc.wantProgress)
			}
			if snap.TotalSize != tc.wantTotal {
				t.Errorf("TotalSize = %d, want %d", snap.TotalSize, tc.wantTotal)
			}
			close(gate)
			waitForTaskStatus(t, ws, task.ID, "completed")
		})
	}
}

// --- 3. 背压与生命周期 ---

func TestEnqueueBackpressureReturnsQueueFullAndLeaksNoGoroutine(t *testing.T) {
	// 反向证据：修复前队列满时执行 go func(){ ch <- task }()，每次提交泄漏一个永久
	// 阻塞的 goroutine，且 handler 立刻回 201（调用方完全不知道任务没被接受）。
	ws, _, _ := newTestServer(t, &Options{})
	for i := 0; i < cap(ws.queue); i++ {
		ws.queue <- &TaskStatus{ID: fmt.Sprintf("filler-%d", i), Type: "single"}
	}

	before := runtime.NumGoroutine()

	// 先证明它真的会「等一会」：给一个有界预算，确认返回的是 ErrQueueFull
	// （而不是因为队列满就直接失败却拒绝说明原因）。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := ws.createAndDispatchTask(waitCtx, "late-wait", "", "single", "")
	waitCancel()
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("有界等待后 err = %v, want ErrQueueFull", err)
	}

	// 再做泄漏检查：用已经过期的 ctx，命中同一条「预算耗尽」分支但不引入等待。
	// 旧实现每次都会多派生一个永久阻塞在 ch<-task 上的 goroutine。
	expired, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	const attempts = 200
	for i := 0; i < attempts; i++ {
		_, err := ws.createAndDispatchTask(expired, fmt.Sprintf("late-%d", i), "", "single", "")
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("第 %d 次入队 err = %v, want ErrQueueFull", i, err)
		}
	}
	// 旧实现会在这里多出 200 个阻塞 goroutine；留出运行时的合理波动余量。
	if after := runtime.NumGoroutine(); after > before+50 {
		t.Errorf("goroutine 数 %d → %d（%d 次失败入队），疑似泄漏", before, after, attempts)
	}

	// 入队失败必须回滚：既不留任务残影，也不能占用 video_id 认领名额（否则重试会被
	// 误判成「重复视频」而永久 409）。
	if n := len(ws.snapshotTasks()); n != 0 {
		t.Errorf("入队失败后任务表 = %d 项, want 0", n)
	}
	if !ws.markSeen("late-0") {
		t.Error("入队失败后 video_id 认领未回滚：重试会被误判为重复视频")
	}
}

func TestEnqueueBackpressureOverHTTPReturns503(t *testing.T) {
	// 走真实 handler：队列填满后 POST 必须得到 503 + JSON 错误体，且任务不得留在任务表里。
	// 旧实现在这条路径上会派生 goroutine 并把任务「偷偷」塞进队列，同时回 201。
	ws, _, _ := newTestServer(t, &Options{})
	for i := 0; i < cap(ws.queue); i++ {
		ws.queue <- &TaskStatus{ID: fmt.Sprintf("filler-%d", i), Type: "single"}
	}
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("SetupRoutes: %v", err)
	}

	// 用比 TaskEnqueueTimeout(5s) 更短的请求上下文，让 handler 的有界等待快速结束。
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"video_id":"v-over","type":"single"}`)).WithContext(ctx)

	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503（队列满时不得谎报 201）; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	apiErr := decodeBody[apiError](t, rec.Body.Bytes())
	if apiErr.Error != ErrQueueFull.Error() {
		t.Errorf("error = %q, want %q", apiErr.Error, ErrQueueFull.Error())
	}
	if n := len(ws.snapshotTasks()); n != 0 {
		t.Errorf("入队失败后任务表 = %d 项, want 0（不得留下未被接受的任务）", n)
	}
	if got := statusForError(ErrQueueFull); got != http.StatusServiceUnavailable {
		t.Errorf("statusForError(ErrQueueFull) = %d, want 503", got)
	}
}

func TestStopReleasesBlockedEnqueue(t *testing.T) {
	ws, _, _ := newTestServer(t, &Options{})
	for i := 0; i < cap(ws.queue); i++ {
		ws.queue <- &TaskStatus{ID: fmt.Sprintf("filler-%d", i), Type: "single"}
	}

	// 用与 ws.ctx 不同的 ctx，才能确定性地命中「服务器停机」分支而不是「请求取消」。
	blockCtx, cancelBlock := context.WithCancel(context.Background())
	defer cancelBlock()

	errCh := make(chan error, 1)
	go func() {
		_, err := ws.createAndDispatchTask(blockCtx, "v-blocked", "", "single", "")
		errCh <- err
	}()

	waitForCond(t, "入队进入阻塞（队列被填满）", func() bool { return len(ws.queue) == cap(ws.queue) })
	if err := ws.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrServerStopped) {
			t.Fatalf("停机后阻塞入队 err = %v, want ErrServerStopped", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Stop 没有唤醒阻塞的入队（旧实现的 go func(){ch<-task} 会永久阻塞）")
	}
}

func TestStopIsIdempotentAndSafeWithoutStart(t *testing.T) {
	ws, _, _ := newTestServer(t, &Options{MaxWorkers: 2})
	ws.startWorkers()

	if err := ws.Stop(); err != nil {
		t.Fatalf("首次 Stop: %v", err)
	}
	if err := ws.Stop(); err != nil {
		t.Fatalf("重复 Stop 必须幂等: %v", err)
	}
	if ws.ctx.Err() == nil {
		t.Error("Stop 应取消生命周期 ctx")
	}

	done := make(chan struct{})
	go func() {
		ws.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("worker 未随 Stop 退出（旧实现的 for range queue 只有 close 队列才退出，而队列从未 close）")
	}

	// 未调用 Start（ws.server 为 nil）时 Stop 不得 panic。
	ws2, _, _ := newTestServer(t, &Options{})
	if err := ws2.Stop(); err != nil {
		t.Fatalf("未 Start 时 Stop: %v", err)
	}
}

func TestSetupRoutesUsesLocalMuxAndIsIdempotent(t *testing.T) {
	// 反向证据：修复前用包级 http.HandleFunc，第二次 SetupRoutes 直接 panic
	// （"multiple registrations for /api/tasks"），且污染全局 mux、无法起隔离 server。
	ws, _, _ := newTestServer(t, &Options{})
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("首次 SetupRoutes: %v", err)
	}
	if err := ws.SetupRoutes(); err != nil {
		t.Fatalf("第二次 SetupRoutes 必须成功（旧实现会 panic）: %v", err)
	}
	if ws.Handler() == nil {
		t.Fatal("Handler() = nil, want 局部 mux")
	}

	// 反向证据之二：全局 mux 上不应出现我们的路由。
	if _, pattern := http.DefaultServeMux.Handler(httptest.NewRequest(http.MethodGet, "/api/tasks", nil)); pattern != "" {
		t.Errorf("/api/tasks 被注册到 http.DefaultServeMux（pattern=%q），全局 mux 被污染", pattern)
	}
}

func TestRunWebServerGracefulShutdown(t *testing.T) {
	// 端到端验证 P3-5：signal ctx 取消 → RunWebServer 调 Stop() → 返回 nil（而非
	// ErrServerClosed 或 panic）。旧实现的 Stop() 没有任何调用点，Ctrl+C 只会杀进程。
	t.Chdir(t.TempDir()) // 让默认目录落在临时工作目录内，不污染仓库

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunWebServer(&Options{Addr: addr, Ctx: ctx, MaxWorkers: 1})
	}()

	waitForHTTPReadyLocal(t, "http://"+addr+"/api/tasks", testTimeout)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅停机应返回 nil，got %v", err)
		}
	case <-time.After(2 * testTimeout):
		t.Fatal("RunWebServer 未在停机信号后返回")
	}

	// 默认目录来自 config 包的唯一权威常量（旧实现 web 侧写死 ./download，
	// 与 config 的 ./downloads 漂移）。
	for _, dir := range []string{config.DefaultCacheDir, config.DefaultDownDir} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("默认目录 %s 未创建：err=%v", dir, err)
		}
	}
}

// --- 4. 并发正确性（-race 下验证 tasksMutex / mutateTask 契约）---

func TestConcurrentSubmitAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	ws, res, dl := newTestServer(t, &Options{MaxWorkers: 4})
	res.resolve = func(videoID, listID string) (scraper.VideoMetadata, error) {
		return testMeta(dir, videoID), nil
	}
	ws.startWorkers()

	const submitters = 8
	const perSubmitter = 25
	const want = submitters * perSubmitter

	var wg sync.WaitGroup
	for g := 0; g < submitters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perSubmitter; i++ {
				id := fmt.Sprintf("c%d-%d", g, i)
				if _, err := ws.createAndDispatchTask(context.Background(), id, "", "single", ""); err != nil {
					t.Errorf("并发提交 %s: %v", id, err)
				}
			}
		}(g)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = ws.snapshotTasks()
				_, _ = ws.snapshotTask("c0-0")
			}
		}()
	}
	wg.Wait()

	tasks := waitForSettled(t, ws)
	if len(tasks) != want {
		t.Fatalf("任务数 = %d, want %d", len(tasks), want)
	}
	// P2-7 回归：旧实现用 videoID+UnixNano 拼 ID，同纳秒并发提交会覆盖 tasks 表项，
	// 导致任务数少于提交数；单调序号后必须恰好 want 个唯一 ID。
	ids := map[string]bool{}
	for _, ts := range tasks {
		if ids[ts.ID] {
			t.Fatalf("任务 ID 重复：%s（旧实现的 UnixNano 拼接会碰撞）", ts.ID)
		}
		ids[ts.ID] = true
		if ts.Status != "completed" {
			t.Errorf("任务 %s 状态 = %s, want completed", ts.ID, ts.Status)
		}
	}
	if dl.count() != want {
		t.Errorf("下载次数 = %d, want %d", dl.count(), want)
	}
}

// --- 小工具 ---

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("释放占位监听: %v", err)
	}
	return addr
}

func waitForHTTPReadyLocal(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			if _, cerr := io.Copy(io.Discard, resp.Body); cerr != nil {
				t.Logf("drain probe body: %v", cerr)
			}
			if cerr := resp.Body.Close(); cerr != nil {
				t.Logf("close probe body: %v", cerr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("web server 未在 %s 内就绪：%s", timeout, url)
}
