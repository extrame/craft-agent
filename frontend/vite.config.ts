import { defineConfig } from "vite";
import wails from "@wailsio/runtime/plugins/vite";

// https://vitejs.dev/config/
export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  build: {
    // 不先清空 dist：产物文件名带哈希，旧文件留着无害；
    // 而某些受管环境会拦截目录删除，导致构建在 prepare-out-dir 阶段直接失败。
    emptyOutDir: false,
  },
  plugins: [wails("./bindings")],
});
