// p1s-bridge serves a phone-friendly control page for a Bambu P1S on the
// local network: bed actions and live status over the printer's MQTT
// interface, camera via its chamber-image stream.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/web"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var %s", key)
	}
	return v
}

func main() {
	ip := mustEnv("PRINTER_IP")
	serial := mustEnv("PRINTER_SERIAL")
	accessCode := mustEnv("PRINTER_ACCESS_CODE")
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	cache := p1s.NewStateCache()
	client := p1s.NewClient(ip, serial, accessCode, cache)
	client.Start()
	defer client.Stop()

	hub := web.NewHub(func(ctx context.Context, yield func([]byte)) error {
		return p1s.StreamFrames(ctx, net.JoinHostPort(ip, "6000"), "bblp", accessCode, yield)
	})

	log.Printf("p1s-bridge listening on %s (printer %s)", addr, ip)
	log.Fatal(http.ListenAndServe(addr, web.NewServer(cache, client, hub).Handler()))
}
