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
	BedDropGcode = "G90\nG1 Z200 F900\n"
	HomeGcode    = "G28\n"
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

// Extrude pushes a short length of filament for manual purging / cold pulls.
// M83 = relative extrusion so the move is a fixed 20mm regardless of position;
// F150 (2.5 mm/s) is slow enough not to skip. Requires a hot nozzle — the
// caller guards on temperature.
func (c *Client) Extrude() { c.SendGcode("M83\nG1 E20 F150\n") }

// UnloadFilament ejects the currently loaded filament back to the AMS (or out
// the top for an external spool). Payload verified against Doridian/
// OpenBambuAPI mqtt.md; unverified against this specific printer.
func (c *Client) UnloadFilament() { c.sendPrintCommand("unload_filament") }

// LoadFilament feeds the given AMS tray (0-3) into the hotend, heating to
// tarTemp. currTemp is the current nozzle temperature. "ams_change_filament"
// payload verified against OpenBambuAPI mqtt.md; unverified against this
// printer.
func (c *Client) LoadFilament(slot, currTemp, tarTemp int) {
	req := map[string]any{"print": map[string]any{
		"sequence_id": strconv.FormatInt(c.seq.Add(1), 10),
		"command":     "ams_change_filament",
		"target":      slot,
		"curr_temp":   currTemp,
		"tar_temp":    tarTemp,
	}}
	b, _ := json.Marshal(req)
	c.publish(string(b))
}

// SetAmsFilament writes the profile for one AMS tray: material type, colour
// (tray_color as RRGGBBAA hex), and the nozzle temperature range. trayInfoIdx
// is the printer's own filament-profile id — we round-trip whatever the last
// report carried so a colour edit doesn't clobber it. This is a full-tray write
// (ams_filament_setting), so every field is sent, not just the changed one.
// Payload from OpenBambuAPI mqtt.md; unverified against this printer.
func (c *Client) SetAmsFilament(amsID, trayID int, trayInfoIdx, color, trayType string, tempMin, tempMax int) {
	req := map[string]any{"print": map[string]any{
		"sequence_id":     strconv.FormatInt(c.seq.Add(1), 10),
		"command":         "ams_filament_setting",
		"ams_id":          amsID,
		"tray_id":         trayID,
		"tray_info_idx":   trayInfoIdx,
		"tray_color":      color,
		"nozzle_temp_min": tempMin,
		"nozzle_temp_max": tempMax,
		"tray_type":       trayType,
	}}
	b, _ := json.Marshal(req)
	c.publish(string(b))
}

// SetChamberLight turns the chamber LED on or off. "ledctrl" is a system-level
// command (not print); the timing fields only matter for flashing mode but are
// included to match the documented payload. Verified against OpenBambuAPI.
func (c *Client) SetChamberLight(on bool) {
	mode := "off"
	if on {
		mode = "on"
	}
	req := map[string]any{"system": map[string]any{
		"sequence_id":   strconv.FormatInt(c.seq.Add(1), 10),
		"command":       "ledctrl",
		"led_node":      "chamber_light",
		"led_mode":      mode,
		"led_on_time":   500,
		"led_off_time":  500,
		"loop_times":    1,
		"interval_time": 1000,
	}}
	b, _ := json.Marshal(req)
	c.publish(string(b))
}

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
