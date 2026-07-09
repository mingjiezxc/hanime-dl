#!/bin/bash

# =============================================================================
# GitHub Release 自动化脚本
#
# 功能:
#   - 交叉编译 Go 项目，支持 Linux, Windows, macOS
#   - 为编译好的二进制文件打包
#   - 创建并推送一个新的 Git 标签
#   - 使用 gh-cli 创建 GitHub Release 并上传所有打包好的文件
#
# 使用方法:
#   ./release.sh [v1.2.3]
#   若未提供版本号，则自动在最新标签上增加 0.0.1（补丁位）
#
# =============================================================================
export http_proxy=http://192.168.188.1:3128
export https_proxy=http://192.168.188.1:3128
# --- 配置区 ---
# 修改成你的二进制文件名
APP_NAME="hanime-dl"
# 编译输出目录
RELEASE_DIR="release"
# ----------------

# 1. 解析版本号参数（可选），缺省则自动 +0.0.1
if [ -z "$1" ]; then
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

# 2. 检查 gh 命令是否存在
if ! command -v gh &> /dev/null; then
    echo "❌ 错误: 未找到 GitHub CLI (gh) 命令。"
    echo "   请先根据文档安装: https://github.com/cli/cli#installation"
    exit 1
fi

# 3. 检查 Git 工作区是否干净
if ! git diff-index --quiet HEAD --; then
    echo "❌ 错误: 你的 Git 工作区有未提交的更改。请先提交或暂存。"
    exit 1
fi

echo "✅ Git 工作区干净，准备开始构建..."

# 创建一个干净的输出目录
rm -rf $RELEASE_DIR
mkdir -p $RELEASE_DIR

# 4. 交叉编译
echo "🛠️  正在为 Linux, Windows, macOS (amd64 + arm64) 交叉编译..."
GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-linux-amd64" .
GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-linux-arm64" .
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-windows-amd64.exe" .
GOOS=windows GOARCH=arm64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-windows-arm64.exe" .
GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-macos-amd64" .
GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-macos-arm64" .

if [ $? -ne 0 ]; then
    echo "❌ 编译失败。"
    exit 1
fi
echo "✅ 编译成功！"

# 5. 打包压缩
echo "📦 正在打包文件..."
cd $RELEASE_DIR
zip "${APP_NAME}-windows-amd64.zip" "${APP_NAME}-windows-amd64.exe"
zip "${APP_NAME}-windows-arm64.zip" "${APP_NAME}-windows-arm64.exe"
tar -czvf "${APP_NAME}-linux-amd64.tar.gz" "${APP_NAME}-linux-amd64"
tar -czvf "${APP_NAME}-linux-arm64.tar.gz" "${APP_NAME}-linux-arm64"
tar -czvf "${APP_NAME}-macos-amd64.tar.gz" "${APP_NAME}-macos-amd64"
tar -czvf "${APP_NAME}-macos-arm64.tar.gz" "${APP_NAME}-macos-arm64"

# 删除未打包的二进制文件，只保留压缩包
rm "${APP_NAME}-windows-amd64.exe"
rm "${APP_NAME}-windows-arm64.exe"
rm "${APP_NAME}-linux-amd64"
rm "${APP_NAME}-linux-arm64"
rm "${APP_NAME}-macos-amd64"
rm "${APP_NAME}-macos-arm64"

cd ..
echo "✅ 打包完成！"

# 6. 创建并推送 Git 标签
echo "🔖 正在创建并推送 Git 标签: $VERSION"
git tag -a "$VERSION" -m "Release $VERSION"
git push origin "$VERSION"

if [ $? -ne 0 ]; then
    echo "❌ 推送标签失败。请检查你的 Git 远程配置和权限。"
    exit 1
fi
echo "✅ 标签已成功推送到远程仓库！"

# 7. 创建 GitHub Release 并上传文件
echo "🎉 正在创建 GitHub Release 并上传产物..."
gh release create "$VERSION" ./${RELEASE_DIR}/* \
    --title "Release $VERSION" \
    --generate-notes

if [ $? -ne 0 ]; then
    echo "❌ 创建 Release 失败。请检查 gh 是否已登录 (gh auth status) 并有足够权限。"
    exit 1
fi

echo "✅ 发布成功！快去 GitHub Releases 页面看看吧！"