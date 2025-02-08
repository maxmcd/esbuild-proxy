import { defineConfig } from 'vite'

export default defineConfig({
  build: {
    // Configure the build output
    outDir: 'dist',
    rollupOptions: {
      input: 'src/index.ts',
      output: {
        // Output as ES module
        format: 'es',
        // Name of the output file
        entryFileNames: 'bundle.js'
      }
    }
  }
})
