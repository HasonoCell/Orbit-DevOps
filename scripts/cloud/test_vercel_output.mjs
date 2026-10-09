import assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { after } from "node:test";
import {
  managementOrigin,
  prepareOutput,
  routingConfig,
} from "./prepare-vercel-output.mjs";

const temporaryRoots = [];
after(async () => {
  // 仅回收本测试自行创建的临时目录，不扫描或删除其他路径。
  for (const directory of temporaryRoots)
    await rm(directory, { recursive: true, force: true });
});

test("production Git deploys are restricted to main and run validation before packaging", async () => {
  const settings = JSON.parse(
    await readFile(new URL("../../vercel.json", import.meta.url), "utf8"),
  );
  // ** 覆盖带斜线的功能分支；main 的显式 true 规则只放行生产分支。
  assert.deepEqual(settings.git?.deploymentEnabled, {
    "**": false,
    main: true,
  });
  assert.equal(
    settings.buildCommand,
    "pnpm web:format:check && node --test scripts/cloud/test_vercel_output.mjs && pnpm web:build && node scripts/cloud/prepare-vercel-output.mjs",
  );
  assert.match(settings.installCommand, /--frozen-lockfile/);
  assert.match(settings.installCommand, /--prod=false/);
});

test("only public IPv4 HTTPS origins are accepted", () => {
  assert.equal(managementOrigin("https://8.8.8.8/"), "https://8.8.8.8");
  for (const value of [
    undefined,
    "invalid",
    "http://8.8.8.8",
    "https://127.0.0.1",
    "https://10.0.0.1",
    "https://192.168.1.1",
    "https://100.64.0.1",
    "https://169.254.0.1",
    "https://198.18.0.1",
    "https://192.0.2.1",
    "https://203.0.113.1",
    "https://224.0.0.1",
    "https://[::1]",
    "https://example.com",
    "https://8.8.8.8:8443",
    "https://name:password@8.8.8.8",
    "https://8.8.8.8/path",
    "https://8.8.8.8?query=1",
    "https://8.8.8.8#hash",
  ]) {
    assert.throws(() => managementOrigin(value));
  }
});

test("API precedes static files and always overwrites the server-only source header", () => {
  const config = routingConfig("https://8.8.8.8");
  const api = config.routes[0];
  for (const path of ["/api", "/api/v1/projects", "/api/v1/auth/login"]) {
    assert.ok(new RegExp(api.src).test(path));
    assert.equal(
      path.replace(new RegExp(api.src), api.dest),
      `https://8.8.8.8${path}`,
    );
  }
  for (const path of [
    "/api-other",
    "/assets/main.js",
    "/projects",
    "/healthz",
  ]) {
    assert.equal(new RegExp(api.src).test(path), false);
  }
  assert.deepEqual(api.transforms, [
    {
      type: "request.headers",
      op: "set",
      target: { key: "x-orbit-origin-secret" },
      args: "$ORBIT_DEVOPS_ORIGIN_SECRET",
      env: ["ORBIT_DEVOPS_ORIGIN_SECRET"],
    },
  ]);
  assert.equal(api.headers["x-vercel-enable-rewrite-caching"], "0");
  assert.equal(api.headers["cache-control"], "private, no-store");
  assert.equal(api.headers["vercel-cdn-cache-control"], "no-store");
  assert.equal(config.routes[2].handle, "filesystem");
  assert.equal(config.routes[3].status, 404);
  assert.equal(config.routes[4].dest, "/index.html");
});

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), "orbit-vercel-test-"));
  temporaryRoots.push(root);
  const dist = join(root, "dist");
  const output = join(root, ".vercel", "output");
  await mkdir(dist);
  await writeFile(join(dist, "index.html"), "<html>Orbit</html>");
  return { root, dist, output, origin: "https://8.8.8.8" };
}

test("output contains static files and an env reference but never reads a source secret", async () => {
  const inputs = await fixture();
  await prepareOutput(inputs);
  assert.equal(
    await readFile(join(inputs.output, "static", "index.html"), "utf8"),
    "<html>Orbit</html>",
  );
  const config = JSON.parse(
    await readFile(join(inputs.output, "config.json"), "utf8"),
  );
  assert.deepEqual(config, routingConfig(inputs.origin));
  // 已生成的目录不覆盖；部署重复执行需使用新的输出目录。
  await assert.rejects(prepareOutput(inputs), { code: "EEXIST" });
});

test("cross-origin frontend or broad output targets are refused", async () => {
  const inputs = await fixture();
  await assert.rejects(
    prepareOutput({ ...inputs, frontendApiURL: "https://8.8.8.8" }),
    /same-origin/,
  );
  await assert.rejects(
    prepareOutput({ ...inputs, output: inputs.root }),
    /new .vercel\/output/,
  );
});

test("hidden files, symlinks and API-shadowing output are refused", async () => {
  for (const entry of ["hidden", "symlink", "api"]) {
    const inputs = await fixture();
    if (entry === "hidden")
      await writeFile(join(inputs.dist, ".env"), "PRIVATE=value");
    if (entry === "symlink")
      await symlink(
        join(inputs.dist, "index.html"),
        join(inputs.dist, "linked"),
      );
    if (entry === "api") await mkdir(join(inputs.dist, "api"));
    await assert.rejects(prepareOutput(inputs));
  }
});

test("a symlink used as the static root is refused", async () => {
  const inputs = await fixture();
  const linked = join(inputs.root, "linked-dist");
  await symlink(inputs.dist, linked);
  await assert.rejects(
    prepareOutput({ ...inputs, dist: linked }),
    /regular directory/,
  );
});
