package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gablue316/ref-ledger-monitor/internal/model"
)

type fakeReader struct {
	calls int
	fail  bool
}

func (f *fakeReader) LastMinute(ctx context.Context, now time.Time) (model.LastMinuteResponse, error) {
	f.calls++
	if _, ok := ctx.Deadline(); !ok {
		panic("missing query timeout")
	}
	if f.fail {
		return model.LastMinuteResponse{}, errors.New("database unavailable")
	}
	return model.LastMinuteResponse{Users: []model.UserMetrics{}}, nil
}
func TestMetricsHTTPResponses(t *testing.T) {
	for _, test := range []struct {
		method string
		fail   bool
		status int
	}{
		{"GET", false, 200}, {"HEAD", false, 200}, {"POST", false, 405}, {"GET", true, 503},
	} {
		reader := &fakeReader{fail: test.fail}
		w := httptest.NewRecorder()
		LastMinute(reader)(w, httptest.NewRequest(test.method, "/api/metrics/last-minute", nil))
		if w.Code != test.status {
			t.Fatalf("%s got %d", test.method, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("metrics must not be cached")
		}
		if test.method == "POST" && reader.calls != 0 {
			t.Fatal("POST must not query MongoDB")
		}
		if test.method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD must have no body")
		}
		if test.fail && strings.Contains(w.Body.String(), "database unavailable") {
			t.Fatal("internal error leaked")
		}
	}
}
