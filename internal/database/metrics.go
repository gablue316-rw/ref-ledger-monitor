package database

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gablue316/ref-ledger-monitor/internal/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MetricsStore reads the same database that Ref Ledger writes. It never writes
// records or creates indexes. Grant read access to users, sessions and metrics.
type MetricsStore struct {
	DB *mongo.Database
}

type userDocument struct {
	TenantID string `bson:"tenantId"`
}

type metricGroup struct {
	ID struct {
		TenantID   string `bson:"tenantId"`
		Collection string `bson:"collection"`
	} `bson:"_id"`
	model.Counts `bson:",inline"`
}

func (s *MetricsStore) LastMinute(ctx context.Context, now time.Time) (model.LastMinuteResponse, error) {
	if s.DB == nil {
		return model.LastMinuteResponse{}, fmt.Errorf("monitor database is not initialized")
	}

	now = now.UTC()
	start, end := minuteWindow(now)

	// Only retrieve the identity needed by the dashboard, never password hashes.
	cursor, err := s.DB.Collection("users").Find(ctx, bson.D{},
		options.Find().SetProjection(bson.D{
			{Key: "_id", Value: 0}, {Key: "tenantId", Value: 1},
		}))
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("read users: %w", err)
	}
	var users []userDocument
	if err := cursor.All(ctx, &users); err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("decode users: %w", err)
	}

	// TTL deletion is asynchronous. Explicitly exclude expired sessions.
	// Distinct counts a user once even when signed in on multiple devices.
	activeIDs, err := s.DB.Collection("sessions").Distinct(ctx, "tenantId", bson.D{
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}},
	})
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("read active sessions: %w", err)
	}

	cursor, err = s.DB.Collection("metrics").Aggregate(ctx, collectionPipeline(start, end))
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("aggregate collection metrics: %w", err)
	}
	var groups []metricGroup
	if err := cursor.All(ctx, &groups); err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("decode collection metrics: %w", err)
	}

	return assembleResponse(now, users, activeIDs, groups)
}

func minuteWindow(now time.Time) (time.Time, time.Time) {
	start := now.UTC().Truncate(time.Minute)
	return start, start.Add(time.Minute)
}

func collectionPipeline(start, end time.Time) mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "category", Value: "collection"},
			{Key: "collection", Value: bson.D{{Key: "$in", Value: model.CollectionNames}}},
			{Key: "bucketStart", Value: bson.D{
				{Key: "$gte", Value: start}, {Key: "$lt", Value: end},
			}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "tenantId", Value: "$tenantId"},
				{Key: "collection", Value: "$collection"},
			}},
			{Key: "added", Value: bson.D{{Key: "$sum", Value: "$added"}}},
			{Key: "updated", Value: bson.D{{Key: "$sum", Value: "$updated"}}},
			{Key: "deleted", Value: bson.D{{Key: "$sum", Value: "$deleted"}}},
			{Key: "errors", Value: bson.D{{Key: "$sum", Value: "$errors"}}},
		}}},
	}
}

func assembleResponse(now time.Time, users []userDocument, activeIDs []interface{}, groups []metricGroup) (model.LastMinuteResponse, error) {
	start, end := minuteWindow(now)
	response := model.LastMinuteResponse{
		WindowStart: start, WindowEnd: end, ActiveAsOf: now.UTC(),
		ActiveSource: "unexpired_sessions", HTTPAvailable: false,
		Users: make([]model.UserMetrics, 0, len(users)),
	}

	active := make(map[string]bool)
	for _, value := range activeIDs {
		if id, ok := value.(string); ok {
			active[id] = true
		}
	}

	positions := make(map[string]int, len(users))
	for _, user := range users {
		if user.TenantID == "" {
			return response, fmt.Errorf("a users document has no tenantId")
		}
		if _, exists := positions[user.TenantID]; exists {
			return response, fmt.Errorf("multiple users share a tenantId; per-user attribution is ambiguous")
		}
		row := model.UserMetrics{
			TenantID: user.TenantID, Active: active[user.TenantID],
			Collections: make(map[string]model.Counts, len(model.CollectionNames)),
		}
		for _, collection := range model.CollectionNames {
			row.Collections[collection] = model.Counts{}
		}
		// Nil HTTP pointers encode as null, not a misleading measured zero.
		positions[row.TenantID] = len(response.Users)
		response.Users = append(response.Users, row)
		if row.Active {
			response.ActiveUsers++
		}
	}

	for _, group := range groups {
		position, exists := positions[group.ID.TenantID]
		if !exists {
			// Historical buckets for deleted users are excluded from current-user totals.
			continue
		}
		if _, allowed := response.Users[position].Collections[group.ID.Collection]; !allowed {
			continue
		}
		counts := group.Counts
		const maxSafeInteger = int64(9007199254740991)
		for _, count := range []int64{counts.Added, counts.Updated, counts.Deleted, counts.Errors} {
			if count < 0 || count > maxSafeInteger {
				return response, fmt.Errorf("collection counter is outside the supported range")
			}
		}
		response.Users[position].Collections[group.ID.Collection] = counts
	}

	response.TotalUsers = len(response.Users)
	sort.Slice(response.Users, func(i, j int) bool {
		return response.Users[i].TenantID < response.Users[j].TenantID
	})
	return response, nil
}
