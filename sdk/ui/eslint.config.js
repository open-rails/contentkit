import js from "@eslint/js";
import { defineConfig, globalIgnores } from "eslint/config";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default defineConfig([
  globalIgnores(["dist", "test-results", "playwright-report", "e2e/.app", "e2e/.cache"]),
  {
    files: ["**/*.{ts,tsx,js}"],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    plugins: { "react-hooks": reactHooks },
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_", caughtErrors: "none" }],
    },
  },
  {
    // Test doubles and the benchmark poke at untyped globals and request bodies.
    files: ["bench/**", "test/**"],
    rules: { "@typescript-eslint/no-explicit-any": "off" },
  },
]);
