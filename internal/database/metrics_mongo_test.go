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
					{Key: "_id", Value: bson.D{{Key: "tenantId", Value: "user1"}, {Key: "collection", Value: "games"}}},
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
		if len(events) != 3 || events[0].CommandName != "find" || events[1].CommandName != "distinct" || events[2].CommandName != "aggregate" {
			mt.Fatalf("unexpected database operations %+v", events)
		}
	})
}
