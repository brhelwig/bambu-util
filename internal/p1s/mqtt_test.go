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
	log := activity.New(20)
	c, _ := testClient()
	c.log = log
	c.StopPrint()

	entries := log.Entries()
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
		if got := log.Entries()[0]; got.Acked != nil {
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
	log := activity.New(20)
	cache := NewStateCache()
	payload := `{"print":{"gcode_state":"RUNNING"}}`
	log.Record(activity.Report, "report", payload)
	HandleReport(cache, []byte(payload))

	entries := log.Entries()
	if len(entries) != 1 || entries[0].Kind != activity.Report {
		t.Fatalf("entries = %+v, want one report", entries)
	}
	if entries[0].Payload != payload {
		t.Errorf("payload = %q, want it kept raw for reading", entries[0].Payload)
	}
}
