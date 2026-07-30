package p1s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/brhelwig/bambu-util/internal/activity"
)

// Config is what it takes to reach one printer.
type Config struct {
	IP         string
	Serial     string
	AccessCode string
}

// Complete reports whether there is enough here to try connecting.
func (c Config) Complete() bool {
	return c.IP != "" && c.Serial != "" && c.AccessCode != ""
}

// ErrUnconfigured is returned by Stream while no printer has been set up. The
// camera loop retries on error, so it simply keeps asking until one is.
var ErrUnconfigured = errors.New("p1s: no printer configured")

// Link is the connection to one printer, which can be pointed at another while
// the process runs.
//
// It stands in for the client everywhere the app used to hold one directly, so
// that reconfiguring is a matter of swapping what is behind the link rather
// than rebuilding everything that refers to it. Commands sent while no printer
// is configured go nowhere; the state cache reports not connected, so the
// existing guards already refuse them with a reason.
type Link struct {
	mu           sync.Mutex
	cache        *StateCache
	log          *activity.Log
	conf         Config
	client       *Client
	cancelStream context.CancelFunc
}

func NewLink(cache *StateCache, log *activity.Log) *Link {
	return &Link{cache: cache, log: log}
}

// Config returns the printer currently configured.
func (l *Link) Config() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conf
}

// Configure points the link at a printer, replacing whatever it was on. The old
// connection is dropped and the cached state cleared, so nothing the previous
// printer said is still on screen a moment later.
func (l *Link) Configure(conf Config) {
	l.mu.Lock()
	old, cancel := l.client, l.cancelStream
	l.conf = conf
	l.client = nil
	if conf.Complete() {
		l.client = NewClient(conf.IP, conf.Serial, conf.AccessCode, l.cache, l.log)
	}
	client := l.client
	l.mu.Unlock()

	if old != nil {
		old.Stop()
	}
	l.cache.Reset()
	if client != nil {
		client.Start()
	}
	// Drop any camera attempt still running against the old printer; the loop
	// that owns it redials against the new one.
	if cancel != nil {
		cancel()
	}
}

// Stop closes the connection, if there is one.
func (l *Link) Stop() {
	l.mu.Lock()
	client, cancel := l.client, l.cancelStream
	l.client = nil
	l.mu.Unlock()
	if client != nil {
		client.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

// Stream feeds camera frames from the configured printer until ctx is cancelled,
// the connection breaks, or the printer is reconfigured. It matches the shape
// the recording loop wants, so that loop needs to know nothing about which
// printer it is talking to.
func (l *Link) Stream(ctx context.Context, yield func([]byte)) error {
	l.mu.Lock()
	conf := l.conf
	ctx, cancel := context.WithCancel(ctx)
	l.cancelStream = cancel
	l.mu.Unlock()
	defer cancel()

	if !conf.Complete() {
		return ErrUnconfigured
	}
	return StreamFrames(ctx, net.JoinHostPort(conf.IP, "6000"), "bblp", conf.AccessCode, yield)
}

// send runs f against the current client, and does nothing when there is no
// printer. Commands are already refused by the guards in that case; this is the
// backstop for a printer removed between the guard and the send.
func (l *Link) send(f func(*Client)) {
	l.mu.Lock()
	client := l.client
	l.mu.Unlock()
	if client != nil {
		f(client)
	}
}

func (l *Link) LowerBed()           { l.send((*Client).LowerBed) }
func (l *Link) Home()               { l.send((*Client).Home) }
func (l *Link) Extrude()            { l.send((*Client).Extrude) }
func (l *Link) UnloadFilament()     { l.send((*Client).UnloadFilament) }
func (l *Link) PausePrint()         { l.send((*Client).PausePrint) }
func (l *Link) ResumePrint()        { l.send((*Client).ResumePrint) }
func (l *Link) StopPrint()          { l.send((*Client).StopPrint) }
func (l *Link) SetBedTemp(t int)    { l.send(func(c *Client) { c.SetBedTemp(t) }) }
func (l *Link) SetNozzleTemp(t int) { l.send(func(c *Client) { c.SetNozzleTemp(t) }) }
func (l *Link) SetChamberLight(on bool) {
	l.send(func(c *Client) { c.SetChamberLight(on) })
}

func (l *Link) SetAmsFilament(amsID, trayID int, trayInfoIdx, color, trayType string, tempMin, tempMax int) {
	l.send(func(c *Client) {
		c.SetAmsFilament(amsID, trayID, trayInfoIdx, color, trayType, tempMin, tempMax)
	})
}

// Describe reports the configured printer for a log line, without the access
// code.
func (l *Link) Describe() string {
	conf := l.Config()
	if !conf.Complete() {
		return "no printer configured"
	}
	return fmt.Sprintf("printer %s", conf.IP)
}
