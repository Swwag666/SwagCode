import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  plugins: [svelte({ hot: false })],
  test: {
    // jsdom нужен только если появятся компонентные тесты; логика в lib/
    // намеренно чистая и гоняется в node — быстрее и честнее.
    environment: 'node',
    include: ['src/**/*.test.ts'],
    reporters: ['default'],
  },
})
