package p1s

import (
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/brhelwig/bambu-util/internal/activity"
)

func TestHandleReportMergesPrintFields(t *testing.T) {
	c := NewStateCache()
	HandleReport(c, []byte(`{"print":{"gcode_state":"RUNNING","bed_temper":55.5}}`))
	fields, _ := c.Snapshot()
	if fields["gcode_state"] != "RUNNING" || fields["bed_temper"] != 55.5 {
		t.Fatalf("merge failed: %v", fields)
	}
}

func TestHandleReportIgnoresGarbage(t *testing.T) {
	c := NewStateCache()
	HandleReport(c, []byte(`not json`))
	HandleReport(c, []byte(`{"system":{"x":1}}`))
	HandleReport(c, []byte(`{"print":"not an object"}`))
	if fields, _ := c.Snapshot(); len(fields) != 0 {
		t.Fatalf("cache should be empty, got %v", fields)
	}
}

func TestPrintCommandPayload(t *testing.T) {
	got := printCommandPayload(7, "pause")
	want := `{"print":{"command":"pause","sequence_id":"7"}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// fakeToken is the minimum of mqtt.Token needed to stand in for a publish
// result.
type fakeToken struct{ done chan struct{} }

func newFakeToken() *fakeToken {
	t := &fakeToken{done: make(chan struct{})}
	close(t.done)
	return t
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{}          { return t.done }
func (t *fakeToken) Error() error                   { return nil }

type sentMessage struct {
	topic   string
	qos     byte
	payload string
}

type fakePublisher struct {
	mu   sync.Mutex
	sent []sentMessage
}

func (p *fakePublisher) Publish(topic string, qos byte, _ bool, payload any) mqtt.Token {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, sentMessage{topic: topic, qos: qos, payload: payload.(string)})
	return newFakeToken()
}

func (p *fakePublisher) last() sentMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[len(p.sent)-1]
}

func testClient() (*Client, *fakePublisher) {
	pub := &fakePublisher{}
	return &Client{serial: "SERIAL123", cache: NewStateCache(), pub: pub}, pub
}

// Losing one of these is worse than repeating it, so they must be acknowledged
// — at level 0 the broker never confirms and a dropped Stop is silent.
func TestCommandsWorthRepeatingAreAcknowledged(t *testing.T) {
	cases := map[string]func(*Client){
		"pause":  (*Client).PausePrint,
		"resume": (*Client).ResumePrint,
		"stop":   (*Client).StopPrint,
		"unload": (*Client).UnloadFilament,
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			c, pub := testClient()
			send(c)
			got := pub.last()
			if got.qos != qosAcknowledged {
				t.Errorf("%s went out at level %d, want %d", name, got.qos, qosAcknowledged)
			}
			if want := "device/SERIAL123/request"; got.topic != want {
				t.Errorf("topic = %q, want %q", got.topic, want)
			}
		})
	}
}

// Repeating these is the worse outcome: an extrude run twice pushes twice the
// filament, and a temperature is corrected by the next status report anyway.
func TestCommandsUnsafeToRepeatAreNotAcknowledged(t *testing.T) {
	cases := map[string]func(*Client){
		"extrude":     (*Client).Extrude,
		"home":        (*Client).Home,
		"lower bed":   (*Client).LowerBed,
		"bed temp":    func(c *Client) { c.SetBedTemp(60) },
		"nozzle temp": func(c *Client) { c.SetNozzleTemp(220) },
		"lamp":        func(c *Client) { c.SetChamberLight(true) },
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			c, pub := testClient()
			send(c)
			if got := pub.last(); got.qos != qosUnacknowledged {
				t.Errorf("%s went out at level %d, want %d", name, got.qos, qosUnacknowledged)
			}
		})
	}
}

func TestEachCommandCarriesItsOwnSequenceNumber(t *testing.T) {
	c, pub := testClient()
	c.PausePrint()
	c.ResumePrint()
	c.StopPrint()
	seen := map[string]bool{}
	for _, m := range pub.sent {
		if seen[m.payload] {
			t.Errorf("two commands went out identical: %s", m.payload)
		}
		seen[m.payload] = true
	}
	if len(pub.sent) != 3 {
		t.Errorf("sent %d messages, want 3", len(pub.sent))
	}
}

func TestACommandIsLoggedAndThenAcknowledged(t *testing.T) {
	log := openTestLog()
	c, _ := testClient()
	c.log = log
	c.StopPrint()

	entries := log.Entries(100)
	if len(entries) != 1 {
		t.Fatalf("logged %d entries, want 1", len(entries))
	}
	if entries[0].Kind != activity.Command || entries[0].Summary != "stop" {
		t.Errorf("logged %s %q, want a command named stop", entries[0].Kind, entries[0].Summary)
	}
	if !strings.Contains(entries[0].Payload, `"command":"stop"`) {
		t.Errorf("payload = %q, want the raw message", entries[0].Payload)
	}

	// The acknowledgement arrives on its own goroutine, so the entry fills in
	// behind the call rather than the call waiting for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := log.Entries(100)[0]; got.Acked != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the command was never marked acknowledged")
}

// The log should read as a list of what was asked for, not a wall of JSON.
func TestCommandsAreNamedInTheLog(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"print":{"command":"pause"}}`, "pause"},
		{`{"system":{"command":"ledctrl"}}`, "ledctrl"},
		{`{"print":{"command":"gcode_line","param":"G28\n"}}`, "gcode G28"},
		{`not json`, "command"},
		{`{"print":{}}`, "command"},
	} {
		if got := summarize(tc.payload); got != tc.want {
			t.Errorf("summarize(%s) = %q, want %q", tc.payload, got, tc.want)
		}
	}
}

