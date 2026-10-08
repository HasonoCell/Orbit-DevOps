#!/usr/bin/env bash
# 只在服务器上生成随机依赖密码；配置与 Secret 不经终端输出、参数或 Git 传递。
set -euo pipefail
[[ "$EUID" == 0 && "${1:-}" == "--confirm-new-test-database" && $# == 1 ]] || {
  echo "需要 root 与 --confirm-new-test-database" >&2; exit 1;
}
k=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
[[ "$(systemctl is-active orbit-private-network.service)" == active ]] || {
  echo "私有网络防护尚未生效" >&2; exit 1;
}
for namespace in orbit-system orbit-apps orbit-builds; do
  [[ "$("${k[@]}" get namespace "$namespace" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')" == orbit-devops ]] || {
    echo "受管 Namespace 未核验" >&2; exit 1;
  }
done
for secret in orbit-database orbit-redis; do
  if "${k[@]}" -n orbit-system get secret "$secret" >/dev/null 2>&1; then
    echo "已有依赖 Secret，停止以避免覆盖" >&2; exit 1
  fi
done
directory=/etc/orbit-devops/private-k3s
[[ ! -e "$directory" ]] || { echo "已有私密配置目录，停止以避免覆盖" >&2; exit 1; }
umask 077
install -d -m 0700 "$directory"
openssl rand -hex 32 | tr -d '\n' > "$directory/database-password"
openssl rand -hex 32 | tr -d '\n' > "$directory/redis-password"
printf 'postgres://orbitdevops:%s@postgres.orbit-system.svc.cluster.local:5432/orbitdevops?sslmode=disable' \
  "$( < "$directory/database-password")" > "$directory/database-url"
"${k[@]}" -n orbit-system create secret generic orbit-database \
  --from-file=password="$directory/database-password" --from-file=url="$directory/database-url" >/dev/null
"${k[@]}" -n orbit-system create secret generic orbit-redis \
  --from-file=password="$directory/redis-password" >/dev/null
cluster_uid="$("${k[@]}" get namespace kube-system -o jsonpath='{.metadata.uid}')"
[[ -n "$cluster_uid" ]] || { echo "集群身份为空" >&2; exit 1; }
"${k[@]}" -n orbit-system create configmap orbit-cluster-identity \
  --from-literal=ORBIT_DEVOPS_KUBERNETES_MODE=in-cluster \
  --from-literal=ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID="$cluster_uid" >/dev/null
echo "私有依赖配置已生成；管理员密码仍由本人单独初始化"
