package storage

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// fakeConn records the deadlines set on it; the rest of net.Conn is inert.
type fakeConn struct{ deadlines []time.Time }

func (c *fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (c *fakeConn) Write([]byte) (int, error)        { return 0, nil }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}

// TestDeadlineWriterRefreshesPerWrite locks the fix: each write pushes the
// connection's deadline to now+smbIdleTimeout, so a long steady transfer is
// never capped (only inactivity between chunks can trip it), while the data
// still passes through unchanged.
func TestDeadlineWriterRefreshesPerWrite(t *testing.T) {
	fc := &fakeConn{}
	var buf bytes.Buffer
	dw := &deadlineWriter{w: &buf, conn: fc}

	before := time.Now()
	if n, err := dw.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if buf.String() != "hello" {
		t.Fatalf("data not passed through: %q", buf.String())
	}
	if len(fc.deadlines) != 1 {
		t.Fatalf("expected 1 deadline set, got %d", len(fc.deadlines))
	}
	// The deadline is ~now+smbIdleTimeout, i.e. in the future by roughly the idle
	// window — never an absolute cap on the whole transfer.
	got := fc.deadlines[0]
	if got.Before(before.Add(smbIdleTimeout-time.Second)) || got.After(time.Now().Add(smbIdleTimeout+time.Second)) {
		t.Errorf("deadline %v not within ~now+%v", got, smbIdleTimeout)
	}

	// A second chunk refreshes the deadline again (bounds inactivity, not total
	// time), and must not move backwards.
	if _, err := dw.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	if len(fc.deadlines) != 2 {
		t.Fatalf("expected 2 deadline refreshes, got %d", len(fc.deadlines))
	}
	if fc.deadlines[1].Before(fc.deadlines[0]) {
		t.Error("deadline moved backwards across writes")
	}
}
