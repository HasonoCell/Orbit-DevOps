#!/usr/bin/env bash
# 消费已下载的官方安装清单；不在云端重复下载，也不执行清单中的任何命令说明。
set -euo pipefail
[[ "$EUID" == 0 && $# == 3 && "$1" == "--confirm-private-controllers" ]] || {
  echo "用法：root install-private-controllers.sh --confirm-private-controllers <Envoy YAML> <cert-manager YAML>" >&2; exit 1;
}
k=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
[[ "$(systemctl is-active orbit-private-network.service)" == active ]] || { echo "私有网络防护未启动" >&2; exit 1; }
[[ "$("${k[@]}" get nodes -l orbit-devops.dev/environment=private-test -o name | wc -l)" == 1 ]] || {
  echo "不是预期的单节点私有测试集群" >&2; exit 1;
}
[[ "$("${k[@]}" get nodes -o name | wc -l)" == 1 ]] || {
  echo "存在额外节点，停止单节点安装" >&2; exit 1;
}
printf '%s  %s\n' \
  72b3971364f172eb0b9636c7142cc84ff695467bc065897958bde85a3c06cfd5 "$2" \
  e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f "$3" \
  | sha256sum --check --strict -
directory="$(mktemp -d /var/tmp/orbit-controller-install.XXXXXX)"
chmod 0700 "$directory"
install -m 0600 "$2" "$directory/envoy.yaml"
install -m 0600 "$3" "$directory/cert-manager.yaml"
cat > "$directory/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [envoy.yaml, cert-manager.yaml]
images:
  - name: envoyproxy/gateway
    digest: sha256:0049bcb384c591c6a6dd043fe5c9929ef6e74f230e12dd678d2d3701df9b301e
  - name: quay.io/jetstack/cert-manager-controller
    digest: sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8
  - name: quay.io/jetstack/cert-manager-cainjector
    digest: sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562
  - name: quay.io/jetstack/cert-manager-webhook
    digest: sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae
EOF
"${k[@]}" apply --server-side --field-manager=orbit-private-bootstrap -k "$directory"
echo "控制器资源已提交；仍须核验 Deployment 就绪与 GatewayClass/Issuer 状态"
