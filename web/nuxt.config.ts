export default defineNuxtConfig({
  ssr: false,                          // SPA embedded in the Go binary
  modules: ['@nuxt/ui'],
  devtools: { enabled: false },
  app: { baseURL: '/' },
  runtimeConfig: { public: { apiBase: '/' } }, // same-origin
  nitro: {
    static: true,
    // `nuxt dev` proxies the authed API + SSE/WS to a running node (self-signed TLS).
    devProxy: {
      '/v1':      { target: 'https://localhost:8443/v1',      secure: false, ws: true, changeOrigin: true },
      '/healthz': { target: 'https://localhost:8443/healthz', secure: false },
      '/metrics': { target: 'https://localhost:8443/metrics', secure: false },
    },
  },
})
