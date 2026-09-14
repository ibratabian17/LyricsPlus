# Architecture Overview

This document outlines the architectural design of the LyricsPlus Backend, which is built using the Hono.js framework. The refactoring aimed to enhance modularity, readability, and provide multi-environment support.

## Hono.js Framework

Hono.js is a lightweight, fast, and edge-optimized web framework. It provides a minimalistic yet powerful API for building web applications and APIs, compatible with various JavaScript runtimes like Bun, Node.js, Cloudflare Workers, and Vercel Edge Functions.

### Key Concepts in Hono.js

*   **App Instance**: The core of a Hono.js application is the `Hono` instance, which manages routes, middleware, and request handling.
*   **Routes**: Routes are defined using HTTP methods (e.g., `app.get()`, `app.post()`) and path patterns. They map incoming requests to specific handler functions.
*   **Middleware**: Functions that execute before or after a route handler. Middleware can perform tasks like authentication, logging, CORS handling, or data parsing. Hono.js middleware can be applied globally or to specific routes/groups of routes.
*   **Context (`c`)**: The `Context` object is passed to every handler and middleware function. It provides access to the request, response, environment variables, and utility methods.

## Project Structure

The project follows a modular structure, primarily organized within the `src/` directory:

*   **`src/index.js`**: The main entry point for edge environments (e.g., Cloudflare Workers), exposing the Hono app's fetch handler.
*   **`src/app/app.js`**: Initializes the Hono.js application instance and applies global middleware.
*   **`src/app/server.js`**: Configures and starts the HTTP server using native `Bun.serve` (with fallback to `@hono/node-server` for Node.js).
*   **`src/modules/`**: Contains feature-specific modules, each encapsulating its own routes, handlers, and services. This promotes separation of concerns and easier maintenance.
    *   Each module typically has a `handler.js` file that defines the module's routes and their corresponding logic. Some modules may also include a `controller.js` file for additional business logic or data processing.
*   **`src/shared/`**: Contains common utilities, configurations, middleware, parsers, and services that are shared across different modules.
    *   **`config.js`**: Application-wide configuration settings.
    *   **`middleware/`**: Global or shared middleware functions (e.g., `cors.middleware.js`).
    *   **`parsers/`**: Logic for parsing lyrics from various formats (e.g., KPOE, LRC, Musixmatch, Spotify, TTML, QRC/QQ).
    *   **`services/`**: Integrations with external APIs or business logic components (e.g., `appleMusic.service.js`, `lyricsPlus.service.js`, `musixmatch.service.js`, `qq.service.js`, `spotify.service.js`, `songCatalog.service.js`).
    *   **`utils/`**: General utility functions (e.g., `db.util.js`, `file.util.js`, `googleDrive.util.js`, `kv.emulator.js`, `similarity.util.js`).

## Request Flow

1.  An incoming HTTP request is received by the server (e.g., Bun, Node.js, Cloudflare Worker).
2.  The request is passed to the `app` instance.
3.  Global middleware (defined in `src/app/app.js`) are executed.
4.  Hono.js matches the request path and method to a defined route within one of the modules in `src/modules/`.
5.  Any route-specific middleware are executed.
6.  The corresponding handler function (e.g., in `lyrics.handler.js`) is invoked.
7.  The handler uses services (from `src/shared/services/`) and utilities (from `src/shared/utils/`) to process the request, interact with external APIs, or fetch data.
8.  The handler constructs a response using the `Context` object and sends it back to the client.

This architecture ensures a clear separation of concerns, making the application scalable, testable, and easy to understand.
