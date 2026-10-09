"""固定 Origin 切换与 API 专用重启的边界测试，不访问实际集群。"""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("browser", Path(__file__).with_name("configure-management-browser.py"))
browser = importlib.util.module_from_spec(spec)
spec.loader.exec_module(browser)


class ManagementBrowserTest(unittest.TestCase):
    def test_requires_fixed_https_vercel_origin(self):
        values = browser.browser_values("https://orbit-example.vercel.app")
        self.assertEqual(values[browser.KEYS[1]], "https://orbit-example.vercel.app")
        self.assertEqual(values[browser.KEYS[2]], "false")
        for value in ["http://orbit-example.vercel.app", "https://*.vercel.app", "https://example.com",
                      "https://orbit-example.vercel.app/", "https://name@orbit-example.vercel.app",
                      "https://orbit-example.vercel.app:443", "https://orbit-example.vercel.app?x=1",
                      "https://orbit-example.vercel.app#hash", "https://orbit-example.vercel.app.evil.test"]:
            with self.assertRaises(ValueError):
                browser.browser_values(value)

    def test_patch_only_changes_browser_keys_with_compare_and_swap(self):
        old = dict(zip(browser.KEYS, ["http://127.0.0.1:18080", "http://127.0.0.1:18080", "true"]))
        config = {"metadata": {"uid": "example", "resourceVersion": "1"}, "data": {**old, "OTHER": "keep"}}
        desired = browser.browser_values("https://orbit-example.vercel.app")
        patch = browser.config_patch(config, old, desired)
        self.assertEqual([item["path"] for item in patch if item["op"] == "replace"], ["/data/" + k for k in browser.KEYS])
        self.assertEqual(patch[0]["path"], "/metadata/uid")
        self.assertEqual(patch[1]["path"], "/metadata/resourceVersion")
        config["data"][browser.KEYS[0]] = "https://different.vercel.app"
        with self.assertRaises(ValueError):
            browser.config_patch(config, old, desired)

    def test_api_fingerprint_restart_is_idempotent_and_does_not_change_image(self):
        deployment = {"metadata": {"resourceVersion": "1"}, "spec": {"replicas": 1, "template": {
            "metadata": {"annotations": {"keep": "value"}}, "spec": {"serviceAccountName": "orbit-api", "containers": [{"name": "api", "image": "example@sha256:fixed", "envFrom": [{"configMapRef": {"name": "orbit-config"}}]}]}}}}
        values = browser.browser_values("https://orbit-example.vercel.app")
        patch = browser.api_restart_patch(deployment, values)
        self.assertEqual(patch[1]["path"], "/spec/template/metadata/annotations")
        self.assertEqual(patch[1]["value"]["keep"], "value")
        deployment["spec"]["template"]["metadata"]["annotations"] = patch[1]["value"]
        self.assertEqual(browser.api_restart_patch(deployment, values), [])
        deployment["spec"]["template"]["spec"]["serviceAccountName"] = "orbit-release-worker"
        with self.assertRaises(ValueError):
            browser.api_restart_patch(deployment, values)


if __name__ == "__main__":
    unittest.main()
