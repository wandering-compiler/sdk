// jsdom lacks two browser APIs Mantine touches on mount. Without them a
// render throws an AggregateError with no useful message, which reads
// like a component bug rather than a missing polyfill.
import { vi } from "vitest";

Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(), // deprecated, still probed by some libs
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }),
});

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverStub;

// Mantine's scroll-area / transition code paths call this.
window.HTMLElement.prototype.scrollIntoView = vi.fn();
