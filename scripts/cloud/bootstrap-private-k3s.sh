#!/usr/bin/env bash
# 仅用于已人工确认的全新个人测试主机。先封闭入口，再以官方固定摘要安装单节点 K3s。
set -euo pipefail

if [[ "${1:-}" != "--confirm-fresh-private-host" || $# != 1 || "$EUID" != 0 ]]; then
  echo "需要 root 与 --confirm-fresh-private-host；不适用于已有集群或生产主机" >&2
  exit 1
fi
[[ "$(uname -m)" == x86_64 ]] || { echo "仅支持 x86_64" >&2; exit 1; }
for target in /usr/local/bin/k3s /etc/rancher/k3s/config.yaml /etc/systemd/system/k3s.service /etc/orbit-private-network.nft; do
  [[ ! -e "$target" ]] || { echo "检测到已有配置，停止以避免覆盖" >&2; exit 1; }
done
for command in curl sha256sum nft ip modprobe systemctl; do
  command -v "$command" >/dev/null || { echo "缺少 $command" >&2; exit 1; }
done
interface="$(ip -4 route show default | awk 'NR==1 {print $5}')"
[[ "$interface" =~ ^[a-zA-Z0-9_.-]+$ ]] || { echo "无法确定公网入站网卡" >&2; exit 1; }
cache=/var/cache/orbit-bootstrap
install -d -m 0700 "$cache"

# 国内镜像只负责运输，摘要固定为 GitHub 官方 Release API 提供的同版本资产摘要。
curl -fL --retry 2 --connect-timeout 10 --max-time 300 \
  https://rancher-mirror.rancher.cn/k3s/v1.36.5-k3s1/k3s -o "$cache/k3s"
curl -fL --retry 2 --connect-timeout 10 --max-time 300 \
  https://rancher-mirror.rancher.cn/k3s/v1.36.5-k3s1/k3s-airgap-images-amd64.tar.gz -o "$cache/k3s-airgap-images-amd64.tar.gz"
printf '%s\n' \
  'd73847bcd3c5fccef0115b372e2f9a91f3032dc84bbf71518a4617565294d313  k3s' \
  '53b76460862db28d81651e5bb1f7d9dbdf32adb25bc35b6db8b1d4c2423efb56  k3s-airgap-images-amd64.tar.gz' \
  | (cd "$cache" && sha256sum --check --strict -)

# 单独的 nft 表不 flush 现有防火墙；在 kube-proxy DNAT 前拒绝入口与 NodePort，SSH 不受影响。
[[ -z "$(nft list tables | awk '$3 == "orbit_private" {print $3}')" ]] || { echo "已存在 Orbit 防火墙表，停止" >&2; exit 1; }
cat > /etc/orbit-private-network.nft <<EOF
table inet orbit_private {
  chain ingress {
    type filter hook prerouting priority -310; policy accept;
    iifname "$interface" tcp dport { 80, 443, 2379, 2380, 5000, 5432, 6379, 6443, 8080, 9090-9095, 10250, 30000-32767 } counter drop
    iifname "$interface" udp dport { 8472, 30000-32767 } counter drop
  }
}
EOF
nft --check --file /etc/orbit-private-network.nft
cat > /etc/systemd/system/orbit-private-network.service <<'EOF'
[Unit]
Description=Orbit private test network protection
Before=k3s.service
After=network-pre.target

[Service]
Type=oneshot
ExecStart=/usr/sbin/nft --file /etc/orbit-private-network.nft
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
install -d -m 0700 /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml <<'EOF'
write-kubeconfig-mode: "0600"
secrets-encryption: true
disable:
  - traefik
  - servicelb
node-label:
  - orbit-devops.dev/environment=private-test
kubelet-arg:
  - system-reserved=cpu=250m,memory=512Mi
  - kube-reserved=cpu=250m,memory=512Mi
  - eviction-hard=memory.available<300Mi,nodefs.available<10%
EOF
chmod 0600 /etc/rancher/k3s/config.yaml /etc/orbit-private-network.nft
install -m 0755 "$cache/k3s" /usr/local/bin/k3s
install -d -m 0700 /var/lib/rancher/k3s/agent/images
install -m 0600 "$cache/k3s-airgap-images-amd64.tar.gz" /var/lib/rancher/k3s/agent/images/
cat > /etc/systemd/system/k3s.service <<'EOF'
[Unit]
Description=K3s private Orbit test cluster
Wants=network-online.target
After=network-online.target orbit-private-network.service
Requires=orbit-private-network.service

[Service]
Type=notify
Environment=K3S_CONFIG_FILE=/etc/rancher/k3s/config.yaml
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now orbit-private-network.service
systemctl enable --now --no-block k3s.service
echo "K3s 启动已提交；需继续核验节点、系统 Pod 与公网端口隔离"
