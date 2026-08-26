import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'
import { defineConfig } from 'vite'

const repoRoot = path.resolve(import.meta.dirname, '../..')

export default defineConfig({
  root: import.meta.dirname,
  base: './',
  plugins: [tailwindcss(), react()],
  build: {
    outDir: path.join(repoRoot, 'docs/dist'),
    emptyOutDir: true,
  },
})
