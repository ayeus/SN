import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../web',
    emptyOutDir: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/join': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/install.sh': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
