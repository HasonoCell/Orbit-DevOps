#!/usr/bin/env bash
# 仅在服务器回环地址建立恢复入口，外部访问还需本人的 SSH 本地转发。
set -euo pipefail
if [[ "$EUID" != 0 || "${1:-}" != --confirm-private-api-tunnel || $# != 1 ]]; then
  echo "需要 root 与 --confirm-private-api-tunnel" >&2; exit 1
fi
kube=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
systemctl is-active --quiet orbit-private-network.service
[[ "$("${kube[@]}" get nodes -l orbit-devops.dev/environment=private-test -o name | wc -l)" == 1 ]]
[[ "$("${kube[@]}" get nodes -o name | wc -l)" == 1 ]]
[[ "$("${kube[@]}" -n orbit-system get service orbit-api -o jsonpath='{.spec.selector.app}')" == orbit-api ]]
[[ "$("${kube[@]}" -n orbit-system get service orbit-api -o jsonpath='{.spec.type}')" == ClusterIP ]]
[[ ! -e /etc/systemd/system/orbit-api-tunnel.service ]] || {
  echo "已有 API 隧道配置，停止以避免覆盖" >&2; exit 1
}
cat > /etc/systemd/system/orbit-api-tunnel.service <<'EOF'
[Unit]
Description=Orbit API loopback-only recovery tunnel
Requires=k3s.service
After=k3s.service

[Service]
Type=simple
ExecStart=/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml -n orbit-system port-forward --address 127.0.0.1 service/orbit-api 18080:8080
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now orbit-api-tunnel.service
echo "API 隧道仅监听服务器 127.0.0.1:18080"
