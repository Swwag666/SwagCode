import { vitePreprocess } from '@sveltejs/vite-plugin-svelte'

export default {
  preprocess: vitePreprocess(),
  compilerOptions: {
    // Svelte 5 runes — основа реактивности без VDOM (D-003).
    runes: true,
  },
}
