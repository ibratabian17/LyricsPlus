# Project Overview

This document provides an overview of the LyricsPlus Backend, which has undergone a significant refactoring to improve its architecture, modularity, and adaptability across various JavaScript runtimes.

## Architectural Changes

The project has transitioned from a traditional Express.js setup and Cloudflare Workers (Wrangler) to a more modern and lightweight approach using the [Hono.js](https://hono.dev/) framework. This change was driven by the desire for:

*   **Improved Readability**: A cleaner, more functional approach to API route definition and middleware.
*   **Enhanced Modularity**: Services and handlers are now more distinctly separated, making the codebase easier to navigate and maintain.
*   **Multi-Environment Support**: Hono.js allows the application to run seamlessly on Bun, Node.js, Cloudflare Workers, Vercel Edge Functions, and other JavaScript runtimes, providing greater deployment flexibility and performance optimization.

## Key Technologies

*   **Hono.js**: A small, simple, and ultrafast web framework for the Edge. It provides a fast and efficient way to build APIs.
*   **Bun**: Fast all-in-one JavaScript runtime used as the primary execution engine and local server.
*   **Cloudflare Workers**: For serverless edge computing, leveraging Hono.js's compatibility.
*   **Node.js**: Supported as an alternative/fallback server runtime.


## Module Structure

The application now follows a modular structure, with each major feature or domain having its own module within the `src/modules` directory. Each module typically contains:

*   **Handlers**: Logic for processing requests and interacting with services.
*   **Controllers**: Additional business logic or data processing, complementing handlers.
*   **Services**: Business logic and external API integrations (e.g., Apple Music, Musixmatch, Spotify, QQ Music).

This structure promotes better organization, reusability, and testability of code.

## Benefits of Refactoring

The refactoring efforts have resulted in:

*   **Easier Maintenance**: Due to improved code organization and readability.
*   **Fix ai broken shit**: Trusting ai overly can causing the project hard to maintain.

This new architecture sets the foundation for future enhancements and ensures the LyricsPlus Backend remains a high-performance and maintainable service.
