"""离线核验入口配置与端口边界；不读取 kubeconfig、不访问网络或创建 Secret。"""

import copy
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("management_https", Path(__file__).with_name("prepare-management-https.py"))
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)

PRIVATE = "2379, 2380, 5000, 5432, 6379, 6443, 8080, 9090-9095, 10250, 30000-32767"
SOURCE = ('table inet orbit_private {\n  chain ingress {\n'
          '    type filter hook prerouting priority -310; policy accept;\n'
          '    iifname "eth0" tcp dport { 80, 443, ' + PRIVATE + ' } counter drop\n'
          '    iifname "eth0" udp dport { 8472, 30000-32767 } counter drop\n'
          '  }\n}\n')


def snapshot():
    entries = [{"table": {"family": "inet", "name": "orbit_private", "handle": 1}},
               {"chain": {"family": "inet", "table": "orbit_private", "name": "ingress", "handle": 2,
                          "type": "filter", "hook": "prerouting", "prio": -310, "policy": "accept"}}]
    for protocol, ports in [("tcp", [80, 443, 2379, 2380, 5000, 5432, 6379, 6443, 8080,
                                    {"range": [9090, 9095]}, 10250, {"range": [30000, 32767]}]),
                            ("udp", [8472, {"range": [30000, 32767]}])]:
        entries.append({"rule": {"expr": [
            {"match": {"op": "==", "left": {"meta": {"key": "iifname"}}, "right": "eth0"}},
            {"match": {"op": "==", "left": {"payload": {"protocol": protocol, "field": "dport"}}, "right": {"set": ports}}},
            {"counter": {"packets": 123, "bytes": 456}}, {"drop": None}]}})
    return {"nftables": entries}


