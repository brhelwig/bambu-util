// bambu-util serves a phone-friendly control page for a Bambu P1S on the
// local network: bed actions and live status over the printer's MQTT
// interface, camera via its chamber-image stream, recorded continuously
// into a rolling history buffer.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/auth"
	"github.com/brhelwig/bambu-util/internal/capacity"
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

// app is the assembled program — everything below main, in one piece, so it can
// be started without a process around it.
type app struct {
	handler  http.Handler
	cache    *p1s.StateCache
	describe func() string
	close    func()
}

// newApp opens the database under dataDir and wires everything to it. The
// background loops run until ctx is cancelled.
//
// The decision about authentication is passed in rather than read here, so a
// test says which it wants instead of arranging an environment.
func newApp(ctx context.Context, dataDir string, decided auth.Decision) (*app, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}

	cache := p1s.NewStateCache()

	db, err := sqlitedb.Open(filepath.Join(dataDir, "bambu-util.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	config, err := settings.New(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open settings: %w", err)
	}
	// The log reads its budget on every entry, so changing it on the settings
	// page takes hold without a restart.
	events, err := activity.New(db, func() int64 { return config.Values().ActivityLimit })
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open event log: %w", err)
	}

	link := p1s.NewLink(cache, events)
	closeAll := func() {
		link.Stop()
		db.Close()
	}

	store, err := history.New(db)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("open history store: %w", err)
	}

	hub := web.NewHub(link.Stream, store)

	notifyStore, err := push.New(db)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("open notification store: %w", err)
	}
	notifier, err := push.NewSender(notifyStore)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("load notification identity: %w", err)
	}
	notifier.Watch(events)

	timers, err := deadlines.New(db)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("open pending timers: %w", err)
	}

	// Whatever printer was set up last time. With none, the app still serves the
	// page — that is where one is entered.
	v := config.Values()
	link.Configure(p1s.Config{IP: v.PrinterIP, Serial: v.PrinterSerial, AccessCode: v.AccessCode})

	logins, err := auth.NewStore(db)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("open the session store: %w", err)
	}
	var guard *auth.Authenticator
	if !decided.Disabled {
		// Discovery happens now rather than at the first login, so a wrong
		// issuer stops the app here instead of when someone tries to get in.
		guard, err = auth.New(ctx, decided.Config, logins,
			func() time.Duration { return config.Values().SessionLength })
		if err != nil {
			closeAll()
			return nil, err
		}
	}

	srv := web.NewServer(cache, link, store, notifier, timers, config.Values, config, link, events)
	go auth.RunSweeper(ctx, logins, time.Hour, time.Now)
	go hub.Start(ctx)
	go srv.EnforceAutoOff(ctx)
	go srv.EnforceLampAutomation(ctx)
	go srv.EnforceEventNotifications(ctx)
	// The size cap runs after the retention pruner on the same cadence, and only
	// bites when what retention left is still more than the disk should hold.
	go capacity.Run(ctx, capacity.New(db,
		func() int64 { return config.Values().DatabaseLimit }, store, events), 5*time.Minute)
	go history.RunPruner(ctx, store, func() history.Policy {
		v := config.Values()
		return history.Policy{Window: v.Retention, KeptJobs: v.KeptJobs}
	}, 5*time.Minute, time.Now)
	go history.NewJobWatcher(store).Run(ctx, 2*time.Second, func() (string, string) {
		fields, _ := cache.Snapshot()
		return p1s.GcodeState(fields), jobNameString(fields)
	})

	// Everything the app serves goes behind the login, apart from the health
	// check and the handful of files a phone needs before it can log in.
	return &app{
		handler:  guard.Handler(srv.Handler()),
		cache:    cache,
		describe: link.Describe,
		close:    closeAll,
	}, nil
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

	// Before anything is opened or served: either a provider was configured or
	// running without one was expressly asked for. Neither is not a default.
	decided, err := auth.Decide(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if decided.Disabled {
		log.Printf("WARNING: %s is set, so anything that can reach this port can drive the printer and watch the camera",
			auth.EnvDisabled)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := newApp(ctx, dataDir, decided)
	if err != nil {
		log.Fatal(err)
	}
	defer a.close()

	log.Printf("bambu-util listening on %s (%s)", addr, a.describe())
	log.Fatal(http.ListenAndServe(addr, a.handler))
}
