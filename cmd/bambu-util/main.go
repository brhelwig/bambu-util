// bambu-util serves a phone-friendly control page for a Bambu P1S on the
// local network: bed actions and live status over the printer's MQTT
// interface, camera via its chamber-image stream, recorded continuously
// into a rolling history buffer.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brhelwig/bambu-util/internal/history"
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

// DefaultRetention is how long recorded frames are kept when
// RECORDING_RETENTION isn't set.
const DefaultRetention = 24 * time.Hour

func jobNameString(fields map[string]any) string {
	if v, ok := p1s.JobName(fields).(string); ok {
		return v
	}
	return ""
}

func main() {
	ip := mustEnv("PRINTER_IP")
	serial := mustEnv("PRINTER_SERIAL")
	accessCode := mustEnv("PRINTER_ACCESS_CODE")
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	retention := DefaultRetention
	if v := os.Getenv("RECORDING_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid RECORDING_RETENTION %q: %v", v, err)
		}
		retention = d
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	cache := p1s.NewStateCache()
	client := p1s.NewClient(ip, serial, accessCode, cache)
	client.Start()
	defer client.Stop()

	store, err := history.Open(filepath.Join(dataDir, "bambu-util.db"))
	if err != nil {
		log.Fatalf("open history store: %v", err)
	}
	defer store.Close()

	hub := web.NewHub(func(ctx context.Context, yield func([]byte)) error {
		return p1s.StreamFrames(ctx, net.JoinHostPort(ip, "6000"), "bblp", accessCode, yield)
	}, store)

	srv := web.NewServer(cache, client, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Start(ctx)
	go srv.EnforceAutoOff(ctx)
	go history.RunPruner(ctx, store, retention, 5*time.Minute, time.Now)
	go history.NewJobWatcher(store).Run(ctx, 2*time.Second, func() (string, string) {
		fields, _ := cache.Snapshot()
		return p1s.GcodeState(fields), jobNameString(fields)
	})

	log.Printf("bambu-util listening on %s (printer %s, recording retention %s)", addr, ip, retention)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
