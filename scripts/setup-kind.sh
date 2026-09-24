#!/usr/bin/env bash

set -euo pipefail

cluster_name="${ORBIT_DEVOPS_KIND_CLUSTER_NAME:-orbit-devops-s1}"
context_name="kind-${cluster_name}"
namespace="${ORBIT_DEVOPS_NAMESPACE:-orbit-devops-s3}"
build_namespace="${ORBIT_DEVOPS_BUILD_NAMESPACE:-orbit-devops-s4-build}"
node_image="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
registry_name="orbit-devops-s4-registry"
registry_port="${ORBIT_DEVOPS_KIND_REGISTRY_PORT:-5002}"
registry_image="registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"
registry_service="${registry_name}.${build_namespace}.svc.cluster.local:5000"
git_image="alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"
buildkit_image="moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"
nginx_image="nginx:mainline-alpine@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3"
image_mirror_prefix="${ORBIT_DEVOPS_KIND_IMAGE_MIRROR_PREFIX:-}"

# 镜像源只改变下载路径；版本和 digest 仍由上面的固定引用约束。
docker_image_source() {
  local image="$1"
  if [ -z "${image_mirror_prefix}" ]; then
    printf '%s' "${image}"
    return
  fi
  printf '%s/%s' "${image_mirror_prefix%/}" "${image}"
}

node_pull_image="$(docker_image_source "${node_image}")"
registry_pull_image="$(docker_image_source "library/${registry_image}")"
if [ -z "${image_mirror_prefix}" ]; then
  registry_pull_image="${registry_image}"
fi
git_pull_image="$(docker_image_source "${git_image}")"
buildkit_pull_image="$(docker_image_source "${buildkit_image}")"
nginx_pull_image="$(docker_image_source "${nginx_image}")"

for command_name in docker kind kubectl rg; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "缺少本地命令：${command_name}" >&2
    exit 1
  fi
done

if ! kind get clusters | rg --fixed-strings --line-regexp --quiet "${cluster_name}"; then
  kind create cluster \
    --name "${cluster_name}" \
    --image "${node_pull_image}" \
    --wait 120s
fi

# Registry 运行在 Kind Docker 网络中；宿主机端口只用于验收时读取 OCI Manifest。
if ! docker inspect "${registry_name}" >/dev/null 2>&1; then
  docker run -d \
    --restart unless-stopped \
    --name "${registry_name}" \
    --network kind \
    -p "127.0.0.1:${registry_port}:5000" \
    "${registry_pull_image}" >/dev/null
fi
if [ "$(docker inspect "${registry_name}" --format '{{.Config.Image}}')" != "${registry_pull_image}" ]; then
  echo "本地 Registry ${registry_name} 没有使用固定镜像 ${registry_pull_image}" >&2
  exit 1
fi
if [ "$(docker inspect "${registry_name}" --format '{{.State.Running}}')" != "true" ]; then
  docker start "${registry_name}" >/dev/null
fi
registry_ip="$(docker inspect "${registry_name}" --format '{{range .NetworkSettings.Networks}}{{if eq .NetworkID ""}}{{else}}{{.IPAddress}}{{end}}{{end}}')"
if [ -z "${registry_ip}" ]; then
  echo "无法读取本地 Registry 在 Kind 网络中的地址" >&2
  exit 1
fi

# 由宿主 Docker 拉取并只导入本机架构，避免 Kind 节点重复访问公共 Registry。
docker pull "${git_pull_image}" >/dev/null
docker pull "${buildkit_pull_image}" >/dev/null
# 固定基础镜像预置到任务 Registry，BuildKit 通过服务端 Mirror 配置读取，避免验收依赖 Docker Hub 实时可用性。
kind_node="$(kind get nodes --name "${cluster_name}" | head -n 1)"
case "$(docker exec "${kind_node}" uname -m)" in
  aarch64) kind_architecture="arm64" ;;
  x86_64) kind_architecture="amd64" ;;
  *) echo "无法识别 Kind 节点架构" >&2; exit 1 ;;
