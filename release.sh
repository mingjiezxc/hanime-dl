#!/bin/bash

# =============================================================================
# GitHub Release 自动化脚本
#
# 功能:
#   - 发布前跑一遍质量门（make all：fmt-check / vet / build / test -race / lint）
#   - 交叉编译 Go 项目，支持 Linux, Windows, macOS（六个目标并行构建）
#   - 为编译好的二进制文件打包，并把版本号与提交号注入产物
#   - 创建并推送一个新的 Git 标签（必要时先把当前分支推上去）
#   - 使用 gh-cli 创建 GitHub Release 并上传所有打包好的文件
#
# 使用方法:
#   ./release.sh [v1.2.3]
#   若未提供版本号，则自动在最新标签上增加 0.0.1（补丁位）
#
# 环境变量:
#   HTTP_PROXY / http_proxy   出网代理；未设置则直连
#   SKIP_CHECKS=1             跳过发布前的质量门（仅紧急情况使用）
#   GH_TOKEN                  gh 的认证令牌（也可用 `gh auth login` 的登录态）
#
# =============================================================================
# 任一命令失败立即中止（含管道中的失败），避免"构建失败但脚本继续"。
set -euo pipefail

# 代理：从环境变量读取（不再硬编码任何内网地址）。
# 兼容 HTTP_PROXY / http_proxy 两种写法；都未设置时不导出代理变量（直连）。
PROXY_URL="${HTTP_PROXY:-${http_proxy:-}}"
if [ -n "$PROXY_URL" ]; then
  export http_proxy="$PROXY_URL"
  export https_proxy="${HTTPS_PROXY:-${https_proxy:-$PROXY_URL}}"
  echo "ℹ️  使用代理: $PROXY_URL"
else
  echo "ℹ️  未设置 HTTP_PROXY，按直连处理（如需外网代理请先 export HTTP_PROXY=http://host:port）"
fi
# --- 配置区 ---
# 修改成你的二进制文件名
APP_NAME="hanime-dl"
# 编译输出目录
RELEASE_DIR="release"
# ----------------

# 0. 依赖检查（前置于一切昂贵操作）。
#
#    为什么单独列这一步：上一版只检查了 gh，结果本机缺 zip 时脚本在**编译全部完成之后**
#    才失败——白烧几分钟 CPU，还把 release/ 目录留成了半成品。缺什么就该在开始时说清楚。
missing=()
for cmd in go git gh zip tar; do
  command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
