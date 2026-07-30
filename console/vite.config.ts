import { defineConfig } from 'vite'
import { devtools } from '@tanstack/devtools-vite'

import { tanstackStart } from '@tanstack/react-start/plugin/vite'

import viteReact from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

const config = defineConfig({
  resolve: { tsconfigPaths: true },
  // Loopback only, deliberately. This console has no authentication of any kind
  // -- Phase 1 has none anywhere, and the analysis doc's section 2.5 records
  // that attach will hand out a credential for any session in a workspace -- so
  // it is safe exactly as long as it is not reachable from anywhere else.
  // Binding here makes that structural rather than a line in a README.
  server: { host: '127.0.0.1' },
  preview: { host: '127.0.0.1' },
  plugins: [devtools(), tailwindcss(), tanstackStart(), viteReact()],
})

export default config
