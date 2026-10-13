// Flat ESLint config for @w17/dpop: typescript-eslint with type-aware rules
// (recommendedTypeChecked catches unsafe `any` and floating promises, which a
// syntactic linter misses). Prettier owns formatting; eslint-config-prettier
// (last) turns off any rule that would fight it.
import js from "@eslint/js";
import tseslint from "typescript-eslint";
import prettier from "eslint-config-prettier";

export default tseslint.config(
  { ignores: ["node_modules/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    files: ["**/*.ts"],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
  // Plain .js (this file) is not in the TS program.
  { files: ["**/*.js"], ...tseslint.configs.disableTypeChecked },
  prettier,
);
