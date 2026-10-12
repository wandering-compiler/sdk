package restgw_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// syncBuffer is a log sink safe to write from the bus goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A delivery whose envelope cannot be transcoded is dropped AND leaves a
// trace naming its channel, topic and cause; it used to vanish silently.
func TestEventbusEventSource_TranscodeErrorIsLogged(t *testing.T) {
	var sink syncBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	bus := eventbus.NewMemoryBus()
	transcode := func(string, []byte) ([]byte, map[string]string, bool, error) {
		return nil, nil, false, errors.New("bogus envelope")
	}
	source := restgw.NewEventbusEventSource(bus, []string{"orders"}, transcode, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=x.y", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "orders", "x.y", wrapperspb.String("corrupt"))
	<-done
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !strings.Contains(sink.String(), "bogus envelope"); {
		time.Sleep(5 * time.Millisecond)
	}

	if strings.Contains(rec.Body.String(), "event: x.y") {
		t.Errorf("an untranscodable event reached the wire:\n%s", rec.Body.String())
	}
	got := sink.String()
	for _, want := range []string{`"orders"`, `"x.y"`, "bogus envelope"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not mention %s", got, want)
		}
	}
}
