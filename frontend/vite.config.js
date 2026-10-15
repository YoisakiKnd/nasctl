import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  server: {
    proxy: { '/api': 'http://127.0.0.1:18090' },
  },
  build: {
    // These generated assets are committed and embedded by Go.
    outDir: '../internal/hubweb/web',
    emptyOutDir: true,
    cssCodeSplit: false,
    sourcemap: false,
    rollupOptions: {
      output: {
        entryFileNames: 'app.js',
        assetFileNames: (asset) => asset.names?.some((name) => name.endsWith('.css')) ? 'app.css' : '[name][extname]',
      },
    },
  },
});
