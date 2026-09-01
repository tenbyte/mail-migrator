package retry

import (
	"math/rand/v2"
	"strings"
	"time"
)

var schedule = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(schedule) {
		attempt = len(schedule) - 1
	}
	base := schedule[attempt]
	jitter := time.Duration(rand.Int64N(int64(base/2)+1)) - base/4
	return base + jitter
}

func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"timeout", "temporary", "try again", "rate limit", "throttl", "too many connections", "server busy", "resource unavailable", "[unavailable]", "[limit]", "[serverbug]", "reset by peer", "unexpected eof", "broken pipe"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
