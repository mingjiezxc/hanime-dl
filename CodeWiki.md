## Codewiki: `hanime-dl` Project Summary

### 1. Project Overview

*   **Purpose:** A specialized command-line downloader for the `hanime1.me` website. It is designed to bypass Cloudflare protection by utilizing the Chrome DevTools Protocol (CDP) to drive a real Chrome browser session for link resolution.
*   **Technology:** Go (Golang)
*   **Key Features:** Cloudflare bypass via CDP, YAML configuration, resumable downloads, concurrent downloading, and metadata caching.
*   **Dependencies:** `github.com/chromedp/chromedp` (for CDP communication) and `gopkg.in/yaml.v3` (for configuration).

### 2. Architecture and Workflow

The application uses a **Producer-Consumer** pattern:

1.  **Configuration Loading:** `main()` loads settings from `config.yaml` (or a specified path).
2.  **CDP Connection:** It connects to a running Chrome instance with remote debugging enabled (specified by `chromeRemoteURL`).
3.  **Producer (Main Thread):**
    *   Collects video IDs from `SingleCode` and `ListCode` in the config.
    *   For playlists (`ListCode`), it uses `ChromedpGetList` to navigate and scrape all video IDs from the playlist's watch page, utilizing a JSON cache (`list_*.json`).
    *   For each video ID, it calls `ResolveVideoInfo`, which uses CDP to navigate to the download page and extract the video title, image URL, and video data URL.
    *   Metadata is cached to disk (`info_*.json`) for each video.
    *   The final, resolved `VideoMetadata` object is pushed onto the `downloadQueue` channel.
4.  **Consumers (Download Workers):**
    *   A pool of concurrent Go routines (`MaxDownloadWorkers`) reads from the `downloadQueue`.
    *   Each worker calls `DownloadFileWithRetry` for the image (`.jpg`) and video (`.mp4`).
    *   Downloads are saved to a directory within `DownDir`, named after the video's title.
    *   **Resumability:** The `DownloadFile` function checks for a partial `.tmp` file and uses the HTTP `Range` header to resume interrupted transfers.

### 3. Key Files and Structures

| File/Structure | Description |
| :--- | :--- |
| **`main.go`** | Contains all application logic, from config parsing to the download worker pool. |
| **`config.yaml`** | Defines runtime parameters, including `chromeRemoteURL`, `CacheDir`, `DownDir`, `MaxDownloadWorkers`, `HttpProxy`, `ListCode`, and `SingleCode`. |
| **`VideoMetadata`** | The central data structure for video info, including resolved URLs and target file paths. Used for caching and queuing. |
| **`DownloadFile(...)`** | Implements the core resumable HTTP download logic, including progress logging and proxy support. |
| **`ubuntu-desktop/`** | Contains Docker files to easily spin up a headless Chrome instance with remote debugging, which is a prerequisite for the application. |

### 4. Configuration Details (from current `config.yaml`)

| Setting | Value | Description |
| :--- | :--- | :--- |
| `chromeRemoteURL` | `http://192.168.188.103:9222/json/version` | The current target for the Chrome DevTools connection. |
| `CacheDir` | `./cache` | Local directory for storing metadata caches (`list_*.json`, `info_*.json`). |
| `DownDir` | `/mnt/disk3/video/on/` | The ultimate destination directory for downloaded files. |
| `HttpProxy` | `http://192.168.188.1:3128` | Configured HTTP proxy for downloading media files. |
| `MaxDownloadWorkers` | `2` | Number of parallel downloads allowed. |
| `ListCode` | `[158032, 143645, ...]` | List of playlist IDs to process. |
| `SingleCode` | `[143648]` | List of individual video IDs to process. |