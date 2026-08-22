# Repository Agent Instructions

## Temporary files and caches

- Never place build caches, test caches, package-manager caches, compiler work directories, or other temporary files inside this repository.
- Use each tool's default cache directory when it is outside this repository; do not override it without a concrete isolation or reproducibility need.
- Put task-owned temporary artifacts that need an explicit location in a task-specific directory under the system temporary directory, and clean them after the task.
- Repository-managed build outputs such as `web/dist` are product artifacts, not caches.

## Change preparation

- Before modifying code, collect enough information to identify the root cause, affected paths, existing behavior, and relevant constraints.
- Inspect the surrounding implementation and current changes before editing; do not implement a fix from the visible symptom alone.

## Implementation quality

- Do not add defensive compatibility paths, fallback branches, adapters, or duplicate implementations without a demonstrated current requirement. Prefer removing obsolete behavior over preserving it speculatively.
- Before accumulating localized patches, evaluate whether a focused simplification or refactor produces a smaller and clearer implementation. Update or remove superseded code, tests, documentation, locale keys, and styles as part of the same change.
- Optimize selectively from collected evidence. Keep changes cohesive and maintainable instead of pursuing the requested outcome through layered special cases.

## Frontend copy

- Keep frontend text concise and limited to labels, values, statuses, actionable errors, and instructions required to complete an operation.
- Do not add explanatory, promotional, repetitive, or self-evident helper text.
- Confirmation dialogs contain only a title, required fields, action buttons, and actionable errors. Do not add eyebrow labels, explanatory body copy, repeated consequences, or self-evident warnings.
- When removing UI text, also delete its unused locale keys, markup, and styles.

## Desktop builds

- Never compile, test, or build Rust/Tauri locally.
- Validate desktop changes through source review and GitHub Actions. Local verification is limited to Go tests and the Web build.
