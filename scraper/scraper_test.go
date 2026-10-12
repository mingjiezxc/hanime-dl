package scraper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件只测试纯函数：不启动 Chrome、不访问网络。
// 所有涉及 chromedp 的流程（GetPlaylist / ResolveVideoInfo / RefreshVideoDataURL /
// fetchDownloadLinks / fetchHighResCover）都无法离线运行，因此不被覆盖；
// 它们依赖的可抽取逻辑（JS 结果转换、分辨率选择、目录名推导、文件名截断、
// 搜索 URL 构造）全部抽成纯函数后在此验证。

// shortDir 是一个足够短、不会触碰全路径预算的目录。
const shortDir = "/d"

func TestTruncateFilenameKeepsExtension(t *testing.T) {
	tests := []struct {
		name     string
		fullDir  string
		base     string
		ext      string
		maxBytes int
		want     string
	}{
		{
			name:     "远低于上限时原样返回",
			fullDir:  shortDir,
			base:     "short",
			ext:      ".mp4",
			maxBytes: 180,
			want:     "short.mp4",
		},
		{
			name:     "恰好等于上限时不截断",
			fullDir:  shortDir,
			base:     strings.Repeat("a", 180),
			ext:      ".jpg",
			maxBytes: 180,
			want:     strings.Repeat("a", 180) + ".jpg",
		},
		{
			name:     "纯 ASCII 超限时截到上限",
			fullDir:  shortDir,
			base:     strings.Repeat("a", 300),
			ext:      ".mp4",
			maxBytes: 180,
			want:     strings.Repeat("a", 180) + ".mp4",
		},
		{
			name:     "中文超限时截到整数个汉字",
			fullDir:  shortDir,
			base:     strings.Repeat("汉", 100), // 300 字节
			ext:      ".mp4",
			maxBytes: 180, // 60 个汉字
			want:     strings.Repeat("汉", 60) + ".mp4",
		},
		{
			name:     "中英混排截断不产生半个汉字",
			fullDir:  shortDir,
			base:     "动漫作品Title测试", // 12 + 5 + 6 = 23 字节
			ext:      ".mp4",
			maxBytes: 18, // 落在一个汉字中间，须回退
			want:     "动漫作品Title.mp4",
		},
		{
			name:     "扩展名始终保留",
			fullDir:  shortDir,
			base:     strings.Repeat("x", 1000),
			ext:      ".jpg",
			maxBytes: 200,
			want:     strings.Repeat("x", 200) + ".jpg",
		},
		{
			name:    "目录较长时按全路径预算收紧基础名",
			fullDir: "/" + strings.Repeat("d", 99), // 长度 100
			base:    strings.Repeat("a", 300),
			ext:     ".mp4",
			// budget = 240 - 100 - 1 - 4 = 135
			maxBytes: 180,
			want:     strings.Repeat("a", 135) + ".mp4",
		},
		{
			name:    "预算不足时保底到 minBaseNameBytes",
			fullDir: "/" + strings.Repeat("d", 224), // 长度 225
			base:    strings.Repeat("a", 300),
			ext:     ".mp4",
			// budget = 240 - 225 - 1 - 4 = 10 < 16 → 保底 16
			maxBytes: 180,
			want:     strings.Repeat("a", minBaseNameBytes) + ".mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateFilename(tt.fullDir, tt.base, tt.ext, tt.maxBytes)
			if got != tt.want {
				t.Fatalf("truncateFilename() = %q (len=%d), want %q (len=%d)", got, len(got), tt.want, len(tt.want))
			}
			if !strings.HasSuffix(got, tt.ext) {
				t.Fatalf("truncateFilename() = %q 丢失了扩展名 %q", got, tt.ext)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateFilename() = %q 不是合法的 UTF-8", got)
			}
			// 截断只能发生在基础名上：总长不得超过「基础名上限 + 扩展名」。
			baseBudget := tt.maxBytes
			if b := maxPathBytes - len(tt.fullDir) - 1 - len(tt.ext); b < baseBudget {
				baseBudget = b
			}
			if baseBudget < minBaseNameBytes {
				baseBudget = minBaseNameBytes
			}
			if len(got) > baseBudget+len(tt.ext) {
				t.Fatalf("truncateFilename() = %q 长度 %d 超出基础名预算 %d + 扩展名 %d", got, len(got), baseBudget, len(tt.ext))
			}
		})
	}
}

