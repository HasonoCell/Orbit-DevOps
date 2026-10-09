#!/usr/bin/env python3
"""为已确认的单节点私人测试集群分阶段准备管理 HTTPS；浏览器配置另行切换。"""

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import tempfile
import urllib.request

KUBE = ["/usr/local/bin/k3s", "kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml", "--request-timeout=15s"]
OWNER = "orbit-devops"
CONTROLLER_DIGEST = "sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8"
ACME_DIRECTORY = "https://acme-v02.api.letsencrypt.org/directory"
ACCEPTED_TERMS = "https://letsencrypt.org/documents/LE-SA-v1.8-July-06-2026.pdf"
SOLVER_IMAGE = "quay.io/jetstack/cert-manager-acmesolver:v1.21.2@sha256:699b40d622211ab7accad8a21b04c5fbaa1841ef7a12621e8de492dbe27b2503"
MANIFEST_DIGEST = "186dae199d67b384060be9ce7b6b026ab1aa9bf0feeaefe1d5108ac6633f8f14"
NETWORK_FILE = Path("/etc/orbit-private-network.nft")
PRIVATE_TCP = [2379, 2380, 5000, 5432, 6379, 6443, 8080, *range(9090, 9096), 10250, *range(30000, 32768)]


def run(args, data=None):
    """不回显请求体或进程错误正文，避免 Secret 在失败诊断中泄露。"""
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=60)
    if result.returncode:
        raise RuntimeError(f"操作失败（exit={result.returncode}）；请在主机上检查资源状态")
    return result.stdout


def get(kind, name=None, namespace=None, optional=False):
    args = KUBE + ["get", kind]
    if name:
        args.append(name)
    if namespace:
        args += ["-n", namespace]
    if optional:
        args.append("--ignore-not-found")
    return json.loads(run(args + ["-o", "json"]) or "null")


def check_private_cluster():
    """人工授权之外再核验本机身份；不依赖默认 kube context。"""
    if os.geteuid() != 0:
        raise RuntimeError("需要 root")
    run(["systemctl", "is-active", "--quiet", "orbit-private-network.service"])
    nodes = get("nodes")["items"]
    if len(nodes) != 1 or nodes[0]["metadata"].get("labels", {}).get("orbit-devops.dev/environment") != "private-test":
        raise RuntimeError("不是预期的单节点私人测试集群")
    labels = get("namespace", "orbit-system")["metadata"].get("labels", {})
    if labels.get("app.kubernetes.io/managed-by") != OWNER or labels.get("orbit-devops.dev/environment") != "private-test":
        raise RuntimeError("控制 Namespace 身份不匹配")


def apply_json(resource):
    body = json.dumps(resource)
    args = KUBE + ["apply", "--server-side", "--field-manager=orbit-management-bootstrap", "-f", "-"]
    run(args + ["--dry-run=server"], body)
    run(args, body)


def managed(kind, name, spec):
    return {"apiVersion": "cert-manager.io/v1", "kind": kind,
            "metadata": {"name": name, "namespace": "orbit-system",
                         "labels": {"app.kubernetes.io/managed-by": OWNER}}, "spec": spec}


def certificate_resources(public_ip):
    """IP 从操作参数传入，不将真实主机信息写入仓库。"""
    address = ipaddress.ip_address(public_ip)
    if address.version != 4 or not address.is_global or address.is_multicast or address.is_reserved:
        raise ValueError("仅接受公网 IPv4 地址")
    labels = {"orbit-devops.dev/management-acme": "true"}
    solver = {
        "parentRefs": [{"name": "orbit-management", "namespace": "orbit-system", "sectionName": "acme-http"}],
        "labels": labels,
        "podTemplate": {
            "metadata": {"labels": labels},
            "spec": {"resources": {"requests": {"cpu": "10m", "memory": "16Mi"},
                                  "limits": {"cpu": "100m", "memory": "64Mi"}}},
        },
    }
    issuer = managed("Issuer", "orbit-management-acme", {"acme": {
        "server": ACME_DIRECTORY, "profile": "shortlived",
        "privateKeySecretRef": {"name": "orbit-management-acme-account"},
        "solvers": [{"http01": {"gatewayHTTPRoute": solver}}],
    }})
    certificate = managed("Certificate", "orbit-management-tls", {
        "secretName": "orbit-management-tls", "ipAddresses": [str(address)],
        "duration": "144h", "renewBeforePercentage": 33,
        "privateKey": {"algorithm": "ECDSA", "size": 256, "rotationPolicy": "Always"},
        "issuerRef": {"name": "orbit-management-acme", "kind": "Issuer", "group": "cert-manager.io"},
    })
    return {"apiVersion": "v1", "kind": "List", "items": [issuer, certificate]}


