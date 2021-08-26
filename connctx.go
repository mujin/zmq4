// Copyright 2018 The go-zeromq Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zmq4

import (
	"context"
	"sync"
	"time"
)

type setDeadlineCloser interface {
	SetDeadline(time.Time) error
	Close() error
}

func setDeadlineAndCloseOnCancel(ctx context.Context, conn setDeadlineCloser) func() {
	// set deadlines
	var deadlineSet bool
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err == nil {
			deadlineSet = true
		}

	}

	// also watch for cancel
	done := make(chan struct{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		select {
		case <-ctx.Done():
			switch ctx.Err() {
			case context.Canceled:
				conn.Close()
			case context.DeadlineExceeded:
				if !deadlineSet {
					conn.Close()
				}
			}
		case <-done:
		}
	}()

	// return a clean up function
	return func() {
		// signal the goroutine to quit
		close(done)

		// wait for it to quit
		waitGroup.Wait()

		// unset the deadline if we set one
		if deadlineSet {
			_ = conn.SetDeadline(time.Time{})
		}
	}
}