esac
docker pull --platform "linux/${kind_architecture}" "${nginx_pull_image}" >/dev/null
docker tag "${nginx_pull_image}" "127.0.0.1:${registry_port}/library/nginx:mainline-alpine"
docker push "127.0.0.1:${registry_port}/library/nginx:mainline-alpine" >/dev/null
docker tag "${git_pull_image}" orbit-devops-local/alpine-git:s4
docker tag "${buildkit_pull_image}" orbit-devops-local/buildkit:s4
build_image_directory="$(mktemp -d)"
trap 'rm -rf "${build_image_directory}"' EXIT
docker save -o "${build_image_directory}/build-images.tar" \
  orbit-devops-local/alpine-git:s4 \
  orbit-devops-local/buildkit:s4

kubectl config use-context "${context_name}" >/dev/null
if ! kubectl --context "${context_name}" get namespace "${namespace}" >/dev/null 2>&1; then
  kubectl --context "${context_name}" create namespace "${namespace}" >/dev/null
fi
kubectl --context "${context_name}" label namespace "${namespace}" \
  app.kubernetes.io/managed-by=orbit-devops \
  --overwrite >/dev/null

if ! kubectl --context "${context_name}" get namespace "${build_namespace}" >/dev/null 2>&1; then
  kubectl --context "${context_name}" create namespace "${build_namespace}" >/dev/null
fi
kubectl --context "${context_name}" label namespace "${build_namespace}" \
  app.kubernetes.io/managed-by=orbit-devops \
  orbit-devops.dev/scope=source-build \
  --overwrite >/dev/null

# Pod 通过稳定的 Service DNS 推送；无 Selector Service 把流量送到同一 Docker 网络的 Registry。
kubectl --context "${context_name}" --namespace "${build_namespace}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${registry_name}
spec:
  ports:
    - name: registry
      port: 5000
      targetPort: 5000
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: ${registry_name}
  labels:
    kubernetes.io/service-name: ${registry_name}
addressType: IPv4
ports:
  - name: registry
    protocol: TCP
    port: 5000
endpoints:
  - addresses:
      - ${registry_ip}
YAML

# kubelet 不使用集群 DNS；为相同的 Service 主机名配置 HTTP Registry 到固定后端地址。
for node_name in $(kind get nodes --name "${cluster_name}"); do
	docker exec -i "${node_name}" ctr --namespace=k8s.io images import --digests --snapshotter=overlayfs - \
	  < "${build_image_directory}/build-images.tar" >/dev/null
	docker exec "${node_name}" ctr --namespace=k8s.io images tag --force \
	  docker.io/orbit-devops-local/alpine-git:s4 "docker.io/alpine/git@${git_image##*@}" >/dev/null
	docker exec "${node_name}" ctr --namespace=k8s.io images tag --force \
	  docker.io/orbit-devops-local/buildkit:s4 "docker.io/moby/buildkit@${buildkit_image##*@}" >/dev/null
  if ! docker exec "${node_name}" grep -Fq 'config_path = "/etc/containerd/certs.d"' /etc/containerd/config.toml; then
    docker exec "${node_name}" sh -c \
      'printf '\''\n[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "/etc/containerd/certs.d"\n'\'' >> /etc/containerd/config.toml'
    docker exec "${node_name}" systemctl restart containerd
  fi
  docker exec "${node_name}" sh -c \
    'mkdir -p "/etc/containerd/certs.d/$1" && printf '\''server = "http://%s"\n\n[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n'\'' "$2" "$2" > "/etc/containerd/certs.d/$1/hosts.toml"' \
    -- "${registry_service}" "${registry_ip}:5000"
done

# Build Job 使用无 Kubernetes API 权限的身份；Worker 身份只管理专用 Namespace 内的执行资源。
kubectl --context "${context_name}" --namespace "${build_namespace}" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orbit-devops-build-executor
automountServiceAccountToken: false
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orbit-devops-build-worker
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: orbit-devops-build-worker
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods", "events"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: orbit-devops-build-worker
subjects:
  - kind: ServiceAccount
    name: orbit-devops-build-worker
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: orbit-devops-build-worker
YAML

echo "Kind 已就绪：context=${context_name} namespace=${namespace} build_namespace=${build_namespace} registry=${registry_service}"
echo "真实构建验收：ORBIT_DEVOPS_KIND_BUILD_PLATFORM=linux/${kind_architecture} ORBIT_DEVOPS_KIND_BUILD_REGISTRY=${registry_service} ORBIT_DEVOPS_KIND_BUILD_REGISTRY_API=http://127.0.0.1:${registry_port}"
