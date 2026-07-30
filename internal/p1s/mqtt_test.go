package p1s

import (
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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