def owned_or_absent(kind, name, namespace):
    existing = get(kind, name, namespace, optional=True)
    if existing and existing["metadata"].get("labels", {}).get("app.kubernetes.io/managed-by") != OWNER:
        raise RuntimeError("已有同名资源不属于 Orbit，停止")
    return existing


def prepare(manifest):
    # 只消费随脚本交付并已审查的固定资源；不能借参数将任意 YAML 交给 root kubectl。
    if hashlib.sha256(manifest.read_bytes()).hexdigest() != MANIFEST_DIGEST:
        raise RuntimeError("入口清单摘要不匹配，停止")
    source = NETWORK_FILE.read_text()
    closed, interface, ports = network_configuration(source, "closed")
    if source != closed:
        raise RuntimeError("准备入口前必须先关闭公网 80/443")
    check_live_network(json.loads(run(["nft", "-j", "list", "table", "inet", "orbit_private"])), interface, ports)
    for kind, name, namespace in [
        ("envoyproxy", "orbit-management", "envoy-gateway-system"), ("gatewayclass", "orbit-management", None),
        ("gateway", "orbit-management", "orbit-system"), ("securitypolicy", "orbit-management-origin", "orbit-system"),
        ("httproute", "orbit-management-api", "orbit-system"),
        ("networkpolicy", "orbit-management-api-ingress", "orbit-system"),
        ("networkpolicy", "orbit-management-acme-ingress", "orbit-system"),
    ]:
        owned_or_absent(kind, name, namespace)
    args = KUBE + ["apply", "--server-side", "--field-manager=orbit-management-bootstrap", "-f", str(manifest)]
    run(args + ["--dry-run=server"])
    existing = owned_or_absent("secret", "orbit-management-origin", "orbit-system")
    if existing:
        if not existing.get("data", {}).get("vercel"):
            raise RuntimeError("已有来源 Secret 缺少 vercel 凭据，停止")
    else:
        # 密钥仅在进程内存、stdin 和集群 Secret 内流转；不进入参数、文件或输出。
        apply_json({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
                    "metadata": {"name": "orbit-management-origin", "namespace": "orbit-system",
                                 "labels": {"app.kubernetes.io/managed-by": OWNER}},
                    "stringData": {"vercel": secrets.token_urlsafe(32)}})
    run(args)
    print("MANAGEMENT_RESOURCES_PREPARED; 公网防火墙未改变")


