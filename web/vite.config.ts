import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": new URL("./src", import.meta.url).pathname },
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    proxy: {
      // 开发代理独立于浏览器 API 地址，保留同源 Cookie/CSRF 行为。
      "/api": process.env.ORBIT_DEVOPS_WEB_API_PROXY ?? "http://127.0.0.1:8080",
      "/healthz":
        process.env.ORBIT_DEVOPS_WEB_API_PROXY ?? "http://127.0.0.1:8080",
    },
  },
});