class ManagementHttpsTest(unittest.TestCase):
    def test_certificate_has_ip_san_and_shortlived_profile(self):
        issuer, cert = module.certificate_resources("8.8.8.8")["items"]
        self.assertEqual(cert["spec"]["ipAddresses"], ["8.8.8.8"])
        self.assertNotIn("dnsNames", cert["spec"])
        self.assertEqual(cert["spec"]["renewBeforePercentage"], 33)
        self.assertEqual(issuer["spec"]["acme"]["profile"], "shortlived")
        solver = issuer["spec"]["acme"]["solvers"][0]["http01"]["gatewayHTTPRoute"]
        self.assertEqual(solver["parentRefs"][0]["sectionName"], "acme-http")
        self.assertEqual(solver["labels"], solver["podTemplate"]["metadata"]["labels"])

    def test_reject_non_public_ipv4_before_request(self):
        for address in ["127.0.0.1", "10.0.0.11", "192.0.2.1", "239.1.1.1", "255.255.255.255", "::1", "2606:4700:4700::1111", "invalid"]:
            with self.subTest(address=address), self.assertRaises(ValueError), patch.object(module, "run") as run:
                module.certificate_resources(address)
                run.assert_not_called()

    def test_network_stage_only_changes_80_443(self):
        for stage, prefix in [("closed", "80, 443, "), ("acme", "443, "), ("https", "")]:
            with self.subTest(stage=stage):
                configured, interface, _ = module.network_configuration(SOURCE, stage)
                self.assertEqual(interface, "eth0")
                self.assertIn("tcp dport { " + prefix + PRIVATE + " }", configured)
                restored, _, _ = module.network_configuration(configured, "closed")
                self.assertEqual(restored, SOURCE)

    def test_refuses_file_drift(self):
        for altered in [SOURCE.replace("-310", "-300"), SOURCE.replace("5432, ", ""),
                        SOURCE.replace('iifname "eth0" udp', 'iifname "eth1" udp'),
                        SOURCE + "# custom rule\n", SOURCE.replace("counter drop", "accept", 1)]:
            with self.subTest(altered=altered), self.assertRaises(RuntimeError):
                module.network_configuration(altered, "https")

    def test_live_network_ignores_handles_and_counters(self):
        module.check_live_network(snapshot(), "eth0", "80, 443, " + PRIVATE)

    def test_refuses_extra_live_rules(self):
        changed = snapshot()
        changed["nftables"].append(copy.deepcopy(changed["nftables"][-1]))
        with self.assertRaises(RuntimeError):
            module.check_live_network(changed, "eth0", "80, 443, " + PRIVATE)

    def test_refuses_live_drift(self):
        for mutate in [lambda d: d["nftables"][1]["chain"].update(prio=-300),
                       lambda d: d["nftables"][2]["rule"]["expr"][1]["match"]["right"]["set"].remove(5432),
                       lambda d: d["nftables"][2]["rule"]["expr"][0]["match"].update(right="eth1"),
                       lambda d: d["nftables"][2]["rule"]["expr"].__setitem__(3, {"accept": None})]:
            changed = snapshot()
            mutate(changed)
            with self.subTest(mutate=mutate), self.assertRaises(RuntimeError):
                module.check_live_network(changed, "eth0", "80, 443, " + PRIVATE)

    def test_existing_open_state_must_match_persistent_state(self):
        for stage, removals in [("acme", [80]), ("https", [80, 443])]:
            changed = snapshot()
            for port in removals:
                changed["nftables"][2]["rule"]["expr"][1]["match"]["right"]["set"].remove(port)
            configured, _, ports = module.network_configuration(SOURCE, stage)
            module.check_live_network(changed, "eth0", ports if stage == "closed" else configured.split("tcp dport { ")[1].split(" }")[0])

    def test_policy_acceptance_is_read_from_exact_gateway_ancestor(self):
        policy = {"metadata": {"generation": 2}, "status": {"ancestors": [{
            "ancestorRef": {"name": "orbit-management", "namespace": "orbit-system", "sectionName": "management-https"},
            "controllerName": "gateway.envoyproxy.io/gatewayclass-controller",
            "conditions": [{"type": "Accepted", "status": "True", "observedGeneration": 2}],
        }]}}
        self.assertTrue(module.policy_accepted(policy))
        for field, value in [("name", "another-gateway"), ("namespace", "orbit-apps"), ("sectionName", "acme-http")]:
            changed = copy.deepcopy(policy)
            changed["status"]["ancestors"][0]["ancestorRef"][field] = value
            with self.subTest(field=field):
                self.assertFalse(module.policy_accepted(changed))
        changed = copy.deepcopy(policy)
        changed["status"]["ancestors"][0]["conditions"][0]["observedGeneration"] = 1
        self.assertFalse(module.policy_accepted(changed))
        self.assertFalse(module.policy_accepted({"metadata": {"generation": 2}, "status": {"conditions": [{"type": "Accepted", "status": "True"}]}}))

    def test_https_cannot_open_before_certificate_ready(self):
        policy = {"metadata": {"generation": 1}, "status": {"ancestors": [{
            "ancestorRef": {"name": "orbit-management", "namespace": "orbit-system", "sectionName": "management-https"},
            "controllerName": "gateway.envoyproxy.io/gatewayclass-controller",
            "conditions": [{"type": "Accepted", "status": "True", "observedGeneration": 1}],
        }]}}
        certificate = {"metadata": {"generation": 1}, "status": {"conditions": [{"type": "Ready", "status": "False", "observedGeneration": 1}]}}
        with patch.object(module, "NETWORK_FILE") as file, patch.object(module, "get", side_effect=[policy, certificate]), patch.object(module, "run", return_value=json.dumps(snapshot())) as run:
            file.is_symlink.return_value = False
            file.stat.return_value.st_uid = 0
            file.stat.return_value.st_mode = 0o100600
            file.read_text.return_value = SOURCE
            with self.assertRaises(RuntimeError):
                module.configure_network("https")
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0], ["nft", "-j", "list", "table", "inet", "orbit_private"])

    def test_packaged_manifest_digest_matches_guard(self):
        manifest = Path(__file__).resolve().parents[2] / "deploy/private-k3s/management-https/gateway.yaml"
        self.assertEqual(module.hashlib.sha256(manifest.read_bytes()).hexdigest(), module.MANIFEST_DIGEST)


if __name__ == "__main__":
    unittest.main()
