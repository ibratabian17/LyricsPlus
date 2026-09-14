# Contribution Guidelines

We welcome contributions to the LyricsPlus Backend! This document outlines guidelines for contributing to the project, focusing on code structure, adding new features, and maintaining code quality.

## Getting Started

1.  **Fork the repository** and clone it to your local machine.
2.  **Install dependencies**: `bun install`
3.  **Create a new branch** for your feature or bug fix: `git checkout -b feature/your-feature-name` or `git checkout -b bugfix/issue-description`

## Code Structure and Style

*   **Modularity**: Adhere to the existing modular structure. New features should ideally reside within their own module in `src/modules/`, utilizing handlers and controllers as appropriate, or extend existing ones.
*   **Shared Components**: Utilize components in `src/shared/` for common functionalities (e.g., services, parsers, utilities).
*   **Naming Conventions**: Use consistent naming for files, variables, and functions (e.g., `camelCase` for variables/functions, `PascalCase` for classes/constructors, `kebab-case` for filenames).
*   **Verification**: Run `bun test` and `bun audit` before submitting changes.

