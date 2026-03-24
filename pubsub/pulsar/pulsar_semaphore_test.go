/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pulsar

// Tests for the async semaphore concurrency limit in listenMessage.
//
// The bug: in async mode every message spawned a goroutine with NO upper bound.
// MaxConcurrentHandlers (default 100) only controlled the channel buffer size —
// NOT the number of concurrent goroutines. This caused tens of thousands of
// unacked messages to accumulate.
//
// Fix: a semaphore channel of size MaxConcurrentHandlers is created inside
// listenMessage. The loop blocks on semaphore acquisition before spawning each
// async goroutine, and releases the slot when the goroutine finishes.
//
// Shared mock types (mockMessage, ackTrackingConsumer, sendMsg) are defined in
// process_mode_test.go.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pulsarclient "github.com/apache/pulsar-client-go/pulsar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dapr/components-contrib/pubsub"
	"github.com/dapr/kit/logger"
)

// newTestPulsarWithConcurrency builds a minimal *Pulsar with controlled
// MaxConcurrentHandlers for semaphore tests.
func newTestPulsarWithConcurrency(maxConcurrent uint) *Pulsar {
	return &Pulsar{
		logger:  logger.NewLogger("test"),
		closeCh: make(chan struct{}),
		metadata: pulsarMetadata{
			MaxConcurrentHandlers: maxConcurrent,
		},
	}
}

// makeConsumerMessagesN creates n ConsumerMessages using the provided consumer.
func makeConsumerMessagesN(consumer pulsarclient.Consumer, n int) []pulsarclient.ConsumerMessage {
	msgs := make([]pulsarclient.ConsumerMessage, n)
	for i := range msgs {
		msgs[i] = pulsarclient.ConsumerMessage{
			Consumer: consumer,
			Message: &mockMessage{
				payload:    []byte("hello"),
				properties: map[string]string{},
				topic:      "persistent://public/default/test-topic",
			},
		}
	}
	return msgs
}

// TestListenMessage_AsyncConcurrencyLimit verifies that in async mode,
// the number of concurrently executing handlers never exceeds MaxConcurrentHandlers.
// This is the primary regression test for the unbounded goroutine bug.
func TestListenMessage_AsyncConcurrencyLimit(t *testing.T) {
	tt := []struct {
		name              string
		maxConcurrent     uint
		messagesToSend    int
		handlerSleepDelay time.Duration
	}{
		{
			name:              "limit 1 with 10 messages",
			maxConcurrent:     1,
			messagesToSend:    10,
			handlerSleepDelay: 20 * time.Millisecond,
		},
		{
			name:              "limit 5 with 50 messages",
			maxConcurrent:     5,
			messagesToSend:    50,
			handlerSleepDelay: 50 * time.Millisecond,
		},
		{
			name:              "limit 10 with 30 messages",
			maxConcurrent:     10,
			messagesToSend:    30,
			handlerSleepDelay: 20 * time.Millisecond,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPulsarWithConcurrency(tc.maxConcurrent)

			consumer := newMockConsumer(tc.messagesToSend)

			var (
				currentConcurrency atomic.Int64
				peakConcurrency    atomic.Int64
				processed          atomic.Int64
			)

			handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
				current := currentConcurrency.Add(1)
				defer currentConcurrency.Add(-1)

				// Atomically track peak concurrency.
				for {
					peak := peakConcurrency.Load()
					if current <= peak || peakConcurrency.CompareAndSwap(peak, current) {
						break
					}
				}

				time.Sleep(tc.handlerSleepDelay)
				processed.Add(1)
				return nil
			}

			// Pre-fill the consumer channel with all messages before starting.
			msgs := makeConsumerMessagesN(consumer, tc.messagesToSend)
			for _, msg := range msgs {
				consumer.Ch <- msg
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := pubsub.SubscribeRequest{
				Topic:    "test-topic",
				Metadata: map[string]string{processModeKey: processModeAsync},
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				p.listenMessage(ctx, req, consumer, handler)
			}()

			// Wait until all messages are processed.
			require.Eventually(t, func() bool {
				return processed.Load() == int64(tc.messagesToSend)
			}, 30*time.Second, 5*time.Millisecond,
				"not all messages were processed within timeout")

			cancel()
			<-done
			p.wg.Wait()

			assert.LessOrEqual(t, peakConcurrency.Load(), int64(tc.maxConcurrent),
				"peak concurrent handlers exceeded MaxConcurrentHandlers=%d", tc.maxConcurrent)
			assert.Equal(t, int64(tc.messagesToSend), processed.Load(),
				"all messages should have been processed")
		})
	}
}

