package ezsp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A message to a sleepy device lives in the NCP's indirect queue for at most
// EZSP_CONFIG_INDIRECT_TRANSMISSION_TIMEOUT. After that the wait is on
// something that no longer exists, so these tests are about the repeat rather
// than about the wait.

func incomingReply(payload []byte) Message {
	return Message{Callback: true, ID: FrameIncomingMessage, Params: buildIncoming(0x0104, 0x0402, payload)}
}

func matchAnything(IncomingMessage) bool { return true }

func TestAwaitReplyRepeatsUntilAnswered(t *testing.T) {
	replies := make(chan Message, 1)
	sends := make(chan struct{}, 8)
	send := func() error {
		sends <- struct{}{}
		return nil
	}

	// Answer only once the message has gone out more than once, which is what
	// a device that was asleep for the first copy looks like.
	go func() {
		<-sends
		<-sends
		replies <- incomingReply([]byte{0x01})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, attempts, err := awaitReply(ctx, replies, time.Millisecond, send, matchAnything)
	if err != nil {
		t.Fatalf("awaitReply: %v", err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want the message repeated at least once", attempts)
	}
}

// If nothing was ever queued there is nothing to wait for, and the send error
// says more than a timeout would.
func TestAwaitReplyFailsFastWhenTheFirstSendFails(t *testing.T) {
	wantErr := errors.New("no packet buffers")
	replies := make(chan Message)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, attempts, err := awaitReply(ctx, replies, time.Millisecond, func() error { return wantErr }, matchAnything)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want none to have gone out", attempts)
	}
}

// A later send failing is a moment's shortage, not a reason to abandon copies
// that are still queued and still answerable.
func TestAwaitReplySurvivesALaterSendFailure(t *testing.T) {
	replies := make(chan Message, 1)
	calls := 0
	send := func() error {
		calls++
		if calls > 1 {
			return errors.New("no packet buffers")
		}
		return nil
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		replies <- incomingReply([]byte{0x01})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := awaitReply(ctx, replies, time.Millisecond, send, matchAnything); err != nil {
		t.Fatalf("awaitReply gave up on a transient send failure: %v", err)
	}
	if calls < 2 {
		t.Errorf("send calls = %d, want the failing resends to have been attempted", calls)
	}
}

// "No reply after one attempt" and "no reply after thirty" are different
// problems, so the count has to survive the timeout.
func TestAwaitReplyReportsHowManyAttemptsWentOut(t *testing.T) {
	replies := make(chan Message)
	calls := 0

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, attempts, err := awaitReply(ctx, replies, 10*time.Millisecond, func() error { calls++; return nil }, matchAnything)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the deadline", err)
	}
	if attempts < 2 || attempts != calls {
		t.Errorf("attempts = %d after %d sends, want them to agree and to exceed one", attempts, calls)
	}
}

func TestAwaitReplyIgnoresRepliesThatDoNotMatch(t *testing.T) {
	replies := make(chan Message, 4)
	replies <- incomingReply([]byte{0x01})
	replies <- incomingReply([]byte{0x02})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	want := byte(0x02)
	msg, _, err := awaitReply(ctx, replies, time.Second, func() error { return nil }, func(m IncomingMessage) bool {
		return len(m.Payload) > 0 && m.Payload[0] == want
	})
	if err != nil {
		t.Fatalf("awaitReply: %v", err)
	}
	if msg.Payload[0] != want {
		t.Errorf("payload = % X, want the reply that matched", msg.Payload)
	}
}

func TestAwaitReplyReportsAClosedSession(t *testing.T) {
	replies := make(chan Message)
	close(replies)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, _, err := awaitReply(ctx, replies, time.Second, func() error { return nil }, matchAnything); !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want %v", err, ErrClosed)
	}
}
