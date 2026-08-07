import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

/**
 * Vitest, configured for the NODE environment rather than jsdom.
 *
 * WHY NO jsdom, AND WHY NO COMPONENT RENDERING
 * --------------------------------------------
 * The instinct with a React app is to reach for jsdom and @testing-library/react and start
 * rendering components. That buys less here than it costs, because the parts of this dashboard that
 * can be WRONG are not the JSX.
 *
 * What can be wrong is arithmetic and policy: which currency digits a figure gets, whether a
 * negative zero renders as "-0.00", whether a colour follows an entity or its position in a list,
 * and above all whether the proxy that holds the API key can be talked into forwarding somewhere it
 * should not. None of that needs a DOM, and testing it in Node keeps the suite fast enough to run on
 * every save.
 *
 * The route handler is testable this way because it takes a Web `Request` and returns a `Response`,
 * both of which are globals in Node 18+. So the security boundary gets tested with no browser and
 * no mocking framework, which is exactly the payoff for it being a plain function of a Request.
 */
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
  resolve: {
    alias: {
      // Matches the `@/*` path alias in tsconfig.json. Without it, imports resolve under `tsc` and
      // fail under vitest, which is the sort of divergence that makes people give up on the tests.
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
});
