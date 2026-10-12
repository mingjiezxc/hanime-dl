package chrome

import (
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
	"sync"
	"time"
)

const (
	// StartupCDPTimeout 探测「某个已知 CDP 端点是否可用」的总时间预算。
	//
	// 所有探测路径都以它为界：GetWebSocketDebuggerURL 与
	// GetWebSocketDebuggerURLWithTimeout 都按 deadline 轮询，不再存在
	// 「固定重试 5 次 × 5s 间隔」这种与调用方预算互相矛盾的上限 ——
	// 旧实现单次调用最坏 25s+，却同时被 5s 的连接预算和 30s 的就绪循环包住，
	// 语义自相矛盾（审计 P2-14）。
	StartupCDPTimeout = 5 * time.Second
	// OpenPageTimeout 通过 DevTools HTTP 端点打开页面的超时。
	OpenPageTimeout = 10 * time.Second
	// ChromeReadyTimeout 本地拉起 Chrome 后等待其 CDP 就绪的总预算。
	ChromeReadyTimeout = 30 * time.Second
	// cdpProbeTimeout 单次 HTTP 探测 CDP 端点的超时上限。
	cdpProbeTimeout = 2 * time.Second
	// cdpPollInterval CDP 就绪轮询的间隔。
	cdpPollInterval = 500 * time.Millisecond
)

// Launcher 记录一个由本进程拉起的 Chrome 实例，用于回收其进程与临时 profile 目录。
type Launcher struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	profileDir string
	cleaned    bool
}

// 本进程拉起的本地 Chrome 注册表。
//
// 为什么需要它：StartLocalChrome 用 Setsid 让 Chrome 独立成进程组并常驻，本进程退出
// 不会替它收尸。旧实现把 *Launcher 只存在 StartLocalChrome 的局部变量里，函数一返回
// 实例即被丢弃 —— 结果是每次自动拉起都泄漏一个浏览器进程和一个
// /tmp/chrome-profile-* 目录（审计 P2-13 指出的「真实进程泄漏」）。这里用一个受锁
// 保护的注册表把「拉起」与「退出清理」连接起来，避免把 *Launcher 穿透到
// EnsureChromeConnection / refreshWSURL 等所有可能隐式拉起 Chrome 的调用方。
var (
	launchersMu sync.Mutex
	launchers   []*Launcher
)

// closeQuietly 关闭 c 并把失败记录到日志。
//
// errcheck 基线要求所有 Close 的错误都被显式处理；这里的关闭点即将随连接/进程结束
// 而释放资源，失败不影响主流程，因此记日志而不上抛。
func closeQuietly(c io.Closer, what string) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		log.Printf("closing %s failed: %v", what, err)
	}
}

