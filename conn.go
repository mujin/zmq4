// Copyright 2018 The go-zeromq Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zmq4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosedConn = errors.New("zmq4: read/write on closed connection")

// Conn implements the ZeroMQ Message Transport Protocol as defined
// in https://rfc.zeromq.org/spec:23/ZMTP/.
type Conn struct {
	typ    SocketType
	id     SocketIdentity
	rw     net.Conn
	sec    Security
	Server bool
	Meta   Metadata
	Peer   struct {
		Server bool
		Meta   Metadata
	}

	mu     sync.RWMutex
	topics map[string]struct{} // set of subscribed topics

	closed         int32
	onCloseErrorCB func(c *Conn)
}

func (c *Conn) Close() error {
	return c.rw.Close()
}

// Open opens a ZMTP connection over rw with the given security, socket type and identity.
// An optional onCloseErrorCB can be provided to inform the caller when this Conn is closed.
// Open performs a complete ZMTP handshake.
func Open(ctx context.Context, rw net.Conn, sec Security, sockType SocketType, sockID SocketIdentity, server bool, onCloseErrorCB func(c *Conn)) (*Conn, error) {
	if rw == nil {
		return nil, fmt.Errorf("zmq4: invalid nil read-writer")
	}

	if sec == nil {
		return nil, fmt.Errorf("zmq4: invalid nil security")
	}

	conn := &Conn{
		typ:            sockType,
		id:             sockID,
		rw:             rw,
		sec:            sec,
		Server:         server,
		Meta:           make(Metadata),
		topics:         make(map[string]struct{}),
		onCloseErrorCB: onCloseErrorCB,
	}
	conn.Meta[sysSockType] = string(conn.typ)
	conn.Meta[sysSockID] = conn.id.String()
	conn.Peer.Meta = make(Metadata)

	err := conn.init(ctx, sec)
	if err != nil {
		return nil, fmt.Errorf("zmq4: could not initialize ZMTP connection: %w", err)
	}

	return conn, nil
}

// init performs a ZMTP handshake over an io.ReadWriter
func (conn *Conn) init(ctx context.Context, sec Security) error {
	var err error

	err = conn.greet(ctx, conn.Server)
	if err != nil {
		return fmt.Errorf("zmq4: could not exchange greetings: %w", err)
	}

	err = conn.sec.Handshake(ctx, conn, conn.Server)
	if err != nil {
		return fmt.Errorf("zmq4: could not perform security handshake: %w", err)
	}

	peer := SocketType(conn.Peer.Meta[sysSockType])
	if !peer.IsCompatible(conn.typ) {
		return fmt.Errorf("zmq4: peer=%q not compatible with %q", peer, conn.typ)
	}

	// FIXME(sbinet): if security mechanism does not define a client/server
	// topology, enforce that p.server == p.peer.server == 0
	// as per:
	//  https://rfc.zeromq.org/spec:23/ZMTP/#topology

	return nil
}

func (conn *Conn) greet(ctx context.Context, server bool) error {
	var err error
	send := greeting{Version: defaultVersion}
	send.Sig.Header = sigHeader
	send.Sig.Footer = sigFooter
	kind := string(conn.sec.Type())
	if len(kind) > len(send.Mechanism) {
		return errSecMech
	}
	copy(send.Mechanism[:], kind)

	cleanUp, err := setDeadlineAndWatchForCancellation(ctx, conn.rw)
	if err != nil {
		conn.checkIO(err)
		return fmt.Errorf("zmq4: could not set deadline on connection: %w", err)
	}
	defer cleanUp()

	err = send.write(conn.rw)
	if err != nil {
		conn.checkIO(err)
		return fmt.Errorf("zmq4: could not send greeting: %w", err)
	}

	var recv greeting
	err = recv.read(conn.rw)
	if err != nil {
		conn.checkIO(err)
		return fmt.Errorf("zmq4: could not recv greeting: %w", err)
	}

	peerKind := asString(recv.Mechanism[:])
	if peerKind != kind {
		return errBadSec
	}

	conn.Peer.Server, err = asBool(recv.Server)
	if err != nil {
		return fmt.Errorf("zmq4: could not get peer server flag: %w", err)
	}

	return nil
}

// SendCmd sends a ZMTP command over the wire.
func (c *Conn) SendCmd(ctx context.Context, name string, body []byte) error {
	if c.Closed() {
		return ErrClosedConn
	}
	cmd := Cmd{Name: name, Body: body}
	buf, err := cmd.marshalZMTP()
	if err != nil {
		return err
	}
	return c.send(ctx, true, buf, 0)
}

