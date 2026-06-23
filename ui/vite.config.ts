import path from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// The portal is served by the Go binary under /portal/ (see internal/portal/portal.go),
// so assets must be referenced relative to that base. This source tree lives at the repo
// root (/ui); the build emits into internal/portal/dist, which is embedded via go:embed
// (go:embed can only reach files inside the embedding package's directory).
export default defineConfig({
  base: '/portal/',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': path.resolve(__dirname, './src') },
  },
  build: {
    outDir: path.resolve(__dirname, '../internal/portal/dist'),
    emptyOutDir: true,
  },
})
