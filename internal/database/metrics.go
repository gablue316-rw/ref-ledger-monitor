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

type MetricsStore struct{ DB *mongo.Database }
type userDocument struct {
	TenantID string `bson:"tenantId"`
	Username string `bson:"username"`
}
type metricGroup struct {
	ID struct {
		TenantID   string `bson:"tenantId"`
		Collection string `bson:"collection"`
	} `bson:"_id"`
	model.Counts `bson:",inline"`
}
type httpMetricGroup struct {
	TenantID string `bson:"_id"`
	BytesIn  int64  `bson:"bytesIn"`
	BytesOut int64  `bson:"bytesOut"`
	Requests int64  `bson:"requests"`
}

func (s *MetricsStore) LastMinute(ctx context.Context, now time.Time) (model.LastMinuteResponse, error) {
	if s.DB == nil {
		return model.LastMinuteResponse{}, fmt.Errorf("monitor database is not initialized")
	}
	now = now.UTC()
	start, end := minuteWindow(now)
	cursor, err := s.DB.Collection("users").Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{
		{Key: "_id", Value: 0},
		{Key: "tenantId", Value: 1},
		{Key: "username", Value: 1},
	}))
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("read users: %w", err)
	}
	var users []userDocument
	if err = cursor.All(ctx, &users); err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("decode users: %w", err)
	}
	activeIDs, err := s.DB.Collection("sessions").Distinct(ctx, "tenantId", bson.M{"expiresAt": bson.M{"$gt": now}})
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("read active sessions: %w", err)
	}
	cursor, err = s.DB.Collection("metrics").Aggregate(ctx, collectionPipeline(start, end))
	if err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("aggregate collection metrics: %w", err)
	}
	var groups []metricGroup
	if err = cursor.All(ctx, &groups); err != nil {
		return model.LastMinuteResponse{}, fmt.Errorf("decode collection metrics: %w", err)
	}
	response, err := assembleResponse(now, users, activeIDs, groups)
	if err != nil {
		return response, err
	}
	cursor, err = s.DB.Collection("metrics").Aggregate(ctx, httpPipeline(start, end))
	if err != nil {
		return response, fmt.Errorf("aggregate HTTP metrics: %w", err)
	}
	var traffic []httpMetricGroup
	if err = cursor.All(ctx, &traffic); err != nil {
		return response, fmt.Errorf("decode HTTP metrics: %w", err)
	}
	// A successful query means HTTP measurement is available. No traffic in this
	// minute is a measured zero, rather than N/A.
	response.HTTPAvailable = true
	positions := make(map[string]int)
	for i := range response.Users {
		response.Users[i].HTTPBytesIn = new(int64)
		response.Users[i].HTTPBytesOut = new(int64)
		positions[response.Users[i].TenantID] = i
	}
	for _, g := range traffic {
		if !validMetricCounter(g.BytesIn) || !validMetricCounter(g.BytesOut) || !validMetricCounter(g.Requests) {
			return response, fmt.Errorf("HTTP counter is outside the supported range")
		}
		pos, exists := positions[g.TenantID]
		if !exists {
			// Retain traffic for anonymous and deleted accounts in All users totals.
			// These extra traffic rows do not increase TotalUsers or ActiveUsers.
			row := model.UserMetrics{TenantID: g.TenantID, HTTPBytesIn: new(int64), HTTPBytesOut: new(int64), Collections: emptyCollections()}
			pos = len(response.Users)
			positions[g.TenantID] = pos
			response.Users = append(response.Users, row)
		}
		*response.Users[pos].HTTPBytesIn = g.BytesIn
		*response.Users[pos].HTTPBytesOut = g.BytesOut
	}
	sort.Slice(response.Users, func(i, j int) bool { return response.Users[i].TenantID < response.Users[j].TenantID })
	return response, nil
}
func minuteWindow(now time.Time) (time.Time, time.Time) {
	start := now.UTC().Truncate(time.Minute)
	return start, start.Add(time.Minute)
}
func collectionPipeline(start, end time.Time) mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"category": "collection", "collection": bson.M{"$in": model.CollectionNames}, "bucketStart": bson.M{"$gte": start, "$lt": end}}}},
		bson.D{{Key: "$group", Value: bson.M{"_id": bson.M{"tenantId": "$tenantId", "collection": "$collection"}, "added": bson.M{"$sum": "$added"}, "updated": bson.M{"$sum": "$updated"}, "deleted": bson.M{"$sum": "$deleted"}, "errors": bson.M{"$sum": "$errors"}}}},
	}
}
func httpPipeline(start, end time.Time) mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"category": "http", "bucketStart": bson.M{"$gte": start, "$lt": end}}}},
		bson.D{{Key: "$group", Value: bson.M{"_id": "$tenantId", "bytesIn": bson.M{"$sum": "$bytesIn"}, "bytesOut": bson.M{"$sum": "$bytesOut"}, "requests": bson.M{"$sum": "$requests"}}}},
	}
}
func validMetricCounter(n int64) bool { return n >= 0 && n <= 9007199254740991 }
func emptyCollections() map[string]model.Counts {
	result := make(map[string]model.Counts, len(model.CollectionNames))
	for _, name := range model.CollectionNames {
		result[name] = model.Counts{}
	}
	return result
}
func assembleResponse(now time.Time, users []userDocument, activeIDs []interface{}, groups []metricGroup) (model.LastMinuteResponse, error) {
	start, end := minuteWindow(now)
	response := model.LastMinuteResponse{WindowStart: start, WindowEnd: end, ActiveAsOf: now.UTC(), ActiveSource: "unexpired_sessions", Users: make([]model.UserMetrics, 0, len(users))}
	active := make(map[string]bool)
	for _, v := range activeIDs {
		if id, ok := v.(string); ok {
			active[id] = true
		}
	}
	positions := make(map[string]int)
	for _, u := range users {
		if u.TenantID == "" {
			return response, fmt.Errorf("a users document has no tenantId")
		}
		if _, exists := positions[u.TenantID]; exists {
			return response, fmt.Errorf("multiple users share a tenantId; per-user attribution is ambiguous")
		}
		positions[u.TenantID] = len(response.Users)
		response.Users = append(response.Users, model.UserMetrics{
			TenantID:    u.TenantID,
			Username:    u.Username,
			Active:      active[u.TenantID],
			Collections: emptyCollections(),
		})
		if active[u.TenantID] {
			response.ActiveUsers++
		}
	}
	for _, g := range groups {
		pos, exists := positions[g.ID.TenantID]
		if !exists {
			continue
		}
		if _, allowed := response.Users[pos].Collections[g.ID.Collection]; !allowed {
			continue
		}
		c := g.Counts
		for _, n := range []int64{c.Added, c.Updated, c.Deleted, c.Errors} {
			if !validMetricCounter(n) {
				return response, fmt.Errorf("collection counter is outside the supported range")
			}
		}
		response.Users[pos].Collections[g.ID.Collection] = c
	}
	response.TotalUsers = len(response.Users)
	sort.Slice(response.Users, func(i, j int) bool { return response.Users[i].TenantID < response.Users[j].TenantID })
	return response, nil
}
