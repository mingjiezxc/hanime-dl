package main

// 本文件覆盖 task-3 落在 package main 里的 P1-1 修复。离线保证：
//   - 绝不调用 runCLIMode（它内部直接调用包级 chrome.EnsureChromeConnection，
//     没有注入点，会去探测/拉起真实 Chrome）。这里只测提取出来的纯逻辑
//     （taskQueueAdder / ListStatus / registerListMembers / markVideoDone /
//     clearListCacheIfEmpty / resolveAndQueue）与 downloadOne；
//   - downloadOne 的测试用 httptest.Server，只连 127.0.0.1 上的本地假服务，
//     不访问外网、不启动浏览器。
// 反向复现（修复前行为）见 .dbg/probe：那是从 git HEAD 的 main.go 逐字取出的旧实现。

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hanime-dl/config"
	"hanime-dl/downloader"
	"hanime-dl/scraper"
)

// resetListState 清空包级列表进度状态，避免测试间互相污染。
func resetListState(t *testing.T) {
	t.Helper()
	progressMu.Lock()
	listProgress = make(map[string]*ListStatus)
	videoLists = make(map[string][]string)
	videoDone = make(map[string]bool)
	progressMu.Unlock()
}

// stubResolver 是 mainResolver 的纯内存实现。
type stubResolver struct {
	resolve  func(videoID, listID string) (scraper.VideoMetadata, error)
	playlist func(listID string) ([]string, error)
	refresh  func(videoID string) (string, error)
}

func (s *stubResolver) ResolveVideoInfo(wsURL, videoID, listID string) (scraper.VideoMetadata, error) {
	if s.resolve == nil {
		return scraper.VideoMetadata{}, errors.New("stubResolver: resolve 未设置")
	}
	return s.resolve(videoID, listID)
}

func (s *stubResolver) GetPlaylist(wsURL, listID string) ([]string, error) {
	if s.playlist == nil {
		return nil, errors.New("stubResolver: playlist 未设置")
	}
	return s.playlist(listID)
}

func (s *stubResolver) RefreshVideoDataURL(wsURL, videoID string) (string, error) {
	if s.refresh == nil {
		return "", errors.New("stubResolver: refresh 未设置")
	}
	return s.refresh(videoID)
}

type cliDirs struct{ cache, down string }

func newCLIDirs(t *testing.T) cliDirs {
	t.Helper()
	base := t.TempDir()
	dirs := cliDirs{cache: filepath.Join(base, "cache"), down: filepath.Join(base, "downloads")}
	for _, d := range []string{dirs.cache, dirs.down} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("创建目录 %s: %v", d, err)
		}
	}
	return dirs
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s: %v", path, err)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s：%s 应存在，got err=%v", why, path, err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s：%s 不应存在，got err=%v", why, path, err)
	}
}

// --- 1. taskQueueAdder：重复 videoID 只入队一次 ---

func TestTaskQueueAdderEnqueuesEachVideoOnce(t *testing.T) {
	// 反向证据：修复前 processList 把列表成员**直接**投递到 taskQueue
	// （`taskQueue <- VideoTask{...}`），完全不查 taskMap。同一列表里重复出现的 ID、
	// 或同一视频分属两个列表时，会被投递多次 → 多个任务并发下载同一路径。
	q := make(chan VideoTask, 8)
	adder := newTaskQueueAdder(q)

	if !adder.add(VideoTask{VideoID: "v1", ListID: "L1"}) {
		t.Error("首次 add 应返回 true")
	}
	if adder.add(VideoTask{VideoID: "v1", ListID: "L1"}) {
		t.Error("同一列表内重复的 videoID 不得再次入队（旧实现会重复投递）")
	}
	if adder.add(VideoTask{VideoID: "v1", ListID: "L2"}) {
		t.Error("跨列表重复的 videoID 不得再次入队（旧实现会重复投递）")
	}
	if !adder.add(VideoTask{VideoID: "v2"}) {
		t.Error("新 videoID 应入队")
	}
	close(q)

	var got []string
	for task := range q {
		got = append(got, task.VideoID)
	}
	if len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
		t.Errorf("入队序列 = %v, want [v1 v2]（每个 videoID 恰好一次）", got)
	}
	if !adder.has("v1") || !adder.has("v2") {
		t.Error("has 应报告已入队的 videoID")
	}
	if adder.has("v3") {
		t.Error("has 不该报告未入队的 videoID")
	}
}

