package database

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gablue316/ref-ledger-monitor/internal/model"

	"go.mongodb.org/mongo-driver/bson"
)

func TestWindowExcludesCurrentMinute(t *testing.T) {
	now := time.Date(2026, 10, 29, 8, 22, 59, 0, time.FixedZone("EDT", -4*3600))
	start, end := minuteWindow(now)
	if start.Format(time.RFC3339) != "2026-10-29T12:21:00Z" || end.Format(time.RFC3339) != "2026-10-29T12:22:00Z" {
		t.Fatalf("unexpected UTC window %v to %v", start, end)
	}
	pipeline := collectionPipeline(start, end)
	encoded, err := bson.MarshalExtJSON(bson.D{{Key: "pipeline", Value: pipeline}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"$gte"`, `"$lt"`, `"category":"collection"`, `"$sum":"$errors"`, `"levels"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("pipeline missing %s: %s", required, encoded)
		}
	}
}

func TestResponseJoinsUsersSessionsAndCounters(t *testing.T) {
	group := metricGroup{Counts: model.Counts{Added: 1000, Updated: 196, Deleted: 174, Errors: 2}}
	group.ID.TenantID, group.ID.Collection = "user-b", "games"
	orphan := group
	orphan.ID.TenantID = "deleted-user"
	response, err := assembleResponse(time.Now(), []userDocument{{"user-b"}, {"user-a"}}, []interface{}{"user-b", "user-b", "deleted-user"}, []metricGroup{group, orphan})
	if err != nil {
		t.Fatal(err)
	}
	if response.TotalUsers != 2 || response.ActiveUsers != 1 {
		t.Fatalf("wrong summary: %+v", response)
	}
	if response.Users[0].TenantID != "user-a" || response.Users[0].Active {
		t.Fatal("users must sort and zero-activity users must remain")
	}
	for _, user := range response.Users {
		if len(user.Collections) != 7 {
			t.Fatal("every collection must be present")
		}
		if user.HTTPBytesIn != nil || user.HTTPBytesOut != nil {
			t.Fatal("unrecorded HTTP counters must be null")
		}
	}
	if response.Users[1].Collections["games"] != group.Counts {
		t.Fatal("lost game counters")
	}
	if response.Users[0].Collections["games"] != (model.Counts{}) {
		t.Fatal("missing bucket must be zero")
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"httpBytesIn":null`) {
		t.Fatal("null was not preserved")
	}
}

func TestEmptyDatabaseHasArrayAndNoActiveUsers(t *testing.T) {
	r, err := assembleResponse(time.Now(), nil, nil, nil)
	if err != nil || r.Users == nil || r.TotalUsers != 0 || r.ActiveUsers != 0 {
		t.Fatalf("invalid empty response: %+v, %v", r, err)
	}
}

func TestInvalidUserIdentityAndCountersFail(t *testing.T) {
	for _, users := range [][]userDocument{{{""}}, {{"same"}, {"same"}}} {
		if _, err := assembleResponse(time.Now(), users, nil, nil); err == nil {
			t.Fatal("expected identity validation error")
		}
	}
	group := metricGroup{Counts: model.Counts{Errors: -1}}
	group.ID.TenantID, group.ID.Collection = "user", "games"
	if _, err := assembleResponse(time.Now(), []userDocument{{"user"}}, nil, []metricGroup{group}); err == nil {
		t.Fatal("negative counter accepted")
	}
}
