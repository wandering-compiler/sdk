// Flat ESLint config for the admin-runtime TS/React library.
//
// The lint tier of `make audit-js`: typescript-eslint with TYPE-AWARE rules
// (recommendedTypeChecked — catches unsafe `any`, floating promises, etc.,
// which a syntactic linter misses) + react-hooks. Prettier owns formatting, so
// eslint-config-prettier (last) turns off any rules that would fight it.
import js from "@eslint/js";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import prettier from "eslint-config-prettier";

export default tseslint.config(
  { ignores: ["dist/**", "node_modules/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: { "react-hooks": reactHooks },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // Async event handlers (onClick/onSubmit={async …}) are idiomatic React;
      // the attribute void-return check is overly strict for them. The rule
      // still flags genuine promise misuse elsewhere (conditions, &&, etc.).
      "@typescript-eslint/no-misused-promises": [
        "error",
        { checksVoidReturn: { attributes: false } },
      ],
    },
  },
  // The config file itself (and any plain .js) isn't in the TS program — don't
  // run type-aware rules on it.
  { files: ["**/*.js"], ...tseslint.configs.disableTypeChecked },
  prettier,
);
