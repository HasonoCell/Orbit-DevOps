#!/usr/bin/python3 -I
"""固定主机只读巡检入口；仅输出有限健康类别，绝不回显主机数据或子进程日志。"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import stat
import re
import subprocess
import sys
import tempfile
import uuid

sys.dont_write_bytecode = True
WORKLOADS = ("orbit-api", "orbit-build-worker", "orbit-release-worker", "orbit-pipeline-worker", "orbit-gateway-worker")

# 排队延迟使用「该任务应被处理的时间」，不是普通运行时长；持有有效租约的构建不触发误报。
# 隔离与人工恢复状态独立计数，避免反复重发更新 next_dispatch_at 掩盖业务任务积压。
TASK_LAG_SQL = """BEGIN READ ONLY;
SET LOCAL statement_timeout='5s';
WITH due(at) AS (
SELECT available_at FROM build_operations WHERE status='pending' AND available_at<=now()
UNION ALL SELECT lease_expires_at FROM build_operations WHERE status IN ('running','cancel_requested') AND lease_expires_at<=now()
UNION ALL SELECT available_at FROM release_operations WHERE status='pending' AND available_at<=now()
UNION ALL SELECT lease_expires_at FROM release_operations WHERE status IN ('running','cancel_requested') AND lease_expires_at<=now()
UNION ALL SELECT next_dispatch_at FROM build_dispatches WHERE state IN ('pending','published') AND next_dispatch_at<=now()
UNION ALL SELECT next_dispatch_at FROM operation_dispatches WHERE state IN ('pending','published') AND next_dispatch_at<=now()
UNION ALL SELECT next_dispatch_at FROM internal_event_outbox WHERE state IN ('pending','published') AND next_dispatch_at<=now()
UNION ALL SELECT received_at FROM webhook_deliveries WHERE state='pending'
UNION ALL SELECT coalesce(r.source_check_next_at,r.updated_at) FROM delivery_runs r
  JOIN delivery_pipelines p ON p.id=r.delivery_pipeline_id
  JOIN delivery_pipeline_revisions v ON v.delivery_pipeline_id=r.delivery_pipeline_id AND v.revision=r.pipeline_revision
  WHERE r.phase='artifact_ready' AND v.mode='auto_release' AND p.enabled
    AND p.current_revision=r.pipeline_revision AND p.activation_generation=r.activation_generation
    AND coalesce(r.source_check_next_at,r.updated_at)<=now()
UNION ALL SELECT next_attempt_at FROM project_gateway_sync
  WHERE state IN ('pending','applying','retrying','deleting') AND next_attempt_at<=now()
UNION ALL SELECT updated_at FROM project_gateway_sync WHERE state='applied' AND desired_revision<>applied_revision
), isolated(n) AS (
SELECT count(*) FROM build_dispatches WHERE state='quarantined'
UNION ALL SELECT count(*) FROM operation_dispatches WHERE state='quarantined'
UNION ALL SELECT count(*) FROM internal_event_outbox WHERE state='quarantined'
UNION ALL SELECT count(*) FROM webhook_deliveries WHERE state='quarantined'
), attention(n) AS (
SELECT count(*) FROM build_operations WHERE status='attention_required'
UNION ALL SELECT count(*) FROM release_operations WHERE status='attention_required'
UNION ALL SELECT count(*) FROM project_gateway_sync WHERE state='attention_required'
)
SELECT json_build_object('max_overdue_seconds',coalesce((SELECT greatest(0,floor(extract(epoch FROM now()-min(at))))::bigint FROM due),0),
  'quarantined_total',(SELECT sum(n) FROM isolated),'attention_total',(SELECT sum(n) FROM attention));
