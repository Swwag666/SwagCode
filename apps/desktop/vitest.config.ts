import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  plugins: [svelte({ hot: false })],
  resolve: {
    // Компонентные тесты: Svelte обязан резолвиться в клиентскую сборку,
    // иначе монтирование в jsdom падает с lifecycle_function_unavailable.
    conditions: ['browser'],
  },
  test: {
    // jsdom включается per-file директивой @vitest-environment: логика в lib/
    // намеренно чистая и гоняется в node — быстрее и честнее.
    environment: 'node',
    include: ['src/**/*.test.ts'],
    reporters: ['default'],
  },
})