// TestListenMessage_AsyncAllMessagesProcessed ensures no messages are dropped
// even under concurrency limiting with a fast handler.
func TestListenMessage_AsyncAllMessagesProcessed(t *testing.T) {
	const (
		maxConcurrent  = 5
		messagesToSend = 100
	)

	p := newTestPulsarWithConcurrency(maxConcurrent)
	consumer := newMockConsumer(messagesToSend)

	msgs := makeConsumerMessagesN(consumer, messagesToSend)
	for _, msg := range msgs {
		consumer.Ch <- msg
	}

	var processed atomic.Int64
	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		processed.Add(1)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	require.Eventually(t, func() bool {
		return processed.Load() == messagesToSend
	}, 10*time.Second, 5*time.Millisecond)

	cancel()
	<-done
	p.wg.Wait()

	assert.Equal(t, int64(messagesToSend), processed.Load())
}

// TestListenMessage_ContextCancellationUnblocksSemaphore tests that cancelling
// the context while the semaphore is full causes listenMessage to return
// promptly without blocking indefinitely on semaphore acquisition.
func TestListenMessage_ContextCancellationUnblocksSemaphore(t *testing.T) {
	const maxConcurrent = 1

	p := newTestPulsarWithConcurrency(maxConcurrent)
	consumer := newMockConsumer(2)

	handlerStarted := make(chan struct{})
	handlerCanProceed := make(chan struct{})

	var handlerCallCount atomic.Int64

	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		count := handlerCallCount.Add(1)
		if count == 1 {
			// Signal that the first handler started, then block.
			close(handlerStarted)
			<-handlerCanProceed
		}
		return nil
	}

	// First message: blocks in the handler, holding the semaphore slot.
	// Second message: queued; the loop will block on semaphore acquisition for it.
	sendMsg(consumer.Ch, consumer, []byte("first"))
	sendMsg(consumer.Ch, consumer, []byte("second"))

	ctx, cancel := context.WithCancel(context.Background())

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	// Wait for the first handler to start and hold the semaphore.
	<-handlerStarted

	// Cancel context; this should unblock the semaphore wait for the second message.
	cancel()

	// listenMessage should return within a reasonable time after context cancellation.
	select {
	case <-done:
		// Expected: loop exited due to context cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("listenMessage did not exit after context cancellation — semaphore likely blocked")
	}

	// Allow the blocked handler to finish cleanly.
	close(handlerCanProceed)
	p.wg.Wait()
}

// TestListenMessage_SyncModeNotAffectedBySemaphore verifies that sync mode still
// processes messages serially and is not broken by the semaphore addition.
func TestListenMessage_SyncModeNotAffectedBySemaphore(t *testing.T) {
	const messagesToSend = 5

	// semaphore is not used in sync mode; provide any value.
	p := newTestPulsarWithConcurrency(3)

	consumer := newMockConsumer(messagesToSend)

	msgs := makeConsumerMessagesN(consumer, messagesToSend)
	for _, msg := range msgs {
		consumer.Ch <- msg
	}

	var (
		maxObservedConcurrency int64
		mu                     sync.Mutex
		currentConcurrency     int64
		processed              atomic.Int64
	)

	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		mu.Lock()
		currentConcurrency++
		if currentConcurrency > maxObservedConcurrency {
			maxObservedConcurrency = currentConcurrency
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)
		processed.Add(1)

		mu.Lock()
		currentConcurrency--
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeSync},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	require.Eventually(t, func() bool {
		return processed.Load() == messagesToSend
	}, 5*time.Second, 5*time.Millisecond)

	cancel()
	<-done
	p.wg.Wait()

	// In sync mode, concurrency should always be exactly 1.
	assert.Equal(t, int64(1), maxObservedConcurrency,
		"sync mode should process exactly one message at a time")
	assert.Equal(t, int64(messagesToSend), processed.Load())
}

// TestListenMessage_BackpressureStopsChannelDrain verifies that when all
// semaphore slots are occupied by slow handlers, the consumer channel stops
// draining — the loop blocks on semaphore acquisition rather than reading more.
func TestListenMessage_BackpressureStopsChannelDrain(t *testing.T) {
	const maxConcurrent = 2

	p := newTestPulsarWithConcurrency(maxConcurrent)

	// Buffer large enough that extra messages can queue behind the semaphore.
	consumer := newMockConsumer(20)

	handlerStarted := make(chan struct{}, maxConcurrent)
	handlerCanProceed := make(chan struct{})

	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		handlerStarted <- struct{}{}
		<-handlerCanProceed
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	// Pre-fill exactly maxConcurrent messages to occupy all semaphore slots.
	for i := 0; i < maxConcurrent; i++ {
		sendMsg(consumer.Ch, consumer, []byte("occupying"))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	// Wait for all slots to be taken.
	for i := 0; i < maxConcurrent; i++ {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("handlers did not start within timeout")
		}
	}

	// Now add more messages while the semaphore is fully occupied.
	const extraMessages = 5
	for i := 0; i < extraMessages; i++ {
		sendMsg(consumer.Ch, consumer, []byte("extra"))
	}

	// Give the loop a chance to drain (it should NOT drain since semaphore is full).
	time.Sleep(100 * time.Millisecond)

	// The channel should be nearly full. The loop reads one message at a time:
	// it removes the message from the channel and THEN blocks on the semaphore.
	// So at most one message is "in flight" inside the select (removed from the
	// channel but waiting for a semaphore slot). All remaining messages stay queued.
	channelLen := len(consumer.Ch)
	assert.GreaterOrEqual(t, channelLen, extraMessages-1,
		"backpressure: channel should retain most messages when semaphore is full (got %d, want >= %d)",
		channelLen, extraMessages-1)

	// Unblock handlers and let everything finish.
	close(handlerCanProceed)

	cancel()
	<-done
	p.wg.Wait()
}