COMMIT;"""


class Boundary:
    """生产路径不可配置；fixture 只适配外部文件和命令，不获得 root 权限。"""
    def __init__(self, fixture_root=None):
        self.owner = os.geteuid()
        self.fixture = fixture_root is not None
        self.root = Path("/")
        if self.fixture:
            candidate = Path(fixture_root)
            if self.owner == 0 or candidate.is_symlink() or not candidate.is_dir():
                raise ValueError("fixture")
            self.root = candidate.resolve()
            if self.root.parent != Path(tempfile.gettempdir()).resolve() or \
                    not self.root.name.startswith("orbit-host-health-fixture-") or \
                    self.root.stat().st_uid != self.owner or stat.S_IMODE(self.root.stat().st_mode) != 0o700:
                raise ValueError("fixture")
        elif self.owner != 0:
            raise ValueError("root")

    def path(self, relative):
        current = self.root
        for part in Path(relative).parts:
            current /= part
            info = current.lstat()
            if stat.S_ISLNK(info.st_mode) or info.st_uid != self.owner or info.st_mode & 0o022:
                raise ValueError("path")
        return current

    def read(self, relative, *, limit=1024 * 1024):
        path = self.path(relative)
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != self.owner or info.st_nlink != 1 or \
                    stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > limit:
                raise ValueError("file")
            value = handle.read(limit + 1)
            if len(value) > limit:
                raise ValueError("file")
            return value

    def digest(self, relative, expected):
        """分块校验已封存的备份；文件身份或内容在读取期间变化时拒绝成功。"""
        if not isinstance(expected, dict) or type(expected.get("size")) is not int or expected["size"] < 1 or \
                not isinstance(expected.get("sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", expected["sha256"]):
            raise ValueError("artifact")
        path = self.path(relative)
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as handle:
            before = os.fstat(handle.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_uid != self.owner or before.st_nlink != 1 or \
                    stat.S_IMODE(before.st_mode) != 0o600 or before.st_size != expected["size"]:
                raise ValueError("artifact")
            digest = hashlib.sha256()
            while chunk := handle.read(65536):
                digest.update(chunk)
            after = os.fstat(handle.fileno())
            if (before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != \
                    (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns) or \
                    path.lstat().st_ino != before.st_ino or digest.hexdigest() != expected["sha256"]:
                raise ValueError("artifact")

    def execute(self, name, arguments, *, content=None):
        relative = "bin/" + name if self.fixture else {"k3s": "usr/local/bin/k3s", "systemctl": "usr/bin/systemctl"}[name]
        executable = self.path(relative)
        if not executable.is_file():
            raise ValueError("command")
        result = subprocess.run([str(executable), *arguments], text=True, input=content,
                                capture_output=True, timeout=20,
                                env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8"})
        if result.returncode or len(result.stdout) > 4 * 1024 * 1024:
            raise ValueError("command")
        return result.stdout

    def kubectl(self, arguments):
        return self.execute("k3s", ["kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
                                    "--request-timeout=15s", *arguments])

    def get(self, kind, names, namespace=None):
        """集合名称逐项作为 argv；kubectl 不把单个 CSV 名称解释成多个资源。"""
        if isinstance(names, str):
            names = (names,)
        arguments = ["get", kind, *names, "-o", "json"]
        if namespace:
            arguments.extend(["-n", namespace])
        return json.loads(self.kubectl(arguments))

    def sql(self, query):
        """只向固定数据库 Pod 提交只读事务；密码仅从 Pod 环境进入 psql，不读 Secret。"""
        command = 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
        return self.execute("k3s", ["kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml", "--request-timeout=15s",
                                    "exec", "-i", "-n", "orbit-system", "postgres-0", "--", "sh", "-ec", command], content=query)

    def filesystem(self, relative):
        if self.fixture:
            return json.loads(self.read("capacity.json"))[relative]
        path = self.root if relative == "/" else self.path(relative)
        value = os.statvfs(path)
        return {key: getattr(value, "f_" + key) for key in ("blocks", "bfree", "bavail", "frsize", "files", "ffree")}


class Health:
    """先核对环境身份，再检查固定资源；失败即停止，不能把错集群报告为健康。"""
    def __init__(self, boundary):
        self.boundary = boundary
        self.config = None

    def identity(self):
        directory = self.boundary.path("etc/orbit-devops")
        if stat.S_IMODE(directory.stat().st_mode) != 0o700:
            raise ValueError("config")
        config = json.loads(self.boundary.read("etc/orbit-devops/host-health.json"))
        if not isinstance(config, dict) or set(config) != {"kind", "version", "cluster_uid", "namespace_uid", "certificate_name"} or \
                config["kind"] != "orbit-host-health" or type(config["version"]) is not int or config["version"] != 1 or \
                config["certificate_name"] != "orbit-management-tls":
            raise ValueError("config")
        for key in ("cluster_uid", "namespace_uid"):
            if str(uuid.UUID(config[key])) != config[key]:
                raise ValueError("config")
        for namespace, expected in (("kube-system", config["cluster_uid"]), ("orbit-system", config["namespace_uid"])):
            if self.boundary.get("namespace", namespace)["metadata"]["uid"] != expected:
                raise ValueError("identity")
        self.config = config

    def node(self):
        nodes = json.loads(self.boundary.kubectl(["get", "nodes", "-o", "json"]))["items"]
        if len(nodes) != 1:
            raise ValueError("node")

        conditions = {item["type"]: item["status"] for item in nodes[0]["status"]["conditions"]}
        if conditions.get("Ready") != "True" or conditions.get("DiskPressure") != "False":
            raise ValueError("node")

    def system_services(self):
        # 多单位 is-active 只要求至少一个 active；逐项调用才保证全部必要服务存活。
        for service in ("k3s.service", "orbit-private-network.service", "ssh.service"):
            self.boundary.execute("systemctl", ["is-active", "--quiet", service])

    def ready_resources(self, kind, names, namespace):
        """只有控制器已观察本轮配置、所有副本就绪时才报告健康，不接受旧版本 Ready。"""
        resources = self.boundary.get(kind, names, namespace)
        items = resources.get("items", [resources])
        if len(items) != len(names) or {item["metadata"]["name"] for item in items} != set(names):
            raise ValueError("workload")
        for item in items:
            generation = item["metadata"]["generation"]
            status = item["status"]
            if item["metadata"]["namespace"] != namespace or type(generation) is not int or generation < 1 or \
                    type(status.get("observedGeneration")) is not int or status["observedGeneration"] < generation or \
                    item["spec"].get("replicas") != 1 or \
                    any(type(status.get(key)) is not int or status[key] != 1 for key in ("replicas", "readyReplicas", "updatedReplicas")) or \
                    status.get("unavailableReplicas", 0) != 0:
                raise ValueError("workload")
            if kind == "deployments" and status.get("availableReplicas") != 1:
                raise ValueError("workload")

    def workloads(self):
        self.ready_resources("deployments", WORKLOADS, "orbit-system")

    def dependencies(self):
        self.ready_resources("statefulsets", ("postgres", "redis"), "orbit-system")
        self.ready_resources("statefulsets", ("registry",), "orbit-builds")
        if self.boundary.sql("BEGIN READ ONLY; SET LOCAL statement_timeout='5s'; SELECT version,dirty FROM schema_migrations; COMMIT;").strip() != "26|f":
            raise ValueError("schema")

    def backup(self):
        """仅完整、已恢复验证、同集群 schema 26 的最新封存备份可满足 48 小时时效。"""
        root = self.boundary.path("var/lib/orbit-devops/backups")
        if stat.S_IMODE(root.stat().st_mode) != 0o700:
            raise ValueError("backup")
        entries = list(root.iterdir())
        if len(entries) > 4096:
            raise ValueError("backup")
        candidates = []
        for entry in entries:
            if not re.fullmatch(r"[a-z0-9][a-z0-9_]{0,31}", entry.name):
                continue
            relative = "var/lib/orbit-devops/backups/" + entry.name
            directory = self.boundary.path(relative)
            if not directory.is_dir() or stat.S_IMODE(directory.stat().st_mode) != 0o700:
                raise ValueError("backup")
            receipt = json.loads(self.boundary.read(relative + "/receipt.json"))
            verified_at = datetime.fromisoformat(receipt["verified_at"].replace("Z", "+00:00"))
            if verified_at.tzinfo is None:
                raise ValueError("backup")
            candidates.append((verified_at, relative, entry.name, receipt))
        if not candidates:
            raise ValueError("backup")
        timestamp, relative, name, receipt = max(candidates, key=lambda entry: entry[0])
        if not 0 <= (datetime.now(timezone.utc) - timestamp).total_seconds() <= 48 * 3600 or \
                receipt.get("kind") != "orbit-control-plane-backup" or receipt.get("version") != 1 or receipt.get("checkpoint_id") != name:
            raise ValueError("backup")
        source, verified = receipt["source"], receipt["verification"]
        if source.get("cluster_uid") != self.config["cluster_uid"] or source.get("orbit_system_uid") != self.config["namespace_uid"] or \
                source.get("schema_version") != 26 or source.get("schema_dirty") is not False or \
                verified.get("status") != "verified" or verified.get("validation_database_dropped") is not True or \
                verified.get("checks") != ["restore", "schema", "known_tables", "constraints", "row_counts"] or \
                set(receipt["artifacts"]) != {"database.dump", "globals.sql", "role-contract.json", "runtime.json"}:
            raise ValueError("backup")
        for artifact, expected in receipt["artifacts"].items():
            self.boundary.digest(relative + "/" + artifact, expected)

    def capacity(self):
        """只告警固定文件系统，不自动清理；按可用空间与 inode 预算留出恢复余量。"""
        for path in ("/", "var/lib/rancher/k3s", "var/lib/orbit-devops/backups"):
            value = self.boundary.filesystem(path)
            if set(value) != {"blocks", "bfree", "bavail", "frsize", "files", "ffree"} or \
                    any(type(number) is not int or number < 0 for number in value.values()) or \
                    value["blocks"] < 1 or value["files"] < 1 or value["frsize"] < 1 or \
                    not value["bavail"] <= value["bfree"] <= value["blocks"] or value["ffree"] > value["files"] or \
                    value["bavail"] * value["frsize"] < 20 * 1024 ** 3 or \
                    (value["blocks"] - value["bfree"]) * 100 > value["blocks"] * 80 or \
                    (value["files"] - value["ffree"]) * 100 > value["files"] * 80:
                raise ValueError("capacity")

    def task_lag(self):
        result = json.loads(self.boundary.sql(TASK_LAG_SQL))
        if not isinstance(result, dict) or set(result) != {"max_overdue_seconds", "quarantined_total", "attention_total"} or \
                any(type(value) is not int or value < 0 for value in result.values()) or \
                result["max_overdue_seconds"] > 1800 or result["quarantined_total"] or result["attention_total"]:
            raise ValueError("task_lag")

    def certificate(self):
        """读取 Certificate 状态及续期控制器，绝不读取 TLS Secret；公网有效期由外部探针验收。"""
        self.ready_resources("deployments", ("cert-manager", "cert-manager-cainjector", "cert-manager-webhook"), "cert-manager")
        value = self.boundary.get("certificate", self.config["certificate_name"], "orbit-system")
        generation = value["metadata"]["generation"]
        ready = [item for item in value["status"]["conditions"] if item["type"] == "Ready"]
        if value["metadata"]["name"] != self.config["certificate_name"] or value["metadata"]["namespace"] != "orbit-system" or \
                type(generation) is not int or generation < 1 or len(ready) != 1 or ready[0].get("status") != "True" or \
                type(ready[0].get("observedGeneration")) is not int or ready[0]["observedGeneration"] < generation:
            raise ValueError("certificate")


def main():
    """公开入口只发固定类别；异常详情不流入 forced-command 或 CI 输出。"""
    category = "identity"
    try:
        parser = argparse.ArgumentParser(add_help=False, exit_on_error=False)
        parser.add_argument("--fixture-root")
        args, unknown = parser.parse_known_args()
        if unknown:
            raise ValueError("arguments")
        health = Health(Boundary(args.fixture_root))
        for category in ("identity", "node", "system_services", "workloads", "dependencies", "backup", "capacity", "task_lag", "certificate"):
            getattr(health, category)()
            print("HOST_HEALTH_OK check=" + category)
        print("HOST_HEALTH_READY")
        return 0
    except Exception:
        print("HOST_HEALTH_FAILED check=" + category)
        return 1


if __name__ == "__main__":
    sys.exit(main())
