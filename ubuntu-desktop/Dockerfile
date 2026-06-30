# 使用 Ubuntu 22.04 作为基础镜像
FROM ubuntu:22.04

# 1. 设置环境变量
ENV DEBIAN_FRONTEND=noninteractive
ENV DISPLAY=:0
ENV RESOLUTION=1280x800x24
ENV VNC_PASSWORD=secret
# 设置语言环境为中文 UTF-8
ENV LANG=zh_CN.UTF-8
ENV LANGUAGE=zh_CN:zh
ENV LC_ALL=zh_CN.UTF-8

# 2. 安装系统依赖及中文字体
RUN apt-get update && apt-get install -y \
    wget curl gnupg ca-certificates \
    supervisor nginx libnginx-mod-stream apache2-utils \
    xvfb x11vnc fluxbox \
    novnc websockify \
    # 安装中文支持和字体 (文泉驿微米黑、正黑)
    locales fonts-wqy-zenhei fonts-wqy-microhei \
    && locale-gen zh_CN.UTF-8 \
    && rm -rf /var/lib/apt/lists/*

# 3. 安装 Google Chrome
RUN wget -q -O - https://dl-ssl.google.com/linux/linux_signing_key.pub | apt-key add - \
    && echo "deb [arch=amd64] http://dl.google.com/linux/chrome/deb/ stable main" >> /etc/apt/sources.list.d/google.list \
    && apt-get update \
    && apt-get install -y google-chrome-stable \
    && rm -rf /var/lib/apt/lists/*

# 4. 创建必要目录
RUN mkdir -p /app/files /root/.vnc /tmp/chrome-data /var/log/supervisor

# 5. 配置 Nginx
RUN cat > /etc/nginx/nginx.conf <<'EOF'
user www-data;
worker_processes auto;
pid /run/nginx.pid;
include /etc/nginx/modules-enabled/*.conf;

events {
    worker_connections 768;
}

stream {
    server {
        listen 9222;
        proxy_pass 127.0.0.1:9221;
    }
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    sendfile on;
    keepalive_timeout 65;
    include /etc/nginx/conf.d/*.conf;
    include /etc/nginx/sites-enabled/*;
}
EOF

# 修改 sites-available/default (包含之前的重定向修复)
RUN cat > /etc/nginx/sites-available/default <<'EOF'
server {
    listen 80;
    location /files/ {
        alias /app/files/;
        autoindex on;
        charset utf-8;
    }
}

server {
    listen 6080;
    # 彻底修复：关闭绝对重定向，支持任意端口映射 (如 16080)
    absolute_redirect off;
    
    auth_basic "Restricted Desktop";
    auth_basic_user_file /etc/nginx/.htpasswd;

    # 自动重定向到 vnc.html 并注入密码
    location = / {
        return 301 vnc.html?autoconnect=true&resize=scale&password=VNC_PASSWORD_PLACEHOLDER;
    }

    location / {
        proxy_pass http://127.0.0.1:6081;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "Upgrade";
        proxy_set_header Host $host;
    }
}
EOF

# 6. 配置 Supervisor
RUN cat > /etc/supervisor/conf.d/supervisord.conf <<'EOF'
[supervisord]
nodaemon=true
user=root

[program:xvfb]
# 使用占位符代替硬编码
command=Xvfb :0 -screen 0 XVFB_RES_PLACEHOLDER
priority=10
startsecs=5
autorestart=true

[program:fluxbox]
command=fluxbox
environment=DISPLAY=":0"
priority=20
startsecs=5
autorestart=true

[program:x11vnc]
command=x11vnc -display :0 -rfbauth /root/.vnc/passwd -forever -shared
priority=30
startsecs=5
autorestart=true

[program:novnc]
command=websockify --web /usr/share/novnc 6081 localhost:5900
priority=40
autorestart=true

[program:nginx]
command=nginx -g "daemon off;"
priority=50
autorestart=true

[program:chrome]
# 使用占位符代替硬编码的 --window-size
command=google-chrome-stable --no-sandbox --disable-gpu --disable-dev-shm-usage --remote-allow-origins=* --window-size=CHROME_SIZE_PLACEHOLDER --remote-debugging-port=9221 --remote-debugging-address=127.0.0.1 --user-data-dir=/tmp/chrome-data --no-first-run --lang=zh-CN
environment=DISPLAY=":0"
priority=60
autorestart=true
startsecs=5
EOF

# 7. 启动脚本 entrypoint.sh
RUN cat > /entrypoint.sh <<'EOF'
#!/bin/bash
VNC_PASS=${VNC_PASSWORD:-secret}
RES=${RESOLUTION:-1280x800x24}

# 1. 解析 RESOLUTION 变量 (例如从 1280x800x24 中提取 1280 和 800)
WIDTH=$(echo "$RES" | cut -d'x' -f1)
HEIGHT=$(echo "$RES" | cut -d'x' -f2)
CHROME_SIZE="${WIDTH},${HEIGHT}"

# 2. 锁文件清理
rm -f /tmp/.X0-lock
rm -rf /tmp/.X11-unix/X0
rm -rf /tmp/chrome-data/SingletonLock

# 3. 密码生成
mkdir -p /root/.vnc
x11vnc -storepasswd "$VNC_PASS" /root/.vnc/passwd
chmod 600 /root/.vnc/passwd
htpasswd -bc /etc/nginx/.htpasswd admin "$VNC_PASS"

# 4. 动态替换配置
sed -i "s/VNC_PASSWORD_PLACEHOLDER/$VNC_PASS/g" /etc/nginx/sites-available/default
sed -i "s/XVFB_RES_PLACEHOLDER/$RES/g" /etc/supervisor/conf.d/supervisord.conf
sed -i "s/CHROME_SIZE_PLACEHOLDER/$CHROME_SIZE/g" /etc/supervisor/conf.d/supervisord.conf

exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf
EOF

RUN chmod +x /entrypoint.sh
EXPOSE 80 6080 5900 9222
CMD ["/entrypoint.sh"]
