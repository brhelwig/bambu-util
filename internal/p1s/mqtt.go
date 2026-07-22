package p1s

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	// Z200 — 50mm above the bottom of the ~250mm travel; covers position
	// drift without homing (a blind Z250 once hit the bottom limit).
	BedDropGcode    = "G90\nG1 Z200 F900\n"
	HomeGcode       = "G28\n"
	BedDryTemp      = 100
	NozzleCleanTemp = 200
)

// Client is the MQTT link to the printer: cached merged state plus
// fire-and-forget commands. Port of the Python TUI's PrinterClient.
type Client struct {
	serial string
	cache  *StateCache
	mqtt   mqtt.Client
	seq    atomic.Int64
}

func NewClient(ip, serial, accessCode string, cache *StateCache) *Client {
	c := &Client{serial: serial, cache: cache}
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("ssl://%s:8883", ip)).
		SetUsername("bblp").
		SetPassword(accessCode).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}). // self-signed printer cert
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(15 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second)
	opts.OnConnect = func(m mqtt.Client) {
		cache.SetConnected(true)
		m.Subscribe(fmt.Sprintf("device/%s/report", serial), 0, func(_ mqtt.Client, msg mqtt.Message) {
			HandleReport(cache, msg.Payload())
		})
		c.publish(`{"pushing":{"sequence_id":"0","command":"pushall"}}`)
	}
	opts.OnConnectionLost = func(mqtt.Client, error) { cache.SetConnected(false) }
	c.mqtt = mqtt.NewClient(opts)
	return c
}

func (c *Client) Start() { c.mqtt.Connect() }
func (c *Client) Stop()  { c.mqtt.Disconnect(250) }

// HandleReport merges the "print" object of a report payload into the cache.
// Anything else is ignored.
func HandleReport(cache *StateCache, payload []byte) {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}
	if fields, ok := data["print"].(map[string]any); ok {
		cache.Merge(fields)
	}
}

func (c *Client) publish(payload string) {
	c.mqtt.Publish(fmt.Sprintf("device/%s/request", c.serial), 0, false, payload)
}

func (c *Client) SendGcode(gcode string) {
	req := map[string]any{"print": map[string]any{
		"sequence_id": strconv.FormatInt(c.seq.Add(1), 10),
		"command":     "gcode_line",
		"param":       gcode,
	}}
	b, _ := json.Marshal(req)
	c.publish(string(b))
}

func (c *Client) LowerBed()           { c.SendGcode(BedDropGcode) }
func (c *Client) Home()               { c.SendGcode(HomeGcode) }
func (c *Client) SetBedTemp(t int)    { c.SendGcode(fmt.Sprintf("M140 S%d\n", t)) }
func (c *Client) SetNozzleTemp(t int) { c.SendGcode(fmt.Sprintf("M104 S%d\n", t)) }

// printCommandPayload builds a print-flow command (pause/resume/stop) —
// payload shape verified against ha-bambulab's pybambu commands.
func printCommandPayload(seq int64, command string) string {
	req := map[string]any{"print": map[string]any{
		"sequence_id": strconv.FormatInt(seq, 10),
		"command":     command,
	}}
	b, _ := json.Marshal(req)
	return string(b)
}

func (c *Client) sendPrintCommand(command string) {
	c.publish(printCommandPayload(c.seq.Add(1), command))
}

func (c *Client) PausePrint()  { c.sendPrintCommand("pause") }
func (c *Client) ResumePrint() { c.sendPrintCommand("resume") }
func (c *Client) StopPrint()   { c.sendPrintCommand("stop") }
