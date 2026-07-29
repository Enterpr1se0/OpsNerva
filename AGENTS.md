# Repository Agent Instructions

## Temporary files and caches

- Never place build caches, test caches, package-manager caches, compiler work directories, or other temporary files inside this repository.
- Use each tool's default cache directory when it is outside this repository; do not override it without a concrete isolation or reproducibility need.
- Put task-owned temporary artifacts that need an explicit location in a task-specific directory under the system temporary directory, and clean them after the task.
- Repository-managed build outputs such as `web/dist` are product artifacts, not caches.

## Frontend copy

- Keep frontend text concise and limited to labels, values, statuses, actionable errors, and instructions required to complete an operation.
- Do not add explanatory, promotional, repetitive, or self-evident helper text.
- When removing UI text, also delete its unused locale keys, markup, and styles.
