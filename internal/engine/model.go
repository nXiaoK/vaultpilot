package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type Retention struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}
type Plan struct {
	ID           string    `json:"id"`
	Version      int       `json:"version"`
	Name         string    `json:"name"`
	SourceIDs    []string  `json:"source_ids"`
	PoolID       string    `json:"pool_id"`
	Schedule     string    `json:"schedule"`
	Timezone     string    `json:"timezone"`
	Enabled      bool      `json:"enabled"`
	Excludes     []string  `json:"excludes"`
	Retention    Retention `json:"retention"`
	RetryCount   int       `json:"retry_count"`
	BandwidthKiB int       `json:"bandwidth_kib"`
	NextRuns     []string  `json:"next_runs,omitempty"`
}
type Source struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	MountIdentity string   `json:"mount_identity"`
	Excludes      []string `json:"excludes"`
	IncludeHidden bool     `json:"include_hidden"`
	MaxFileBytes  int64    `json:"max_file_bytes"`
	OneFileSystem bool     `json:"one_file_system"`
}
type Account struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Backend            string `json:"backend"`
	Remote             string `json:"remote"`
	DriveID            string `json:"drive_id"`
	Region             string `json:"region"`
	TenantID           string `json:"tenant_id"`
	QuotaDomain        string `json:"quota_domain"`
	Status             string `json:"status"`
	Enabled            bool   `json:"enabled"`
	Verified           bool   `json:"verified"`
	LocalPath          string `json:"local_path"`
	Budget             int64  `json:"budget_bytes"`
	CloudReserve       int64  `json:"cloud_reserve_bytes"`
	MaintenanceReserve int64  `json:"maintenance_reserve_bytes"`
	Free               int64  `json:"free_bytes"`
	ProjectUsed        int64  `json:"project_used_bytes"`
	QuotaAt            string `json:"quota_at"`
}
type Pool struct {
	ID              string   `json:"id"`
	AccountIDs      []string `json:"account_ids"`
	CatalogReplicas int      `json:"catalog_replicas"`
	ShardTarget     int64    `json:"shard_target_bytes"`
}
type Location struct {
	Status    string `json:"status,omitempty"`
	AccountID string `json:"account_id"`
	DriveID   string `json:"drive_id"`
	Region    string `json:"region"`
	TenantID  string `json:"tenant_id"`
	Path      string `json:"path"`
	Verified  bool   `json:"verified"`
	At        string `json:"at"`
}
type Repository struct {
	ID           string     `json:"id"`
	Version      int        `json:"version"`
	PlanID       string     `json:"plan_id"`
	AccountID    string     `json:"account_id"`
	Path         string     `json:"path"`
	Password     string     `json:"password,omitempty"`
	Status       string     `json:"status"`
	Locations    []Location `json:"locations"`
	LogicalBytes int64      `json:"logical_bytes"`
}
type Snapshot struct {
	RepositoryID string `json:"repository_id"`
	SnapshotID   string `json:"snapshot_id"`
	Empty        bool   `json:"empty"`
}
type Point struct {
	ID               string     `json:"id"`
	Version          int        `json:"version"`
	PlanID           string     `json:"plan_id"`
	RunID            string     `json:"run_id"`
	Name             string     `json:"name"`
	StartedAt        string     `json:"started_at"`
	FinishedAt       string     `json:"finished_at"`
	Status           string     `json:"status"`
	Protected        bool       `json:"protected"`
	FileCount        int64      `json:"file_count"`
	LogicalBytes     int64      `json:"logical_bytes"`
	Snapshots        []Snapshot `json:"snapshots"`
	CommitID         string     `json:"commit_id"`
	DeletedAt        string     `json:"deleted_at,omitempty"`
	ProtectionStatus string     `json:"protection_status,omitempty"`
}
type Task struct {
	partialRestore   bool
	ProcessedBytes   int64 `json:"processed_bytes"`
	progressBase     float64
	progressWeight   float64
	processedBase    int64
	ID               string          `json:"id"`
	Version          int             `json:"version"`
	Kind             string          `json:"kind"`
	PlanID           string          `json:"plan_id,omitempty"`
	Status           string          `json:"status"`
	Stage            string          `json:"stage"`
	Progress         float64         `json:"progress"`
	StartedAt        string          `json:"started_at"`
	FinishedAt       string          `json:"finished_at"`
	ScannedBytes     int64           `json:"scanned_bytes"`
	UploadedBytes    int64           `json:"uploaded_bytes"`
	CloudGrowthBytes int64           `json:"cloud_growth_bytes"`
	Error            string          `json:"error,omitempty"`
	Code             string          `json:"code,omitempty"`
	Retryable        bool            `json:"retryable"`
	Attempt          int             `json:"attempt"`
	Request          json.RawMessage `json:"request,omitempty"`
	Result           any             `json:"result,omitempty"`
	CancelRequested  bool            `json:"cancel_requested"`
}
type Entry struct {
	SourceID     string `json:"source_id"`
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
	MtimeNS      int64  `json:"mtime_ns"`
	Mode         uint32 `json:"mode"`
	LinkTarget   string `json:"link_target,omitempty"`
	RepositoryID string `json:"repository_id"`
	SnapshotPath string `json:"snapshot_path"`
}
type Allocation struct {
	RepositoryID    string `json:"repository_id"`
	AccountID       string `json:"account_id"`
	LogicalBytes    int64  `json:"logical_bytes"`
	EstimatedGrowth int64  `json:"estimated_growth_bytes"`
	FileCount       int64  `json:"file_count"`
}
type Planning struct {
	FileCount       int64        `json:"file_count"`
	LogicalBytes    int64        `json:"logical_bytes"`
	Shards          []Allocation `json:"shards"`
	CatalogReplicas int          `json:"catalog_replicas"`
	Warnings        []string     `json:"warnings"`
}
type failure struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *failure) Error() string         { return e.Message }
func fail(code, message string) *failure { return &failure{Code: code, Message: message} }
func uid() string                        { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func now() string                        { return time.Now().UTC().Format(time.RFC3339Nano) }
func terminal(s string) bool {
	return s == "completed" || s == "failed" || s == "partial_failed" || s == "cancelled" || s == "interrupted"
}

func degraded(err error) bool {
	var f *failure
	return errors.As(err, &f) && f.Code == "PROTECTION_DEGRADED"
}
