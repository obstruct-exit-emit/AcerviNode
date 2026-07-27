import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
  },
  server: {
    // Proxies API calls to the Go backend during `npm run dev`, matching
    // LibriNode's own dev-server convention (its Vite proxies /api to :7845;
    // AcerviNode's default port is 7846).
    proxy: {
      '/api': 'http://localhost:7846',
    },
  },
})