func TestAPrinterReportIsLogged(t *testing.T) {
	log := openTestLog()
	cache := NewStateCache()
	payload := `{"print":{"gcode_state":"RUNNING"}}`
	log.Record(activity.Report, "report", payload)
	HandleReport(cache, []byte(payload))

	entries := log.Entries(100)
	if len(entries) != 1 || entries[0].Kind != activity.Report {
		t.Fatalf("entries = %+v, want one report", entries)
	}
	if entries[0].Payload != payload {
		t.Errorf("payload = %q, want it kept raw for reading", entries[0].Payload)
	}
}

// These three payloads were taken from third-party documentation and have never
// been confirmed against this printer, so what goes on the wire is pinned here:
// checking only the delivery level would let a wrong field name through.
func TestDocumentedPayloadsGoOutAsWritten(t *testing.T) {
	cases := []struct {
		name string
		send func(*Client)
		want string
	}{
		{
			"unload",
			(*Client).UnloadFilament,
			`{"print":{"command":"unload_filament","sequence_id":"1"}}`,
		},
		{
			"lamp on",
			func(c *Client) { c.SetChamberLight(true) },
			`{"system":{"command":"ledctrl","interval_time":1000,"led_mode":"on","led_node":"chamber_light","led_off_time":500,"led_on_time":500,"loop_times":1,"sequence_id":"1"}}`,
		},
		{
			"lamp off",
			func(c *Client) { c.SetChamberLight(false) },
			`{"system":{"command":"ledctrl","interval_time":1000,"led_mode":"off","led_node":"chamber_light","led_off_time":500,"led_on_time":500,"loop_times":1,"sequence_id":"1"}}`,
		},
		{
			"ams tray",
			func(c *Client) { c.SetAmsFilament(0, 1, "GFA00", "FF6B35FF", "PLA", 190, 230) },
			`{"print":{"ams_id":0,"command":"ams_filament_setting","nozzle_temp_max":230,"nozzle_temp_min":190,"sequence_id":"1","tray_color":"FF6B35FF","tray_id":1,"tray_info_idx":"GFA00","tray_type":"PLA"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, pub := testClient()
			c.send(client)
			if got := pub.last().payload; got != c.want {
				t.Errorf("sent\n %s\nwant\n %s", got, c.want)
			}
		})
	}
}

// fakeSubscriber records what the connect callback subscribed to and keeps the
// handler so a report can be delivered through it.
type fakeSubscriber struct {
	topic   string
	qos     byte
	handler mqtt.MessageHandler
}

func (s *fakeSubscriber) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	s.topic, s.qos, s.handler = topic, qos, callback
	return newFakeToken()
}

// fakeMessage is the minimum of mqtt.Message needed to carry a payload.
type fakeMessage struct{ payload []byte }

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 0 }
func (m *fakeMessage) Retained() bool    { return false }
func (m *fakeMessage) Topic() string     { return "" }
func (m *fakeMessage) MessageID() uint16 { return 0 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              {}

// A fresh connection knows nothing until the printer reports, and the printer
// only reports what changes — so connecting has to ask for everything.
func TestConnectingSubscribesAndAsksForEverything(t *testing.T) {
	c, pub := testClient()
	sub := &fakeSubscriber{}

	c.onConnect(sub)

	if want := "device/SERIAL123/report"; sub.topic != want {
		t.Errorf("subscribed to %q, want %q", sub.topic, want)
	}
	if _, connected := c.cache.Snapshot(); !connected {
		t.Error("connecting left the cache disconnected")
	}
	want := `{"pushing":{"sequence_id":"0","command":"pushall"}}`
	if got := pub.last().payload; got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}

func TestAReportOnTheSubscriptionReachesTheCache(t *testing.T) {
	c, _ := testClient()
	c.log = openTestLog()
	sub := &fakeSubscriber{}
	c.onConnect(sub)

	sub.handler(nil, &fakeMessage{payload: []byte(`{"print":{"gcode_state":"RUNNING"}}`)})

	fields, _ := c.cache.Snapshot()
	if fields["gcode_state"] != "RUNNING" {
		t.Errorf("cache = %v, want the report merged in", fields)
	}
	var reports int
	for _, e := range c.log.Entries(100) {
		if e.Kind == activity.Report {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("logged %d reports, want 1", reports)
	}
}

func TestLosingTheConnectionClearsTheFlag(t *testing.T) {
	c, _ := testClient()
	c.onConnect(&fakeSubscriber{})

	c.onConnectionLost()

	if _, connected := c.cache.Snapshot(); connected {
		t.Error("cache still reports connected after the connection dropped")
	}
}
