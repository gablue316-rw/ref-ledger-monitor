package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"net/http"
	"strings"
)

var errAdminConflict = errors.New("record changed or was removed since it was loaded; refresh the list or reload the settings before trying again")

const adminMissingVersion = "missing"

func adminRecordVersion(raw bson.Raw) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
func adminExpectedVersion(r *http.Request) string {
	return strings.Trim(r.Header.Get("If-Match"), "\"")
}
func adminRequireVersion(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := adminExpectedVersion(r)
	if v == "" {
		adminUsersError(w, http.StatusConflict, errAdminConflict.Error())
		return "", false
	}
	return v, true
}
func adminConflict(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errAdminConflict) || errors.Is(err, mongo.ErrNoDocuments) {
		adminUsersError(w, http.StatusConflict, errAdminConflict.Error())
		return true
	}
	return false
}

// Compare the complete original BSON atomically, including added and missing
// fields. Preserve embedded field order; $literal keeps user data out of expressions.
func adminSnapshotFilter(raw bson.Raw) bson.M {
	return bson.M{"_id": raw.Lookup("_id"), "$expr": bson.M{"$eq": bson.A{"$$ROOT", bson.M{"$literal": raw}}}}
}
func adminConditionalFilter(ctx context.Context, coll *mongo.Collection, identity bson.M, version string) (bson.M, error) {
	if version == "" {
		return nil, errAdminConflict
	}
	raw, err := coll.FindOne(ctx, identity).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, errAdminConflict
	}
	if err != nil {
		return nil, err
	}
	if adminRecordVersion(raw) != version {
		return nil, errAdminConflict
	}
	return adminSnapshotFilter(raw), nil
}

func (u *adminUserRecord) UnmarshalBSON(data []byte) error {
	type plain adminUserRecord
	var value plain
	if err := bson.Unmarshal(data, &value); err != nil {
		return err
	}
	*u = adminUserRecord(value)
	u.Version = adminRecordVersion(data)
	u.raw = append(bson.Raw(nil), data...)
	return nil
}
func (s *adminSport) UnmarshalBSON(data []byte) error {
	type plain adminSport
	var value plain
	if err := bson.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = adminSport(value)
	s.Version = adminRecordVersion(data)
	s.raw = append(bson.Raw(nil), data...)
	return nil
}
