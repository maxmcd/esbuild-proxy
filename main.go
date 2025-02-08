package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

type responseWriter struct {
	http.ResponseWriter
	status int
	size   int64
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	size, err := rw.ResponseWriter.Write(b)
	rw.size += int64(size)
	return size, err
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)

		slog.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"size", wrapped.size,
			"duration", duration,
		)
	})
}

func sendError(w http.ResponseWriter, msg string, err error) {
	w.Header().Set("Content-Type", "application/javascript")
	v, _ := json.Marshal(msg)
	_, _ = w.Write([]byte(fmt.Sprintf(`console.error(%s);`, v)))
	_, _ = w.Write([]byte(fmt.Sprintf(`console.error(%q)`, err.Error())))
}

var htmlPage = `
<!DOCTYPE html>
<html>
<head>
	<title>TypeScript Bundle Service</title>
	<link rel="icon" href="https://fav.farm/💐">
	<style>
		body { font-family: system-ui; max-width: 800px; margin: 40px auto; padding: 0 20px; line-height: 1.6; }
		pre { background: #f4f4f4; padding: 15px; border-radius: 5px; }
	</style>
</head>
<body>
	<h1>TypeScript Bundle Service</h1>
	<p>This service bundles TypeScript files into JavaScript. To use it, append a URL to a TypeScript file to this domain.</p>
	<p>Example usage:</p>
	<pre>import "<a href="%s/https://esm.town/v/maxm/blitheJadeBee">%s/https://esm.town/v/maxm/blitheJadeBee</a>"</pre>
</body>
</html>`