done
if [ ${#missing[@]} -gt 0 ]; then
  echo "❌ 错误: 缺少必需命令: ${missing[*]}"
  echo "   - gh  : https://github.com/cli/cli#installation"
  echo "   - zip : apt-get install zip（Windows 产物打包需要）"
  echo "   - tar : Linux/macOS 产物打包需要"
  exit 1
fi

# 1. 解析版本号参数（可选），缺省则自动 +0.0.1
if [ -z "${1:-}" ]; then
  last_tag=$(git describe --tags --abbrev=0 2>/dev/null || true)
  if [ -z "$last_tag" ]; then
    VERSION="v0.0.1"
  else
    numver="${last_tag#v}"
    IFS='.' read -r MAJOR MINOR PATCH <<< "$numver"
    PATCH=$((PATCH + 1))
    VERSION="v${MAJOR}.${MINOR}.${PATCH}"
  fi
  echo "ℹ️ 未提供版本号，自动计算为: $VERSION"
else
  VERSION=$1
fi

echo "🚀 准备发布版本: $VERSION"

# 2. 标签重名检查：提前失败，错误信息比 git/gh 的原始报错更可操作。
if [ -n "$(git tag -l "$VERSION")" ]; then
  echo "❌ 错误: 标签 $VERSION 已存在。请换一个版本号，或先删除远端标签："
  echo "   git tag -d $VERSION && git push origin :refs/tags/$VERSION"
  exit 1
fi

# 3. 检查 Git 工作区是否干净
if ! git diff-index --quiet HEAD --; then
    echo "❌ 错误: 你的 Git 工作区有未提交的更改。请先提交或暂存。"
    exit 1
fi

echo "✅ Git 工作区干净，准备开始构建..."

# 4. 发布前质量门。
#
#    为什么要有：本仓库的基线是「gofmt 干净 + vet 无告警 + 全部测试 -race 通过 +
#    lint 0 issues」，而它只靠本地手动执行来维持。把 make all 放进发布路径，
#    坏掉的提交就不可能变成 Release。
if [ "${SKIP_CHECKS:-0}" = "1" ]; then
  echo "⚠️  SKIP_CHECKS=1：跳过发布前质量门（不推荐）"
else
  echo "🧪 正在执行发布前质量门..."
  if [ -f Makefile ] && command -v make >/dev/null 2>&1; then
    make all
  else
    # 没有 make 的最小退路：仍然要编译 + 跑测试，不能直接发布。
    go vet ./...
    go test ./...
  fi
  echo "✅ 质量门通过"
fi

# 5. 版本与提交号注入。
#
#    产物里不带版本号的后果是：线上出问题时没人能确认用户跑的是哪一个二进制。
#    main.version / main.commit 是 main.go 里的可注入变量，`hanime-dl -version` 读取它们。
COMMIT="$(git rev-parse --short HEAD)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}"
echo "ℹ️  注入版本信息: version=${VERSION} commit=${COMMIT}"

# 创建一个干净的输出目录（变量一律加引号，避免路径含空格时被拆词）
rm -rf "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR"

# 6. 交叉编译（六个目标并行；任一失败即中止）
echo "🛠️  正在为 Linux, Windows, macOS (amd64 + arm64) 交叉编译..."

build_one() {
  local goos="$1" goarch="$2" ext="$3"
  local out="${RELEASE_DIR}/${APP_NAME}-${goos}-${goarch}${ext}"
  if GOOS="$goos" GOARCH="$goarch" go build -ldflags="$LDFLAGS" -o "$out" .; then
    echo "  ✓ ${goos}/${goarch}${ext}"
  else
    echo "  ✗ ${goos}/${goarch}${ext} 构建失败" >&2
    return 1
  fi
}

pids=()
for target in "linux amd64" "linux arm64" "windows amd64 .exe" "windows arm64 .exe" "darwin amd64" "darwin arm64"; do
  # shellcheck disable=SC2086  # 这里就是要按空格拆成 goos/goarch/ext 三段
  set -- $target
  build_one "$1" "$2" "${3:-}" &
  pids+=("$!")
done

build_failed=0
for pid in "${pids[@]}"; do
  if ! wait "$pid"; then
    build_failed=1
  fi
done
if [ "$build_failed" -ne 0 ]; then
  echo "❌ 错误: 至少一个目标构建失败，未创建标签与 Release。"
  exit 1
fi
echo "✅ 编译成功！"

# 7. 打包压缩
echo "📦 正在打包文件..."
cd "$RELEASE_DIR"
zip "${APP_NAME}-windows-amd64.zip" "${APP_NAME}-windows-amd64.exe"
zip "${APP_NAME}-windows-arm64.zip" "${APP_NAME}-windows-arm64.exe"
tar -czf "${APP_NAME}-linux-amd64.tar.gz" "${APP_NAME}-linux-amd64"
tar -czf "${APP_NAME}-linux-arm64.tar.gz" "${APP_NAME}-linux-arm64"
tar -czf "${APP_NAME}-macos-amd64.tar.gz" "${APP_NAME}-macos-amd64"
tar -czf "${APP_NAME}-macos-arm64.tar.gz" "${APP_NAME}-macos-arm64"

# 删除未打包的二进制文件，只保留压缩包
rm "${APP_NAME}-windows-amd64.exe"
rm "${APP_NAME}-windows-arm64.exe"
rm "${APP_NAME}-linux-amd64"
rm "${APP_NAME}-linux-arm64"
rm "${APP_NAME}-macos-amd64"
rm "${APP_NAME}-macos-arm64"

cd ..
echo "✅ 打包完成！"

# 8. 确保当前提交已在远端
#
#    标签指向 HEAD，而 GitHub Release 要求该提交在仓库里存在。旧脚本只推标签不推分支，
#    于是「本地提交了 README 但忘了 push」时，Release 会创建在一个远端不存在的提交上。
branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch origin "$branch" >/dev/null 2>&1 || true
ahead="$(git rev-list --count "origin/${branch}..HEAD" 2>/dev/null || echo 0)"
if [ "$ahead" != "0" ]; then
  echo "⬆️  本地领先 origin/${branch} ${ahead} 个提交，先推送分支..."
  if ! git push origin "$branch"; then
    echo "❌ 推送分支失败。请检查你的 Git 远程配置和权限。"
    exit 1
  fi
fi

# 9. 创建并推送 Git 标签
echo "🔖 正在创建并推送 Git 标签: $VERSION"
git tag -a "$VERSION" -m "Release $VERSION"
if ! git push origin "$VERSION"; then
    echo "❌ 推送标签失败。请检查你的 Git 远程配置和权限。"
    exit 1
fi
echo "✅ 标签已成功推送到远程仓库！"

# 10. 创建 GitHub Release 并上传文件
echo "🎉 正在创建 GitHub Release 并上传产物..."
if ! gh release create "$VERSION" ./${RELEASE_DIR}/* \
    --title "Release $VERSION" \
    --generate-notes; then
    echo "❌ 创建 Release 失败。请检查 gh 是否已登录 (gh auth status) 并有足够权限。"
    echo "   提示：标签 $VERSION 已经推到远端了，重跑前请先删除它："
    echo "   git tag -d $VERSION && git push origin :refs/tags/$VERSION"
    exit 1
fi

echo "✅ 发布成功！快去 GitHub Releases 页面看看吧！"
