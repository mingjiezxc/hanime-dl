// Package idutil 校验视频 ID 与播放列表 ID 的形状。
//
// 为什么需要它：这两个 ID 不只是查询参数，它们会被拼进**磁盘路径**——
//
//	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("info_%s.json", videoID))
//
// 而 filepath.Join 内部会 Clean，未校验的 ID 因此可以改变路径结构。已复现：
//
//	videoID = "a/../../b"  ->  "b.json"   // 逃出 cacheDir
//
// 于是 scraper 的 writeJSONFile 变成任意路径写、os.Remove 变成任意路径删。
// Web 模式的 ID 直接来自 HTTP 请求体，所以这条校验必须在**入口**完成，
// 而不是依赖各调用点的自觉（与 taskQueueAdder 内部加锁同一取向：
// 不变量由被调方保证，而不是由调用方纪律保证）。
//
// 字符集为什么这样定：hanime1.me 的真实 ID 是纯数字（如 405852），放宽到
// 允许中间的 `.`、`_`、`-` 只是为了兼容用户手写的可读别名。关键是**不含任何
// 路径分隔符**（`/`、`\`）与空字节：此时 `info_<id>.json` 永远是单个路径元素，
// Clean 无从穿越（元素只有恰好等于 ".." 才会回退，而 "info_.." 不等于 ".."）。
// 首字符要求字母或数字，避免 ID 以 "-" 开头被下游当成命令行开关。
package idutil

import (
	"errors"
	"fmt"
	"regexp"
)

// MaxLength 是 ID 允许的最大字节数。真实 ID 远短于此，上限的作用是
// 让异常输入尽早失败，同时给下游（文件名、日志、前端渲染）一个确定的长度界。
const MaxLength = 64

// pattern 见包注释的字符集说明：首字符字母或数字，其后允许字母、数字、点、
// 下划线、连字符，总长 1..MaxLength。
var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrInvalidID 是「ID 形状不合法」的哨兵错误。调用方用 errors.Is 判定，
// 并据此映射为 HTTP 400（见 web.statusForError）。
var ErrInvalidID = errors.New("invalid id")

// Validate 报告 id 是否可以安全地用于查询与路径拼接。
//
// 返回的错误里带上原始输入，便于排查；但调用方**不应**把该错误串直接回显给
// 未转义的前端（见 web/index.html 的 escapeHtml）。
func Validate(id string) error {
	if len(id) > MaxLength {
		return fmt.Errorf("%w: length %d exceeds %d", ErrInvalidID, len(id), MaxLength)
	}
	if !pattern.MatchString(id) {
		return fmt.Errorf("%w: %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,%d}", ErrInvalidID, id, MaxLength-1)
	}
	return nil
}

// IsSafe 是 Validate 的布尔形式，供扫描缓存目录这类「跳过而不报错」的场景使用。
func IsSafe(id string) bool {
	return Validate(id) == nil
}
