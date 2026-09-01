package retry

import (
	"errors"
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := Backoff(i)
		if d < 750*time.Millisecond || d > 75*time.Second {
			t.Fatalf("backoff outside bounds: %s", d)
		}
	}
}

func TestTransientClassification(t *testing.T) {
	if !IsTransient(errors.New("server busy, try again")) {
		t.Fatal("expected transient")
	}
	if IsTransient(errors.New("authentication failed")) {
		t.Fatal("authentication must be persistent")
	}
}
