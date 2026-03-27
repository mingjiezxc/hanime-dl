# 基础镜像
FROM dorowu/ubuntu-desktop-lxde-vnc

# 切换到 root 用户以执行安装操作
USER root

# 设置环境变量，避免 apt 安装过程中出现交互式提示
ENV DEBIAN_FRONTEND=noninteractive

# 安装 Google Chrome
RUN \
    # 1. 基础镜像中可能存在一个损坏的 chrome 源列表，先将其删除以避免初始更新失败
    #    -f 参数确保即使文件不存在也不会报错
    rm -f /etc/apt/sources.list.d/google-chrome.list && \
    \
    # 2. 现在可以安全地更新软件包列表了
    apt-get update && \
    \
    # 3. 安装添加密钥和软件源所必需的工具
    apt-get install -y wget gnupg software-properties-common && \
    \
    # 4. 下载并添加 Google Chrome 的官方 GPG 密钥
    wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome-keyring.gpg && \
    \
    # 5. 用正确的方式（指定签名密钥）重新创建 Chrome 的软件源列表
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome-keyring.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list && \
    \
    # 6. 再次更新软件包列表，以便 apt 识别新添加的 Chrome 软件源
    apt-get update && \
    \
    # 7. 从配置好的软件源中安装 Google Chrome
    apt-get install -y google-chrome-stable && \
    \
    # 9. 清理工作，减小镜像体积
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*