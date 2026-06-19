import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In dev (`npm run dev`), proxy API + SSE to the Go server on :8733.
// In production the Go binary serves the built assets from web/dist itself.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8733",
        // Keep the original Host header (changeOrigin would rewrite it to the
        // backend while the browser's Origin stays :5173, tripping the
        // server's same-origin CSRF check). Reverse proxies in production must
        // likewise preserve Host.
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
