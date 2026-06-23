import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist', 'coverage']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: globals.browser,
    },
    rules: {
      // shadcn/ui files export a component plus its cva variants (a constant); that is a
      // supported pattern for Fast Refresh.
      'react-refresh/only-export-components': [
        'warn',
        { allowConstantExport: true },
      ],
      // Every flagged site is an intentional fetch-on-mount / refetch-on-filter-change
      // effect (the app has no data-fetching library); the synchronous loading-flag set is
      // the desired UX, not a cascading-render bug.
      'react-hooks/set-state-in-effect': 'off',
    },
  },
  {
    // Test files run under Vitest (jsdom + node), so allow those globals too.
    files: ['**/*.{test,spec}.{ts,tsx}', 'src/test/**'],
    languageOptions: {
      globals: { ...globals.node, ...globals.vitest },
    },
  },
])
