package chrome

// 本文件是 chrome 包的第一个测试文件。全部离线：只用 httptest 在 127.0.0.1 上
// 起我们自己的服务，不拨号任何真实 CDP 端点、不拉起 Chrome。
//
// 覆盖重点是 Tranche A 改动的语义：证书校验默认开启、探测有确定的时间界。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cdpVersionServer 模拟一个 /json/version 端点。
func cdpVersionServer(t *testing.T, wsDebuggerURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 编码失败会让用例后续断言变成误导性的「超时」，所以这里直接 Fatal。
		if err := json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "Chrome/120.0.0.0",
			"Protocol-Version":     "1.3",
			"webSocketDebuggerUrl": wsDebuggerURL,
		}); err != nil {
			t.Errorf("encode fixture: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeVerifiesTLSCertificatesByDefault 是 A4 的回归用例。
//
// 反向证据：修复前 fetchWebSocketDebuggerURL 无条件写 InsecureSkipVerify: true，
// 因此对同一个自签名 httptest 服务，旧实现会**成功**返回——证书形同虚设，
// 中间人可以随意伪造「Chrome 调试端点」并接管 DevTools。
func TestProbeVerifiesTLSCertificatesByDefault(t *testing.T) {
	const debuggerURL = "ws://127.0.0.1:9222/devtools/browser/fixed-id"
	srv := cdpVersionServer(t, debuggerURL)
	endpoint := srv.URL + "/json/version"

	// 默认（零值 ProbeOptions）：必须因为证书无法验证而失败。
	if _, err := GetWebSocketDebuggerURLWith(endpoint, 2*time.Second, ProbeOptions{}); err == nil {
		t.Fatalf("默认探测竟接受了一个自签名证书，证书校验没有生效")
	} else if !strings.Contains(strings.ToLower(err.Error()), "x509") &&
		!strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Errorf("默认探测失败原因 = %v, want 证书相关（x509）", err)
	}

	// 显式打开开关：同一个端点应当成功，说明开关确实控制的是这一件事。
	got, err := GetWebSocketDebuggerURLWith(endpoint, 2*time.Second, ProbeOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("InsecureSkipVerify=true 时探测失败: %v", err)
	}
	// 127.0.0.1 开头的 debugger URL 会被改写成请求主机，这里只断言路径部分保留。
	if !strings.Contains(got, "/devtools/browser/fixed-id") {
		t.Errorf("探测结果 = %q, want 含 /devtools/browser/fixed-id", got)
	}
}

// TestSecureProbeKeepsVerificationOn 锁住包内默认选项的取值方向：
// 一旦有人把 secureProbe 改成跳过校验，这条用例就红。
func TestSecureProbeKeepsVerificationOn(t *testing.T) {
	if secureProbe.InsecureSkipVerify {
		t.Errorf("secureProbe.InsecureSkipVerify = true, want false（默认必须校验证书）")
	}
}

// TestWebSocketEndpointsPassThroughWithoutProbe 覆盖 ws:// / wss:// 短路分支：
// 这类地址不需要探测，也就与证书开关无关。
func TestWebSocketEndpointsPassThroughWithoutProbe(t *testing.T) {
	for _, uri := range []string{
		"ws://host:9222/devtools/browser/x",
		"wss://host:9222/devtools/browser/x",
	} {
		got, err := GetWebSocketDebuggerURLWith(uri, 50*time.Millisecond, ProbeOptions{})
		if err != nil {
			t.Errorf("GetWebSocketDebuggerURLWith(%q) err = %v, want nil", uri, err)
			continue
		}
		if got != uri {
			t.Errorf("GetWebSocketDebuggerURLWith(%q) = %q, want 原样返回", uri, got)
		}
	}

	if _, err := GetWebSocketDebuggerURLWith("", time.Second, ProbeOptions{}); err == nil {
		t.Errorf("空端点应当报错")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("空端点错误 = %v, want 含 empty", err)
	}
}

// TestProbeHonorsTimeoutBudget 是探测时间界的契约测试：
// 调用方必须能确定「这一次探测最多花多久」——旧实现同时被 5s 连接预算和
// 30s 就绪循环包住，语义自相矛盾（审计 P2-14）。
func TestProbeHonorsTimeoutBudget(t *testing.T) {
	// 127.0.0.1:1 基本不会有监听者，探测必然失败；关键是失败要落在预算内。
	const budget = 400 * time.Millisecond
	start := time.Now()
	_, err := GetWebSocketDebuggerURLWith("http://127.0.0.1:1/json/version", budget, ProbeOptions{})
	elapsed := time.Since(start)

	if err == nil {
		t.Skip("本机 127.0.0.1:1 竟然有服务在应答，这条时间界用例不适用")
	}
	// 预算内会按 cdpPollInterval 轮询，因此允许略超；但绝不能像旧实现那样
	// 被更外层的 30s 就绪循环拖住。
	if elapsed > budget+2*time.Second {
		t.Errorf("探测耗时 %v 超出预算 %v 太多，deadline 没有被遵守", elapsed, budget)
	}
}

// TestMinDuration 覆盖探测里反复用到的时长钳制。
func TestMinDuration(t *testing.T) {
	cases := []struct {
		a, b, want time.Duration
	}{
		{0, 5 * time.Second, 5 * time.Second},
		{5 * time.Second, 0, 5 * time.Second},
		{-1, 3 * time.Second, 3 * time.Second},
		{2 * time.Second, 7 * time.Second, 2 * time.Second},
		{7 * time.Second, 2 * time.Second, 2 * time.Second},
		{4 * time.Second, 4 * time.Second, 4 * time.Second},
	}
	for _, tc := range cases {
		if got := minDuration(tc.a, tc.b); got != tc.want {
			t.Errorf("minDuration(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestDevToolsNewTabURLSchemeMapping 覆盖 ws→http / wss→https 的映射：
// CreateTab 靠它把 WebSocket 地址换算成 DevTools HTTP 端点。
func TestDevToolsNewTabURLSchemeMapping(t *testing.T) {
	got, err := devToolsNewTabURL("ws://127.0.0.1:9222/devtools/browser/abc", "about:blank")
	if err != nil {
		t.Fatalf("devToolsNewTabURL: %v", err)
	}
	if !strings.HasPrefix(got, "http://127.0.0.1:9222/json/new?") {
		t.Errorf("ws 端点换算结果 = %q, want http://127.0.0.1:9222/json/new?...", got)
	}
	if !strings.Contains(got, "about%3Ablank") && !strings.Contains(got, "about:blank") {
		t.Errorf("目标 URL 没有出现在新标签页请求里: %q", got)
	}

	gotWSS, err := devToolsNewTabURL("wss://example.invalid:9222/devtools/browser/abc", "about:blank")
	if err != nil {
		t.Fatalf("devToolsNewTabURL(wss): %v", err)
	}
	if !strings.HasPrefix(gotWSS, "https://example.invalid:9222/json/new?") {
		t.Errorf("wss 端点换算结果 = %q, want https://... 前缀", gotWSS)
	}
}

// TestCreateTabRejectsEmptyInputs 是入参契约：空 wsURL 直接失败，不发请求。
func TestCreateTabRejectsEmptyInputs(t *testing.T) {
	if _, err := CreateTab("", "about:blank", time.Second); err == nil {
		t.Errorf("CreateTab(空 wsURL) 应当报错")
	}
}
