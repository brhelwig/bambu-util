// bambu-util serves a phone-friendly control page for a Bambu P1S on the
// local network: bed actions and live status over the printer's MQTT
// interface, camera via its chamber-image stream, recorded continuously
// into a rolling history buffer.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
	"github.com/brhelwig/bambu-util/internal/settings"
	"github.com/brhelwig/bambu-util/internal/sqlitedb"
	"github.com/brhelwig/bambu-util/internal/web"
)

func jobNameString(fields map[string]any) string {
	if v, ok := p1s.JobName(fields).(string); ok {
		return v
	}
	return ""
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	cache := p1s.NewStateCache()
	link := p1s.NewLink(cache)
	defer link.Stop()

	db, err := sqlitedb.Open(filepath.Join(dataDir, "bambu-util.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	store, err := history.New(db)
	if err != nil {
		log.Fatalf("open history store: %v", err)
	}

	hub := web.NewHub(link.Stream, store)

	notifyStore, err := push.New(db)
	if err != nil {
		log.Fatalf("open notification store: %v", err)
	}
	notifier, err := push.NewSender(notifyStore)
	if err != nil {
		log.Fatalf("load notification identity: %v", err)
	}

	timers, err := deadlines.New(db)
	if err != nil {
		log.Fatalf("open pending timers: %v", err)
	}

	config, err := settings.New(db)
	if err != nil {
		log.Fatalf("open settings: %v", err)
	}
	// Whatever printer was set up last time. With none, the app still serves the
	// page — that is where one is entered.
	v := config.Values()
	link.Configure(p1s.Config{IP: v.PrinterIP, Serial: v.PrinterSerial, AccessCode: v.AccessCode})

	// The scrub bar reaches back exactly as far as frames are kept, so raising
	// retention doesn't record footage that can't be scrubbed to.
	srv := web.NewServer(cache, link, store, notifier, timers, config.Values, config, link)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Start(ctx)
	go srv.EnforceAutoOff(ctx)
	go srv.EnforceLampAutomation(ctx)
	go srv.EnforceEventNotifications(ctx)
	go history.RunPruner(ctx, store, func() history.Policy {
		v := config.Values()
		return history.Policy{Window: v.Retention, KeptJobs: v.KeptJobs}
	}, 5*time.Minute, time.Now)
	go history.NewJobWatcher(store).Run(ctx, 2*time.Second, func() (string, string) {
		fields, _ := cache.Snapshot()
		return p1s.GcodeState(fields), jobNameString(fields)
	})

	log.Printf("bambu-util listening on %s (%s)", addr, link.Describe())
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