def request_certificate(public_ip):
    resources = certificate_resources(public_ip)
    # 注册 ACME 账号会接受条款；条款版本变化时必须重新征得用户同意。
    with urllib.request.urlopen(ACME_DIRECTORY, timeout=15) as response:
        directory = json.load(response)
    if directory.get("meta", {}).get("termsOfService") != ACCEPTED_TERMS:
        raise RuntimeError("ACME 条款已变化，需要重新确认后申请")
    if "shortlived" not in directory.get("meta", {}).get("profiles", {}):
        raise RuntimeError("ACME 不支持 shortlived profile")
    for kind, name in [("issuer", "orbit-management-acme"), ("certificate", "orbit-management-tls")]:
        owned_or_absent(kind, name, "orbit-system")
    run(KUBE + ["apply", "--server-side", "--field-manager=orbit-management-bootstrap",
                "--dry-run=server", "-f", "-"], json.dumps(resources))
    controller = get("deployment", "cert-manager", "cert-manager")
    containers = controller["spec"]["template"]["spec"]["containers"]
    if len(containers) != 1 or not containers[0]["image"].endswith("@" + CONTROLLER_DIGEST):
        raise RuntimeError("cert-manager 不符合已核验版本，停止修改参数")
    args = containers[0].get("args", [])
    desired = list(args)
    if "--enable-gateway-api=true" not in args:
        if any(arg.startswith("--enable-gateway-api") or arg.startswith("--config") for arg in args):
            raise RuntimeError("已有 Gateway 或文件配置，需要人工核验")
        desired.append("--enable-gateway-api=true")
    indexes = [i for i, arg in enumerate(args) if arg.startswith("--acme-http01-solver-image=")]
    if len(indexes) != 1 or args[indexes[0]] not in (
        "--acme-http01-solver-image=quay.io/jetstack/cert-manager-acmesolver:v1.21.2",
        "--acme-http01-solver-image=" + SOLVER_IMAGE,
    ):
        raise RuntimeError("solver 镜像参数不符合已核验版本")
    desired[indexes[0]] = "--acme-http01-solver-image=" + SOLVER_IMAGE
    if desired != args:
        # 条件更新保留其他参数，拒绝并发修改；不重新安装控制器。
        patch = [
            {"op": "test", "path": "/metadata/resourceVersion", "value": controller["metadata"]["resourceVersion"]},
            {"op": "test", "path": "/spec/template/spec/containers/0/args", "value": args},
            {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": desired},
        ]
        run(KUBE + ["patch", "deployment", "cert-manager", "-n", "cert-manager",
                    "--type=json", "--patch-file=/dev/stdin"], json.dumps(patch))
        run(KUBE + ["rollout", "status", "deployment/cert-manager", "-n", "cert-manager", "--timeout=45s"])
    apply_json(resources)
    print("IP_CERTIFICATE_REQUESTED; 需验证 Ready、TLS 与自动续期时间")


def network_configuration(source, stage):
    """仅接受引导脚本的精确布局；只改变 80/443，其余规则完整保留。"""
    match = re.fullmatch(
        r'table inet orbit_private \{\n  chain ingress \{\n'
        r'    type filter hook prerouting priority -310; policy accept;\n'
        r'    iifname "([a-zA-Z0-9_.-]+)" tcp dport \{ (.*?) \} counter drop\n'
        r'    iifname "\1" udp dport \{ 8472, 30000-32767 \} counter drop\n'
        r'  \}\n\}\n', source)
    if not match:
        raise RuntimeError("私有网络配置不是受支持的引导布局")
    private = "2379, 2380, 5000, 5432, 6379, 6443, 8080, 9090-9095, 10250, 30000-32767"
    states = {"closed": "80, 443, " + private, "acme": "443, " + private, "https": private}
    if match[2] not in states.values():
        raise RuntimeError("私有端口配置已变化，停止覆盖")
    return source.replace("{ " + match[2] + " }", "{ " + states[stage] + " }", 1), match[1], match[2]


def expand_ports(values):
    ports = set()
    for value in values:
        if isinstance(value, int):
            ports.add(value)
        elif isinstance(value, dict) and set(value) == {"range"}:
            ports.update(range(value["range"][0], value["range"][1] + 1))
        else:
            raise RuntimeError("无法解析运行中的端口规则")
    return ports


def check_live_network(snapshot, interface, configured_ports):
    """忽略动态计数和 handle，要求运行规则与磁盘规则一致。"""
    entries = snapshot["nftables"]
    if any(set(entry) not in ({"metainfo"}, {"table"}, {"chain"}, {"rule"}) for entry in entries):
        raise RuntimeError("网络表包含未识别对象")
    tables = [entry["table"] for entry in entries if "table" in entry]
    chains = [entry["chain"] for entry in entries if "chain" in entry]
    rules = [entry["rule"] for entry in entries if "rule" in entry]
    if len(tables) != 1 or len(chains) != 1 or len(rules) != 2:
        raise RuntimeError("运行网络规则数量已变化")
    chain = {key: value for key, value in chains[0].items() if key != "handle"}
    if chain != {"family": "inet", "table": "orbit_private", "name": "ingress", "type": "filter",
                 "hook": "prerouting", "prio": -310, "policy": "accept"}:
        raise RuntimeError("私有网络链身份或优先级已变化")
    expected_tcp = set(PRIVATE_TCP)
    if configured_ports.startswith("80, "):
        expected_tcp.update([80, 443])
    elif configured_ports.startswith("443, "):
        expected_tcp.add(443)
    for rule, protocol, expected in zip(rules, ["tcp", "udp"], [expected_tcp, {8472, *range(30000, 32768)}]):
        expr = rule["expr"]
        if (len(expr) != 4 or expr[0] != {"match": {"op": "==", "left": {"meta": {"key": "iifname"}}, "right": interface}}
                or set(expr[2]) != {"counter"} or expr[3] != {"drop": None}):
            raise RuntimeError("私有网络表达式已变化")
        match = expr[1].get("match", {})
        if (match.get("op") != "==" or match.get("left") != {"payload": {"protocol": protocol, "field": "dport"}}
                or set(match.get("right", {})) != {"set"} or expand_ports(match["right"]["set"]) != expected):
            raise RuntimeError("运行端口与磁盘规则不一致")


def policy_accepted(policy):
    # Gateway Policy 的状态按 ancestor 保存，不是顶层 conditions。
    return any(
        ancestor.get("ancestorRef", {}).get("name") == "orbit-management"
        and ancestor.get("ancestorRef", {}).get("namespace") == "orbit-system"
        and ancestor.get("ancestorRef", {}).get("sectionName") == "management-https"
        and ancestor.get("controllerName") == "gateway.envoyproxy.io/gatewayclass-controller"
        and any(c.get("type") == "Accepted" and c.get("status") == "True"
                and c.get("observedGeneration") == policy["metadata"]["generation"]
                for c in ancestor.get("conditions", []))
        for ancestor in policy.get("status", {}).get("ancestors", []))


def configure_network(stage):
    if NETWORK_FILE.is_symlink() or NETWORK_FILE.stat().st_uid != 0 or NETWORK_FILE.stat().st_mode & 0o022:
        raise RuntimeError("网络配置文件所有权或权限不符合预期")
    source = NETWORK_FILE.read_text()
    desired, interface, ports = network_configuration(source, stage)
    check_live_network(json.loads(run(["nft", "-j", "list", "table", "inet", "orbit_private"])), interface, ports)
    if stage != "closed":
        if not policy_accepted(get("securitypolicy", "orbit-management-origin", "orbit-system")):
            raise RuntimeError("来源保护策略未被控制器接受")
        if stage == "https":
            cert = get("certificate", "orbit-management-tls", "orbit-system")
            if not any(c.get("type") == "Ready" and c.get("status") == "True"
                       and c.get("observedGeneration") == cert["metadata"]["generation"]
                       for c in cert.get("status", {}).get("conditions", [])):
                raise RuntimeError("IP 证书未就绪，不能打开 HTTPS")
    if desired == source:
        print("MANAGEMENT_NETWORK_UNCHANGED")
        return
    batch = "delete table inet orbit_private\n" + desired
    run(["nft", "--check", "--file", "-"], batch)
    # 仅在一个 nft transaction 中替换自己的表；不 flush 其他表，不出现开放窗口。
    # 持久文件不带 delete，以便主机重启时加载空表。
    with tempfile.NamedTemporaryFile(mode="w", prefix=".orbit-private-", dir=NETWORK_FILE.parent, delete=False) as temporary:
        candidate = Path(temporary.name)
        temporary.write(desired)
        temporary.flush()
        os.fsync(temporary.fileno())
    try:
        if NETWORK_FILE.read_text() != source:
            raise RuntimeError("网络文件发生并发修改")
        run(["nft", "--file", "-"], batch)
        try:
            os.replace(candidate, NETWORK_FILE)
        except OSError:
            run(["nft", "--file", "-"], "delete table inet orbit_private\n" + source)
            raise
    finally:
        candidate.unlink(missing_ok=True)
    print("MANAGEMENT_NETWORK_STAGE=" + stage)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--confirm-private-management", action="store_true", required=True)
    sub = parser.add_subparsers(dest="command", required=True)
    preparation = sub.add_parser("prepare")
    preparation.add_argument("manifest", type=Path)
    certificate = sub.add_parser("certificate")
    certificate.add_argument("public_ip")
    certificate.add_argument("--accept-acme-terms", action="store_true", required=True,
                             help="仅在用户已阅读并接受当前 ACME 条款后传入")
    network = sub.add_parser("network")
    network.add_argument("stage", choices=["closed", "acme", "https"])
    network.add_argument("--confirm-public-management-ports", action="store_true",
                         help="仅在用户明确授权本机管理入口的公网 80/443 后传入")
    args = parser.parse_args()
    check_private_cluster()
    if args.command == "prepare":
        prepare(args.manifest)
    elif args.command == "certificate":
        request_certificate(args.public_ip)
    else:
        if args.stage != "closed" and not args.confirm_public_management_ports:
            raise RuntimeError("公网管理端口尚未获得人工授权")
        configure_network(args.stage)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, OSError, subprocess.TimeoutExpired) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
