#!/usr/bin/env bash

set -euo pipefail

cluster_name="${ORBITOPS_KIND_CLUSTER_NAME:-orbitops-s1}"
context_name="kind-${cluster_name}"
namespace="${ORBITOPS_NAMESPACE:-orbitops-s3}"
node_image="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"

for command_name in kind kubectl rg; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "缺少本地命令：${command_name}" >&2
    exit 1
  fi
done

if ! kind get clusters | rg --fixed-strings --line-regexp --quiet "${cluster_name}"; then
  kind create cluster \
    --name "${cluster_name}" \
    --image "${node_image}" \
    --wait 120s
fi

kubectl config use-context "${context_name}" >/dev/null
if ! kubectl --context "${context_name}" get namespace "${namespace}" >/dev/null 2>&1; then
  kubectl --context "${context_name}" create namespace "${namespace}" >/dev/null
fi
kubectl --context "${context_name}" label namespace "${namespace}" \
  app.kubernetes.io/managed-by=orbitops \
  --overwrite >/dev/null

echo "Kind 已就绪：context=${context_name} namespace=${namespace}"
