#!/usr/bin/env python3
"""本地 Kind 真实五进程演练；仅写入明确的任务 Namespace，不操作云集群。"""
import argparse
import copy
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
MANAGE = Path(__file__).with_name("manage-control-plane.py")
MANAGED = {"app.kubernetes.io/managed-by": "orbit-devops"}
ROLES = {"orbit-api": ("api", 8080), "orbit-build-worker": ("build-worker", 9092),
         "orbit-release-worker": ("release-worker", 9091),
         "orbit-pipeline-worker": ("pipeline-worker", 9093), "orbit-gateway-worker": ("gateway-worker", 9094)}
ORIGIN = "http://127.0.0.1:18087"
PASSWORD = "fixture-only!Local-Control-Plane-2026"


def run(command, *, data=None, timeout=60, check=True, cwd=ROOT, env=None):
    """敏感 stdin/stdout 不回显，错误只报告固定阶段与退出码。"""
    result = subprocess.run(command, input=data, text=True, capture_output=True, timeout=timeout, cwd=cwd, env=env)
    if check and result.returncode:
        raise RuntimeError("LOCAL_COMMAND_FAILED " + command[0] + " exit=" + str(result.returncode))
    return result


def eventually(observe, *, timeout=90, label="LOCAL_CONDITION_TIMEOUT"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            value = observe()
            if value:
                return value
        except (RuntimeError, OSError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise RuntimeError(label)


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


class Rehearsal:
    """所有 Kubernetes 写入口在执行前约束到同一已核验的本地任务边界。"""
    def __init__(self, args, temporary):
        self.context, self.namespace = args.context, args.namespace
        self.cluster = self.context.removeprefix("kind-")
        self.temporary = Path(temporary)
        self.state = self.temporary / "upgrade-state"
        self.kube = ["kubectl", "--context", self.context, "--request-timeout=15s"]
        self.processes = []
        self.images = {}
        self.node = ""
        self.cluster_uid = ""
        self.namespace_uid = ""
        self.api_url = ""
        self.api_forward = None
        self.architecture = ""
        self.templates = {}
        self.deployment_uids = {}
        self.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def get(self, resource, name=None, *, namespaced=True):
        command = self.kube + (["-n", self.namespace] if namespaced else []) + ["get", resource]
        if name:
            command.append(name)
        command += ["-o", "json"]
        return json.loads(run(command).stdout)

    def apply(self, document):
        documents = document.get("items", []) if document.get("kind") == "List" else [document]
        for item in documents:
            if item["kind"] == "Namespace":
                if item["metadata"]["name"] != self.namespace:
                    raise RuntimeError("LOCAL_NAMESPACE_WRITE_REJECTED")
            elif item["metadata"].get("namespace") != self.namespace:
                raise RuntimeError("LOCAL_RESOURCE_WRITE_REJECTED")
            if item["kind"] in {"ClusterRole", "ClusterRoleBinding", "Node", "CustomResourceDefinition"}:
                raise RuntimeError("LOCAL_CLUSTER_WRITE_REJECTED")
        run(self.kube + ["apply", "-f", "-"], data=json.dumps(document))

    def guard(self):
        if not re.fullmatch(r"kind-orbit-upgrade-[a-z0-9-]+", self.context) or \
                self.namespace != self.context.removeprefix("kind-"):
            raise RuntimeError("LOCAL_ENVIRONMENT_REJECTED")
        nodes = self.get("nodes", namespaced=False)["items"]
        if len(nodes) != 1 or nodes[0]["metadata"]["name"] != self.cluster + "-control-plane" or \
                not any(c["type"] == "Ready" and c["status"] == "True" for c in nodes[0]["status"]["conditions"]):
            raise RuntimeError("LOCAL_NODE_IDENTITY_REJECTED")
        self.node = nodes[0]["metadata"]["name"]
        self.architecture = nodes[0]["status"]["nodeInfo"]["architecture"]
        if self.architecture not in {"arm64", "amd64"}:
            raise RuntimeError("LOCAL_ARCHITECTURE_REJECTED")
        self.cluster_uid = self.get("namespace", "kube-system", namespaced=False)["metadata"]["uid"]
        # 不接管已有任务；避免重跑覆盖别人留下的容器或测试数据。
        result = run(self.kube + ["get", "namespace", self.namespace, "-o", "json"], check=False)
        if result.returncode == 0:
            raise RuntimeError("LOCAL_NAMESPACE_ALREADY_EXISTS")
        if "NotFound" not in result.stderr:
            raise RuntimeError("LOCAL_NAMESPACE_LOOKUP_FAILED")
        # create 原子拒绝并发同名 Namespace；不能让 apply 接管检查后新建的任务。
        namespace = {"apiVersion": "v1", "kind": "Namespace",
                     "metadata": {"name": self.namespace, "labels": MANAGED}}
        run(self.kube + ["create", "-f", "-"], data=json.dumps(namespace))
        self.namespace_uid = self.get("namespace", self.namespace, namespaced=False)["metadata"]["uid"]

    def namespaced(self, kind, name, **fields):
        return {"apiVersion": "apps/v1" if kind in {"Deployment", "StatefulSet"} else "v1", "kind": kind,
                "metadata": {"name": name, "namespace": self.namespace, "labels": MANAGED}, **fields}

    def import_image(self, image, archive=None):
        # Docker 的多平台索引可能只缓存本机架构。Kind 默认 --all-platforms 会误要求其它
        # 架构 blob；只导入已核验节点的平台，不从外网补齐不需要的镜像。
        exporter = None if archive else subprocess.Popen(["docker", "image", "save", image], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        stream = archive.open("rb") if archive else exporter.stdout
        try:
            result = subprocess.run(["docker", "exec", "-i", self.node, "ctr", "--namespace=k8s.io", "images", "import",
                            "--platform=linux/" + self.architecture, "--digests", "--snapshotter=overlayfs", "-"],
                            stdin=stream, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=180)
            stream.close()
            if result.returncode or (exporter and exporter.wait(timeout=10)):
                raise RuntimeError("LOCAL_IMAGE_IMPORT_FAILED")
        finally:
            stream.close()
            if exporter and exporter.poll() is None:
                exporter.kill()
                exporter.wait(timeout=10)

    def archive_reference(self, image, repository, archive):
        """取 OCI descriptor 的 Digest 并核对对应 manifest；不用 Docker config ID。"""
        run(["docker", "image", "save", "--output", str(archive), image], timeout=180)
        with tarfile.open(archive) as saved:
            stream = saved.extractfile("index.json")
            if stream is None:
                raise RuntimeError("LOCAL_OCI_INDEX_MISSING")
            index = json.load(stream)
            if len(index.get("manifests", [])) != 1:
                raise RuntimeError("LOCAL_OCI_DESCRIPTOR_AMBIGUOUS")
            descriptor = index["manifests"][0]
            digest = descriptor.get("digest", "")
            if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest) or descriptor.get("size", 0) > 256 * 1024:
                raise RuntimeError("LOCAL_OCI_DESCRIPTOR_INVALID")
            manifest = saved.extractfile("blobs/sha256/" + digest.removeprefix("sha256:"))
            if manifest is None:
                raise RuntimeError("LOCAL_OCI_MANIFEST_MISSING")
            content = manifest.read(256 * 1024 + 1)
            if len(content) != descriptor["size"] or "sha256:" + hashlib.sha256(content).hexdigest() != digest:
                raise RuntimeError("LOCAL_OCI_MANIFEST_INVALID")
        reference = repository + "@" + digest
        self.import_image(image, archive)
        cached = run(["docker", "exec", self.node, "ctr", "-n", "k8s.io", "images", "ls", "-q"]).stdout.splitlines()
        if reference not in cached:
            source = image if "." in image.split("/")[0] or ":" in image.split("/")[0] else "docker.io/" + image
            if "/" not in image:
                source = "docker.io/library/" + image
            run(["docker", "exec", self.node, "ctr", "-n", "k8s.io", "images", "tag", source, reference])
        return reference

    def dependencies(self):
        for image in ("postgres:17-alpine", "redis:7-alpine"):
            if run(["docker", "image", "inspect", image], check=False).returncode:
                mirrored = "docker.m.daocloud.io/library/" + image
                run(["docker", "pull", mirrored], timeout=180)
                run(["docker", "tag", mirrored, image])
            self.import_image(image)
        db_url = "postgres://orbitdevops:local-fixture@postgres." + self.namespace + ".svc.cluster.local:5432/orbitdevops?sslmode=disable"
        self.apply(self.namespaced("Secret", "orbit-database", type="Opaque", stringData={"url": db_url, "password": "local-fixture"}))
        for name, image, port, extra in (
            ("postgres", "postgres:17-alpine", 5432, {"env": [{"name": "POSTGRES_USER", "value": "orbitdevops"},
              {"name": "POSTGRES_DB", "value": "orbitdevops"}, {"name": "POSTGRES_PASSWORD", "value": "local-fixture"}]}),
            ("redis", "redis:7-alpine", 6379, {}),
        ):
            self.apply(self.namespaced("Service", name, spec={"selector": {"app": name},
                        "ports": [{"name": "service", "port": port, "targetPort": port}]}))
            container = {"name": name, "image": image, "imagePullPolicy": "Never", "ports": [{"containerPort": port}],
                         "readinessProbe": {"tcpSocket": {"port": port}, "periodSeconds": 1},
                         "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "512Mi"}}, **extra}
            if name == "postgres":
                container["volumeMounts"] = [{"name": "data", "mountPath": "/var/lib/postgresql/data"}]
            self.apply(self.namespaced("StatefulSet", name, spec={"serviceName": name, "replicas": 1,
                        "selector": {"matchLabels": {"app": name}}, "template": {"metadata": {"labels": {"app": name}},
                        "spec": {"automountServiceAccountToken": False, "containers": [container],
                                 "volumes": [{"name": "data", "emptyDir": {"sizeLimit": "1Gi"}}]}}}))
            run(self.kube + ["-n", self.namespace, "rollout", "status", "statefulset/" + name, "--timeout=120s"], timeout=140)

    def build_images(self):
        """使用本地 Go 与国内依赖源构建真实 binary，验证 OCI manifest 后离线导入。"""
        bins = self.temporary / "bins"
        bins.mkdir()
        environment = {**os.environ, "GOPROXY": "https://goproxy.cn,direct", "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": self.architecture}
        for role in ("api", "release-worker", "build-worker", "pipeline-worker", "gateway-worker", "migrate"):
            run(["go", "build", "-trimpath", "-o", str(bins / ("orbit-devops-" + role)), "./cmd/orbit-devops-" + role], timeout=600, env=environment)
        run(["go", "build", "-trimpath", "-o", str(bins / "local-fixture"), "./test/cloud"], timeout=600, env=environment)
        repository = "docker.io/orbit-local/control-plane-rehearsal"
        for version in ("before", "after", "broken"):
            dockerfile = "FROM scratch\nCOPY bins/ /usr/local/bin/\nUSER 65532:65532\nWORKDIR /tmp\n"
            if version == "broken":
                dockerfile += "COPY bins/local-fixture /usr/local/bin/orbit-devops-api\n"
            dockerfile += 'LABEL orbit.local.rehearsal="' + version + '"\n'
            (self.temporary / "Dockerfile").write_text(dockerfile)
            tag = repository + ":" + version
            run(["docker", "build", "--platform=linux/" + self.architecture, "-t", tag, str(self.temporary)], timeout=120)
            self.images[version] = self.archive_reference(tag, repository, self.temporary / (version + ".tar"))
        self.images["application"] = self.archive_reference("redis:7-alpine", "docker.io/library/redis", self.temporary / "redis.tar")

    def configure(self):
        raw = run(["kind", "get", "kubeconfig", "--internal", "--name", self.cluster]).stdout
        document = json.loads(run(["kubectl", "--kubeconfig=/dev/stdin", "config", "view", "--raw", "-o", "json"], data=raw).stdout)
        for cluster in document["clusters"]:
            cluster["cluster"]["server"] = "https://127.0.0.1:6443"
        self.apply(self.namespaced("Secret", "local-kubeconfig", type="Opaque",
                                  stringData={"kubeconfig": json.dumps(document)}))
        values = {"ORBIT_DEVOPS_MIGRATE_ON_BOOT": "false", "ORBIT_DEVOPS_KUBERNETES_MODE": "local-kind",
                  "ORBIT_DEVOPS_KUBECONFIG": "/fixture/kubeconfig", "ORBIT_DEVOPS_KUBERNETES_CONTEXT": self.context,
                  "ORBIT_DEVOPS_NAMESPACE": self.namespace, "ORBIT_DEVOPS_BUILD_NAMESPACE": self.namespace,
                  "ORBIT_DEVOPS_CLUSTER_REF": self.context, "ORBIT_DEVOPS_EXTERNAL_URL": ORIGIN,
                  "ORBIT_DEVOPS_TRUSTED_ORIGINS": ORIGIN, "ORBIT_DEVOPS_ALLOW_LOOPBACK_HTTP": "true",
                  "ORBIT_DEVOPS_REDIS_ADDRESS": "redis." + self.namespace + ".svc.cluster.local:6379",
                  "ORBIT_DEVOPS_GATEWAY_CLASS_NAME": "local-rehearsal", "ORBIT_DEVOPS_BUILD_REGISTRY_HOST": "registry.invalid:5000",
                  "ORBIT_DEVOPS_BUILD_REGISTRY_PREFIX": "orbit-local", "ORBIT_DEVOPS_BUILD_PLATFORM": "linux/" + self.architecture,
                  "ORBIT_DEVOPS_DB_MAX_OPEN_CONNS": "4", "ORBIT_DEVOPS_RELEASE_QUEUE_CONCURRENCY": "1",
                  "ORBIT_DEVOPS_BUILD_QUEUE_CONCURRENCY": "1", "ORBIT_DEVOPS_PIPELINE_QUEUE_CONCURRENCY": "1",
                  "ORBIT_DEVOPS_GATEWAY_QUEUE_CONCURRENCY": "1", "ORBIT_DEVOPS_KUBERNETES_POLL_INTERVAL": "100ms"}
        self.apply(self.namespaced("ConfigMap", "orbit-config", data=values))
        self.apply(self.namespaced("ConfigMap", "orbit-cluster-identity", data={"ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID": self.cluster_uid}))
        for name in ROLES:
            self.apply(self.namespaced("ServiceAccount", name, automountServiceAccountToken=False))

    def job(self, name, binary, arguments=None):
        self.apply({"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": name, "namespace": self.namespace},
                    "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 90, "template": {"spec": {
                    "restartPolicy": "Never", "automountServiceAccountToken": False, "containers": [{"name": "job",
                    "image": self.images["before"], "imagePullPolicy": "Never", "command": ["/usr/local/bin/" + binary],
                    "args": arguments or [], "env": [{"name": "ORBIT_DEVOPS_DATABASE_URL",
                    "valueFrom": {"secretKeyRef": {"name": "orbit-database", "key": "url"}}}]}]}}}})
        run(self.kube + ["-n", self.namespace, "wait", "--for=condition=complete", "job/" + name, "--timeout=90s"], timeout=110)

    def controls(self):
        for name, (role, port) in ROLES.items():
            container = {"name": role, "image": self.images["before"], "imagePullPolicy": "Never",
                         "command": ["/usr/local/bin/orbit-devops-" + role],
                         "envFrom": [{"configMapRef": {"name": "orbit-config"}}, {"configMapRef": {"name": "orbit-cluster-identity"}}],
                         "env": [{"name": "ORBIT_DEVOPS_" + ("API" if role == "api" else role.upper().replace("-", "_")) + "_ADDRESS",
                                  "value": "0.0.0.0:" + str(port)}, {"name": "ORBIT_DEVOPS_DATABASE_URL",
                                  "valueFrom": {"secretKeyRef": {"name": "orbit-database", "key": "url"}}}],
                         "ports": [{"name": "http", "containerPort": port}],
                         "readinessProbe": {"httpGet": {"path": "/healthz" if role == "api" else "/readyz", "port": "http"}, "periodSeconds": 1},
                         "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "384Mi"}},
                         "volumeMounts": [{"name": "kubeconfig", "mountPath": "/fixture", "readOnly": True}]}
            self.apply(self.namespaced("Deployment", name, spec={"replicas": 1, "strategy": {"type": "Recreate"},
                        "selector": {"matchLabels": {"app": name}}, "template": {"metadata": {"labels": {"app": name}},
                        "spec": {"serviceAccountName": name, "automountServiceAccountToken": False, "hostNetwork": True,
                        "dnsPolicy": "ClusterFirstWithHostNet", "terminationGracePeriodSeconds": 90, "containers": [container],
                        "volumes": [{"name": "kubeconfig", "secret": {"secretName": "local-kubeconfig"}}]}}}))
        for name in ROLES:
            run(self.kube + ["-n", self.namespace, "rollout", "status", "deployment/" + name, "--timeout=120s"], timeout=140)
            current = self.get("deployment", name)
            self.templates[name] = copy.deepcopy(current["spec"]["template"])
            self.deployment_uids[name] = current["metadata"]["uid"]
        # 不绕过 HTTP 身份认证：只用 fixture 初始化第一位测试管理员。
        run(self.kube + ["-n", self.namespace, "exec", "-i", "deployment/orbit-api", "--",
                        "/usr/local/bin/local-fixture", "initialize-admin"], data=PASSWORD)

    def request(self, path, body=None, *, expected=200):
        headers = {"Origin": ORIGIN, "X-Orbit-CSRF": "1", "Content-Type": "application/json",
                   "Idempotency-Key": "local-rehearsal-" + str(time.monotonic_ns())}
        request = urllib.request.Request(self.api_url + path,
                                         data=json.dumps(body).encode() if body is not None else None, headers=headers)
        try:
            response = self.http.open(request, timeout=10)
        except urllib.error.HTTPError as failure:
            response = failure
        with response:
            if response.status != expected:
                raise RuntimeError("LOCAL_HTTP_STATUS " + path + " expected=" + str(expected) + " actual=" + str(response.status))
            payload = response.read()
        return json.loads(payload) if payload else {}

    def connect_api(self):
        if self.api_forward is not None:
            self.api_forward.terminate()
            self.api_forward.wait(timeout=10)
        port = free_port()
        self.api_forward = subprocess.Popen(self.kube + ["-n", self.namespace, "port-forward", "--address=127.0.0.1",
                            "deployment/orbit-api", str(port) + ":8080"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.processes.append(self.api_forward)
        self.api_url = "http://127.0.0.1:" + str(port)
        eventually(lambda: self.request("/healthz"), timeout=30, label="LOCAL_API_FORWARD_TIMEOUT")

    def cli(self, operation, *, expect=0):
        command = [sys.executable, str(MANAGE), "--local-context", self.context, "--namespace", self.namespace,
                   "--cluster-uid", self.cluster_uid, "--namespace-uid", self.namespace_uid,
                   "--state-dir", str(self.state), "--confirm-private-upgrade"] + operation
        result = run(command, timeout=300, check=False)
        if result.returncode != expect:
            raise RuntimeError("LOCAL_UPGRADE_EXIT expected=" + str(expect) + " actual=" + str(result.returncode))
        if operation == ["status"]:
            return json.loads(result.stdout)
        return self.cli(["status"])

    def assert_controls(self, reference):
        pods = self.get("pods")["items"]
        for name in ROLES:
            item = self.get("deployment", name)
            expected = copy.deepcopy(self.templates[name])
            expected["spec"]["containers"][0]["image"] = reference
            if item["spec"]["replicas"] != 1 or item["spec"]["template"]["spec"]["containers"][0]["image"] != reference or \
                    item.get("status", {}).get("readyReplicas") != 1 or item["spec"]["template"] != expected or \
                    item["metadata"]["uid"] != self.deployment_uids[name] or \
                    item.get("status", {}).get("observedGeneration", 0) < item["metadata"]["generation"]:
                raise RuntimeError("LOCAL_CONTROL_NOT_READY " + name)
            current = [pod for pod in pods if pod["metadata"].get("labels", {}).get("app") == name]
            if len(current) != 1 or current[0]["status"]["phase"] != "Running":
                raise RuntimeError("LOCAL_CONTROL_POD_AMBIGUOUS " + name)
            states = current[0]["status"].get("containerStatuses", [])
            if len(states) != 1 or not states[0]["ready"] or states[0]["restartCount"] != 0:
                raise RuntimeError("LOCAL_CONTROL_POD_UNHEALTHY " + name)
            # 从真实 Pod 的运行时镜像身份回读 CRI，不能只断言期望 Deployment 字段。
            image_id = states[0]["imageID"].removeprefix("containerd://")
            runtime = json.loads(run(["docker", "exec", self.node, "crictl", "inspecti", image_id]).stdout)
            if reference not in runtime["status"].get("repoDigests", []):
                raise RuntimeError("LOCAL_CONTROL_RUNTIME_DIGEST_MISMATCH " + name)

    def normal_upgrade(self):
        self.connect_api()
        self.request("/api/v1/auth/login", {"loginName": "local-rehearsal", "password": PASSWORD})
        self.request("/api/v1/projects", expected=200)
        result = self.cli(["upgrade", "--image", self.images["after"], "--drain-timeout", "20", "--ready-timeout", "90"])
        if result.get("phase") != "runtime_ready":
            raise RuntimeError("LOCAL_UPGRADE_PHASE_INVALID")
        self.assert_controls(self.images["after"])
        self.connect_api()
        self.request("/api/v1/projects", expected=200)
        result = self.cli(["rollback"])
        if result.get("phase") != "rolled_back":
            raise RuntimeError("LOCAL_ROLLBACK_PHASE_INVALID")
        self.assert_controls(self.images["before"])
        self.connect_api()
        self.request("/api/v1/projects", expected=200)
        print("PASS real five-process upgrade and explicit rollback", flush=True)

    def active_task_timeout(self):
        project = self.request("/api/v1/projects", {"name": "Local Upgrade", "slug": "local-upgrade"}, expected=201)
        application = self.request("/api/v1/projects/" + project["id"] + "/applications",
                                   {"name": "Held Release", "slug": "held-release"}, expected=201)
        target = self.request("/api/v1/applications/" + application["id"] + "/deployment-targets",
                              {"stage": "development", "replicas": 1, "containerPort": 6379}, expected=201)
        target_id = target["id"]
        application_name = "orbit-devops-" + target_id.replace("-", "")
        # 先经公开 HTTP 发布正常实例，避免预制 Deployment 与真实 FieldManager 冲突。
        initial = self.request("/api/v1/deployment-targets/" + target_id + "/releases",
                               {"imageReference": self.images["application"]}, expected=201)
        first_path = "/api/v1/release-operations/" + initial["releaseOperation"]["id"]
        eventually(lambda: self.request(first_path).get("status") == "succeeded", timeout=45,
                   label="LOCAL_FIRST_RELEASE_NOT_COMPLETED")
        # 只给已创建的测试应用添加 Ready 阻塞；字段由测试操作者拥有，不改变控制进程。
        run(self.kube + ["-n", self.namespace, "patch", "deployment", application_name, "--type=json", "--patch-file=/dev/stdin"],
            data=json.dumps([{"op": "replace", "path": "/spec/strategy", "value": {"type": "Recreate"}},
                             {"op": "add", "path": "/spec/template/spec/containers/0/readinessProbe",
                              "value": {"exec": {"command": ["sh", "-c", "test -f /tmp/orbit-ready"]}, "periodSeconds": 1}}]))
        eventually(lambda: self.get("deployment", application_name).get("status", {}).get("readyReplicas", 0) == 0,
                   timeout=30, label="LOCAL_READY_BLOCK_NOT_ACTIVE")
        acceptance = self.request("/api/v1/deployment-targets/" + target_id + "/releases",
                                  {"imageReference": self.images["application"]}, expected=201)
        operation_id = acceptance["releaseOperation"]["id"]
        operation_path = "/api/v1/release-operations/" + operation_id
        def started_release():
            value = self.request(operation_path)
            return value if value.get("status") != "pending" else None

        started = eventually(started_release, timeout=30, label="LOCAL_RELEASE_NOT_STARTED")
        if started["status"] != "running":
            code = started.get("errorCode") or "none"
            if not re.fullmatch(r"[a-z0-9_]{1,80}", code):
                code = "unclassified"
            raise RuntimeError("LOCAL_RELEASE_NOT_RUNNING code=" + code)
        worker_pods = {name: sorted(p["metadata"]["uid"] for p in self.get("pods")["items"]
                                  if p["metadata"].get("labels", {}).get("app") == name) for name in ROLES if name != "orbit-api"}
        result = self.cli(["upgrade", "--image", self.images["after"], "--drain-timeout", "1", "--ready-timeout", "90"], expect=1)
        if result.get("phase") != "aborted" or result.get("last_error") != "DRAIN_TIMEOUT":
            raise RuntimeError("LOCAL_DRAIN_TIMEOUT_NOT_ABORTED")
        self.assert_controls(self.images["before"])
        for name, expected in worker_pods.items():
            actual = sorted(p["metadata"]["uid"] for p in self.get("pods")["items"]
                            if p["metadata"].get("labels", {}).get("app") == name)
            if actual != expected:
                raise RuntimeError("LOCAL_DRAIN_ABORT_RESTARTED_WORKER")
        self.connect_api()
        current = self.request(operation_path)
        if current["status"] != "running" or current["attemptCount"] != 1:
            raise RuntimeError("LOCAL_DRAIN_ABORT_CHANGED_USER_TASK")
        # 解除测试 Pod 的 Ready 阻塞，而非改写业务 SQL 或调用取消/重试命令。
        run(self.kube + ["-n", self.namespace, "patch", "deployment", application_name, "--type=json", "--patch-file=/dev/stdin"],
            data=json.dumps([{"op": "replace", "path": "/spec/template/spec/containers/0/readinessProbe",
                              "value": {"tcpSocket": {"port": 6379}, "periodSeconds": 1}}]))
        def completed_release():
            value = self.request(operation_path)
            return value if value.get("status") == "succeeded" else None

        current = eventually(completed_release, timeout=90, label="LOCAL_RELEASE_NOT_COMPLETED_AFTER_ABORT")
        if current["attemptCount"] != 1:
            raise RuntimeError("LOCAL_DRAIN_ABORT_CREATED_EXTRA_ATTEMPT")
        print("PASS real active release drains timeout; API resumes; original task finishes without cancellation", flush=True)

    def bad_upgrade(self):
        result = self.cli(["upgrade", "--image", self.images["broken"], "--drain-timeout", "20", "--ready-timeout", "12"], expect=1)
        if result.get("phase") != "rolled_back" or not result.get("last_error"):
            raise RuntimeError("LOCAL_BROKEN_API_NOT_ROLLED_BACK")
        self.assert_controls(self.images["before"])
        self.connect_api()
        self.request("/api/v1/projects", expected=200)
        print("PASS real unready API image fails upgrade; independent rollback restores all five roles and session", flush=True)

    def cleanup(self):
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)
        if self.namespace_uid:
            # 删除前重新核验身份，绝不靠当前 kubeconfig 或通配符清理。
            current = self.get("namespace", self.namespace, namespaced=False)
            if current["metadata"]["uid"] != self.namespace_uid or current["metadata"]["labels"].get("app.kubernetes.io/managed-by") != "orbit-devops":
                raise RuntimeError("LOCAL_CLEANUP_IDENTITY_REJECTED")
            run(self.kube + ["delete", "namespace", self.namespace, "--wait=true", "--timeout=120s"], timeout=140)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--scenario", choices=("all", "normal", "active", "broken"), default="all")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="orbit-control-plane-rehearsal-") as temporary:
        rehearsal = Rehearsal(args, temporary)
        try:
            print("SETUP verify local Kind identity", flush=True)
            rehearsal.guard()
            print("SETUP PostgreSQL and Redis fixtures", flush=True)
            rehearsal.dependencies()
            print("SETUP compile real API and four Workers; import immutable images", flush=True)
            rehearsal.build_images()
            rehearsal.configure()
            print("SETUP schema migration and real services", flush=True)
            rehearsal.job("local-migrate", "orbit-devops-migrate")
            rehearsal.controls()
            if args.scenario in {"all", "normal"}:
                rehearsal.normal_upgrade()
            else:
                rehearsal.connect_api()
                rehearsal.request("/api/v1/auth/login", {"loginName": "local-rehearsal", "password": PASSWORD})
            if args.scenario in {"all", "active"}:
                rehearsal.active_task_timeout()
            if args.scenario in {"all", "broken"}:
                rehearsal.bad_upgrade()
        finally:
            rehearsal.cleanup()


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as failure:
        # RuntimeError 仅由本文件固定阶段/整数状态构成，没有外部 stderr/配置值。
        raise SystemExit("LOCAL_CONTROL_PLANE_REHEARSAL_FAILED " + str(failure)) from None
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        raise SystemExit("LOCAL_CONTROL_PLANE_REHEARSAL_FAILED; no raw logs or credentials printed") from None
