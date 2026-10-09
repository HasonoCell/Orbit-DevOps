/** 将正式 Vite 产物封装成 Build Output API；来源凭据只保留服务端环境变量引用。 */
import { cp, lstat, mkdir, readdir, writeFile } from "node:fs/promises";
import { isIP } from "node:net";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// 本轮源站是公共 IPv4 证书入口，拒绝把非 HTTPS、私有地址或 URL 附加字段封装成代理目标。
export function managementOrigin(input) {
  let url;
  try {
    url = new URL(input);
  } catch {
    throw new Error(
      "ORBIT_DEVOPS_API_ORIGIN must be a public IPv4 HTTPS origin",
    );
  }
  const octets = url.hostname.split(".").map(Number);
  const [a, b, c] = octets;
  const privateOrReserved =
    a === 0 ||
    a === 10 ||
    a === 127 ||
    a >= 224 ||
    (a === 100 && b >= 64 && b <= 127) ||
    (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && (b === 168 || (b === 0 && (c === 0 || c === 2)))) ||
    (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) ||
    (a === 203 && b === 0 && c === 113);
  if (
    url.protocol !== "https:" ||
    isIP(url.hostname) !== 4 ||
    privateOrReserved ||
    url.username ||
    url.password ||
    url.port ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  ) {
    throw new Error(
      "ORBIT_DEVOPS_API_ORIGIN must be a public IPv4 HTTPS origin",
    );
  }
  return url.origin;
}

export function routingConfig(origin) {
  const destination = managementOrigin(origin);
  return {
    version: 3,
    routes: [
      {
        src: "^(/api(?:/.*)?)$",
        dest: `${destination}$1`,
        // 覆盖来访者自带的来源 Header；不改写 Origin、Cookie 或 CSRF Header。
        transforms: [
          {
            type: "request.headers",
            op: "set",
            target: { key: "x-orbit-origin-secret" },
            args: "$ORBIT_DEVOPS_ORIGIN_SECRET",
            env: ["ORBIT_DEVOPS_ORIGIN_SECRET"],
          },
        ],
        headers: {
          "cache-control": "private, no-store",
          "cdn-cache-control": "no-store",
          "vercel-cdn-cache-control": "no-store",
          "x-vercel-enable-rewrite-caching": "0",
        },
      },
      {
        src: "/(.*)",
        headers: {
          "x-content-type-options": "nosniff",
          "referrer-policy": "same-origin",
        },
        continue: true,
      },
      { handle: "filesystem" },
      // 不存在的资源不能返回 index.html，否则升级期间失效的 chunk 会变成 HTML 响应。
      { src: "/assets/.*", status: 404 },
      {
        src: "/(.*)",
        dest: "/index.html",
        headers: { "cache-control": "no-cache" },
      },
    ],
  };
}

async function validateStaticTree(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    if (entry.isSymbolicLink() || entry.name.startsWith(".")) {
      throw new Error(
        "static output must not contain symlinks or hidden files",
      );
    }
    if (entry.isDirectory())
      await validateStaticTree(join(directory, entry.name));
    else if (!entry.isFile())
      throw new Error("static output must contain only regular files");
  }
}

// 只向全新、确定的 .vercel/output 写入，不递归清理调用者提供的目录。
export async function prepareOutput({
  dist,
  output,
  origin,
  frontendApiURL = "",
}) {
  const config = routingConfig(origin);
  if (frontendApiURL)
    throw new Error("Vercel frontend must use the same-origin API");
  if (
    basename(output) !== "output" ||
    basename(dirname(output)) !== ".vercel"
  ) {
    throw new Error("output must be a new .vercel/output directory");
  }
  if (!(await lstat(dist)).isDirectory())
    throw new Error("static output root must be a regular directory");
  if (!(await lstat(join(dist, "index.html"))).isFile())
    throw new Error("build index.html is required");
  if ((await readdir(dist)).includes("api"))
    throw new Error("static files must not shadow the API");
  await validateStaticTree(dist);
  await mkdir(dirname(output), { recursive: true });
  await mkdir(output);
  await cp(dist, join(output, "static"), {
    recursive: true,
    dereference: false,
  });
  await writeFile(
    join(output, "config.json"),
    `${JSON.stringify(config, null, 2)}\n`,
    { flag: "wx" },
  );
  // 输出中只含静态资源与路由；不生成 .env，也不读取或落盘来源 Secret。
  return config;
}

if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const [dist = "web/dist", output = ".vercel/output", ...extra] =
      process.argv.slice(2);
    if (extra.length)
      throw new Error("usage: prepare-vercel-output.mjs [dist] [output]");
    await prepareOutput({
      dist: resolve(dist),
      output: resolve(output),
      origin: process.env.ORBIT_DEVOPS_API_ORIGIN,
      frontendApiURL: process.env.VITE_ORBIT_DEVOPS_API_URL,
    });
    console.log("VERCEL_OUTPUT_READY");
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