// --- 2. resolve 失败：不触碰列表进度、保留缓存 ---

func TestResolveFailureKeepsVideoPendingAndPreservesListCache(t *testing.T) {
	// 反向证据：旧实现在解析失败的 continue 之前**无条件** Pending--；与 processList
	// 的不去重投递叠加后，pending 会提前归零甚至为负，于是 list_*.json 被删除，
	// 整个列表的续传能力丢失。这里的断言在旧实现下必然失败。
	resetListState(t)
	dirs := newCLIDirs(t)
	registerListMembers("L1", []string{"v1", "v2"})
	listCache := filepath.Join(dirs.cache, "list_L1.json")
	writeFile(t, listCache, `["v1","v2"]`)

	out := make(chan scraper.VideoMetadata, 4)
	failing := &stubResolver{resolve: func(videoID, listID string) (scraper.VideoMetadata, error) {
		return scraper.VideoMetadata{}, errors.New("410 Gone")
	}}

	if ok := resolveAndQueue(failing, "ws://x", VideoTask{VideoID: "v1", ListID: "L1"}, out); ok {
		t.Fatal("解析失败应返回 false")
	}
	if n := len(out); n != 0 {
		t.Errorf("解析失败不得向下载队列投递任务，got %d 个", n)
	}

	status := listProgress["L1"]
	if status == nil {
		t.Fatal("L1 进度缺失")
	}
	if !status.contains("v1") {
		t.Error("解析失败后 v1 必须仍在待处理集合中（旧实现无条件 Pending--）")
	}
	if status.isEmpty() {
		t.Error("解析失败不得把集合清空（旧实现会因重复项多减而提前归零）")
	}

	clearListCacheIfEmpty(1, dirs.cache, "L1")
	mustExist(t, listCache, "待处理集合非空时 list 缓存必须保留（P1-1「提前删缓存 → 丢续传」的核心回归）")
}

func TestResolveSuccessStillKeepsVideoPending(t *testing.T) {
	// 「移出待处理集合」的唯一入口是 downloadOne 成功路径上的 markVideoDone。
	// 解析成功本身**不**移出集合，否则「解析成功但下载失败」的视频会被误判完成。
	resetListState(t)
	registerListMembers("L1", []string{"v1"})

	out := make(chan scraper.VideoMetadata, 4)
	okResolver := &stubResolver{resolve: func(videoID, listID string) (scraper.VideoMetadata, error) {
		return scraper.VideoMetadata{VideoID: videoID, ListID: listID, VideoFilePath: "/tmp/" + videoID}, nil
	}}

	if ok := resolveAndQueue(okResolver, "ws://x", VideoTask{VideoID: "v1", ListID: "L1"}, out); !ok {
		t.Fatal("解析成功应返回 true")
	}
	if n := len(out); n != 1 {
		t.Fatalf("解析成功应投递 1 个任务，got %d", n)
	}
	if !listProgress["L1"].contains("v1") {
		t.Error("解析成功不得移出待处理集合：必须等 downloadOne 真正下载成功")
	}
}

// --- 3. ListStatus 集合计数：幂等、绝不为负 ---