// GetWebSocketDebuggerURL 获取 Chrome WebSocket 调试 URL。
//
// 总预算为 StartupCDPTimeout：在该预算内按 cdpPollInterval 轮询，单次请求不超过
// cdpProbeTimeout。调用方因此能确定「这一次探测最多花多久」。
func GetWebSocketDebuggerURL(uri string) (string, error) {
	return GetWebSocketDebuggerURLWithTimeout(uri, StartupCDPTimeout)
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

		requestTimeout := minDuration(cdpProbeTimeout, remaining)
		wsURL, err := fetchWebSocketDebuggerURL(uri, requestTimeout)
		if err == nil {
			return wsURL, nil
		}
		lastErr = err

		sleepFor := minDuration(cdpPollInterval, time.Until(deadline))
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
	// defer 而非内联关闭：错误返回路径也必须释放连接（bodyclose 基线要求）。
	defer closeQuietly(resp.Body, "CDP version response body")

	body, err := io.ReadAll(resp.Body)
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
		closeQuietly(conn, fmt.Sprintf("probe connection to port %d", port))

		var wsURL string
		if deadline.IsZero() {
			// 无总预算（AutoDetectChrome 的显式语义）：每个端口最多花 StartupCDPTimeout。
			wsURL, err = GetWebSocketDebuggerURL(url)
		} else {
			wsURL, err = GetWebSocketDebuggerURLWithTimeout(url, minDuration(cdpProbeTimeout, time.Until(deadline)))
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
		// 兼容旧版 Chrome：DevTools 的 /json/new 在部分版本只接受 GET。
		fallbackReq, fbErr := http.NewRequest(http.MethodGet, devtoolsURL, http.NoBody)
		if fbErr != nil {
			return err
		}
		resp, err = client.Do(fallbackReq)
		if err != nil {
			return err
		}
	}
	defer closeQuietly(resp.Body, "devtools new tab response body")

	if _, cerr := io.Copy(io.Discard, resp.Body); cerr != nil {
		log.Printf("Failed to drain devtools response body: %v", cerr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("devtools endpoint returned status %s", resp.Status)
	}
	return nil
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
	defer closeQuietly(resp.Body, "new tab response body")

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
		closeQuietly(conn, fmt.Sprintf("port probe connection to %d", port))
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

	launcher := &Launcher{profileDir: tempDir}
	launcher.cmd = exec.Command(chromePath, args...)
	launcher.cmd.Stdout = os.Stdout
	launcher.cmd.Stderr = os.Stderr

	// 平台特定的进程管理
	setProcessGroupAttr(launcher.cmd)

	// 即将拉起新实例：先把此前由本进程拉起的实例回收掉。
	// 能走到这里说明 AutoDetectChromeWithTimeout 在所有候选端口都没找到可用 CDP
	// 端点，因此旧实例要么已退出、要么已不可用，回收是安全的；这同时把注册表长度
	// 限制在 ~1，避免反复重连时进程与临时目录累积。
	retireLaunchers()

	if err := launcher.cmd.Start(); err != nil {
		// 启动失败：临时 profile 目录没人用了，立即回收。
		launcher.Cleanup()
		return "", fmt.Errorf("failed to start Chrome: %w", err)
	}
	registerLauncher(launcher)

	log.Printf("Chrome launched with PID %d, waiting for it to be ready...", launcher.cmd.Process.Pid)

	// 等待 Chrome 就绪：总预算 ChromeReadyTimeout，每次探测不超过 cdpProbeTimeout。
	// 旧实现在这个 30s 循环里调用的是「固定重试 5 次 × 5s」的版本，单次调用就可能
	// 花掉 25s+，导致整个循环最坏跑到 ~50s 甚至更久（审计 P2-14）。
	deadline := time.Now().Add(ChromeReadyTimeout)
	versionURL := fmt.Sprintf("http://localhost:%d/json/version", debugPort)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wsURL, err := GetWebSocketDebuggerURLWithTimeout(versionURL, minDuration(cdpProbeTimeout, remaining))
		if err == nil && wsURL != "" {
			log.Printf("Chrome is ready! WebSocket URL: %s", wsURL)
			return wsURL, nil
		}
		sleepFor := minDuration(cdpPollInterval, time.Until(deadline))
		if sleepFor <= 0 {
			break
		}
		time.Sleep(sleepFor)
	}

	// 就绪失败：必须回收刚拉起的进程，否则又是一个失去引用的孤儿浏览器。
	launcher.Cleanup()
	return "", fmt.Errorf("chrome failed to become ready within %s", ChromeReadyTimeout)
}

// registerLauncher 记录一个已成功拉起的 Chrome 实例。
func registerLauncher(l *Launcher) {
	launchersMu.Lock()
	launchers = append(launchers, l)
	launchersMu.Unlock()
}

// retireLaunchers 终止并移除注册表中全部由本进程拉起的 Chrome 实例。
func retireLaunchers() {
	launchersMu.Lock()
	old := launchers
	launchers = nil
	launchersMu.Unlock()
	for _, l := range old {
		l.Cleanup()
	}
}

// CleanupLaunchedChrome 终止本进程拉起的全部本地 Chrome 实例，并删除它们的临时
// profile 目录。应在进程退出路径调用一次（main 的 defer）。
//
// 覆盖所有隐式拉起路径：main 启动阶段的 EnsureChromeConnectionDetailed、web 侧
// refreshWSURL 调用的 EnsureChromeConnection、以及 StartLocalChrome 的重复调用。
func CleanupLaunchedChrome() {
	retireLaunchers()
}

// Cleanup 清理 Chrome 进程与其临时 profile 目录（跨平台）。
//
// 幂等且并发安全：同一个 Launcher 可能既在 StartLocalChrome 的失败路径上被清理，
// 又在进程退出时被 CleanupLaunchedChrome 通过注册表再次清理（多个 worker 并发
// refreshWSURL 时也可能并发触发），因此这里用锁 + 一次性标志保证真正的回收只做一次。
func (l *Launcher) Cleanup() {
	if l == nil {
		return
	}

	l.mu.Lock()
	if l.cleaned {
		l.mu.Unlock()
		return
	}
	l.cleaned = true
	cmd, profileDir := l.cmd, l.profileDir
	l.profileDir = ""
	l.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		log.Println("Cleaning up Chrome process...")
		cleanupProcess(cmd)
	}
	if profileDir != "" {
		if err := os.RemoveAll(profileDir); err != nil {
			log.Printf("Failed to remove Chrome temp profile %s: %v", profileDir, err)
		}
	}
}
