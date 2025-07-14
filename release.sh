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
#   ./release.sh v1.0.0
#
# =============================================================================

# --- 配置区 ---
# 修改成你的二进制文件名
APP_NAME="hanime-dl"
# 编译输出目录
RELEASE_DIR="release"
# ----------------

# 1. 检查是否提供了版本号参数
if [ -z "$1" ]; then
  echo "❌ 错误: 请提供一个版本号作为参数。"
  echo "   用法: $0 v1.2.3"
  exit 1
fi

VERSION=$1
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
echo "🛠️  正在为 Linux, Windows, macOS 交叉编译..."
GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-linux-amd64" .
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-windows-amd64.exe" .
GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o "${RELEASE_DIR}/${APP_NAME}-macos-amd64" .

if [ $? -ne 0 ]; then
    echo "❌ 编译失败。"
    exit 1
fi
echo "✅ 编译成功！"

# 5. 打包压缩
echo "📦 正在打包文件..."
cd $RELEASE_DIR
zip "${APP_NAME}-windows-amd64.zip" "${APP_NAME}-windows-amd64.exe"
tar -czvf "${APP_NAME}-linux-amd64.tar.gz" "${APP_NAME}-linux-amd64"
tar -czvf "${APP_NAME}-macos-amd64.tar.gz" "${APP_NAME}-macos-amd64"

# 删除未打包的二进制文件，只保留压缩包
rm "${APP_NAME}-windows-amd64.exe"
rm "${APP_NAME}-linux-amd64"
rm "${APP_NAME}-macos-amd64"

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