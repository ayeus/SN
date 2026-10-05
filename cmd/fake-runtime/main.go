// Command fake-runtime runs the stand-in for Ollama from internal/fakeruntime,
// so the platform can be exercised end to end with no GPU and no model
// download (see `make smoke-fake`).
//
// The default port is 11435, one above Ollama's, so it never collides with a
// real runtime on the same machine. Point an agent at it with
// SN_RUNTIME_URL=http://127.0.0.1:11435.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/fakeruntime"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:11435", "address to listen on")
	models := flag.String("models", os.Getenv("FAKE_RUNTIME_MODELS"), "comma-separated models that are already downloaded")
	firstByte := flag.Int("first-byte-delay-ms", 0, "delay before every completion starts")
	tokenDelay := flag.Int("token-delay-ms", 0, "pause between streamed tokens")
	flag.Parse()

	var cached []string
	for _, m := range strings.Split(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			cached = append(cached, m)
		}
	}
	rt := fakeruntime.New(fakeruntime.Config{FirstByteDelayMs: *firstByte, TokenDelayMs: *tokenDelay}, cached...)

	srv := &http.Server{Addr: *addr, Handler: rt.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fake runtime listening on %s (cached models: %v)", *addr, cached)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
