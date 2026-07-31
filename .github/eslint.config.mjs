import globals from "globals";

// Only rules that catch outright mistakes. The page is the one part of this
// repo with no tests, so the bar is "would have been a bug", not style.
export default [
  {
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: "script",
      globals: { ...globals.browser, ...globals.serviceworker },
    },
    rules: {
      "no-undef": "error",
      "no-const-assign": "error",
      "no-dupe-args": "error",
      "no-dupe-else-if": "error",
      "no-dupe-keys": "error",
      "no-func-assign": "error",
      "no-redeclare": "error",
      "no-self-assign": "error",
      "no-sparse-arrays": "error",
      "no-unreachable": "error",
      "no-unsafe-negation": "error",
      "use-isnan": "error",
      "valid-typeof": "error",
    },
  },
];