func TestTruncateFilenameDefaultBudget(t *testing.T) {
	// maxBytes <= 0 时回落到 maxBaseNameBytes。
	got := truncateFilename(shortDir, strings.Repeat("b", 500), ".mp4", 0)
	if want := strings.Repeat("b", maxBaseNameBytes) + ".mp4"; got != want {
		t.Fatalf("truncateFilename(maxBytes=0) = %q (len=%d), want len=%d", got, len(got), len(want))
	}
}

// TestTruncateBytesNeverSplitsRunes 覆盖所有可能的截断点，确保永不截出半个多字节字符。
func TestTruncateBytesNeverSplitsRunes(t *testing.T) {
	inputs := []string{
		"动漫作品Title测试",
		strings.Repeat("汉", 10),
		"a动漫b作品c",
		"日本語テスト",
		"plain ascii only",
	}

	for _, in := range inputs {
		for maxBytes := 0; maxBytes <= len(in)+2; maxBytes++ {
			got := truncateBytes(in, maxBytes)
			if !utf8.ValidString(got) {
				t.Fatalf("truncateBytes(%q, %d) = %q 不是合法 UTF-8", in, maxBytes, got)
			}
			if !strings.HasPrefix(in, got) {
				t.Fatalf("truncateBytes(%q, %d) = %q 不是原串前缀", in, maxBytes, got)
			}
			if maxBytes > 0 && len(got) > maxBytes {
				t.Fatalf("truncateBytes(%q, %d) = %q 长度 %d 超过上限", in, maxBytes, got, len(got))
			}
		}
	}

	if got := truncateBytes("abc", 0); got != "" {
		t.Fatalf("truncateBytes(maxBytes=0) = %q, want 空串", got)
	}
	if got := truncateBytes("abc", -1); got != "" {
		t.Fatalf("truncateBytes(maxBytes=-1) = %q, want 空串", got)
	}
}

func TestDirNameFromTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "圆括号开头取到右括号", title: "(合集) 某某作品", want: "(合集)"},
		{name: "方括号开头取到右括号", title: "[社团] 某某作品", want: "[社团]"},
		{name: "无标注回落为首词", title: "某某 作品 第二集", want: "某某"},
		{name: "括号未闭合回落为首词", title: "(未闭合 某某作品", want: "(未闭合"},
		{name: "圆括号开头但用方括号闭合", title: "(abc] 某某", want: "(abc]"},
		{name: "斜杠被替换", title: "A/B 某某", want: "A_B"},
		{name: "反斜杠被替换", title: `A\B 某某`, want: "A_B"},
		{name: "两种分隔符都替换", title: `A/B\C rest`, want: "A_B_C"},
		{name: "前导空白被裁剪", title: "  某某 作品", want: "某某"},
		{name: "空标题返回空串", title: "", want: ""},
		{name: "纯空白返回空串", title: "   ", want: ""},
		{name: "括号内含斜杠一并替换", title: "(A/B) 作品", want: "(A_B)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dirNameFromTitle(tt.title); got != tt.want {
				t.Fatalf("dirNameFromTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestSanitizeFileName(t *testing.T) {
	if got, want := sanitizeFileName(`a/b\c`), "a_b_c"; got != want {
		t.Fatalf("sanitizeFileName() = %q, want %q", got, want)
	}
	if got, want := sanitizeFileName("普通 名字"), "普通 名字"; got != want {
		t.Fatalf("sanitizeFileName() = %q, want %q", got, want)
	}
}

func TestBuildMediaFilePaths(t *testing.T) {
	dir := filepath.Join("downloads", "(合集) 某某作品")
	imgPath, videoPath := buildMediaFilePaths(dir, "(合集) 某某作品")

	if !strings.HasSuffix(imgPath, ".jpg") {
		t.Fatalf("图片路径 = %q, 缺少 .jpg 扩展名", imgPath)
	}
	if !strings.HasSuffix(videoPath, ".mp4") {
		t.Fatalf("视频路径 = %q, 缺少 .mp4 扩展名", videoPath)
	}
	if filepath.Dir(imgPath) != dir || filepath.Dir(videoPath) != dir {
		t.Fatalf("路径目录 = %q / %q, want %q", filepath.Dir(imgPath), filepath.Dir(videoPath), dir)
	}
	imgBase := strings.TrimSuffix(filepath.Base(imgPath), ".jpg")
	videoBase := strings.TrimSuffix(filepath.Base(videoPath), ".mp4")
	if imgBase != videoBase {
		t.Fatalf("图片与视频基础名不一致: %q vs %q", imgBase, videoBase)
	}
	if imgBase != sanitizeFileName("(合集) 某某作品") {
		t.Fatalf("基础名 = %q, want %q", imgBase, sanitizeFileName("(合集) 某某作品"))
	}
}

func TestPickByResolution(t *testing.T) {
	links := []downloadLink{
		{Resolution: "1080p", URL: "https://example.test/1080.mp4"},
		{Resolution: "720p", URL: "https://example.test/720.mp4"},
		{Resolution: "480p", URL: "https://example.test/480.mp4"},
	}

	t.Run("精确命中", func(t *testing.T) {
		got, matched := pickByResolution(links, "720")
		if !matched {
			t.Fatal("matched = false, want true")
		}
		if got.URL != "https://example.test/720.mp4" {
			t.Fatalf("URL = %q", got.URL)
		}
	})

	t.Run("子串包含即命中", func(t *testing.T) {
		got, matched := pickByResolution(links, "080")
		if !matched || got.Resolution != "1080p" {
			t.Fatalf("got %+v matched=%v, want 1080p/true", got, matched)
		}
	})

	t.Run("命中第一条匹配的链接", func(t *testing.T) {
		got, matched := pickByResolution(links, "p")
		if !matched || got.Resolution != "1080p" {
			t.Fatalf("got %+v matched=%v, want 1080p/true", got, matched)
		}
	})

	t.Run("未命中回落第一条", func(t *testing.T) {
		got, matched := pickByResolution(links, "2160")
		if matched {
			t.Fatal("matched = true, want false")
		}
		if got.URL != links[0].URL {
			t.Fatalf("URL = %q, want 回落 %q", got.URL, links[0].URL)
		}
	})

	t.Run("空分辨率请求回落第一条", func(t *testing.T) {
		got, matched := pickByResolution(links, "")
		if matched {
			t.Fatal("matched = true, want false")
		}
		if got.URL != links[0].URL {
			t.Fatalf("URL = %q, want 回落 %q", got.URL, links[0].URL)
		}
	})

	t.Run("links 为空返回零值", func(t *testing.T) {
		got, matched := pickByResolution(nil, "1080")
		if matched {
			t.Fatal("matched = true, want false")
		}
		if got != (downloadLink{}) {
			t.Fatalf("got %+v, want 零值", got)
		}
	})

	t.Run("空切片返回零值", func(t *testing.T) {
		got, matched := pickByResolution([]downloadLink{}, "1080")
		if matched || got != (downloadLink{}) {
			t.Fatalf("got %+v matched=%v, want 零值/false", got, matched)
		}
	})
}

func TestSelectDataURL(t *testing.T) {
	links := []downloadLink{
		{Resolution: "1080p", URL: "https://example.test/1080.mp4"},
		{Resolution: "720p", URL: "https://example.test/720.mp4"},
	}

	t.Run("命中请求分辨率", func(t *testing.T) {
		s := &Scraper{resolution: "1080"}
		if got := s.selectDataURL(links, "v1"); got != links[0].URL {
			t.Fatalf("selectDataURL() = %q, want %q", got, links[0].URL)
		}
	})

	t.Run("未命中回落第一条", func(t *testing.T) {
		s := &Scraper{resolution: "2160"}
		if got := s.selectDataURL(links, "v1"); got != links[0].URL {
			t.Fatalf("selectDataURL() = %q, want %q", got, links[0].URL)
		}
	})

	t.Run("未指定分辨率取第一条", func(t *testing.T) {
		s := &Scraper{}
		if got := s.selectDataURL(links, "v1"); got != links[0].URL {
			t.Fatalf("selectDataURL() = %q, want %q", got, links[0].URL)
		}
	})

	t.Run("links 为空返回空串", func(t *testing.T) {
		s := &Scraper{resolution: "1080"}
		if got := s.selectDataURL(nil, "v1"); got != "" {
			t.Fatalf("selectDataURL() = %q, want 空串", got)
		}
	})

	t.Run("首行缺 URL 时返回空串", func(t *testing.T) {
		s := &Scraper{resolution: "1080"}
		broken := []downloadLink{{Resolution: "1080p", URL: ""}}
		if got := s.selectDataURL(broken, "v1"); got != "" {
			t.Fatalf("selectDataURL() = %q, want 空串", got)
		}
	})
}

func TestToDownloadLinks(t *testing.T) {
	if got := toDownloadLinks(nil); got != nil {
		t.Fatalf("toDownloadLinks(nil) = %+v, want nil", got)
	}
	if got := toDownloadLinks([]map[string]string{}); got != nil {
		t.Fatalf("toDownloadLinks(empty) = %+v, want nil", got)
	}

	raw := []map[string]string{
		{"resolution": "1080p", "url": "https://example.test/1080.mp4"},
		{}, // 保持行数语义：不填充、不过滤
		{"resolution": "480p"},
	}
	got := toDownloadLinks(raw)
	if len(got) != len(raw) {
		t.Fatalf("len = %d, want %d (行数语义不得改变)", len(got), len(raw))
	}
	if got[0].Resolution != "1080p" || got[0].URL != "https://example.test/1080.mp4" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1] != (downloadLink{}) {
		t.Fatalf("got[1] = %+v, want 零值", got[1])
	}
	if got[2].Resolution != "480p" || got[2].URL != "" {
		t.Fatalf("got[2] = %+v", got[2])
	}
}

