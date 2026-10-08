#!/usr/bin/env bash
# 在首次启动 Orbit 进程前设置国内镜像与集群内 Registry；重启 K3s，不重启服务器。
set -euo pipefail
if [[ "$EUID" != 0 || "${1:-}" != --confirm-private-registry-restart || $# != 1 ]]; then
  echo "需要 root 与 --confirm-private-registry-restart" >&2; exit 1
fi
kube=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
systemctl is-active --quiet orbit-private-network.service
[[ "$("${kube[@]}" get nodes -l orbit-devops.dev/environment=private-test -o name | wc -l)" == 1 ]]
[[ "$("${kube[@]}" get nodes -o name | wc -l)" == 1 ]]
[[ -z "$("${kube[@]}" -n orbit-system get deployments -o name)" ]] || {
  echo "Orbit 进程已经部署，停止首次 Registry 配置" >&2; exit 1
}
for file in /etc/rancher/k3s/registries.yaml /etc/rancher/k3s/config.yaml.d/20-orbit-registry.yaml; do
  [[ ! -e "$file" ]] || { echo "已有 Registry 配置，停止以避免覆盖" >&2; exit 1; }
done
[[ "$("${kube[@]}" -n orbit-builds get service registry -o jsonpath='{.metadata.labels.app}')" == orbit-registry ]]
registry_ip="$("${kube[@]}" -n orbit-builds get service registry -o jsonpath='{.spec.clusterIP}')"
[[ "$registry_ip" =~ ^10\.43\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || {
  echo "Registry 不在预期的私有 Service 网段" >&2; exit 1
}
umask 077
install -d -m 0700 /etc/rancher/k3s/config.yaml.d
# containerd 使用宿主机 DNS，不能解析 Service 域名；只映射这个已核验的私有 Service。
cat > /etc/rancher/k3s/registries.yaml <<EOF
mirrors:
  docker.io:
    endpoint: ["https://docker.m.daocloud.io"]
  quay.io:
    endpoint: ["https://quay.m.daocloud.io"]
  "registry.orbit-builds.svc.cluster.local:5000":
    endpoint: ["http://$registry_ip:5000"]
EOF
cat > /etc/rancher/k3s/config.yaml.d/20-orbit-registry.yaml <<'EOF'
# 显式镜像源失效时失败，不静默回退到海外 Registry。
disable-default-registry-endpoint: true
EOF
systemctl restart k3s.service
echo "Registry 配置已生效；需核验节点与私有镜像拉取"
