## hanime1.me 下载工具

1. 因 Cloudflare 验证，好多工具都无法下载。
2. 使用 chrome dev 进行人工跳过验证，基本一开始需要验证。
3. 访问 hanime1.me 需要配置代理（请手动修改  ubuntu-desktop 的 docker-compose.yaml)
4. 下载空间是可以直接访问，不需要经代理，也可以直接下载。

### 使用
```
## cp bin
chmod +x hanime-dl 
cp hanime-dl /usr/local/bin/

## 启动 chrome
这里不一定使用项目内的 docker，你本地运行也是可以的。

### docker-compose 启动 chrome
cd ubuntu-desktop
docker compose up -d

### 本地 chrome
# google-chrome --remote-debugging-port=9222

## 下载
1. 下载为当前目录 
2. 自建目录（标题）
```
例： 105532
./偶像女友墮落NTR/偶像女友墮落NTR 6.mkv
./偶像女友墮落NTR/偶像女友墮落NTR 6.jpeg
```
3. chromeRemoteURL 请更换为你的 chrome dev tools version api 路径

### 列表
/usr/local/bin/hanime-dl  -chromeRemoteURL=http://192.168.188.103:9222/json/version -mode list $1

### 单个
/usr/local/bin/hanime-dl  -chromeRemoteURL=http://192.168.188.103:9222/json/version -mode single $1

```