// SendMsg sends a ZMTP message over the wire.
func (c *Conn) SendMsg(ctx context.Context, msg Msg) error {
	if c.Closed() {
		return ErrClosedConn
	}
	if msg.multipart {
		return c.sendMulti(ctx, msg)
	}

	nframes := len(msg.Frames)
	for i, frame := range msg.Frames {
		var flag byte
		if i < nframes-1 {
			flag ^= hasMoreBitFlag
		}
		err := c.send(ctx, false, frame, flag)
		if err != nil {
			return fmt.Errorf("zmq4: error sending frame %d/%d: %w", i+1, nframes, err)
		}
	}
	return nil
}

// RecvMsg receives a ZMTP message from the wire.
func (c *Conn) RecvMsg(ctx context.Context) (Msg, error) {
	if c.Closed() {
		return Msg{}, ErrClosedConn
	}
	msg := c.read(ctx)
	if msg.err != nil {
		return msg, fmt.Errorf("zmq4: could not read recv msg: %w", msg.err)
	}

	if !msg.isCmd() {
		return msg, nil
	}

	switch len(msg.Frames) {
	case 0:
		msg.err = fmt.Errorf("zmq4: empty command")
		return msg, msg.err
	case 1:
		// ok
	default:
		msg.err = fmt.Errorf("zmq4: invalid length command")
		return msg, msg.err
	}

	var cmd Cmd
	msg.err = cmd.unmarshalZMTP(msg.Frames[0])
	if msg.err != nil {
		return msg, fmt.Errorf("zmq4: could not unmarshal ZMTP recv msg: %w", msg.err)
	}

	switch cmd.Name {
	case CmdPing:
		// send back a PONG immediately.
		msg.err = c.SendCmd(ctx, CmdPong, nil)
		if msg.err != nil {
			return msg, msg.err
		}
	}

	switch len(cmd.Body) {
	case 0:
		msg.Frames = nil
	default:
		msg.Frames = msg.Frames[:1]
		msg.Frames[0] = cmd.Body
	}
	return msg, nil
}

func (c *Conn) RecvCmd(ctx context.Context) (Cmd, error) {
	var cmd Cmd

	if c.Closed() {
		return cmd, ErrClosedConn
	}

	msg := c.read(ctx)
	if msg.err != nil {
		return cmd, fmt.Errorf("zmq4: could not read recv cmd: %w", msg.err)
	}

	if !msg.isCmd() {
		return cmd, ErrBadFrame
	}

	switch len(msg.Frames) {
	case 0:
		msg.err = fmt.Errorf("zmq4: empty command")
		return cmd, msg.err
	case 1:
		// ok
	default:
		msg.err = fmt.Errorf("zmq4: invalid length command")
		return cmd, msg.err
	}

	err := cmd.unmarshalZMTP(msg.Frames[0])
	if err != nil {
		return cmd, fmt.Errorf("zmq4: could not unmarshal ZMTP recv cmd: %w", err)
	}

	return cmd, nil
}

func (c *Conn) sendMulti(ctx context.Context, msg Msg) error {
	var buffers net.Buffers

	nframes := len(msg.Frames)
	for i, frame := range msg.Frames {
		var flag byte
		if i < nframes-1 {
			flag ^= hasMoreBitFlag
		}

		size := len(frame)
		isLong := size > 255
		if isLong {
			flag ^= isLongBitFlag
		}

		var (
			hdr = [8 + 1]byte{flag}
			hsz int
		)
		if isLong {
			hsz = 9
			binary.BigEndian.PutUint64(hdr[1:], uint64(size))
		} else {
			hsz = 2
			hdr[1] = uint8(size)
		}

		switch c.sec.Type() {
		case NullSecurity:
			buffers = append(buffers, hdr[:hsz], frame)
		default:
			var secBuf bytes.Buffer
			if _, err := c.sec.Encrypt(&secBuf, frame); err != nil {
				return err
			}
			buffers = append(buffers, hdr[:hsz], secBuf.Bytes())
		}
	}

	cleanUp, err := setDeadlineAndWatchForCancellation(ctx, c.rw)
	if err != nil {
		c.checkIO(err)
		return fmt.Errorf("zmq4: could not set deadline on connection: %w", err)
	}
	defer cleanUp()

	if _, err := buffers.WriteTo(c.rw); err != nil {
		c.checkIO(err)
		return err
	}

	return nil
}

