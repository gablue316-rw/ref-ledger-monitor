package main

import (
	"context"
	"errors"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

var errMaintenanceLocked = errors.New("maintenance mode is active")

// Shared by both services. A single atomic document serializes admission and
// enabling maintenance across replicas. Counters do not expire: a failed/crashed
// writer must never silently become a green backup indicator.
func maintenanceCollection(db *mongo.Database) *mongo.Collection {
	return db.Collection("system_control", options.Collection().SetReadConcern(readconcern.Majority()).SetWriteConcern(writeconcern.Majority()))
}

func beginMaintenanceWrite(db *mongo.Database) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coll := maintenanceCollection(db)
	_, err := coll.UpdateOne(ctx, bson.M{"_id": "maintenance"}, bson.M{"$setOnInsert": bson.M{"enabled": false, "activeWrites": int64(0)}}, options.Update().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return nil, err
	}
	var state struct {
		Active int64 `bson:"activeWrites"`
	}
	err = coll.FindOneAndUpdate(ctx, bson.M{"_id": "maintenance", "enabled": false}, bson.M{"$inc": bson.M{"activeWrites": int64(1)}}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&state)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, errMaintenanceLocked
	}
	if err != nil {
		return nil, err
	}
	return func() {
		// Request cancellation must not prevent releasing the admitted write.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer releaseCancel()
		result, releaseErr := coll.UpdateOne(releaseCtx, bson.M{"_id": "maintenance", "activeWrites": bson.M{"$gt": 0}}, bson.M{"$inc": bson.M{"activeWrites": int64(-1)}})
		if releaseErr != nil || result.MatchedCount != 1 {
			log.Printf("Maintenance write accounting needs attention: result=%v error=%v", result, releaseErr)
		}
	}, nil
}
