// Copyright 2018 The go-zeromq Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zmq4

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type testConn struct {
	close            chan struct{}
	allowSetDeadline bool
	deadline         time.Time
}

func newTestConn(allowSetDeadline bool) *testConn {
	return &testConn{
		close:            make(chan struct{}),
		allowSetDeadline: allowSetDeadline,
	}
}

func (self *testConn) Close() error {
	close(self.close)
	return nil
}

func (self *testConn) SetDeadline(deadline time.Time) error {
	if !self.allowSetDeadline {
		return fmt.Errorf("setting deadline is not allowed")
	}
	self.deadline = deadline
	return nil
}

func TestCancel(t *testing.T) {
	conn := newTestConn(true)
	ctx, cancel := context.WithCancel(context.Background())
	cleanUp := setDeadlineAndCloseOnCancel(ctx, conn)

	if !conn.deadline.IsZero() {
		t.Fatalf("deadline should not have been set: %v", conn.deadline)
	}

	cancel()

	select {
	case <-conn.close:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("cancel did not close the conection in time")
	}

	cleanUp()
}

func TestDeadline(t *testing.T) {
	conn := newTestConn(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	cleanUp := setDeadlineAndCloseOnCancel(ctx, conn)

	if conn.deadline.IsZero() {
		t.Fatalf("deadline should have been set")
	}

	select {
	case <-conn.close:
		t.Fatalf("should have let deadline close the conection")
	case <-time.After(250 * time.Millisecond):
	}

	cleanUp()
	cancel()
}

func TestFakeDeadline(t *testing.T) {
	conn := newTestConn(false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	cleanUp := setDeadlineAndCloseOnCancel(ctx, conn)

	if !conn.deadline.IsZero() {
		t.Fatalf("deadline should not have been allowed: %v", conn.deadline)
	}

	select {
	case <-conn.close:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("daedline did not close the conection in time")
	}

	cleanUp()
	cancel()
}
