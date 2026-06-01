package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	listen := ":9999"
	baseDir := "~/.cache/blobserver/blobs"
	flag.Parse()

	if strings.HasPrefix(baseDir, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("getting home directory: %w", err)
		}
		baseDir = filepath.Join(homeDir, strings.TrimPrefix(baseDir, "~/"))
	}
	s := &httpServer{
		baseDir: baseDir,
	}

	klog.Infof("serving on %q", listen)
	if err := http.ListenAndServe(listen, s); err != nil {
		return fmt.Errorf("serving on %q: %w", listen, err)
	}

	return nil
}

type httpServer struct {
	baseDir string
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tokens := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(tokens) == 1 {
		if r.Method == "GET" {
			hash := tokens[0]
			s.serveGETBlob(w, r, hash)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (s *httpServer) serveGETBlob(w http.ResponseWriter, r *http.Request, hash string) {
	// TODO: Validate hash is hex, right length etc
	p := filepath.Join(s.baseDir, hash)
	_, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			switch hash {
			case "16d824ee771e0e33b762bb3dc3232b972ac8dce4d2d449128fca5081962a1a9e":
				// TODO: Download and cache
				target := "https://huggingface.co/bartowski/Meta-Llama-3-8B-Instruct-GGUF/resolve/main/Meta-Llama-3-8B-Instruct-Q5_K_M.gguf"
				klog.Infof("redirecting blob %q to %q", hash, target)
				http.Redirect(w, r, target, http.StatusFound)
			default:
				klog.Infof("blob %q not found", p)
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}
		klog.Warningf("unable to get stat(%q): %v", p, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	klog.Infof("serving blob %q", p)
	http.ServeFile(w, r, p)
}