// TestCoverSearchURLHasNoGenreFilter 锁定第 1 项缺陷的修复：
// 搜索 URL 不得再带任何固定分类过滤。
func TestCoverSearchURLHasNoGenreFilter(t *testing.T) {
	got := coverSearchURL("某某 作品")

	if strings.Contains(got, "genre") {
		t.Fatalf("coverSearchURL() = %q 仍包含 genre 过滤", got)
	}
	// 原始硬编码值：&genre=%E8%A3%85%E7%95%8C（"装界"）
	if strings.Contains(got, "%E8%A3%85%E7%95%8C") {
		t.Fatalf("coverSearchURL() = %q 仍包含硬编码的装界分类", got)
	}
	if !strings.HasPrefix(got, "https://hanime1.me/search?query=") {
		t.Fatalf("coverSearchURL() = %q, 前缀不符合预期", got)
	}
	if !strings.Contains(got, "%E6%9F%90%E6%9F%90+%E4%BD%9C%E5%93%81") &&
		!strings.Contains(got, "%E6%9F%90%E6%9F%90%20%E4%BD%9C%E5%93%81") {
		t.Fatalf("coverSearchURL() = %q, 未包含 URL 转义后的查询词", got)
	}
}

func TestCoverSearchQueriesFallbackOrder(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  []string
	}{
		{
			name:  "完整标题优先，再放宽为首词",
			title: "某某作品 第二集 (2024)",
			want:  []string{"某某作品 第二集 (2024)", "某某作品"},
		},
		{
			name:  "方括号标注被剥离后取首词",
			title: "[社团] Real Title",
			want:  []string{"[社团] Real Title", "Real"},
		},
		{
			name:  "无标注且无空格时只有一个候选",
			title: "PlainTitle",
			want:  []string{"PlainTitle"},
		},
		{
			name:  "前导空白被裁剪",
			title: "  Padded Title  ",
			want:  []string{"Padded Title", "Padded"},
		},
		{
			name:  "空标题返回 nil",
			title: "",
			want:  nil,
		},
		{
			name:  "纯空白返回 nil",
			title: "   ",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coverSearchQueries(tt.title)
			if len(got) != len(tt.want) {
				t.Fatalf("coverSearchQueries(%q) = %q, want %q", tt.title, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("coverSearchQueries(%q)[%d] = %q, want %q", tt.title, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestCoverSearchURLsAreDistinctAndUnfiltered 断言回落链每一步都是无分类过滤的合法 URL。
func TestCoverSearchURLsAreDistinctAndUnfiltered(t *testing.T) {
	queries := coverSearchQueries("某某作品 第二集 (2024)")
	if len(queries) < 2 {
		t.Fatalf("queries = %q, want 至少两个候选", queries)
	}
	seen := make(map[string]bool, len(queries))
	for _, q := range queries {
		u := coverSearchURL(q)
		if strings.Contains(u, "genre") {
			t.Fatalf("候选 URL %q 仍包含 genre 过滤", u)
		}
		if seen[u] {
			t.Fatalf("候选 URL 重复: %q", u)
		}
		seen[u] = true
	}
}

// TestWriteJSONFileCacheHasNoListID 锁定第 8 项的缓存语义：
// info_<id>.json 不保存 ListID。
func TestWriteJSONFileCacheHasNoListID(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "info_v1.json")

	meta := VideoMetadata{
		VideoID:       "v1",
		Title:         "某某作品",
		ImageURL:      "https://example.test/cover.jpg",
		DataURL:       "https://example.test/v.mp4",
		ImageFilePath: filepath.Join(dir, "某某作品.jpg"),
		VideoFilePath: filepath.Join(dir, "某某作品.mp4"),
		TargetDir:     dir,
		ListID:        "", // 缓存写入时被清空
	}
	writeJSONFile(cacheFile, meta)

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("读取缓存失败: %v", err)
	}
	if strings.Contains(string(data), "list_id") {
		t.Fatalf("缓存内容不应包含 list_id，实际: %s", data)
	}

	var got VideoMetadata
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("缓存不是合法 JSON: %v", err)
	}
	if got.VideoID != "v1" || got.Title != "某某作品" || got.DataURL != "https://example.test/v.mp4" {
		t.Fatalf("缓存往返结果不符: %+v", got)
	}
	if got.ListID != "" {
		t.Fatalf("缓存中的 ListID = %q, want 空", got.ListID)
	}
}

// TestWriteJSONFileOverwritesAndSurvivesMissingDir 只覆盖成功路径与静默失败路径（不 panic）。
func TestWriteJSONFileBehaviour(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "list_x.json")

	writeJSONFile(cacheFile, []string{"a", "b"})
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("读取缓存失败: %v", err)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("播放列表缓存不是合法 JSON: %v", err)
	}
	if len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Fatalf("播放列表缓存往返结果不符: %q", list)
	}

	// 目录不存在：写入失败必须只是打 WARN，不得 panic。
	writeJSONFile(filepath.Join(dir, "missing", "sub", "info.json"), map[string]string{"a": "b"})

	// 不可序列化的值：同样只打 WARN。
	writeJSONFile(filepath.Join(dir, "bad.json"), make(chan int))
}
