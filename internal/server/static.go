package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

func (s *Server) staticHandler() http.Handler {
	root, ok := webFS()
	if !ok {
		return http.HandlerFunc(servePlaceholder)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}
		info, err := fs.Stat(root, clean)
		missing := err != nil || info.IsDir()
		if missing {
			if clean != "index.html" && path.Ext(clean) != "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			index := r.Clone(r.Context())
			index.URL.Path = "/"
			files.ServeHTTP(w, index)
			return
		}
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// servePlaceholder is served by binaries built without the embedded console.
func servePlaceholder(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Model Relay</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #0b0d12; color: #e6e9f0;
         font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
  main { max-width: 620px; padding: 48px 40px; border: 1px solid #232838; border-radius: 18px; background: #11141c; }
  .brand { display: flex; align-items: center; gap: 12px; margin-bottom: 28px; }
  .logo { width: 38px; height: 38px; border-radius: 10px; background: linear-gradient(135deg, #6366f1, #8b5cf6);
          display: grid; place-items: center; font-weight: 700; }
  h1 { font-size: 20px; margin: 0; }
  .tag { color: #8d94a8; font-size: 13px; margin-top: 2px; }
  .status { display: inline-flex; align-items: center; gap: 8px; font-size: 13px; color: #86efac;
            background: #0f241a; border: 1px solid #1d4d33; padding: 6px 12px; border-radius: 999px; margin-bottom: 22px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: #4ade80; }
  p { line-height: 1.65; color: #b6bdcf; font-size: 14px; }
  pre { background: #0b0d12; border: 1px solid #232838; border-radius: 10px; padding: 14px 16px;
        font-size: 12.5px; color: #c8d0e4; overflow-x: auto; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .note { font-size: 12.5px; color: #7d8498; margin-top: 20px; }
</style>
</head>
<body>
<main>
  <div class="brand">
    <div class="logo">MR</div>
    <div>
      <h1>Model Relay</h1>
      <div class="tag">OpenAI-compatible gateway</div>
    </div>
  </div>
  <div class="status"><span class="dot"></span>Backend is running</div>
  <p>The admin console was not built into this binary. The relay API and admin API are available, but this build only ships the placeholder page.</p>
  <p>Build the web console and embed it:</p>
  <pre><code>cd web
pnpm install
pnpm build

cd ..
go build -tags embed -o bin/gateway ./cmd/server</code></pre>
  <div class="note">Without the embed build tag, ./bin/gateway serves this placeholder. REST endpoints stay available at /api and /v1.</div>
</main>
</body>
</html>`))
}
