package model

import "time"

// CollectionNames is the allowlist used by both the reader and response builder.
var CollectionNames = []string{
	"associations", "expenses", "games", "levels", "officials", "payments", "sites",
}

type Counts struct {
	Added   int64 `bson:"added" json:"added"`
	Updated int64 `bson:"updated" json:"updated"`
	Deleted int64 `bson:"deleted" json:"deleted"`
	Errors  int64 `bson:"errors" json:"errors"`
}

type UserMetrics struct {
	TenantID     string            `json:"tenantId"`
	Active       bool              `json:"active"`
	HTTPBytesIn  *int64            `json:"httpBytesIn"`
	HTTPBytesOut *int64            `json:"httpBytesOut"`
	Collections  map[string]Counts `json:"collections"`
}

type LastMinuteResponse struct {
	WindowStart   time.Time     `json:"windowStart"`
	WindowEnd     time.Time     `json:"windowEnd"`
	ActiveAsOf    time.Time     `json:"activeAsOf"`
	ActiveSource  string        `json:"activeSource"`
	HTTPAvailable bool          `json:"httpAvailable"`
	TotalUsers    int           `json:"totalUsers"`
	ActiveUsers   int           `json:"activeUsers"`
	Users         []UserMetrics `json:"users"`
}
