#!/usr/bin/env bash
# 已获维护授权后，在目标 Linux 上部署独立 Agent；不覆盖已有目录或服务。
set -euo pipefail
if [[ $# != 2 ]]; then
  echo '用法：bash deploy-preview.sh <官方静态包绝对路径> <可信 SHA256>' >&2
  exit 2
fi
bundle="$1"
expected="$2"
base=/home/t00906153/simplehtmlwatch-netdata
unit=shw-netdata-preview.service
if [[ "$bundle" != /* || ! "$expected" =~ ^[0-9a-f]{64}$ ]]; then
  echo '需要绝对路径和 64 位可信 SHA256' >&2; exit 2
fi
if [[ "$(id -u)" != 0 || "$(uname -s)" != Linux ]]; then
  echo '本脚本仅供已授权的 Linux root 维护部署使用' >&2; exit 2
fi
command -v systemctl >/dev/null
command -v curl >/dev/null
command -v sha256sum >/dev/null
command -v ss >/dev/null
if curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:19999/api/v1/info >/dev/null 2>&1; then
  echo '已有健康 Agent，请核验并复用；未执行安装'; exit 3
fi
if [[ -n "$(ss -Hln 'sport = :19999')" ]]; then
  echo '19999 已被其他监听服务占用，请先核验；拒绝安装'; exit 3
fi
if [[ -e "$base" || -L "$base" || -e "/etc/systemd/system/$unit" ]] || systemctl cat "$unit" >/dev/null 2>&1; then
  echo '专属目录或服务已存在，请先核验归属和状态；拒绝覆盖'; exit 3
fi
actual=$(sha256sum "$bundle" | cut -d ' ' -f1)
if [[ "$actual" != "$expected" ]]; then
  echo '静态包 SHA256 不匹配，拒绝解压'; exit 2
fi
umask 077
mkdir -p "$base"
printf 'simplehtmlwatch-netdata.preview.v1\n' > "$base/owner-marker"
printf '%s\n' "$expected" > "$base/bundle-sha256"
sh "$bundle" --accept --noexec --target "$base/runtime" > "$base/extract.log" 2>&1
mkdir -p "$base"/{etc,cache,lib,log,run,lock}
touch "$base/etc/.opt-out-from-anonymous-statistics"
cat > "$base/etc/netdata.conf" <<EOF
[global]
    run as user = root
    update every = 2
[directories]
    config = $base/etc
    stock config = $base/runtime/usr/lib/netdata/conf.d
    stock data = $base/runtime/usr/share/netdata
    log = $base/log
    cache = $base/cache
    lib = $base/lib
    home = $base/lib
    lock = $base/lock
    plugins = $base/runtime/usr/libexec/netdata/plugins.d
    web = $base/runtime/usr/share/netdata/web
[web]
    bind to = 127.0.0.1:19999
    allow connections from = localhost
[db]
    mode = dbengine
    storage tiers = 1
    dbengine tier 0 retention size = 128MiB
    dbengine page cache size = 16MiB
[ml]
    enabled = no
[health]
    enabled = no
[statsd]
    enabled = no
[plugins]
    proc = yes
    diskspace = no
    cgroups = no
    tc = no
    idlejitter = no
    apps = no
    go.d = no
    python.d = no
    charts.d = no
    ebpf = no
    perf = no
    debugfs = no
    nfacct = no
    slabinfo = no
    systemd-journal = no
    network-viewer = no
    netflow = no
    otel = no
    scripts.d = no
EOF
# 直接运行静态二进制；包内包装脚本硬编码 /opt/netdata，不适合专属目录。
"$base/runtime/bin/srv/netdata" -v > "$base/agent-version.txt"
if command -v getenforce >/dev/null && [[ "$(getenforce)" == Enforcing ]]; then
  chcon --reference=/usr/bin/true "$base/runtime/bin/srv/netdata"
fi
cat > "/etc/systemd/system/$unit" <<EOF
[Unit]
Description=simpleHtmlWatch Netdata preview Agent
After=network.target
[Service]
Type=simple
User=root
Environment=DISABLE_TELEMETRY=1
Environment=DO_NOT_TRACK=1
Environment=NETDATA_BASH_LOADABLES=DISABLE
Environment=NETDATA_RUN_DIR=$base/run
ExecStart=$base/runtime/bin/srv/netdata -D -u root -c $base/etc/netdata.conf -P $base/run/netdata.pid
WorkingDirectory=$base
Restart=on-failure
RestartSec=10
Nice=10
NoNewPrivileges=true
PrivateTmp=true
UMask=0077
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now "$unit"
for attempt in {1..20}; do
  if curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:19999/api/v1/info > "$base/agent-info.json"; then
    echo 'Agent 已启动，请经中台验证四个 context、绑定地址及实际开销'
    systemctl show "$unit" -p ActiveState -p MainPID -p MemoryCurrent
    exit 0
  fi
  sleep 1
done
echo 'Agent 未通过健康检查；保留专属目录、服务与日志供维护者排查' >&2
exit 2
