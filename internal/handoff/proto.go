package handoff

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
)

// Message types of the control protocol (one request/response at a time, so
// descriptors can never be attributed to the wrong frame).
const (
	MsgHello    = "hello"    // new -> old: I exist (pid, version, mode)
	MsgOffer    = "offer"    // old -> new: listener fd(s) attached (fd-pass mode)
	MsgPrepared = "prepared" // new -> old: I hold a listener and am initialised
	MsgState    = "state"    // old -> new: old stopped accepting; here is the state
	MsgReady    = "ready"    // new -> old: I am serving and passed self-check
	MsgAbort    = "abort"    // either side: give up
)

// State is the application state envelope transferred between processes.
type State struct {
	Count       uint64            `json:"count"`
	FromVersion string            `json:"from_version"`
	FromPID     int               `json:"from_pid"`
	SealedAtNs  int64             `json:"sealed_at_ns"`
	Generation  int               `json:"generation"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// Msg is one control frame.
type Msg struct {
	Type    string `json:"type"`
	PID     int    `json:"pid,omitempty"`
	Version string `json:"version,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Addr    string `json:"addr,omitempty"`
	NumFDs  int    `json:"num_fds,omitempty"`
	State   *State `json:"state,omitempty"`
	Err     string `json:"err,omitempty"`
}

const maxFrame = 1 << 20

// WriteMsg sends a length-prefixed JSON frame with optional SCM_RIGHTS files.
func WriteMsg(c *net.UnixConn, m Msg, files ...*os.File) error {
	m.NumFDs = len(files)
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	return SendFiles(c, frame, files...)
}

// ReadMsg reads one frame and any descriptors sent with it.
func ReadMsg(c *net.UnixConn) (Msg, []*os.File, error) {
	var m Msg
	var hdr [4]byte
	files, err := RecvFiles(c, hdr[:])
	if err != nil {
		closeAll(files)
		return m, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrame {
		closeAll(files)
		return m, nil, fmt.Errorf("handoff: bad frame length %d", n)
	}
	body := make([]byte, n)
	more, err := RecvFiles(c, body)
	files = append(files, more...)
	if err != nil {
		closeAll(files)
		return m, nil, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		closeAll(files)
		return m, nil, err
	}
	if len(files) != m.NumFDs {
		closeAll(files)
		return m, nil, fmt.Errorf("handoff: frame announced %d fds, got %d", m.NumFDs, len(files))
	}
	if m.Type == MsgAbort {
		closeAll(files)
		return m, nil, errors.New("handoff: peer aborted: " + m.Err)
	}
	return m, files, nil
}

// Expect reads a frame and checks its type.
func Expect(c *net.UnixConn, typ string) (Msg, []*os.File, error) {
	m, files, err := ReadMsg(c)
	if err != nil {
		return m, nil, err
	}
	if m.Type != typ {
		closeAll(files)
		return m, nil, fmt.Errorf("handoff: expected %q, got %q", typ, m.Type)
	}
	return m, files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
