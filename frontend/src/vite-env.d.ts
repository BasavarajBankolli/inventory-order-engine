/// <reference types="vite/client" />

// Typed environment variables (see .env.example).
interface ImportMetaEnv {
  /** Backend base URL, e.g. http://localhost:8080 or https://my-api.onrender.com */
  readonly VITE_API_BASE_URL?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
