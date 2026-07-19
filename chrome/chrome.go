package chrome

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	MaxRetries        = 5
	RetryInterval     = 5 * time.Second
	StartupCDPTimeout = 5 * time.Second
	OpenPageTimeout   = 10 * time.Second
	chromeWatchURL    = "https://hanime1.me/watch?v=%s"
	playlistSelector  = `#video-playlist-wrapper a`
)

// Launcher Chrome 启动器
type Launcher struct {
	cmd *exec.Cmd
}

// GetWebSocketDebuggerURL 获取 Chrome WebSocket 调试 URL
func GetWebSocketDebuggerURL(uri string) (string, error) {
	var lastErr error

	// 如果是 WebSocket URL，直接返回
	if strings.HasPrefix(uri, "ws://") || strings.HasPrefix(uri, "wss://") {
		return uri, nil
	}

	for i := 0; i < MaxRetries; i++ {
		wsURL, err := fetchWebSocketDebuggerURL(uri, 10*time.Second)
		if err == nil {
			return wsURL, nil
		}
		lastErr = err
		time.Sleep(RetryInterval)
	}

	return "", fmt.Errorf("failed after %d attempts, last error: %w", MaxRetries, lastErr)
}

// GetWebSocketDebuggerURLWithTimeout 在限定时间内快速检查 CDP 端点
func GetWebSocketDebuggerURLWithTimeout(uri string, timeout time.Duration) (string, error) {
	if uri == "" {
		return "", errors.New("CDP endpoint is empty")
	}

	// 如果是 WebSocket URL，直接返回
	if strings.HasPrefix(uri, "ws://") || strings.HasPrefix(uri, "wss://") {
		return uri, nil
	}

	if timeout <= 0 {
		timeout = StartupCDPTimeout
	}

	deadline := time.Now().Add(timeout)
	var lastErr error

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		requestTimeout := minDuration(2*time.Second, remaining)
		wsURL, err := fetchWebSocketDebuggerURL(uri, requestTimeout)
		if err == nil {
			return wsURL, nil
		}
		lastErr = err

		sleepFor := minDuration(500*time.Millisecond, time.Until(deadline))
		if sleepFor > 0 {
			time.Sleep(sleepFor)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("connection timed out")
	}

	return "", fmt.Errorf("failed to connect within %s: %w", timeout, lastErr)
}

func fetchWebSocketDebuggerURL(uri string, requestTimeout time.Duration) (string, error) {
	// 解析 URL 获取主机
	parsedURL, parseErr := url.Parse(uri)
	baseHost := ""
	if parseErr == nil {
		baseHost = parsedURL.Host
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   requestTimeout,
	}

	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return "", fmt.Errorf("creating request failed: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "curl/7.81.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http client.Do failed: %w", err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("reading response body failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyText := strings.TrimSpace(string(body))
		if bodyText == "" {
			return "", fmt.Errorf("server returned non-200 status: %s", resp.Status)
		}
		return "", fmt.Errorf("server returned non-200 status: %s, body: %s", resp.Status, bodyText)
	}

	var wsInfo struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &wsInfo); err != nil {
		return "", fmt.Errorf("json unmarshal failed: %w", err)
	}

	if wsInfo.WebSocketDebuggerURL == "" {
		return "", errors.New("WebSocketDebuggerURL is empty")
	}

	// 如果是 0.0.0.0 或 127.0.0.1，替换为实际主机
	if baseHost != "" {
		if wsURL, err := url.Parse(wsInfo.WebSocketDebuggerURL); err == nil && wsURL.Host != "" {
			if strings.HasPrefix(wsURL.Host, "0.0.0.0") || strings.HasPrefix(wsURL.Host, "127.0.0.1") {
				wsURL.Host = baseHost
				if parseErr == nil && parsedURL.RawQuery != "" {
					if wsURL.RawQuery == "" {
						wsURL.RawQuery = parsedURL.RawQuery
					} else {
						wsURL.RawQuery = wsURL.RawQuery + "&" + parsedURL.RawQuery
					}
				}
				return wsURL.String(), nil
			}
		}
	}

	return wsInfo.WebSocketDebuggerURL, nil
}

// AutoDetectChrome 扫描常见 Chrome 调试端口
func AutoDetectChrome() string {
	return AutoDetectChromeWithTimeout(0)
}

// AutoDetectChromeWithTimeout 在限定时间内扫描常见 Chrome 调试端口
func AutoDetectChromeWithTimeout(timeout time.Duration) string {
	ports := []int{9222, 9223, 9224, 9225, 9226, 9227, 9228, 9229, 9230}

	log.Println("Scanning for available Chrome instances on common debugging ports...")

	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for _, port := range ports {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		url := fmt.Sprintf("http://localhost:%d/json/version", port)
		dialTimeout := 500 * time.Millisecond
		if !deadline.IsZero() {
			dialTimeout = minDuration(dialTimeout, time.Until(deadline))
		}

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), dialTimeout)
		if err != nil {
			continue
		}
		conn.Close()

		var wsURL string
		if deadline.IsZero() {
			wsURL, err = GetWebSocketDebuggerURL(url)
		} else {
			wsURL, err = GetWebSocketDebuggerURLWithTimeout(url, minDuration(time.Second, time.Until(deadline)))
		}
		if err == nil && wsURL != "" {
			log.Printf("Found Chrome instance on port %d: %s", port, wsURL)
			return wsURL
		}
	}

	log.Println("No Chrome instances found on common debugging ports")
	return ""
}

