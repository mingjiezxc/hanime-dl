# hanime1.me Downloader

[中文说明](#hanime1me-下载工具) | [English Instructions](#hanime1me-downloader-english)

## hanime1.me 下载工具

由于 Cloudflare 验证机制，许多常规下载工具无法使用。本工具通过连接 Chrome DevTools Protocol (CDP) 来绕过验证（需人工辅助完成首次验证）。

### 功能特性
- **绕过 Cloudflare**: 利用真实的 Chrome 浏览器会话。
- **配置灵活**: 支持 YAML 配置文件。
- **断点续传**: 支持下载中断后继续下载。
- **并发下载**: 支持多线程下载（可配置 Worker 数量）。
- **元数据缓存**: 解析过的视频信息会缓存到本地，避免重复请求。
- **代理支持**: 支持配置 HTTP 代理。

### 快速开始

#### 1. 准备环境
你需要一个开启了远程调试端口的 Chrome 浏览器。

**选项 A: 使用 Docker (推荐)**
```bash
cd ubuntu-desktop
docker compose up -d
# 访问 http://localhost:80 (或其他配置端口) 并手动完成 Cloudflare 验证
# user: root , password: Password
```

**选项 B: 本地 Chrome**
```bash
# 确保所有 Chrome 窗口已关闭，然后运行：
google-chrome --remote-debugging-port=9222
```

#### 2. 安装/编译
```bash
go mod tidy
go build -o hanime-dl main.go
# 可选：移动到系统路径
# cp hanime-dl /usr/local/bin/
```

#### 3. 配置文件 (config.yaml)
在运行目录下创建 `config.yaml`：

```yaml
# Chrome 远程调试地址
chromeRemoteURL: http://localhost:9222/json/version

# 缓存与下载目录
CacheDir: ./cache
DownDir: ./downloads

# 下载并发数
MaxDownloadWorkers: 3

# HTTP 代理 (可选)
# HttpProxy: http://127.0.0.1:7890

# 视频列表下载 (填入视频 ID)
ListCode: 
  # - 123456

# 单个视频下载 (填入视频 ID)
SingleCode: 
  # - 654321
```

#### 4. 运行
```bash
# 使用默认 config.yaml
./hanime-dl

# 指定配置文件
./hanime-dl -config my_config.yaml
```

---

## hanime1.me Downloader (English)

A specialized downloader for hanime1.me that bypasses Cloudflare protection by leveraging Chrome DevTools Protocol (CDP).

### Features
- **Cloudflare Bypass**: Uses a real Chrome session (requires initial manual verification).
- **Configurable**: Easy-to-use YAML configuration.
- **Resumable Downloads**: Supports resuming interrupted downloads.
- **Concurrent Downloading**: Multi-threaded download workers.
- **Caching**: Caches resolved video metadata locally to avoid redundant requests.
- **Proxy Support**: Optional HTTP proxy configuration.

### Usage

#### 1. Prerequisites
You need a running Chrome instance with remote debugging enabled.

**Option A: Docker (Recommended)**
```bash
cd ubuntu-desktop
docker compose up -d
# Access via VNC/Web (e.g., http://localhost:6080) and pass the Cloudflare check manually.
```

**Option B: Local Chrome**
```bash
# Close all existing Chrome instances first
google-chrome --remote-debugging-port=9222
```

#### 2. Build
```bash
go mod tidy
go build -o hanime-dl main.go
```

#### 3. Configuration (config.yaml)
Create a `config.yaml` file in the working directory:

```yaml
# Chrome Remote Debugging URL
chromeRemoteURL: http://localhost:9222/json/version

# Directories
CacheDir: ./cache
DownDir: ./downloads

# Number of concurrent download workers
MaxDownloadWorkers: 3

# HTTP Proxy (Optional)
# HttpProxy: http://127.0.0.1:7890

# Download from Playlist IDs
ListCode: 
  # - 123456

# Download Single Video IDs
SingleCode: 
  # - 654321
```

#### 4. Run
```bash
# Run with default config.yaml
./hanime-dl

# Run with custom config file
./hanime-dl -config my_config.yaml
```

### Directory Structure
Downloads are organized by video title:
```
./downloads/
  ├── Video_Title_A/
  │   ├── Video_Title_A.mp4
  │   └── Video_Title_A.jpg
  └── Video_Title_B/
      ├── ...
```
