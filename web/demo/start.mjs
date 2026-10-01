import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

// 专用子进程只改变 Vite 的开发代理；正式 dev/build 与生产 API 均不加载 Mock。
const web = fileURLToPath(new URL("../", import.meta.url));
const apiPort = Number(process.env.ORBIT_DEMO_API_PORT ?? 18090);
const webPort = Number(process.env.ORBIT_DEMO_WEB_PORT ?? 5173);
for (const port of [apiPort, webPort])
  if (!Number.isInteger(port) || port < 1024 || port > 65535)
    throw new Error("Invalid demo port");
const children = new Set();
let stopping = false;
function stop(code = 0) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill("SIGTERM");
  process.exitCode = code;
}
function launch(args, env = {}) {
  const child = spawn(process.execPath, args, {
    cwd: web,
    stdio: "inherit",
    env: { ...process.env, ...env },
  });
  children.add(child);
  child.on("error", (error) => {
    console.error(error.message);
    stop(1);
  });
  child.on("exit", (code) => {
    children.delete(child);
    if (!stopping) stop(code ?? 1);
  });
  return child;
}
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"])
  process.on(signal, () => stop());
launch(["demo/server.ts"]);
let ready = false;
for (let attempt = 0; attempt < 50 && !stopping; attempt++) {
  try {
    const response = await fetch(`http://127.0.0.1:${apiPort}/healthz`);
    ready = response.ok && response.headers.get("x-orbit-demo") === "1";
    if (ready) break;
  } catch {
    /* 等待专用 Mock 子进程绑定端口。 */
  }
  await new Promise((resolve) => setTimeout(resolve, 100));
}
if (!ready || stopping) stop(1);
else {
  launch(
    [
      "node_modules/vite/bin/vite.js",
      "--host",
      "127.0.0.1",
      "--port",
      String(webPort),
      "--strictPort",
    ],
    {
      ORBIT_DEVOPS_WEB_API_PROXY: `http://127.0.0.1:${apiPort}`,
      VITE_ORBIT_DEVOPS_API_URL: "",
    },
  );
  console.log(
    `\nOrbit Mock: http://127.0.0.1:${webPort}\n场景控制: http://127.0.0.1:${apiPort}/__demo\n账号 demo / developer / viewer；密码 Orbit-Demo-2026!\n`,
  );
}
