package handoff

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// SendFiles writes data on a unix socket with the given files attached as
// SCM_RIGHTS ancillary data (the mechanism used by Envoy hot restart and by
// Meta's socket takeover). The kernel installs duplicates of the descriptors
// in the receiving process; the sender keeps its own copies.
func SendFiles(c *net.UnixConn, data []byte, files ...*os.File) error {
	if len(data) == 0 {
		return errors.New("handoff: SendFiles needs at least one data byte")
	}
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = syscall.UnixRights(fds...)
	}
	n, oobn, err := c.WriteMsgUnix(data, oob, nil)
	if err != nil {
		return err
	}
	if oobn != len(oob) {
		return fmt.Errorf("handoff: short oob write %d/%d", oobn, len(oob))
	}
	// Ancillary data rides on the first byte; finish any remaining payload.
	for n < len(data) {
		m, err := c.Write(data[n:])
		if err != nil {
			return err
		}
		n += m
	}
	return nil
}

// recvChunk reads up to len(buf) bytes and returns any SCM_RIGHTS descriptors
// that arrived with them, wrapped as *os.File.
func recvChunk(c *net.UnixConn, buf []byte) (int, []*os.File, error) {
	oob := make([]byte, syscall.CmsgSpace(4*maxFDsPerMsg))
	n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return n, nil, err
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return n, nil, errors.New("handoff: control message truncated (too many fds)")
	}
	if oobn == 0 {
		return n, nil, nil
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return n, nil, err
	}
	var files []*os.File
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return n, files, err
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), fmt.Sprintf("handoff-fd-%d", fd)))
		}
	}
	return n, files, nil
}

const maxFDsPerMsg = 16

// RecvFiles reads exactly len(buf) bytes, collecting every descriptor that
// arrives along the way.
func RecvFiles(c *net.UnixConn, buf []byte) ([]*os.File, error) {
	var all []*os.File
	got := 0
	for got < len(buf) {
		n, files, err := recvChunk(c, buf[got:])
		all = append(all, files...)
		if err != nil {
			return all, err
		}
		if n == 0 {
			return all, errors.New("handoff: unexpected EOF")
		}
		got += n
	}
	return all, nil
}

// ListenerFile returns a duplicate *os.File of a TCP listener's socket.
func ListenerFile(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("handoff: listener %T is not *net.TCPListener", ln)
	}
	return tl.File()
}
