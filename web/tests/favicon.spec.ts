import { readFile } from "node:fs/promises";
import { expect, test } from "@playwright/test";

test("登录页和应用深链使用可解码的 Orbit 图标", async ({ page, request }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ status: 401, json: { code: "authentication_required" } }),
  );
  await page.route("**/api/v1/auth/providers", (route) =>
    route.fulfill({ json: [] }),
  );

  // SPA 回退也可能返回 200，必须检查 MIME 与实际图标字节，而不是只看状态码。
  const response = await request.get("/orbit-icon.svg");
  expect(response.status()).toBe(200);
  expect(response.headers()["content-type"]).toContain("image/svg+xml");
  expect(await response.body()).toEqual(
    await readFile(new URL("../public/orbit-icon.svg", import.meta.url)),
  );

  for (const path of ["/login", "/projects/preview/applications/preview"]) {
    await page.goto(path);
    const icon = page.locator('head link[rel="icon"]');
    await expect(icon).toHaveCount(1);
    await expect(icon).toHaveAttribute("href", "/orbit-icon.svg");
    await expect(icon).toHaveAttribute("type", "image/svg+xml");
    await expect(icon).toHaveAttribute("sizes", "any");
    expect(
      await page.evaluate(async () => {
        const image = new Image();
        image.src =
          document.querySelector<HTMLLinkElement>('link[rel="icon"]')!.href;
        await image.decode();
        return image.naturalWidth > 0 && image.naturalHeight > 0;
      }),
    ).toBe(true);
  }
});
