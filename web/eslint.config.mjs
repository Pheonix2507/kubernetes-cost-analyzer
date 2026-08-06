import tsParser from "@typescript-eslint/parser";
import nextPlugin from "@next/eslint-plugin-next";
import reactHooks from "eslint-plugin-react-hooks";

/**
 * ESLint, flat config.
 *
 * WHY NOT `eslint-config-next`
 * ===========================
 * It pulls in @rushstack/eslint-patch, which monkey-patches ESLint's module resolution for the legacy
 * eslintrc format. Under ESLint 9's flat config that patch fails outright -- "Failed to patch ESLint
 * because the calling module was not recognized" -- and the whole lint step dies.
 *
 * The two ways out are FlatCompat, which keeps the patch alive behind a shim, or composing the plugins
 * directly. Direct is chosen because the shim exists to run code that is incompatible on purpose, and
 * because it makes visible exactly which rules are enforced. `eslint-config-next` is a bundle whose
 * contents nobody reads.
 *
 * WHAT IS DELIBERATELY NOT HERE
 * No typescript-eslint RULES. `tsc --noEmit` runs in `pnpm check` and is strict -- strict,
 * noUncheckedIndexedAccess, noImplicitOverride -- so type problems already fail the build.
 * typescript-eslint's type-aware rules would add a second, slower type-checking pass over the same code
 * to catch a marginal extra set. The compiler is the type checker; the linter is here for what it cannot
 * see.
 *
 * The PARSER is a different matter and is required: ESLint's default parser is espree, which speaks
 * JavaScript. Without @typescript-eslint/parser every file fails with "Parsing error: Unexpected token as"
 * on the first type assertion, so the linter reports eighteen syntax errors and zero real findings.
 * Taking the parser without the rules is the whole point -- read the syntax, do not re-check the types.
 */
export default [
  {
    ignores: [".next/**", "node_modules/**", "src/lib/api/schema.d.ts"],
  },
  {
    files: ["**/*.{ts,tsx}"],
    plugins: { "@next/next": nextPlugin, "react-hooks": reactHooks },
    languageOptions: {
      parser: tsParser,
      parserOptions: {
        ecmaFeatures: { jsx: true },
        // No `project` setting, deliberately: that is what switches on type-aware linting and the second
        // type-checking pass this config exists to avoid.
        sourceType: "module",
      },
    },
    rules: {
      // The framework rules: they catch mistakes that are invisible to the type system because they are
      // about Next.js semantics rather than about types -- an <a> where <Link> belongs, a synchronous
      // script in the wrong place, an <img> that skips the image pipeline.
      ...nextPlugin.configs.recommended.rules,
      ...nextPlugin.configs["core-web-vitals"].rules,

      /**
       * THE TWO RULES THAT MATTER MOST HERE.
       *
       * rules-of-hooks catches a conditional hook, which produces a bug that manifests as unrelated
       * state corruption several renders later and is close to undebuggable by reading the code.
       *
       * exhaustive-deps is the one that would actually bite this dashboard: a useMemo over `shown` and
       * `labels` that forgot a dependency would silently serve a stale colour mapping, so a filter change
       * would repaint series with the previous mapping. That is precisely the "colour follows rank rather
       * than entity" failure the palette module exists to prevent -- reintroduced by a missing array
       * entry, and invisible because the chart still looks correct.
       *
       * ERROR, not warn. A warning in CI is a warning nobody reads.
       */
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
    },
  },
  {
    // The generated schema is not ours to lint, and it is already excluded above. This block documents
    // the one deliberate exemption so a future reader does not assume it was forgotten.
    files: ["src/lib/api/schema.d.ts"],
    rules: {},
  },
];
