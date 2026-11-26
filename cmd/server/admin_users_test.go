package main

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestStoredSessionClientLabels(t *testing.T) {
	tests := []struct {
		name   string
		fields bson.M
		want   string
	}{
		{"managed Android", bson.M{"sessionId": "android-id", "type": "mobile", "platform": "android"}, "Android app"},
		{"legacy Android activity", bson.M{"platform": "android"}, "Android app"},
		{"web sign-in", bson.M{"sessionId": "web-id", "client": "web"}, "Web browser"},
		{"older web sign-in", bson.M{"sessionId": "older-id"}, "Web browser"},
		{"unidentified activity", bson.M{}, "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document, err := bson.Marshal(tt.fields)
			if err != nil {
				t.Fatal(err)
			}
			var session storedSession
			if err := bson.Unmarshal(document, &session); err != nil {
				t.Fatal(err)
			}
			if got := session.clientLabel(); got != tt.want {
				t.Fatalf("client label = %q, want %q", got, tt.want)
			}
		})
	}
}