func TestListStatusMarkDoneIsIdempotentAndNeverNegative(t *testing.T) {
	// 反向证据：旧实现是整数减法（status.Pending--），重复标记会把它减到负数，
	// 而 -1 == 0 不成立却已经越过「全部完成」的判断边界（重复项多减时提前归零）。
	resetListState(t)
	registerListMembers("L1", []string{"v1", "v2"})
	st := listProgress["L1"]
	if st.Total != 2 {
		t.Fatalf("Total = %d, want 2", st.Total)
	}

	if got := st.markDone("v1"); got != 1 {
		t.Errorf("首次 markDone(v1) 剩余 = %d, want 1", got)
	}
	if got := st.markDone("v1"); got != 1 {
		t.Errorf("重复 markDone(v1) 剩余 = %d, want 1（幂等；旧实现 Pending-- 会成为 -1）", got)
	}
	if got := st.markDone("ghost"); got != 1 {
		t.Errorf("对不存在的成员 markDone 剩余 = %d, want 1（不得改变计数）", got)
	}
	if st.isEmpty() {
		t.Error("v2 尚未完成，集合不该为空")
	}

	if got := st.markDone("v2"); got != 0 {
		t.Errorf("markDone(v2) 剩余 = %d, want 0", got)
	}
	if !st.isEmpty() {
		t.Error("全部完成后集合应为空")
	}
	if got := st.markDone("v2"); got != 0 {
		t.Errorf("重复标记后剩余 = %d, want 0（绝不能为负）", got)
	}
}

func TestRegisterListMembersDeduplicatesAndSkipsAlreadyDone(t *testing.T) {
	resetListState(t)
	registerListMembers("L1", []string{"v1", "v1", "v2"})
	st := listProgress["L1"]

	if st.Total != 2 {
		t.Errorf("Total = %d, want 2（同一列表内重复成员只登记一次）", st.Total)
	}
	if !st.contains("v1") || !st.contains("v2") {
		t.Error("成员应全部登记")
	}
	if n := len(videoLists["v1"]); n != 1 {
		t.Errorf("反向索引中 v1 → %d 个列表, want 1（不得重复登记同一列表）", n)
	}

	registerListMembers("L1", []string{"v1", "v2"})
	if st.Total != 2 {
		t.Errorf("重复登记后 Total = %d, want 2", st.Total)
	}

	markVideoDone(1, t.TempDir(), "v1")
	if st.contains("v1") {
		t.Error("markVideoDone 后 v1 应移出待处理集合")
	}
	if !videoDone["v1"] {
		t.Error("markVideoDone 应记录 videoDone")
	}

	// 重复展开同一列表时，已完成的视频不得被加回集合：它的任务已被 taskQueueAdder
	// 去重、不会再跑，加回集合会让集合永远清不空、list_*.json 永不回收。
	registerListMembers("L1", []string{"v1", "v2"})
	if st.contains("v1") {
		t.Error("已成功下载的视频不得被重复展开重新加回待处理集合")
	}
	if st.Total != 2 {
		t.Errorf("Total = %d, want 2", st.Total)
	}
}

// --- 4. 反向索引：完成时更新所有包含该视频的列表 ---

func TestMarkVideoDoneUpdatesEveryContainingList(t *testing.T) {
	// 审计遗漏维度：taskQueueAdder 的去重是全局的，任务只带**第一个**列表的 ListID。
	// 若完成时只更新那一个列表，其它列表的 pending 永远清不空、它们的
	// list_*.json 永远不会被回收。
	resetListState(t)
	dirs := newCLIDirs(t)
	registerListMembers("L1", []string{"v1", "v2"})
	registerListMembers("L2", []string{"v1", "v2"})

	caches := map[string]string{
		"L1": filepath.Join(dirs.cache, "list_L1.json"),
		"L2": filepath.Join(dirs.cache, "list_L2.json"),
	}
	for id, path := range caches {
		writeFile(t, path, `["v1","v2"]`)
		if n := len(videoLists["v1"]); n != 2 {
			t.Errorf("v1 应在 %d 个列表的反向索引里, want 2（%s）", n, id)
		}
	}

	markVideoDone(1, dirs.cache, "v1")
	for _, id := range []string{"L1", "L2"} {
		if listProgress[id].contains("v1") {
			t.Errorf("%s 中的 v1 未随完成被移出（只更新 task.ListID 的旧逻辑会漏掉另一个列表）", id)
		}
		mustExist(t, caches[id], "尚余 v2 未完成，"+id+" 缓存必须保留")
	}

	markVideoDone(1, dirs.cache, "v2")
	for _, id := range []string{"L1", "L2"} {
		if !listProgress[id].isEmpty() {
			t.Errorf("%s 应已清空", id)
		}
		mustNotExist(t, caches[id], "两个列表都已完成，"+id+" 缓存应被回收")
	}
}