// TestListenMessage_GracefulShutdown verifies that after listenMessage exits,
// all in-flight handler goroutines complete (p.wg.Wait() returns promptly),
// demonstrating that Close() will not leak goroutines.
func TestListenMessage_GracefulShutdown(t *testing.T) {
	const (
		maxConcurrent  = 3
		messagesToSend = 6
	)

	p := newTestPulsarWithConcurrency(maxConcurrent)
	consumer := newMockConsumer(messagesToSend)

	msgs := makeConsumerMessagesN(consumer, messagesToSend)
	for _, msg := range msgs {
		consumer.Ch <- msg
	}

	handlerCanProceed := make(chan struct{})

	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		<-handlerCanProceed
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	// Give some handlers time to start and block.
	time.Sleep(50 * time.Millisecond)

	// Cancel the context to trigger shutdown of the listen loop.
	cancel()

	// Wait for listenMessage to exit.
	select {
	case <-listenDone:
	case <-time.After(2 * time.Second):
		t.Fatal("listenMessage did not exit after context cancel")
	}

	// Allow handlers to proceed.
	close(handlerCanProceed)

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		p.wg.Wait()
	}()

	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("wg.Wait() did not return within timeout — goroutines may be leaked")
	}
}

// TestListenMessage_HandlerErrorDoesNotLeakGoroutine verifies that handlers
// returning errors do not cause goroutine leaks under the semaphore.
func TestListenMessage_HandlerErrorDoesNotLeakGoroutine(t *testing.T) {
	const (
		maxConcurrent  = 5
		messagesToSend = 20
	)

	p := newTestPulsarWithConcurrency(maxConcurrent)
	consumer := newMockConsumer(messagesToSend)

	msgs := makeConsumerMessagesN(consumer, messagesToSend)
	for _, msg := range msgs {
		consumer.Ch <- msg
	}

	var processed atomic.Int64
	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		processed.Add(1)
		return assert.AnError
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	require.Eventually(t, func() bool {
		return processed.Load() == messagesToSend
	}, 5*time.Second, 5*time.Millisecond)

	cancel()
	<-done
	p.wg.Wait()

	assert.Equal(t, int64(messagesToSend), processed.Load(),
		"all messages should be processed even when handlers return errors")
}

// TestListenMessage_KeyVerification is the canonical scenario from the spec:
//   - MaxConcurrentHandlers = 5
//   - 50 messages sent through a mock consumer channel
//   - each handler sleeps for 50ms
//   - peak concurrency must not exceed 5
//   - all 50 messages must be processed
func TestListenMessage_KeyVerification(t *testing.T) {
	const (
		maxConcurrent     = 5
		messagesToSend    = 50
		handlerSleepDelay = 50 * time.Millisecond
	)

	p := newTestPulsarWithConcurrency(maxConcurrent)
	consumer := newMockConsumer(messagesToSend)

	msgs := makeConsumerMessagesN(consumer, messagesToSend)
	for _, msg := range msgs {
		consumer.Ch <- msg
	}

	var (
		currentConcurrency atomic.Int64
		peakConcurrency    atomic.Int64
		processed          atomic.Int64
	)

	handler := func(ctx context.Context, msg *pubsub.NewMessage) error {
		current := currentConcurrency.Add(1)
		defer currentConcurrency.Add(-1)

		for {
			peak := peakConcurrency.Load()
			if current <= peak || peakConcurrency.CompareAndSwap(peak, current) {
				break
			}
		}

		time.Sleep(handlerSleepDelay)
		processed.Add(1)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := pubsub.SubscribeRequest{
		Topic:    "test-topic",
		Metadata: map[string]string{processModeKey: processModeAsync},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.listenMessage(ctx, req, consumer, handler)
	}()

	require.Eventually(t, func() bool {
		return processed.Load() == messagesToSend
	}, 30*time.Second, 10*time.Millisecond,
		"all 50 messages should be processed within the timeout")

	cancel()
	<-done
	p.wg.Wait()

	assert.LessOrEqual(t, peakConcurrency.Load(), int64(maxConcurrent),
		"peak concurrent handlers (%d) exceeded MaxConcurrentHandlers (%d)",
		peakConcurrency.Load(), maxConcurrent)

	assert.Equal(t, int64(messagesToSend), processed.Load(),
		"all 50 messages should have been processed")
}
