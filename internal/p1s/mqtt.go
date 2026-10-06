package p1s

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/brhelwig/bambu-util/internal/activity"
)

const (
	// Z200 — 50mm above the bottom of the ~250mm travel; covers position
	// drift without homing (a blind Z250 once hit the bottom limit).
	BedDropGcode = "G90\nG1 Z200 F900\n"
	HomeGcode    = "G28\n"
)

// publisher is the part of the MQTT client this package sends through, kept
// narrow so tests can watch what goes out and at what delivery level.
type publisher interface {
	Publish(topic string, qos byte, retained bool, payload any) mqtt.Token
}

// subscriber is the part of the MQTT client the connect callback needs, kept
// narrow for the same reason.
type subscriber interface {
	Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token
}

// Client is the MQTT link to the printer: cached merged state plus commands.
type Client struct {
	serial string
	cache  *StateCache
	mqtt   mqtt.Client
	pub    publisher
	log    *activity.Log
	seq    atomic.Int64
}

func NewClient(ip, serial, accessCode string, cache *StateCache, log *activity.Log) *Client {
	c := &Client{serial: serial, cache: cache, log: log}
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
	opts.OnConnect = func(m mqtt.Client) { c.onConnect(m) }
	opts.OnConnectionLost = func(mqtt.Client, error) { c.onConnectionLost() }
	c.mqtt = mqtt.NewClient(opts)
	c.pub = c.mqtt
	return c
}

// onConnect subscribes to the printer's reports and asks for a full state dump,
// since the printer otherwise only sends changes.
func (c *Client) onConnect(sub subscriber) {
	c.cache.SetConnected(true)
	sub.Subscribe(fmt.Sprintf("device/%s/report", c.serial), 0, func(_ mqtt.Client, msg mqtt.Message) {
		c.log.Record(activity.Report, "report", string(msg.Payload()))
		HandleReport(c.cache, msg.Payload())
	})
	c.publish(`{"pushing":{"sequence_id":"0","command":"pushall"}}`)
}

func (c *Client) onConnectionLost() { c.cache.SetConnected(false) }

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

// MQTT QoS levels. Most commands use QoS 0, since they are either harmless to
// lose (a temperature) or unsafe to repeat (extrude). Pause, resume, stop and
// unload use QoS 1: losing a Stop is worse than sending it twice.
const (
	qosUnacknowledged = 0
	qosAcknowledged   = 1
)

func (c *Client) publish(payload string) {
	c.publishAt(qosUnacknowledged, payload)
}

func (c *Client) publishAt(qos byte, payload string) mqtt.Token {
	entry := c.log.Record(activity.Command, summarize(payload), payload)
	token := c.pub.Publish(fmt.Sprintf("device/%s/request", c.serial), qos, false, payload)
	// Record the acknowledgement in the background rather than blocking.
	go func() {
		if !token.WaitTimeout(ackTimeout) {
			c.log.Acknowledge(entry, time.Time{}, errNoAcknowledgement)
			return
		}
		c.log.Acknowledge(entry, time.Now(), token.Error())
	}()
	return token
}

// ackTimeout is how long to wait for the broker to confirm before recording
// that it didn't.
const ackTimeout = 30 * time.Second

var errNoAcknowledgement = errors.New("no acknowledgement")

// summarize names a command from its payload, so the log reads as a list of
// what was asked for rather than a wall of JSON.
func summarize(payload string) string {
	var msg struct {
		Print  map[string]any `json:"print"`
		System map[string]any `json:"system"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return "command"
	}
	for _, section := range []map[string]any{msg.Print, msg.System} {
		if name, ok := section["command"].(string); ok {
			if name == "gcode_line" {
				if line, ok := section["param"].(string); ok {
					return "gcode " + strings.TrimSpace(strings.ReplaceAll(line, "\n", " "))
				}
			}
			return name
		}
	}
	return "command"
}

// request builds a command payload: {namespace: {sequence_id, command, ...fields}}.
func request(seq int64, namespace, command string, fields map[string]any) string {
	body := map[string]any{"sequence_id": strconv.FormatInt(seq, 10), "command": command}
	for k, v := range fields {
		body[k] = v
	}
	b, _ := json.Marshal(map[string]any{namespace: body})
	return string(b)
}

func (c *Client) send(namespace, command string, fields map[string]any) {
	c.publish(request(c.seq.Add(1), namespace, command, fields))
}

func (c *Client) SendGcode(gcode string) {
	c.send("print", "gcode_line", map[string]any{"param": gcode})
}

func (c *Client) LowerBed()           { c.SendGcode(BedDropGcode) }
func (c *Client) Home()               { c.SendGcode(HomeGcode) }
func (c *Client) SetBedTemp(t int)    { c.SendGcode(fmt.Sprintf("M140 S%d\n", t)) }
func (c *Client) SetNozzleTemp(t int) { c.SendGcode(fmt.Sprintf("M104 S%d\n", t)) }

// Extrude pushes a short length of filament for manual purging / cold pulls.
// M83 = relative extrusion so the move is a fixed 20mm regardless of position;
// F150 (2.5 mm/s) is slow enough not to skip. Requires a hot nozzle — the
// caller guards on temperature.
func (c *Client) Extrude() { c.SendGcode("M83\nG1 E20 F150\n") }

// UnloadFilament ejects the currently loaded filament back to the AMS (or out
// the top for an external spool). Payload verified against Doridian/
// OpenBambuAPI mqtt.md; unverified against this specific printer.
func (c *Client) UnloadFilament() { c.sendPrintCommand("unload_filament") }

// SetAmsFilament writes the profile for one AMS tray: material type, colour
// (tray_color as RRGGBBAA hex), and the nozzle temperature range. trayInfoIdx
// is the printer's own filament-profile id — we round-trip whatever the last
// report carried so a colour edit doesn't clobber it. This is a full-tray write
// (ams_filament_setting), so every field is sent, not just the changed one.
// Payload from OpenBambuAPI mqtt.md; unverified against this printer.
func (c *Client) SetAmsFilament(amsID, trayID int, trayInfoIdx, color, trayType string, tempMin, tempMax int) {
	c.send("print", "ams_filament_setting", map[string]any{
		"ams_id":          amsID,
		"tray_id":         trayID,
		"tray_info_idx":   trayInfoIdx,
		"tray_color":      color,
		"nozzle_temp_min": tempMin,
		"nozzle_temp_max": tempMax,
		"tray_type":       trayType,
	})
}

// SetChamberLight turns the chamber LED on or off. "ledctrl" is a system-level
// command (not print); the timing fields only matter for flashing mode but are
// included to match the documented payload. Verified against OpenBambuAPI.
func (c *Client) SetChamberLight(on bool) {
	mode := "off"
	if on {
		mode = "on"
	}
	c.send("system", "ledctrl", map[string]any{
		"led_node":      "chamber_light",
		"led_mode":      mode,
		"led_on_time":   500,
		"led_off_time":  500,
		"loop_times":    1,
		"interval_time": 1000,
	})
}

// sendPrintCommand sends an acknowledged command in the "print" namespace
// (pause, resume, stop, unload_filament). Shape matches ha-bambulab's pybambu.
func (c *Client) sendPrintCommand(command string) {
	c.publishAt(qosAcknowledged, request(c.seq.Add(1), "print", command, nil))
}

func (c *Client) PausePrint()  { c.sendPrintCommand("pause") }
func (c *Client) ResumePrint() { c.sendPrintCommand("resume") }
func (c *Client) StopPrint()   { c.sendPrintCommand("stop") }