// --- 5. clearListCacheIfEmpty：只在集合为空时删除 ---

func TestClearListCacheIfEmptyOnlyDeletesWhenEmpty(t *testing.T) {
	resetListState(t)
	dirs := newCLIDirs(t)
	registerListMembers("L1", []string{"v1"})
	listCache := filepath.Join(dirs.cache, "list_L1.json")

	writeFile(t, listCache, `["v1"]`)
	clearListCacheIfEmpty(1, dirs.cache, "L1")
	mustExist(t, listCache, "待处理集合非空时不得删除（旧实现在计数提前归零时会删）")

	markVideoDone(1, dirs.cache, "v1")
	if !listProgress["L1"].isEmpty() {
		t.Fatal("v1 完成后集合应为空")
	}
	writeFile(t, listCache, `["v1"]`)
	clearListCacheIfEmpty(1, dirs.cache, "L1")
	mustNotExist(t, listCache, "集合为空时应回收 list 缓存")

	unknown := filepath.Join(dirs.cache, "list_L9.json")
	writeFile(t, unknown, `[]`)
	clearListCacheIfEmpty(1, dirs.cache, "L9")
	mustExist(t, unknown, "未知列表不得被删除")
}

// --- 6. downloadOne：成功判定依据下载返回值，而非「文件是否存在」 ---

func TestDownloadOneAccounting(t *testing.T) {
	const body = "video-bytes-0123456789"

	tests := []struct {
		name string
		// handler 为空表示「预置成品文件，服务端不该被访问」。
		handler func(w http.ResponseWriter)
		preFile bool
		wantOK  bool
	}{
		{
			name:    "正常下载",
			handler: func(w http.ResponseWriter) { _, _ = w.Write([]byte(body)) },
			wantOK:  true,
		},
		{
			name:    "服务端 500",
			handler: func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
			wantOK:  false,
		},
		{
			// P0-1 在 CLI 记账层要解决的问题：服务端声明了总长却提前断流。
			// 下载器会返回错误并把残缺内容留在 .tmp（绝不改名），因此 downloadOne
			// 必须据此判失败，而不是靠「文件存在」。
			name: "声明总长后提前断流",
			handler: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", "100000")
				_, _ = w.Write([]byte("short"))
			},
			wantOK: false,
		},
		{
			name:    "成品文件已存在",
			preFile: true,
			wantOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetListState(t)
			dirs := newCLIDirs(t)

			var hits atomic.Int32
			var srv *httptest.Server
			if tc.handler != nil {
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					tc.handler(w)
				}))
				defer srv.Close()
			}

			registerListMembers("L1", []string{"v1"})
			infoCache := filepath.Join(dirs.cache, "info_v1.json")
			writeFile(t, infoCache, "{}")
			listCache := filepath.Join(dirs.cache, "list_L1.json")
			writeFile(t, listCache, `["v1"]`)

			videoPath := filepath.Join(dirs.down, "v1.mp4")
			if tc.preFile {
				writeFile(t, videoPath, body)
			}

			dataURL := "http://127.0.0.1:1/unused"
			if srv != nil {
				dataURL = srv.URL + "/v1.mp4"
			}
			cfg := &config.Config{CacheDir: dirs.cache, DownDir: dirs.down, ClearCache: true}
			// 用 httptest 的 client（只连本地假服务）；MaxRetries=1 + 短退避避免慢测。
			d := downloader.NewDownloaderWithClient("", httpClientFor(srv))
			d.MaxRetries = 1
			d.RetryInterval = time.Millisecond

			meta := scraper.VideoMetadata{
				VideoID:       "v1",
				Title:         "title-v1",
				DataURL:       dataURL,
				VideoFilePath: videoPath,
			}

			if got := downloadOne(1, cfg, d, &meta); got != tc.wantOK {
				t.Fatalf("downloadOne = %v, want %v", got, tc.wantOK)
			}

			status := listProgress["L1"]
			if tc.wantOK {
				if status.contains("v1") {
					t.Error("成功下载后 v1 应已移出待处理集合")
				}
				if !videoDone["v1"] {
					t.Error("成功下载后应记录 videoDone")
				}
				mustNotExist(t, infoCache, "成功下载后应清理 info_*.json 缓存")
				if b, err := os.ReadFile(videoPath); err != nil || string(b) != body {
					t.Errorf("成品文件内容 = %q (err=%v), want %q", b, err, body)
				}
			} else {
				// 反向证据：旧实现丢弃 d.DownloadWithRetry 的返回值，改用下载后的
				// os.Stat 判成功，于是失败会被当成成功（清缓存、递减列表进度）。
				if !status.contains("v1") {
					t.Error("下载失败不得移出待处理集合，否则 list_*.json 会被提前回收、续传能力丢失")
				}
				if videoDone["v1"] {
					t.Error("下载失败不得记录 videoDone")
				}
				mustExist(t, infoCache, "下载失败时 info 缓存应保留（ClearCache 只对成功生效）")
				mustNotExist(t, videoPath, "失败不得留下被改名的成品文件")
			}

			// 列表缓存的保留/回收与 downloadOne 的记账一致：这里只断言「非空必保留」。
			if !tc.wantOK {
				clearListCacheIfEmpty(1, dirs.cache, "L1")
				mustExist(t, listCache, "集合非空时 list 缓存必须保留")
			}
			if tc.preFile && hits.Load() != 0 {
				t.Errorf("成品文件已存在时服务端被访问 %d 次, want 0（不得重复下载）", hits.Load())
			}
		})
	}
}

