#!/usr/bin/env python3
"""平台外部的同 schema 升级入口；只允许既有 API 与四类 Worker 的受控维护。"""
import argparse
import copy
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time
import uuid

NAMES = ("orbit-api", "orbit-build-worker", "orbit-release-worker", "orbit-pipeline-worker", "orbit-gateway-worker")
WORKERS = NAMES[1:]
TERMINAL = {"complete", "aborted", "rolled_back"}
IMAGE = re.compile(r"[a-z0-9][a-z0-9.:/_-]*@sha256:[a-f0-9]{64}")

# SQL 只作为运维只读协议，不修改状态、不取消任务；运输死记录宁可阻止维护，不能误判排空。
DRAIN_SQL = """BEGIN READ ONLY;
WITH facts(category,n) AS (
SELECT 'build',count(*) FROM build_operations WHERE status IN ('pending','running','cancel_requested')
UNION ALL SELECT 'release',count(*) FROM release_operations WHERE status IN ('pending','running','cancel_requested')
UNION ALL SELECT 'outbox',count(*) FROM internal_event_outbox WHERE state IN ('pending','published')
UNION ALL SELECT 'webhook',count(*) FROM webhook_deliveries w WHERE state='pending' AND EXISTS (
    SELECT 1 FROM internal_event_outbox e WHERE e.topic='webhook_delivery.received.v1' AND e.aggregate_id=w.id AND e.state IN ('pending','published'))
UNION ALL SELECT 'source_check',count(*) FROM delivery_runs r JOIN delivery_pipelines p ON p.id=r.delivery_pipeline_id
         JOIN delivery_pipeline_revisions v ON v.delivery_pipeline_id=r.delivery_pipeline_id AND v.revision=r.pipeline_revision
         WHERE r.phase='artifact_ready' AND v.mode='auto_release' AND p.enabled
           AND p.current_revision=r.pipeline_revision AND p.activation_generation=r.activation_generation
UNION ALL SELECT 'gateway',count(*) FROM project_gateway_sync WHERE state IN ('pending','applying','retrying','deleting')
           OR (state='applied' AND desired_revision<>applied_revision)
UNION ALL SELECT 'lease_build',count(*) FROM build_operations WHERE lease_owner IS NOT NULL OR lease_expires_at IS NOT NULL
UNION ALL SELECT 'lease_release',count(*) FROM release_operations WHERE lease_owner IS NOT NULL OR lease_expires_at IS NOT NULL
UNION ALL SELECT 'lease_build_dispatch',count(*) FROM build_dispatches WHERE publish_token IS NOT NULL OR publish_expires_at IS NOT NULL
UNION ALL SELECT 'lease_release_dispatch',count(*) FROM operation_dispatches WHERE publish_token IS NOT NULL OR publish_expires_at IS NOT NULL
UNION ALL SELECT 'lease_outbox',count(*) FROM internal_event_outbox WHERE publish_token IS NOT NULL OR publish_expires_at IS NOT NULL
UNION ALL SELECT 'lease_gateway',count(*) FROM project_gateway_sync WHERE lease_token IS NOT NULL OR lease_expires_at IS NOT NULL)
SELECT json_build_object('active_total',sum(n),'lease_total',sum(CASE WHEN category LIKE 'lease_%' THEN n ELSE 0 END),
    'categories',json_object_agg(category,n)) FROM facts;
COMMIT;"""


