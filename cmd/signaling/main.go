package main

import (
	"context"
	"crypto/tls"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/antoniohauren/go-screen-share/internal/signaling"
	"github.com/antoniohauren/go-screen-share/web"
)

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	origin := flag.String("origin", os.Getenv("SCREENSHARE_URL"), "public HTTPS origin used in viewer URLs")
	cert := flag.String("cert", "", "TLS certificate PEM path")
	key := flag.String("key", "", "TLS private key PEM path")
	flag.Parse()
	u, err := url.Parse(*origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		log.Fatal("-origin must be a public HTTPS origin, for example https://share.example.com")
	}
	if *cert == "" || *key == "" {
		log.Fatal("-cert and -key are required; insecure HTTP is not supported")
	}
	service := signaling.New(strings.TrimSuffix(*origin, "/"))
	defer service.Close()
	assets, err := fs.Sub(web.Assets, "viewer")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/session", service)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	server := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			mux.ServeHTTP(w, r)
		}),
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		service.Close()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("HTTPS signaling listening on %s", *addr)
	if err := server.ListenAndServeTLS(*cert, *key); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
