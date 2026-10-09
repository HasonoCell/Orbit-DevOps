"""仅切换私人测试 API 的明确浏览器 Origin；旧配置留在服务器 root 目录用于恢复。"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
from urllib.parse import urlsplit

KEYS = ("ORBIT_DEVOPS_EXTERNAL_URL", "ORBIT_DEVOPS_TRUSTED_ORIGINS", "ORBIT_DEVOPS_ALLOW_LOOPBACK_HTTP")
ANNOTATION = "orbit-devops.dev/browser-config"


def browser_values(value):
    """只接受单个固定 Vercel HTTPS 地址，不使用通配 Origin 或请求 Header 推导信任。"""
    url = urlsplit(value)
    if not re.fullmatch(r"https://[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.vercel\.app", value):
        raise ValueError("a fixed Vercel HTTPS origin is required")
    if url.username or url.password or url.port or url.path or url.query or url.fragment:
        raise ValueError("only the exact HTTPS origin is allowed")
    return {KEYS[0]: value, KEYS[1]: value, KEYS[2]: "false"}


def config_patch(config, expected, desired):
    """逐字段 CAS，只修改三个浏览器配置键，其他连接与 Worker 参数原样保留。"""
    if any(config.get("data", {}).get(key) != expected[key] for key in KEYS):
        raise ValueError("browser configuration changed; refusing to overwrite it")
    patch = [{"op": "test", "path": "/metadata/uid", "value": config["metadata"]["uid"]},
             {"op": "test", "path": "/metadata/resourceVersion", "value": config["metadata"]["resourceVersion"]}]
    for key in KEYS:
        patch.append({"op": "test", "path": "/data/" + key, "value": expected[key]})
        patch.append({"op": "replace", "path": "/data/" + key, "value": desired[key]})
    return patch


def api_restart_patch(deployment, values):
    """只改变 API Pod 的配置指纹；不升级镜像、不重启 Worker、不运行迁移。"""
    spec = deployment["spec"]
    pod = spec["template"]["spec"]
    if spec.get("replicas") != 1 or pod.get("serviceAccountName") != "orbit-api" or \
            [c["name"] for c in pod["containers"]] != ["api"]:
        raise ValueError("unexpected private API deployment")
    container = pod["containers"][0]
    if not any(item.get("configMapRef", {}).get("name") == "orbit-config" for item in container.get("envFrom", [])) or \
            any(item["name"] in KEYS for item in container.get("env", [])):
        raise ValueError("API browser settings must come from orbit-config without overrides")
    annotations = dict(spec["template"]["metadata"].get("annotations", {}))
    fingerprint = hashlib.sha256(json.dumps(values, sort_keys=True).encode()).hexdigest()
    if annotations.get(ANNOTATION) == fingerprint:
        return []
    annotations[ANNOTATION] = fingerprint
    return [{"op": "test", "path": "/metadata/resourceVersion", "value": deployment["metadata"]["resourceVersion"]},
            {"op": "add", "path": "/spec/template/metadata/annotations", "value": annotations}]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--confirm-private-management", action="store_true", required=True)
    sub = parser.add_subparsers(dest="operation", required=True)
    apply = sub.add_parser("apply")
    apply.add_argument("--external-url", required=True)
    sub.add_parser("restore")
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise ValueError("run on the private test server as root")
    root = Path(__file__).resolve().parent
    module_spec = importlib.util.spec_from_file_location("management", root / "prepare-management-https.py")
    m = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(m)
    m.check_private_cluster()
    config = m.get("configmap", "orbit-config", "orbit-system")
    deployment = m.get("deployment", "orbit-api", "orbit-system")
    backup = root / "browser-config-before.json"
    if backup.exists():
        saved = json.loads(backup.read_text())
        if config["metadata"]["uid"] != saved["uid"]:
            raise ValueError("configuration identity changed")
    elif args.operation == "restore":
        raise ValueError("no saved browser configuration")
    else:
        saved = {"uid": config["metadata"]["uid"],
                 "old": {key: config["data"][key] for key in KEYS},
                 "new": browser_values(args.external_url)}
    desired = saved["new"] if args.operation == "apply" else saved["old"]
    if args.operation == "apply" and desired != browser_values(args.external_url):
        raise ValueError("saved cutover targets a different origin")
    current = {key: config["data"][key] for key in KEYS}
    expected = saved["old"] if args.operation == "apply" else saved["new"]
    restart = api_restart_patch(deployment, desired)
    restart_command = m.KUBE + ["patch", "deployment", "orbit-api", "-n", "orbit-system", "--type=json", "--patch-file=/dev/stdin"]
    if restart:
        m.run(restart_command + ["--dry-run=server"], json.dumps(restart))
    if current != desired:
        patch = config_patch(config, expected, desired)
        command = m.KUBE + ["patch", "configmap", "orbit-config", "-n", "orbit-system", "--type=json", "--patch-file=/dev/stdin"]
        m.run(command + ["--dry-run=server"], json.dumps(patch))
        if not backup.exists():
            fd = os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w") as file:
                json.dump(saved, file)
        m.run(command, json.dumps(patch))
    if restart:
        m.run(restart_command, json.dumps(restart))
    print("MANAGEMENT_BROWSER_" + args.operation.upper() + "_READY")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, RuntimeError) as error:
        raise SystemExit(str(error)) from None
