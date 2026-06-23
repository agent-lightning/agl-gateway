// Vitest global setup: register jest-dom matchers (toBeInTheDocument, etc.) on expect.
import '@testing-library/jest-dom/vitest'

// jsdom lacks ResizeObserver, which some Radix primitives (e.g. ScrollArea) construct on
// mount. A no-op stub lets those components render in component tests.
if (!('ResizeObserver' in globalThis)) {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}
