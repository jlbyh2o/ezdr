import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  build: {
    // A single bundle is fine for an admin portal (about 170 kB gzipped).
    chunkSizeWarningLimit: 800,
  },
  server: {
    // During development, forward API calls to a locally running portal.
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
