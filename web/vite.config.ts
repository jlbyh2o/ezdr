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
    // During development, forward API calls to the development portal
    // (go run ./cmd/ezdr-devportal). The Host header stays localhost:5173,
    // so the portal's same-origin check passes.
    proxy: {
      '^/(ezdr\\.|dev/|api/)': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
})
