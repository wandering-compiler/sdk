package runtime

import (
	"reflect"
	"testing"
	"time"
)

func TestInflight_WaitReturnsWhenHandlersEndOrReportsThem(t *testing.T) {
	f := newInflight()
	if got := f.wait(time.Millisecond); got != nil {
		t.Fatalf("idle wait = %v, want nil", got)
	}

	endA := f.begin("/svc/A")
	endB := f.begin("/svc/B")
	go func() {
		time.Sleep(20 * time.Millisecond)
		endA()
		endB()
	}()
	start := time.Now()
	if got := f.wait(5 * time.Second); got != nil {
		t.Fatalf("wait = %v, want nil once both returned", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait did not return as soon as the handlers ended")
	}

	endC := f.begin("/svc/C")
	defer endC()
	endD := f.begin("/svc/B")
	defer endD()
	if got := f.wait(30 * time.Millisecond); !reflect.DeepEqual(got, []string{"/svc/B", "/svc/C"}) {
		t.Fatalf("wait = %v, want the still-running methods", got)
	}
}
