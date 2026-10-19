package idutil

// 本文件全部离线：只做纯函数与路径拼接断言，不触网、不起浏览器。
//
// 关键用例来自已复现的缺陷：未校验的 ID 经 filepath.Join 的 Clean 之后会逃出
// 缓存目录。这些用例同时锁住两件事——形状校验本身，以及「校验通过后拼接结果
// 一定留在目录内」这条下游不变量。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsRealIDs(t *testing.T) {
	for _, id := range []string{
		"405852",           // hanime1.me 的真实视频 ID（纯数字）
		"1",                // 最短
		"a",                // 字母开头
		"A_b-1.2",          // 允许的可读别名
		"9223372036854775", // 长数字
	} {
		if err := Validate(id); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", id, err)
		}
		if !IsSafe(id) {
			t.Errorf("IsSafe(%q) = false, want true", id)
		}
	}
}

// TestValidateRejectsTraversalAndHostileShapes 锁住已复现的穿越面与相邻的危险形状。
func TestValidateRejectsTraversalAndHostileShapes(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"slash escape", "a/../../b"},
		{"dotdot prefix", "../../etc/passwd"},
		{"bare dotdot", ".."},
		{"single dot", "."},
		{"backslash", `a\..\..\b`},
		{"encoded slash", `..%2f..`},
		{"null byte", "405852\x00.json"},
		{"empty", ""},
		{"leading dash", "-405852"},
		{"leading dot", ".hidden"},
		{"space", "40 5852"},
		{"query char", "405852?x=1"},
		{"html", "<img src=x onerror=alert(1)>"},
		{"unicode digit", "４０５８５２"},
		{"over length", strings.Repeat("a", MaxLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.id)
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want error", tc.id)
			}
			if !errors.Is(err, ErrInvalidID) {
				t.Errorf("Validate(%q) err = %v, want errors.Is ErrInvalidID", tc.id, err)
			}
			if IsSafe(tc.id) {
				t.Errorf("IsSafe(%q) = true, want false", tc.id)
			}
		})
	}
}

// TestValidatedIDStaysInsideCacheDir 是下游不变量：校验通过的 ID 拼进
// info_/list_ 缓存名后，结果必须仍在 cacheDir 之内。
// 反向证据：把校验去掉，"a/../../b" 会解析成 cacheDir 之外的 "b.json"。
func TestValidatedIDStaysInsideCacheDir(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 在 cacheDir 之外放一个诱饵文件，穿越成功就意味着能碰到它。
	outside := filepath.Join(dir, "b.json")
	if err := os.WriteFile(outside, []byte("do-not-touch"), 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}

	for _, id := range []string{"405852", "A_b-1.2", "1"} {
		if err := Validate(id); err != nil {
			t.Fatalf("Validate(%q) = %v", id, err)
		}
		for _, tmpl := range []string{"info_%s.json", "list_%s.json"} {
			got := filepath.Join(cacheDir, fmt.Sprintf(tmpl, id))
			rel, err := filepath.Rel(cacheDir, got)
			if err != nil {
				t.Fatalf("rel: %v", err)
			}
			if rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
				t.Errorf("id=%q tmpl=%s escaped cacheDir: %s", id, tmpl, got)
			}
		}
	}

	// 反向证据：未校验的 ID 确实会落到 cacheDir 之外。
	// 这里只断言路径解析结果，不真的写/删任何文件。
	unguarded := filepath.Join(cacheDir, fmt.Sprintf("info_%s.json", "a/../../b"))
	if unguarded != outside {
		t.Fatalf("反向证据漂移：期望 %q，实际 %q（穿越用例需重新设计）", outside, unguarded)
	}
}
