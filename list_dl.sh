#!/bin/bash
export HTTP_PROXY=http://192.168.188.2:3129
export HTTPS_PROXY=http://192.168.188.2:3129

cd  /mnt/disk2/video/on/

/root/work/dev/hanime-dl/hanime-dl  -chromeRemoteURL=http://192.168.188.103:9222/json/version -mode list $1