// EnsureChromeConnection 确保启动阶段存在可用的 Chrome CDP 连接
func EnsureChromeConnection(remoteURL string, timeout time.Duration) (string, error) {
	wsURL, _, err := EnsureChromeConnectionDetailed(remoteURL, timeout)
	return wsURL, err
}

// EnsureChromeConnectionDetailed 返回 CDP 地址以及是否在本次调用中拉起了本地 Chrome
func EnsureChromeConnectionDetailed(remoteURL string, timeout time.Duration) (string, bool, error) {
	if timeout <= 0 {
		timeout = StartupCDPTimeout
	}

	if remoteURL != "" {
		log.Printf("Checking configured Chrome CDP endpoint with %s timeout: %s", timeout, remoteURL)
		wsURL, err := GetWebSocketDebuggerURLWithTimeout(remoteURL, timeout)
		if err == nil && wsURL != "" {
			log.Printf("Successfully connected to Chrome CDP: %s", wsURL)
			return wsURL, false, nil
		}
		log.Printf("Configured Chrome CDP endpoint is unavailable: %v", err)
	} else {
		log.Printf("Chrome CDP endpoint is empty, trying local Chrome immediately")
	}

	if wsURL := AutoDetectChromeWithTimeout(timeout); wsURL != "" {
		return wsURL, false, nil
	}

	log.Println("Attempting to launch local Chrome...")
	wsURL, err := StartLocalChrome()
	if err != nil {
		return "", false, err
	}
	return wsURL, true, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// OpenURL opens a URL in a Chrome tab through the DevTools HTTP endpoint without attaching a CDP session.
func OpenURL(wsURL, targetURL string, timeout time.Duration) error {
	if wsURL == "" {
		return errors.New("CDP websocket URL is empty")
	}
	if targetURL == "" {
		return errors.New("target URL is empty")
	}
	if timeout <= 0 {
		timeout = OpenPageTimeout
	}

	devtoolsURL, err := devToolsNewTabURL(wsURL, targetURL)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPut, devtoolsURL, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		fallbackReq, fallbackErr := http.NewRequest(http.MethodGet, devtoolsURL, http.NoBody)
		if fallbackErr == nil {
			resp, fallbackErr = client.Do(fallbackReq)
			if fallbackErr == nil {
				err = nil
			} else {
				err = fallbackErr
			}
		}
	}
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err = fmt.Errorf("devtools endpoint returned status %s", resp.Status)
		}
	}
	return err
}

// CreateTab 通过 DevTools HTTP 端点在已有浏览器中打开一个新的「标签页」（而非新窗口），
// 返回新标签页的 target id，供 chromedp.WithTargetID 附着使用。
// 这样可以避免 chromedp（v0.13.x）在远程 allocator 下默认以 newWindow=true 创建新窗口的行为。
func CreateTab(wsURL, targetURL string, timeout time.Duration) (string, error) {
	if wsURL == "" {
		return "", errors.New("CDP websocket URL is empty")
	}
	if targetURL == "" {
		targetURL = "about:blank"
	}
	if timeout <= 0 {
		timeout = OpenPageTimeout
	}

	devtoolsURL, err := devToolsNewTabURL(wsURL, targetURL)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPut, devtoolsURL, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		// 兼容旧版 Chrome：回退到 GET
		fallbackReq, fbErr := http.NewRequest(http.MethodGet, devtoolsURL, http.NoBody)
		if fbErr != nil {
			return "", err
		}
		resp, err = client.Do(fallbackReq)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading new tab response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("devtools endpoint returned status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var info struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("parse new tab response failed: %w", err)
	}
	if info.ID == "" {
		return "", errors.New("new tab id is empty")
	}
	return info.ID, nil
}

func devToolsNewTabURL(wsURL, targetURL string) (string, error) {
	parsedWSURL, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse websocket debugger url failed: %w", err)
	}
	scheme := "http"
	if parsedWSURL.Scheme == "wss" {
		scheme = "https"
	}
	devtoolsBase := url.URL{
		Scheme: scheme,
		Host:   parsedWSURL.Host,
		Path:   "/json/new",
	}
	return devtoolsBase.String() + "?" + url.QueryEscape(targetURL), nil
}