// httpClientFor 返回只连本地假测试服务的 client（srv 为 nil 时返回一个不会用到
// 任何真实地址的 client）。
func httpClientFor(srv *httptest.Server) *http.Client {
	if srv != nil {
		return srv.Client()
	}
	return &http.Client{Timeout: 200 * time.Millisecond}
}

// --- 7. 并发：去重入队与进度更新在 -race 下不竞争 ---

func TestTaskQueueAdderIsSafeUnderConcurrency(t *testing.T) {
	resetListState(t)
	const workers = 8
	const perWorker = 50

	q := make(chan VideoTask, workers*perWorker)
	adder := newTaskQueueAdder(q)
	var accepted atomic.Int32

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// 所有 worker 提交同一个 ID 集合 → 去重后应恰好 perWorker 个。
				if adder.add(VideoTask{VideoID: fmt.Sprintf("v%d", i), ListID: "L1"}) {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(q)

	queued := 0
	for range q {
		queued++
	}
	if got := int(accepted.Load()); got != perWorker {
		t.Errorf("accepted = %d, want %d", got, perWorker)
	}
	if queued != perWorker {
		t.Errorf("入队数 = %d, want %d", queued, perWorker)
	}
}

func TestMarkVideoDoneConcurrentWithRegister(t *testing.T) {
	resetListState(t)
	dirs := newCLIDirs(t)
	members := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		members = append(members, fmt.Sprintf("v%d", i))
	}
	registerListMembers("L1", members)
	registerListMembers("L2", members)

	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(videoID string) {
			defer wg.Done()
			markVideoDone(1, dirs.cache, videoID)
		}(m)
	}
	// 与完成并发地重复展开，验证 progressMu 下的集合操作不竞争。
	wg.Add(1)
	go func() {
		defer wg.Done()
		registerListMembers("L1", members)
	}()
	wg.Wait()

	for _, id := range []string{"L1", "L2"} {
		if !listProgress[id].isEmpty() {
			t.Errorf("%s 应在全部成员完成后清空", id)
		}
	}
}
