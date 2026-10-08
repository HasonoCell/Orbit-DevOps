#!/usr/bin/env bash
# 只从国内镜像下载固定摘要，导入到本机 K3s；不修改默认 Kubernetes Context。
set -euo pipefail
[[ "$EUID" == 0 ]] || { echo "需要 root" >&2; exit 1; }
command -v timeout >/dev/null || { echo "需要 timeout" >&2; exit 1; }
systemctl is-active --quiet orbit-private-network.service
kube=(/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml)
[[ "$("${kube[@]}" get nodes -l orbit-devops.dev/environment=private-test -o name | wc -l)" == 1 ]]
[[ "$("${kube[@]}" get nodes -o name | wc -l)" == 1 ]]
ctr=(/usr/local/bin/k3s ctr)
images=(
  docker.io/library/postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73
  docker.io/library/redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf
  docker.io/library/registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e
  docker.io/envoyproxy/gateway:v1.9.1@sha256:0049bcb384c591c6a6dd043fe5c9929ef6e74f230e12dd678d2d3701df9b301e
  docker.io/envoyproxy/envoy:distroless-v1.39.1@sha256:eb2c01c13125d1629637cb4e4cce7207009fb7cc2c8027f9742758549d15b6f4
  quay.io/jetstack/cert-manager-controller:v1.21.2@sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8
  quay.io/jetstack/cert-manager-cainjector:v1.21.2@sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562
  quay.io/jetstack/cert-manager-webhook:v1.21.2@sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae
  docker.io/alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26
  docker.io/moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef
)
for image in "${images[@]}"; do
  case "$image" in
    docker.io/*) source="docker.m.daocloud.io/${image#docker.io/}" ;;
    quay.io/*) source="quay.m.daocloud.io/${image#quay.io/}" ;;
  esac
  timeout 300 "${ctr[@]}" images pull --platform linux/amd64 "$source"
  # CRI 以去掉 tag 的 digest 引用查找镜像；同时预置安装清单使用的固定版本 tag。
  repository="${image%@*}"
  if [[ "${repository##*/}" == *:* ]]; then repository="${repository%:*}"; fi
  "${ctr[@]}" images tag --force "$source" "$repository@${image##*@}"
  "${ctr[@]}" images tag --force "$source" "${image%@*}"
done