// findChromeBinary 查找 Chrome/Chromium 二进制文件（跨平台）
func findChromeBinary() string {
	var paths []string

	switch runtime.GOOS {
	case "windows":
		paths = []string{
			// 常见 Windows 安装路径
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Users\%USERNAME%\AppData\Local\Google\Chrome\Application\chrome.exe`,
			// 相对路径
			`chrome.exe`,
			`google-chrome.exe`,
		}
		// 替换环境变量
		for i, path := range paths {
			paths[i] = os.ExpandEnv(path)
		}

		// Windows 使用 where 命令
		if out, err := exec.Command("where", "chrome").Output(); err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				log.Printf("Found Chrome via 'where': %s", path)
				return path
			}
		}
		if out, err := exec.Command("where", "google-chrome").Output(); err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				log.Printf("Found Google Chrome via 'where': %s", path)
				return path
			}
		}

	case "darwin": // macOS
		paths = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}

	case "linux":
		fallthrough
	default:
		paths = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome-beta",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/google-chrome",
			"/snap/google-chrome/current/usr/bin/google-chrome",
			"/opt/google/chrome/google-chrome",
			"google-chrome",
			"chromium",
			"chromium-browser",
		}

		// Linux/macOS 使用 which 命令
		if out, err := exec.Command("which", "google-chrome").Output(); err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				log.Printf("Found Chrome via 'which': %s", path)
				return path
			}
		}
		if out, err := exec.Command("which", "chromium").Output(); err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				log.Printf("Found Chromium via 'which': %s", path)
				return path
			}
		}
		if out, err := exec.Command("which", "chromium-browser").Output(); err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				log.Printf("Found Chromium-browser via 'which': %s", path)
				return path
			}
		}
	}

	// 检查预设路径
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			log.Printf("Found Chrome binary at: %s", path)
			return path
		}
	}

	log.Printf("Chrome/Chromium not found in common locations for %s", runtime.GOOS)
	return ""
}

// StartLocalChrome 启动本地 Chrome 实例（跨平台）
func StartLocalChrome() (string, error) {
	log.Println("No Chrome instance found. Attempting to launch local Chrome...")

	chromePath := findChromeBinary()
	if chromePath == "" {
		return "", errors.New("Chrome/Chromium binary not found. Please install Chrome or set the CHROME_PATH environment variable")
	}

	// 查找可用端口
	var debugPort int
	for port := 9222; port <= 9230; port++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 100*time.Millisecond)
		if err != nil {
			debugPort = port
			break
		}
		conn.Close()
	}

	if debugPort == 0 {
		return "", errors.New("no available debugging port found (tried 9222-9230)")
	}

	// 创建临时用户数据目录
	tempDir, err := os.MkdirTemp("", "chrome-profile-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	log.Printf("Launching Chrome with debug port %d and profile: %s", debugPort, tempDir)

	args := []string{
		"--remote-debugging-port=" + fmt.Sprintf("%d", debugPort),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=TranslateUI",
		"--disable-ipc-flooding-protection",
		"--disable-component-extensions-with-background-pages",
		"--disable-default-apps",
		"--user-data-dir=" + tempDir,
		"--disable-blink-features=AutomationControlled",
	}

	// Windows 需要额外参数
	if runtime.GOOS == "windows" {
		args = append(args, "--no-sandbox")
	}

	launcher := &Launcher{}
	launcher.cmd = exec.Command(chromePath, args...)
	launcher.cmd.Stdout = os.Stdout
	launcher.cmd.Stderr = os.Stderr

	// 平台特定的进程管理
	setProcessGroupAttr(launcher.cmd)

	if err := launcher.cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start Chrome: %w", err)
	}

	log.Printf("Chrome launched with PID %d, waiting for it to be ready...", launcher.cmd.Process.Pid)

	// 等待 Chrome 启动
	startTime := time.Now()
	for time.Since(startTime) < 30*time.Second {
		url := fmt.Sprintf("http://localhost:%d/json/version", debugPort)
		wsURL, err := GetWebSocketDebuggerURL(url)
		if err == nil && wsURL != "" {
			log.Printf("Chrome is ready! WebSocket URL: %s", wsURL)
			return wsURL, nil
		}
		time.Sleep(1 * time.Second)
	}

	return "", errors.New("Chrome failed to become ready within 30 seconds")
}

// Cleanup 清理 Chrome 进程（跨平台）
func (l *Launcher) Cleanup() {
	if l.cmd != nil && l.cmd.Process != nil {
		log.Println("Cleaning up Chrome process...")
		cleanupProcess(l.cmd)
	}
}

// ExtractPlaylist 提取播放列表
func ExtractPlaylist(wsURL, videoID string) ([]string, error) {
	ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel1()

	allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(ctx1, wsURL, chromedp.NoModifyURL)
	defer cancelAllocator()

	ctx, cancelCtx := chromedp.NewContext(allocatorContext)
	defer cancelCtx()

	var links []map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf(chromeWatchURL, videoID)),
		chromedp.Sleep(5*time.Second),
		chromedp.AttributesAll(playlistSelector, &links, chromedp.ByQueryAll),
	)

	if err != nil {
		return nil, err
	}

	var result []string
	idMap := make(map[string]int)
	for _, link := range links {
		if v, ok := link["class"]; ok && v == "overlay" {
			if href, okHref := link["href"]; okHref {
				parts := strings.Split(href, "=")
				if len(parts) > 1 {
					idMap[parts[1]] = 0
				}
			}
		}
	}
	for k := range idMap {
		result = append(result, k)
	}

	return result, nil
}