func serveBundle(w http.ResponseWriter, r *http.Request, hash string) {
	// Extract hash from URL and read from cache
	cachePath := ".cache/" + hash
	bundle, err := os.ReadFile(cachePath)
	if err != nil {
		sendError(w, "Failed to read from cache: "+err.Error(), err)
		return
	}

	// Calculate ETag using SHA-256 hash of bundle
	shaHash := sha256.Sum256(bundle)
	etag := fmt.Sprintf(`"%x"`, shaHash[:16]) // Use first 16 bytes for shorter ETag
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Check if client has matching ETag
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("ETag", etag)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000") // Cache for 1 year
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bundle)))
	_, _ = w.Write(bundle)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	// Check cache
	if err := os.MkdirAll(".cache", 0755); err != nil {
		log.Panicln(err)
		return
	}

	log.Printf("Starting server on http://localhost:%s", port)

	// Create TCP listener
	listener, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		log.Panicf("Failed to create listener: %v", err)
	}

	// Create server
	server := &http.Server{
		Handler: loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			// Return helpful HTML page if path is empty
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(fmt.Sprintf(htmlPage, "//"+r.Host, r.URL.Scheme+"https://"+r.Host)))
				return
			}

			path := strings.TrimPrefix(r.URL.Path, "/")
			fullURL := path + "?" + r.URL.RawQuery
			originalURL := fullURL
			start := time.Now()
			slog.Info("starting bundle process", "url", fullURL)

			// Get final redirect location
			client := &http.Client{
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}

			resp, err := client.Get(fullURL)
			if err != nil {
				sendError(w, "Failed to fetch URL: "+err.Error(), err)
				return
			}

			// Follow redirects manually to get final URL
			for resp.StatusCode == http.StatusMovedPermanently ||
				resp.StatusCode == http.StatusFound ||
				resp.StatusCode == http.StatusSeeOther ||
				resp.StatusCode == http.StatusTemporaryRedirect {

				u, _ := resp.Location()
				fullURL = u.String()
				resp, err = client.Get(fullURL)
				if err != nil {
					sendError(w, "Failed to follow redirect: "+err.Error(), err)
					return
				}
			}

			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				sendError(w, "Failed to fetch URL: "+resp.Status, fmt.Errorf("status %d", errors.New(string(b))))
				return
			}
			if originalURL != fullURL {
				w.Header().Set("Location", "/"+fullURL)
				w.WriteHeader(http.StatusFound)
				return
			}
			// Create hash of final URL
			hasher := sha256.New()
			hasher.Write([]byte(fullURL))
			hash := fmt.Sprintf("%x", hasher.Sum(nil))[:20]

			cachePath := ".cache/" + hash
			if _, err := os.Stat(cachePath); err == nil {
				slog.Info("cache hit", "hash", hash, "duration", time.Since(start))
				serveBundle(w, r, hash)
				return
			}
			slog.Info("cache miss", "hash", hash, "duration", time.Since(start))

			// Cache miss - read response and build
			content, err := io.ReadAll(resp.Body)
			if err != nil {
				sendError(w, "Failed to read response: "+err.Error(), err)
				return
			}
			resp.Body.Close()

			// Create temp directory
			tmpDir, err := os.MkdirTemp("", "vite-build-*")
			if err != nil {
				sendError(w, "Failed to create temp dir: "+err.Error(), err)
				return
			}
			// Create src directory
			srcDir := tmpDir + "/src"
			if err := os.MkdirAll(srcDir, 0755); err != nil {
				sendError(w, "Failed to create src dir: "+err.Error(), err)
				return
			}

			fmt.Println(tmpDir)

			// Copy package files
			for _, file := range []string{"package.json", "bun.lock", "tsconfig.json"} {
				content, err := os.ReadFile(file)
				if err != nil {
					sendError(w, "Failed to read "+file+": "+err.Error(), err)
					return
				}
				if err := os.WriteFile(tmpDir+"/"+file, content, 0644); err != nil {
					sendError(w, "Failed to write "+file+": "+err.Error(), err)
					return
				}
			}

			// TODO: this causes weird errors
			// Copy node_modules directory
			// cmd := exec.Command("cp", "-r", "node_modules", tmpDir+"/node_modules")
			// if err := cmd.Run(); err != nil {
			// 	sendError(w, "Failed to copy node_modules: "+err.Error(), err)
			// 	return
			// }

			if err := os.WriteFile(srcDir+"/index.ts", content, 0644); err != nil {
				sendError(w, "Failed to write index.ts: "+err.Error(), err)
				return
			}

			slog.Info("running dependency check", "duration", time.Since(start))

			// Run depcheck
			var stdout, stderr bytes.Buffer
			cmd := exec.Command("bunx", "depcheck", "--json", "src/index.ts")
			cmd.Dir = tmpDir
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					if exitErr.ExitCode() != 255 {
						sendError(w, "Depcheck failed: "+stdout.String()+"\n"+stderr.String(), exitErr)
						return
					}
				} else {
					sendError(w, "Depcheck failed "+err.Error(), err)
					return
				}
			}
			output := stdout.Bytes()

			var depcheck struct {
				Missing map[string][]string `json:"missing"`
			}
			if err := json.Unmarshal(output, &depcheck); err != nil {
				sendError(w, "Failed to parse depcheck output: "+err.Error(), err)
				return
			}

			slog.Info("installed dependencies",
				"missing_count", len(depcheck.Missing),
				"duration", time.Since(start))

			// Install missing dependencies
			args := []string{"install"}
			if len(depcheck.Missing) > 0 {
				args = append(args, "--save")
			}
			for pkg := range depcheck.Missing {
				args = append(args, pkg)
			}
			cmd = exec.Command("bun", args...)
			cmd.Dir = tmpDir
			stdout.Reset()
			stderr.Reset()
			cmd.Stdout = &stdout
			cmd.Stderr = &stdout
			err = cmd.Run()
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					sendError(w, "bun install failed: "+stdout.String(), exitErr)
				} else {
					sendError(w, "bun install failed: "+err.Error(), err)
				}
				return
			}

			result := api.Build(api.BuildOptions{
				EntryPoints:       []string{filepath.Join(srcDir, "index.ts")},
				Bundle:            true,
				Write:             true,
				Outfile:           filepath.Join(tmpDir, "dist", "bundle.js"),
				Target:            api.ES2015,
				Format:            api.FormatESModule,
				Sourcemap:         api.SourceMapLinked,
				MinifyWhitespace:  true,
				MinifyIdentifiers: true,
				MinifySyntax:      true,
			})

			if len(result.Errors) > 0 {
				sendError(w, "Build failed", fmt.Errorf("build failed: %v errors", result.Errors))
				return
			}

			// Read and return bundle.js
			bundle, err := os.ReadFile(tmpDir + "/dist/bundle.js")
			if err != nil {
				sendError(w, "Failed to read bundle.js: "+err.Error(), err)
				return
			}

			// Write bundle to cache
			if err := os.WriteFile(cachePath, bundle, 0644); err != nil {
				sendError(w, "Failed to write to cache: "+err.Error(), err)
				return
			}
			// TODO: don't write and read the same file

			// Redirect to URL with hash
			serveBundle(w, r, hash)

			// After dependency check
			slog.Info("installed dependencies",
				"missing_count", len(depcheck.Missing),
				"duration", time.Since(start))

			// After build
			slog.Info("build completed", "duration", time.Since(start))

			// After caching
			slog.Info("bundle cached and ready to serve",
				"size", len(bundle),
				"total_duration", time.Since(start))
		})),
	}

	// Channel to listen for shutdown signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in goroutine
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-stop
	log.Println("Shutting down server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt graceful shutdown
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}
