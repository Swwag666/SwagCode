import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Tauri ждёт фиксированный порт и не переносит смены порта при занятом.
export default defineConfig({
  plugins: [svelte()],
  clearScreen: false,
  server: {
    port: 5173,
    strictPort: true,
  },
  build: {
    // Tauri использует Chromium/WebView2 последних версий — не занижаем target.
    target: 'chrome110',
    minify: 'esbuild',
    sourcemap: false,
  },
})