func (c *Conn) send(ctx context.Context, isCommand bool, body []byte, flag byte) error {
	cleanUp, err := setDeadlineAndWatchForCancellation(ctx, c.rw)
	if err != nil {
		c.checkIO(err)
		return fmt.Errorf("zmq4: could not set deadline on connection: %w", err)
	}
	defer cleanUp()

	// Long flag
	size := len(body)
	isLong := size > 255
	if isLong {
		flag ^= isLongBitFlag
	}

	if isCommand {
		flag ^= isCommandBitFlag
	}

	var (
		hdr = [8 + 1]byte{flag}
		hsz int
	)

	// Write out the message itself
	if isLong {
		hsz = 9
		binary.BigEndian.PutUint64(hdr[1:], uint64(size))
	} else {
		hsz = 2
		hdr[1] = uint8(size)
	}
	if _, err := c.rw.Write(hdr[:hsz]); err != nil {
		c.checkIO(err)
		return err
	}

	if _, err := c.sec.Encrypt(c.rw, body); err != nil {
		c.checkIO(err)
		return err
	}

	return nil
}

// read returns the isCommand flag, the body of the message, and optionally an error
func (c *Conn) read(ctx context.Context) Msg {
	var (
		header  [2]byte
		longHdr [8]byte
		msg     Msg

		hasMore = true
		isCmd   = false
	)

	cleanUp, err := setDeadlineAndWatchForCancellation(ctx, c.rw)
	if err != nil {
		c.checkIO(err)
		return msg
	}
	defer cleanUp()

	for hasMore {

		// Read out the header
		_, msg.err = io.ReadFull(c.rw, header[:])
		if msg.err != nil {
			c.checkIO(msg.err)
			return msg
		}

		fl := flag(header[0])

		hasMore = fl.hasMore()
		isCmd = isCmd || fl.isCommand()

		// Determine the actual length of the body
		size := uint64(header[1])
		if fl.isLong() {
			// We read 2 bytes of the header already
			// In case of a long message, the length is bytes 2-8 of the header
			// We already have the first byte, so assign it, and then read the rest
			longHdr[0] = header[1]

			_, msg.err = io.ReadFull(c.rw, longHdr[1:])
			if msg.err != nil {
				c.checkIO(msg.err)
				return msg
			}

			size = binary.BigEndian.Uint64(longHdr[:])
		}

		if size > uint64(maxInt64) {
			msg.err = errOverflow
			return msg
		}

		body := make([]byte, size)
		_, msg.err = io.ReadFull(c.rw, body)
		if msg.err != nil {
			c.checkIO(msg.err)
			return msg
		}

		// fast path for NULL security: we bypass the bytes.Buffer allocation.
		switch c.sec.Type() {
		case NullSecurity: // FIXME(sbinet): also do that for non-encrypted PLAIN?
			msg.Frames = append(msg.Frames, body)
			continue
		}

		buf := new(bytes.Buffer)
		if _, msg.err = c.sec.Decrypt(buf, body); msg.err != nil {
			return msg
		}
		msg.Frames = append(msg.Frames, buf.Bytes())
	}
	if isCmd {
		msg.Type = CmdMsg
	}
	return msg
}

func (conn *Conn) subscribe(msg Msg) {
	conn.mu.Lock()
	v := msg.Frames[0]
	k := string(v[1:])
	switch v[0] {
	case 0:
		delete(conn.topics, k)
	case 1:
		conn.topics[k] = struct{}{}
	}
	conn.mu.Unlock()
}

func (conn *Conn) subscribed(topic string) bool {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	for k := range conn.topics {
		switch {
		case k == "":
			// subscribed to everything
			return true
		case strings.HasPrefix(topic, k):
			return true
		}
	}
	return false
}

func (conn *Conn) SetClosed() {
	if wasClosed := atomic.CompareAndSwapInt32(&conn.closed, 0, 1); wasClosed {
		conn.notifyOnCloseError()
	}
}

func (conn *Conn) Closed() bool {
	return atomic.LoadInt32(&conn.closed) == 1
}

func (conn *Conn) checkIO(err error) {
	if err == nil {
		return
	}

	if err == io.EOF || errors.Is(err, io.EOF) {
		conn.SetClosed()
		return
	}

	var e net.Error
	if errors.As(err, &e); e != nil && !e.Timeout() {
		conn.SetClosed()
	}
}

func (conn *Conn) notifyOnCloseError() {
	if conn.onCloseErrorCB == nil {
		return
	}
	conn.onCloseErrorCB(conn)
}

func setDeadlineAndWatchForCancellation(ctx context.Context, conn net.Conn) (func(), error) {
	// set deadlines
	var deadlineSet bool
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		deadlineSet = true
	}

	// also watch for cancellation
	done := make(chan struct{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	// return a clean up function
	return func() {
		close(done)
		waitGroup.Wait()

		// unset the deadline if we set one
		if deadlineSet {
			conn.SetDeadline(time.Time{})
		}
	}, nil
}
