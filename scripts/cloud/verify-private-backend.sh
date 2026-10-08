#!/usr/bin/env bash
# 只读引导验收，不创建账号、不读取 Secret、不代表构建/发布业务 E2E。
set -euo pipefail
[[ "$EUID" == 0 && $# == 0 ]] || { echo "需要 root，不接受参数" >&2; exit 1; }
kube=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
systemctl is-active --quiet orbit-private-network.service
systemctl is-active --quiet k3s.service
[[ "$("${kube[@]}" get nodes -l orbit-devops.dev/environment=private-test -o name | wc -l)" == 1 ]]
[[ "$("${kube[@]}" get nodes -o name | wc -l)" == 1 ]]
for namespace in orbit-system orbit-apps orbit-builds; do
  [[ "$("${kube[@]}" get namespace "$namespace" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')" == orbit-devops ]]
done
"${kube[@]}" wait --for=condition=Ready --timeout=30s nodes --all
for deployment in orbit-api orbit-release-worker orbit-build-worker orbit-pipeline-worker orbit-gateway-worker; do
  "${kube[@]}" -n orbit-system rollout status "deployment/$deployment" --timeout=30s
done
for dependency in postgres redis; do
  "${kube[@]}" -n orbit-system rollout status "statefulset/$dependency" --timeout=30s
done
"${kube[@]}" -n orbit-builds rollout status statefulset/registry --timeout=30s
for deployment in cert-manager cert-manager-webhook cert-manager-cainjector; do
  "${kube[@]}" -n cert-manager rollout status "deployment/$deployment" --timeout=30s
done
"${kube[@]}" -n envoy-gateway-system rollout status deployment/envoy-gateway --timeout=30s
[[ "$("${kube[@]}" get gatewayclass orbit-private -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}')" == True ]]
[[ "$("${kube[@]}" -n orbit-apps get issuer orbit-private-selfsigned -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]]
schema="$("${kube[@]}" -n orbit-system exec postgres-0 -- sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -U orbitdevops -d orbitdevops -At -c "SELECT version, dirty FROM schema_migrations"')"
[[ "$schema" == '26|f' ]] || { echo "迁移版本或 dirty 状态不符合引导基线" >&2; exit 1; }

# 用真实 API Server 授权结果锁定进程边界，不依赖清单看上去正确。
"${kube[@]}" auth can-i patch deployments -n orbit-apps --as=system:serviceaccount:orbit-system:orbit-release-worker >/dev/null
"${kube[@]}" auth can-i create jobs -n orbit-builds --as=system:serviceaccount:orbit-system:orbit-build-worker >/dev/null
"${kube[@]}" auth can-i create httproutes -n orbit-apps --as=system:serviceaccount:orbit-system:orbit-gateway-worker >/dev/null
if "${kube[@]}" auth can-i create deployments -n orbit-apps --as=system:serviceaccount:orbit-system:orbit-api >/dev/null ||
   "${kube[@]}" auth can-i create jobs -n orbit-system --as=system:serviceaccount:orbit-system:orbit-build-worker >/dev/null ||
   "${kube[@]}" auth can-i list secrets -n orbit-system --as=system:serviceaccount:orbit-system:orbit-pipeline-worker >/dev/null; then
  echo "进程权限超出预期边界" >&2; exit 1
fi
systemctl is-active --quiet orbit-api-tunnel.service
curl --fail --silent --show-error --max-time 5 http://127.0.0.1:18080/healthz >/dev/null
[[ "$(curl --silent --show-error --max-time 5 --output /dev/null --write-out '%{http_code}' http://127.0.0.1:18080/api/v1/projects)" == 401 ]]
echo "PRIVATE_BACKEND_BOOTSTRAP_VERIFIED"
