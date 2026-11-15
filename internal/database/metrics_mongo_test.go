package database

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

// Exercise the actual MongoDB driver calls with deterministic wire responses.
// No live database is needed and no application data is written.
func TestMongoReaderQueriesAndDecodesMetrics(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("reads users sessions and collection buckets", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(0, mt.DB.Name()+".users", mtest.FirstBatch,
				bson.D{{Key: "tenantId", Value: "user1"}},
				bson.D{{Key: "tenantId", Value: "user2"}}),
			mtest.CreateSuccessResponse(bson.E{Key: "values", Value: bson.A{"user1"}}),
			mtest.CreateCursorResponse(0, mt.DB.Name()+".metrics", mtest.FirstBatch,
				bson.D{
					{Key: "_id", Value: bson.D{{Key: "tenantId", Value: "user1"}, {Key: "collection", Value: "games"}, {Key: "category", Value: "collection"}}},
					{Key: "added", Value: int64(1000)},
					{Key: "updated", Value: int64(196)},
					{Key: "deleted", Value: int64(174)},
					{Key: "errors", Value: int64(0)},
				}),
		)
		store := &MetricsStore{DB: mt.DB}
		result, err := store.LastMinute(context.Background(), time.Date(2026, 10, 29, 12, 6, 20, 0, time.UTC))
		if err != nil {
			mt.Fatal(err)
		}
		if result.TotalUsers != 2 || result.ActiveUsers != 1 {
			mt.Fatalf("wrong summary %+v", result)
		}
		games := result.Users[0].Collections["games"]
		if games.Added != 1000 || games.Updated != 196 || games.Deleted != 174 {
			mt.Fatalf("decoded wrong counts %+v", games)
		}
		events := mt.GetAllStartedEvents()
		if len(events) != 3 ||
			events[0].CommandName != "find" ||
			events[1].CommandName != "distinct" ||
			events[2].CommandName != "aggregate" {
			mt.Fatalf("unexpected database operations %+v", events)
		}
	})
}

func TestCombinedMetricsTrafficAndFailures(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	for _, tc := range []struct {
		name  string
		bytes int64
		fail  bool
	}{
		{"known and anonymous traffic", 123, false},
		{"negative HTTP counter", -1, true},
		{"unsafe HTTP counter", 9007199254740992, true},
	} {
		mt.Run(tc.name, func(mt *mtest.T) {
			mt.AddMockResponses(
				mtest.CreateCursorResponse(0, mt.DB.Name()+".users", mtest.FirstBatch,
					bson.D{{Key: "tenantId", Value: "user"}, {Key: "username", Value: "user@example.com"}}),
				mtest.CreateSuccessResponse(bson.E{Key: "values", Value: bson.A{"user"}}),
				mtest.CreateCursorResponse(0, mt.DB.Name()+".metrics", mtest.FirstBatch,
					bson.D{{Key: "_id", Value: bson.D{{Key: "tenantId", Value: "user"}, {Key: "category", Value: "http"}, {Key: "collection", Value: ""}}}, {Key: "bytesIn", Value: tc.bytes}, {Key: "bytesOut", Value: int64(456)}},
					bson.D{{Key: "_id", Value: bson.D{{Key: "tenantId", Value: "anonymous"}, {Key: "category", Value: "http"}, {Key: "collection", Value: ""}}}, {Key: "bytesIn", Value: int64(12)}},
					bson.D{{Key: "_id", Value: bson.D{{Key: "tenantId", Value: "user"}, {Key: "category", Value: "collection"}, {Key: "collection", Value: "games"}}}, {Key: "added", Value: int64(7)}}),
			)
			result, err := (&MetricsStore{DB: mt.DB}).LastMinute(context.Background(), time.Now())
			if tc.fail {
				if err == nil {
					mt.Fatal("invalid HTTP counter accepted")
				}
				return
			}
			if err != nil {
				mt.Fatal(err)
			}
			if !result.HTTPAvailable || result.TotalUsers != 1 || result.ActiveUsers != 1 || len(result.Users) != 2 {
				mt.Fatalf("wrong summary: %+v", result)
			}
			if result.Users[0].TenantID != "anonymous" || *result.Users[0].HTTPBytesIn != 12 || result.Users[0].Active {
				mt.Fatalf("lost anonymous traffic: %+v", result.Users[0])
			}
			user := result.Users[1]
			if *user.HTTPBytesIn != 123 || *user.HTTPBytesOut != 456 || user.Collections["games"].Added != 7 {
				mt.Fatalf("lost joined metrics: %+v", user)
			}
		})
	}
	mt.Run("aggregation failure propagates", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(0, mt.DB.Name()+".users", mtest.FirstBatch),
			mtest.CreateSuccessResponse(bson.E{Key: "values", Value: bson.A{}}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 2, Message: "invalid aggregation"}),
		)
		if _, err := (&MetricsStore{DB: mt.DB}).LastMinute(context.Background(), time.Now()); err == nil {
			mt.Fatal("aggregation failure ignored")
		}
	})
}