def run(command, data=None, timeout=30):
    """外部错误正文可能包含凭据或对象内容；错误只暴露固定类别。"""
    result = subprocess.run(command, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("EXTERNAL_COMMAND_FAILED")
    return result.stdout


def protected_directory(path):
    """不接受符号链接、其他所有者或可被其他用户替换的状态目录。"""
    path = Path(path).absolute()
    if path.is_symlink():
        raise RuntimeError("STATE_DIRECTORY_REJECTED")
    # macOS 的 /var 是系统拥有的链接；允许可信祖先解析，但状态目录本身不可为链接。
    for parent in path.parents:
        if parent.is_symlink() and parent.lstat().st_uid != 0:
            raise RuntimeError("STATE_DIRECTORY_REJECTED")
    path = path.resolve()
    for parent in path.parents:
        if parent.exists():
            info = parent.stat()
            if info.st_uid not in (0, os.geteuid()) or (info.st_mode & 0o022 and not info.st_mode & 0o1000):
                raise RuntimeError("STATE_DIRECTORY_REJECTED")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    if info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError("STATE_DIRECTORY_REJECTED")
    return path


def write_state(path, state):
    """先 fsync 临时记录，再原子替换；杀进程后仍可从旧记录显式恢复。"""
    temporary = path.with_name("." + str(uuid.uuid4()) + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as file:
        json.dump(state, file, sort_keys=True)
        file.flush()
        os.fsync(file.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def read_state(path):
    if not path.exists():
        return None
    info = path.lstat()
    if path.is_symlink() or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError("STATE_FILE_REJECTED")
    return json.loads(path.read_text())


def file_digest(path):
    digest = hashlib.sha256()
    with path.open("rb") as file:
        while chunk := file.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def save_rollback_image(directory, operation_id, image):
    """独立 OCI 回退介质保留全部已用架构层，不依赖 kubelet 缓存未被 GC。"""
    archive = directory / (operation_id + "-rollback.tar")
    if archive.exists():
        raise RuntimeError("ROLLBACK_ARCHIVE_EXISTS")
    run(["/usr/local/bin/k3s", "ctr", "--namespace=k8s.io", "images", "export", "--platform=linux/amd64", str(archive), image], timeout=180)
    descriptor = os.open(archive, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_size <= 0:
            raise RuntimeError("ROLLBACK_ARCHIVE_REJECTED")
        os.fchmod(descriptor, 0o600)
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    directory_descriptor = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(directory_descriptor)
    finally:
        os.close(directory_descriptor)
    saved = {"name": archive.name, "sha256": file_digest(archive), "size": archive.stat().st_size, "image": image}
    run(["/usr/local/bin/k3s", "ctr", "--namespace=k8s.io", "images", "import", "--digests", "--platform=linux/amd64", str(archive)], timeout=180)
    cached = json.loads(run(["/usr/local/bin/k3s", "crictl", "inspecti", image]))
    repo_digests = cached.get("status", {}).get("repoDigests")
    if not isinstance(repo_digests, list) or image not in repo_digests:
        raise RuntimeError("ROLLBACK_IMAGE_CACHE_REJECTED")
    return saved


def load_rollback_image(args, state, directory):
    if args.local_context:
        return
    try:
        saved = state["rollback_archive"]
        expected_images = {
            state["before"][name]["spec"]["template"]["spec"]["containers"][0]["image"]
            for name in NAMES
        }
    except (KeyError, TypeError, IndexError):
        raise RuntimeError("ROLLBACK_ARCHIVE_REJECTED") from None
    if not isinstance(saved, dict) or len(expected_images) != 1 or saved.get("image") != next(iter(expected_images)) or \
            saved.get("name") != state["operation_id"] + "-rollback.tar":
        raise RuntimeError("ROLLBACK_ARCHIVE_REJECTED")
    archive = directory / saved["name"]
    info = archive.lstat()
    if archive.is_symlink() or not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or \
            stat.S_IMODE(info.st_mode) != 0o600 or not isinstance(saved.get("size"), int) or \
            isinstance(saved.get("size"), bool) or saved["size"] <= 0 or info.st_size != saved["size"] or \
            not isinstance(saved.get("sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", saved["sha256"]) or \
            file_digest(archive) != saved["sha256"]:
        raise RuntimeError("ROLLBACK_ARCHIVE_REJECTED")
    run(["/usr/local/bin/k3s", "ctr", "--namespace=k8s.io", "images", "import", "--digests", "--platform=linux/amd64", str(archive)], timeout=180)
    cached = json.loads(run(["/usr/local/bin/k3s", "crictl", "inspecti", saved["image"]]))
    repo_digests = cached.get("status", {}).get("repoDigests")
    if not isinstance(repo_digests, list) or saved["image"] not in repo_digests:
        raise RuntimeError("ROLLBACK_IMAGE_CACHE_REJECTED")


class Cluster:
    """固定 Kubernetes / PostgreSQL 外部边界，保持 stdout 不包含运行配置。"""
    def __init__(self, args, kube):
        self.args, self.kube = args, kube

    @staticmethod
    def generation(resource):
        generation = resource["metadata"].get("generation")
        if isinstance(generation, bool) or not isinstance(generation, int) or generation <= 0:
            raise RuntimeError("WORKLOAD_SPEC_CHANGED")
        return generation

    def bind_state(self, state, path):
        """绑定互斥 journal；中断的 patch 只接受相符的前/后 spec 与 generation，不猜测外部写入。"""
        self.state, self.path = state, path
        pending = state.get("pending_change")
        if pending:
            current = self.get("deployment", pending["name"])
            generation = self.generation(current)
            if current["spec"] == pending["before"]:
                expected_generation = pending["before_generation"]
            elif current["spec"] == pending["after"]:
                expected_generation = pending["after_generation"]
            else:
                raise RuntimeError("WORKLOAD_SPEC_CHANGED")
            if current["metadata"]["uid"] != state["before"][pending["name"]]["metadata"]["uid"] or \
                    generation != expected_generation:
                raise RuntimeError("WORKLOAD_SPEC_CHANGED")
            # API Server 可能已执行 patch，但客户端在记账前中断；只认可精确前/后状态。
            state["expected"][pending["name"]] = current["spec"]
            state["expected_generation"][pending["name"]] = generation
            state["pending_change"] = None
            write_state(path, state)

    def check_expected(self):
        for name in NAMES:
            current = self.get("deployment", name)
            generation = self.generation(current)
            if current["metadata"]["uid"] != self.state["before"][name]["metadata"]["uid"] or \
                    current["spec"] != self.state["expected"][name] or \
                    generation != self.state["expected_generation"][name]:
                raise RuntimeError("WORKLOAD_SPEC_CHANGED")

    def get(self, kind, name=None, namespace=True, extra=()):
        command = self.kube + ["get", kind] + ([name] if name else [])
        if namespace:
            command += ["-n", self.args.namespace]
        return json.loads(run(command + list(extra) + ["-o", "json"]))

    def sql(self, sql):
        command = self.kube + ["exec", "-i", "postgres-0", "-n", self.args.namespace, "--", "sh", "-ec",
                              'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"']
        return run(command, sql).strip()

    def guard(self):
        nodes = self.get("nodes", namespace=False)["items"]
        if len(nodes) != 1 or not any(c["type"] == "Ready" and c["status"] == "True" for c in nodes[0].get("status", {}).get("conditions", [])):
            raise RuntimeError("CLUSTER_IDENTITY_REJECTED")
        if self.args.local_context:
            if nodes[0]["metadata"]["name"] != self.args.local_context.removeprefix("kind-") + "-control-plane":
                raise RuntimeError("LOCAL_CLUSTER_REJECTED")
        else:
            run(["systemctl", "is-active", "--quiet", "orbit-private-network.service"])
            labels = nodes[0]["metadata"].get("labels", {})
            if labels.get("orbit-devops.dev/environment") != "private-test" or labels.get("kubernetes.io/arch") != "amd64":
                raise RuntimeError("PRIVATE_CLUSTER_REJECTED")
        system = self.get("namespace", "kube-system", namespace=False)
        namespace = self.get("namespace", self.args.namespace, namespace=False)
        if system["metadata"]["uid"] != self.args.cluster_uid or namespace["metadata"]["uid"] != self.args.namespace_uid or \
                namespace["metadata"].get("labels", {}).get("app.kubernetes.io/managed-by") != "orbit-devops":
            raise RuntimeError("CLUSTER_IDENTITY_REJECTED")
        if self.sql("BEGIN READ ONLY; SELECT version,dirty FROM schema_migrations; COMMIT;") != "26|f":
            raise RuntimeError("SCHEMA_REJECTED")

    def deployments(self):
        result = {}
        for name in NAMES:
            resource = self.get("deployment", name)
            role = "api" if name == "orbit-api" else name.removeprefix("orbit-")
            spec = resource["spec"]
            pod = spec["template"]["spec"]
            containers = pod["containers"]
            if len(containers) != 1 or containers[0]["name"] != role or \
                    containers[0].get("command") != ["/usr/local/bin/orbit-devops-" + role] or \
                    spec["selector"].get("matchLabels") != {"app": name} or spec["selector"].get("matchExpressions") or \
                    (not self.args.local_context and pod.get("serviceAccountName") != name) or \
                    not IMAGE.fullmatch(containers[0]["image"]) or not containers[0].get("readinessProbe"):
                raise RuntimeError("WORKLOAD_IDENTITY_REJECTED")
            result[name] = resource
        return result

    def configuration(self, deployments):
        """冻结引用对象身份/版本；不把 Secret 值写进升级记录，也不覆盖已有环境配置。"""
        references = {("configmap", "orbit-config")}
        for resource in deployments.values():
            container = resource["spec"]["template"]["spec"]["containers"][0]
            if any(e.get("name") == "ORBIT_DEVOPS_MIGRATE_ON_BOOT" for e in container.get("env", [])):
                raise RuntimeError("MIGRATION_OVERRIDE_REJECTED")
            sources = container.get("envFrom", [])
            if not any(e.get("configMapRef", {}).get("name") == "orbit-config" for e in sources):
                raise RuntimeError("CONFIGURATION_REJECTED")
            for source in sources:
                for key, kind in [("configMapRef", "configmap"), ("secretRef", "secret")]:
                    if key in source:
                        references.add((kind, source[key]["name"]))
            for environment in container.get("env", []):
                for key, kind in [("configMapKeyRef", "configmap"), ("secretKeyRef", "secret")]:
                    if key in environment.get("valueFrom", {}):
                        references.add((kind, environment["valueFrom"][key]["name"]))
        versions = {}
        for kind, name in sorted(references):
            value = self.get(kind, name)
            data = value.get("data", {})
            if kind == "configmap" and "ORBIT_DEVOPS_MIGRATE_ON_BOOT" in data and data["ORBIT_DEVOPS_MIGRATE_ON_BOOT"] != "false":
                raise RuntimeError("MIGRATION_CONFIGURATION_REJECTED")
            if kind == "secret" and "ORBIT_DEVOPS_MIGRATE_ON_BOOT" in data:
                raise RuntimeError("MIGRATION_CONFIGURATION_REJECTED")
            versions[kind + "/" + name] = {key: value["metadata"][key] for key in ("uid", "resourceVersion")}
        if self.get("configmap", "orbit-config").get("data", {}).get("ORBIT_DEVOPS_MIGRATE_ON_BOOT") != "false":
            raise RuntimeError("MIGRATION_CONFIGURATION_REJECTED")
        return versions

    def change(self, saved, replicas, image=None):
        """先持久化 patch 意图，再以 UID/generation CAS 修改；未完成记账可由 bind_state 解析。"""
        self.bind_state(self.state, self.path)
        current = self.get("deployment", saved["metadata"]["name"])
        if current["metadata"]["uid"] != saved["metadata"]["uid"]:
            raise RuntimeError("WORKLOAD_REPLACED")
        generation = self.generation(current)
        if generation != self.state["expected_generation"][saved["metadata"]["name"]]:
            raise RuntimeError("WORKLOAD_SPEC_CHANGED")
        if current["spec"]["template"]["spec"]["containers"][0]["image"] not in self.allowed_images:
            raise RuntimeError("WORKLOAD_IMAGE_CHANGED")
        if current["spec"] != self.state["expected"][saved["metadata"]["name"]]:
            raise RuntimeError("WORKLOAD_SPEC_CHANGED")
        template = copy.deepcopy(current["spec"]["template"])
        template["spec"]["containers"][0]["image"] = saved["spec"]["template"]["spec"]["containers"][0]["image"]
        if template != saved["spec"]["template"]:
            raise RuntimeError("WORKLOAD_CONFIGURATION_CHANGED")
        patch = [{"op": "test", "path": "/metadata/uid", "value": current["metadata"]["uid"]}]
        patch.append({"op": "test", "path": "/metadata/generation", "value": generation})
        patch.append({"op": "replace", "path": "/spec/replicas", "value": replicas})
        if image:
            patch.append({"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": image})
        desired = copy.deepcopy(current["spec"])
        desired["replicas"] = replicas
        if image:
            desired["template"]["spec"]["containers"][0]["image"] = image
        after_generation = generation + (desired != current["spec"])
        self.state["pending_change"] = {"name": saved["metadata"]["name"],
                                        "before": current["spec"], "after": desired,
                                        "before_generation": generation, "after_generation": after_generation}
        write_state(self.path, self.state)
        run(self.kube + ["patch", "deployment", saved["metadata"]["name"], "-n", self.args.namespace,
                         "--type=json", "--patch-file=/dev/stdin"], json.dumps(patch))
        self.state["expected"][saved["metadata"]["name"]] = desired
        self.state["expected_generation"][saved["metadata"]["name"]] = after_generation
        self.state["pending_change"] = None
        write_state(self.path, self.state)

    def gone(self, names, timeout):
        deadline = time.monotonic() + timeout
        while True:
            if all(not self.get("pods", extra=["-l", "app=" + name])["items"] for name in names):
                return
            if time.monotonic() >= deadline:
                raise RuntimeError("TERMINATION_TIMEOUT")
            time.sleep(1)

    def ready(self, name, timeout):
        run(self.kube + ["rollout", "status", "deployment/" + name, "-n", self.args.namespace,
                         "--timeout=" + str(timeout) + "s"], timeout=timeout + 20)

    def drain(self, timeout):
        deadline, zeroes = time.monotonic() + timeout, 0
        while True:
            observation = json.loads(self.sql(DRAIN_SQL))
            active = observation["active_total"]
            if not isinstance(active, int) or not isinstance(observation["lease_total"], int):
                raise RuntimeError("DRAIN_OBSERVATION_REJECTED")
            if active < 0:
                raise RuntimeError("DRAIN_OBSERVATION_REJECTED")
            zeroes = zeroes + 1 if active == 0 else 0
            if zeroes == 3:
                return
            if time.monotonic() >= deadline:
                raise RuntimeError("DRAIN_TIMEOUT")
            time.sleep(1)


def freeze_and_drain(cluster, state, path):
    """入口曾重新开放时，先再次排空；失败保留原 Worker 与镜像，不强制终止新任务。"""
    cluster.change(state["before"]["orbit-api"], 0)
    try:
        cluster.gone(("orbit-api",), 120)
        cluster.drain(cluster.args.drain_timeout)
        cluster.check_expected()
        if cluster.configuration(cluster.deployments()) != state["configuration"]:
            raise RuntimeError("RUNTIME_CONFIGURATION_CHANGED")
    except Exception:
        cluster.change(state["before"]["orbit-api"], 1)
        cluster.ready("orbit-api", cluster.args.ready_timeout)
        raise


def restore(cluster, state, path, timeout):
    """先终止所有新版进程，再恢复旧镜像；不会恢复/删除数据库或取消业务任务。"""
    if state["expected"]["orbit-api"]["replicas"] != 0:
        # 中断可能发生在新版 API 已启动但 complete 尚未记账的间隙。
        freeze_and_drain(cluster, state, path)
    state["phase"] = "restoring"
    write_state(path, state)
    load_rollback_image(cluster.args, state, path.parent)
    check_configuration(cluster, state)
    for name in NAMES:
        cluster.change(state["before"][name], 0)
    cluster.gone(NAMES, 120)
    for name in WORKERS + ("orbit-api",):
        check_configuration(cluster, state)
        saved = state["before"][name]
        cluster.change(saved, 1, saved["spec"]["template"]["spec"]["containers"][0]["image"])
        cluster.ready(name, timeout)
    cluster.check_expected()
    check_configuration(cluster, state)
    cluster.guard()
    state["phase"] = "rolled_back"
    state["maintenance_finished_at"] = datetime.now(timezone.utc).isoformat()
    write_state(path, state)


def check_configuration(cluster, state):
    """启动任何旧进程前与最终记账前均核验引用身份；并发修改只能人工处置。"""
    if cluster.configuration(cluster.deployments()) != state["configuration"]:
        raise RuntimeError("RUNTIME_CONFIGURATION_CHANGED")


def verify_backup(directory, cluster_uid, namespace_uid, checkpoint_id, *, owner=0):
    """校验备份文件协议；仅完整且真实恢复验证过的四制品集合能授权维护。"""
    receipt = read_state(directory / "receipt.json")
    if not isinstance(receipt, dict) or receipt.get("kind") != "orbit-control-plane-backup" or receipt.get("version") != 1 or \
            receipt.get("checkpoint_id") != checkpoint_id or not isinstance(receipt.get("source"), dict) or \
            not isinstance(receipt.get("verification"), dict) or not isinstance(receipt.get("artifacts"), dict):
        raise RuntimeError("VERIFIED_BACKUP_REJECTED")
    source, verified = receipt["source"], receipt["verification"]
    if source.get("cluster_uid") != cluster_uid or source.get("orbit_system_uid") != namespace_uid or \
            source.get("schema_version") != 26 or source.get("schema_dirty") is not False or verified.get("status") != "verified" or \
            verified.get("validation_database_dropped") is not True or \
            verified.get("checks") != ["restore", "schema", "known_tables", "constraints", "row_counts"] or \
            set(receipt["artifacts"]) != {"database.dump", "globals.sql", "role-contract.json", "runtime.json"}:
        raise RuntimeError("VERIFIED_BACKUP_REJECTED")
    age = (datetime.now(timezone.utc) - datetime.fromisoformat(receipt["verified_at"].replace("Z", "+00:00"))).total_seconds()
    if not 0 <= age <= 86400:
        raise RuntimeError("BACKUP_TOO_OLD")
    for name, expected in receipt["artifacts"].items():
        if not isinstance(expected, dict) or not isinstance(expected.get("size"), int) or expected["size"] <= 0 or \
                not isinstance(expected.get("sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", expected["sha256"]):
            raise RuntimeError("BACKUP_ARTIFACT_REJECTED")
        file = directory / name
        info = file.lstat()
        if not stat.S_ISREG(info.st_mode) or file.is_symlink() or info.st_uid != owner or info.st_mode & 0o077 or \
                info.st_size != expected["size"] or file_digest(file) != expected["sha256"]:
            raise RuntimeError("BACKUP_ARTIFACT_REJECTED")


def backup_receipt(args):
    if args.local_context:
        return
    if not args.backup_id or not re.fullmatch(r"[a-z0-9][a-z0-9_]{0,31}", args.backup_id):
        raise RuntimeError("VERIFIED_BACKUP_REQUIRED")
    directory = protected_directory("/var/lib/orbit-devops/backups/" + args.backup_id)
    verify_backup(directory, args.cluster_uid, args.namespace_uid, args.backup_id)


def accept_runtime(cluster, state, path, args):
    """运行时切换与业务验收分开记账；人工确认公网边界，SQL 证明维护后真实自动交付。"""
    if not state or state["phase"] != "runtime_ready" or state["cluster_uid"] != args.cluster_uid or \
            state["namespace_uid"] != args.namespace_uid:
        raise RuntimeError("ACCEPTANCE_STATE_REJECTED")
    run_id = str(uuid.UUID(args.delivery_run))
    finished = datetime.fromisoformat(state["maintenance_finished_at"]).isoformat()
    cluster.bind_state(state, path)
    cluster.check_expected()
    check_configuration(cluster, state)
    for name in NAMES:
        cluster.ready(name, args.ready_timeout)
    # UUID 与时间先规范化，再插入固定只读语句；无需生产 Cookie、密码或 Token。
    result = json.loads(cluster.sql("""BEGIN READ ONLY;
/* ACCEPTANCE_PROOF */ SELECT json_build_object('verified',EXISTS (
 SELECT 1 FROM delivery_runs r JOIN build_operations b ON b.build_id=r.build_id
 JOIN image_artifacts a ON a.id=r.image_artifact_id
 JOIN releases l ON l.id=r.release_id JOIN release_operations o ON o.release_id=r.release_id
 JOIN webhook_deliveries w ON w.id=r.webhook_delivery_id
 WHERE r.id='%s'::uuid AND r.phase='completed' AND b.status='succeeded' AND o.status='succeeded'
 AND r.created_at>='%s'::timestamptz AND w.state='processed'
 AND l.image_artifact_id=a.id AND a.build_id=r.build_id)); COMMIT;""" % (run_id, finished)))
    if result.get("verified") is not True:
        raise RuntimeError("BUSINESS_DELIVERY_NOT_VERIFIED")
    state["postchecks"] = {"business_delivery": "verified", "delivery_run": run_id,
                           "public_login": "operator_confirmed", "auth_boundaries": "operator_confirmed",
                           "webhook_redelivery": "operator_reviewed"}
    state["accepted_at"] = datetime.now(timezone.utc).isoformat()
    state["phase"] = "complete"
    write_state(path, state)
    print("CONTROL_PLANE_ACCEPTANCE_COMPLETE")


def operate(args, cluster):
    """在独占锁内推进维护阶段，所有持久化点先于不可恢复的下一步。

    freezing/draining 失败仅恢复接纳；stopping_workers 后才能替换镜像。
    starting_candidate 失败独立恢复旧版；runtime_ready 尚待 accept 的业务验收。
    进程被杀保留 journal，不把 KeyboardInterrupt 或未知外部修改当成可安全覆盖的失败。
    """
    directory = protected_directory(args.state_dir)
    fd = os.open(directory / "lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError("UPGRADE_ALREADY_RUNNING") from None
        path = directory / "current.json"
        state = read_state(path)
        if args.operation == "status":
            print(json.dumps({key: state.get(key) for key in ("operation_id", "phase", "image", "last_error",
                             "maintenance_started_at", "maintenance_finished_at", "postchecks", "accepted_at")} if state else {"phase": "not_started"}))
            return
        if args.operation == "accept":
            accept_runtime(cluster, state, path, args)
            return
        before = cluster.deployments()
        config = cluster.configuration(before)
        cluster.allowed_images = {r["spec"]["template"]["spec"]["containers"][0]["image"] for r in (state["before"] if args.operation == "rollback" and state else before).values()}
        if args.operation == "upgrade":
            cluster.allowed_images.add(args.image)
        elif state:
            cluster.allowed_images.add(state["image"])
        if args.operation == "rollback":
            if not state or state["cluster_uid"] != args.cluster_uid or state["namespace_uid"] != args.namespace_uid or config != state["configuration"]:
                raise RuntimeError("ROLLBACK_STATE_REJECTED")
            cluster.bind_state(state, path)
            cluster.check_expected()
            load_rollback_image(args, state, directory)
            if state["phase"] in {"rolled_back", "aborted"}:
                for name in NAMES:
                    cluster.ready(name, args.ready_timeout)
                check_configuration(cluster, state)
                print("CONTROL_PLANE_ALREADY_RESTORED")
                return
            if state["phase"] in {"freezing", "draining"}:
                # 尚未进入 Worker 停止阶段，恢复入口即可；不得把活动任务一起终止。
                cluster.change(state["before"]["orbit-api"], 1)
                cluster.ready("orbit-api", args.ready_timeout)
                check_configuration(cluster, state)
                state["phase"] = "aborted"
                state["maintenance_finished_at"] = datetime.now(timezone.utc).isoformat()
                write_state(path, state)
                print("CONTROL_PLANE_ADMISSION_RESTORED")
                return
            if state["phase"] == "stopping_workers":
                # 尚未替换镜像；只恢复已停止的旧 Worker，保持其他执行中的进程不动。
                for name in WORKERS + ("orbit-api",):
                    check_configuration(cluster, state)
                    if state["expected"][name]["replicas"] != 1:
                        cluster.change(state["before"][name], 1)
                    cluster.ready(name, args.ready_timeout)
                cluster.check_expected()
                check_configuration(cluster, state)
                state["phase"] = "aborted"
                state["maintenance_finished_at"] = datetime.now(timezone.utc).isoformat()
                write_state(path, state)
                print("CONTROL_PLANE_ADMISSION_RESTORED")
                return
            if state["phase"] in {"complete", "runtime_ready"}:
                freeze_and_drain(cluster, state, path)
            restore(cluster, state, path, args.ready_timeout)
            print("CONTROL_PLANE_ROLLBACK_READY")
            return
        if state and state["phase"] not in TERMINAL:
            raise RuntimeError("INTERRUPTED_UPGRADE_REQUIRES_ROLLBACK")
        if not IMAGE.fullmatch(args.image) or any(r["spec"].get("replicas") != 1 for r in before.values()):
            raise RuntimeError("UPGRADE_BASELINE_REJECTED")
        images = {r["spec"]["template"]["spec"]["containers"][0]["image"] for r in before.values()}
        if len(images) != 1 or args.image.split("@")[0] != next(iter(images)).split("@")[0]:
            raise RuntimeError("IMAGE_REPOSITORY_REJECTED")
        for name in NAMES:
            cluster.ready(name, args.ready_timeout)
        backup_receipt(args)
        if not args.local_context:
            # 引导清单采用 Never；切换前预拉到唯一节点，保留原策略，不让停服窗口依赖网络。
            run(["/usr/local/bin/k3s", "crictl", "pull", args.image], timeout=180)
            cached = json.loads(run(["/usr/local/bin/k3s", "crictl", "inspecti", args.image]))
            if args.image not in cached["status"].get("repoDigests", []):
                raise RuntimeError("IMAGE_CACHE_REJECTED")
        if state:
            write_state(directory / (state["operation_id"] + ".json"), state)
        operation_id = str(uuid.uuid4())
        archive = None if args.local_context else save_rollback_image(directory, operation_id, next(iter(images)))
        state = {"version": 2, "operation_id": operation_id, "phase": "freezing", "image": args.image,
                 "cluster_uid": args.cluster_uid, "namespace_uid": args.namespace_uid,
                 "before": before, "configuration": config, "last_error": None,
                 "expected": {name: copy.deepcopy(r["spec"]) for name, r in before.items()}, "pending_change": None,
                 "expected_generation": {name: cluster.generation(r) for name, r in before.items()},
                 "rollback_archive": archive,
                 "maintenance_started_at": datetime.now(timezone.utc).isoformat(), "maintenance_finished_at": None,
                 "postchecks": {"webhook_redelivery": "pending", "business_delivery": "pending"}}
        write_state(path, state)
        cluster.bind_state(state, path)
        try:
            cluster.change(before["orbit-api"], 0)
            cluster.gone(("orbit-api",), 120)
            state["phase"] = "draining"
            write_state(path, state)
            cluster.drain(args.drain_timeout)
            cluster.check_expected()
            if cluster.configuration(cluster.deployments()) != config:
                raise RuntimeError("RUNTIME_CONFIGURATION_CHANGED")
        except Exception as error:
            state["last_error"] = str(error) if isinstance(error, RuntimeError) else "DRAIN_OBSERVATION_FAILED"
            cluster.change(before["orbit-api"], 1)
            cluster.ready("orbit-api", args.ready_timeout)
            state["phase"] = "aborted"
            state["maintenance_finished_at"] = datetime.now(timezone.utc).isoformat()
            write_state(path, state)
            raise
        try:
            state["phase"] = "stopping_workers"
            write_state(path, state)
            for name in WORKERS:
                cluster.change(before[name], 0)
            cluster.gone(WORKERS, 120)
            cluster.check_expected()
            if cluster.configuration(cluster.deployments()) != config:
                raise RuntimeError("RUNTIME_CONFIGURATION_CHANGED")
            observation = json.loads(cluster.sql(DRAIN_SQL))
            if observation["active_total"] != 0 or observation["lease_total"] != 0:
                raise RuntimeError("POST_STOP_DRAIN_REJECTED")
            state["drain"] = observation
            state["phase"] = "starting_candidate"
            write_state(path, state)
            for name in WORKERS + ("orbit-api",):
                cluster.change(before[name], 1, args.image)
                cluster.ready(name, args.ready_timeout)
            cluster.check_expected()
            if cluster.configuration(cluster.deployments()) != config:
                raise RuntimeError("RUNTIME_CONFIGURATION_CHANGED")
            cluster.guard()
            state["phase"] = "runtime_ready"
            state["maintenance_finished_at"] = datetime.now(timezone.utc).isoformat()
            write_state(path, state)
        except Exception as error:
            state["last_error"] = str(error) if isinstance(error, RuntimeError) else "UPGRADE_FAILED"
            restore(cluster, state, path, args.ready_timeout)
            raise
        print("CONTROL_PLANE_UPGRADE_READY ACCEPTANCE_PENDING")


def main(argv=None):
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--confirm-private-upgrade", action="store_true", required=True)
    parser.add_argument("--cluster-uid", required=True)
    parser.add_argument("--namespace-uid", required=True)
    parser.add_argument("--local-context")
    parser.add_argument("--namespace", default="orbit-system")
    parser.add_argument("--state-dir", default="/var/lib/orbit-devops/upgrades")
    parser.add_argument("--backup-id")
    parser.add_argument("--drain-timeout", type=int, default=300)
    parser.add_argument("--ready-timeout", type=int, default=180)
    commands = parser.add_subparsers(dest="operation", required=True)
    upgrade = commands.add_parser("upgrade")
    upgrade.add_argument("--image", required=True)
    upgrade.add_argument("--drain-timeout", type=int, default=argparse.SUPPRESS)
    upgrade.add_argument("--ready-timeout", type=int, default=argparse.SUPPRESS)
    commands.add_parser("rollback")
    commands.add_parser("status")
    accept = commands.add_parser("accept")
    accept.add_argument("--delivery-run", required=True)
    accept.add_argument("--confirm-public-login", action="store_true", required=True)
    accept.add_argument("--confirm-auth-boundaries", action="store_true", required=True)
    accept.add_argument("--confirm-webhook-review", action="store_true", required=True)
    args = parser.parse_args(argv)
    if not 1 <= args.drain_timeout <= 3600 or not 1 <= args.ready_timeout <= 600:
        raise RuntimeError("TIMEOUT_REJECTED")
    if args.local_context:
        if not re.fullmatch(r"kind-orbit-upgrade-[a-z0-9-]+", args.local_context) or \
                not re.fullmatch(r"orbit-upgrade-[a-z0-9-]+", args.namespace):
            raise RuntimeError("LOCAL_ENVIRONMENT_REJECTED")
        kube = ["kubectl", "--context", args.local_context, "--request-timeout=15s"]
    else:
        if os.geteuid() != 0 or args.namespace != "orbit-system" or args.state_dir != "/var/lib/orbit-devops/upgrades":
            raise RuntimeError("PRIVATE_ENVIRONMENT_REJECTED")
        kube = ["/usr/local/bin/k3s", "kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml", "--request-timeout=15s"]
    cluster = Cluster(args, kube)
    cluster.guard()
    operate(args, cluster)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        raise SystemExit("CONTROL_PLANE_OPERATION_FAILED " + str(error)) from None
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        raise SystemExit("CONTROL_PLANE_OPERATION_FAILED; inspect the protected state on the host") from None
