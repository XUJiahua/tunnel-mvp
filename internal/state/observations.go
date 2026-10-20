package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const ObservationVersion = 1
const ObservationRetention = 7 * 24 * time.Hour
const ObservationAuditLimit = 100000
const ObservationSampleLimit = 100000
const ObservationSamplesPerService = 1440

var ErrObservationCursor = errors.New("cluster audit retention or legacy snapshot changed; restart pagination")
var ErrObservationSchema = errors.New("unsupported cluster observation extension version")

type Observation struct {
	ID             string          `json:"id"`
	Source         string          `json:"source"`
	NodeID         string          `json:"nodeID"`
	Epoch          string          `json:"nodeEpoch,omitempty"`
	Kind           string          `json:"kind"`
	LegacySnapshot string          `json:"legacySnapshot,omitempty"`
	OrganizationID *string         `json:"organizationID,omitempty"`
	ServiceID      string          `json:"serviceID,omitempty"`
	Profile        string          `json:"profile,omitempty"`
	At             time.Time       `json:"at"`
	Payload        json.RawMessage `json:"payload"`
}
type ObservationScope struct {
	All            bool
	OrganizationID string
}
type ObservationAuditFilter struct {
	Epoch, Event, User, Site, Resource, Node string
	Since                                    time.Time
	Before                                   int64
	Limit                                    int
}
type ObservationAuditPage struct {
	Entries []json.RawMessage
	Epoch   string
	Next    int64
}
type ObservationNode struct {
	Alive      bool            `json:"alive"`
	Fresh      bool            `json:"fresh"`
	Draining   bool            `json:"draining"`
	NodeID     string          `json:"nodeID"`
	Epoch      string          `json:"nodeEpoch"`
	Expires    time.Time       `json:"expires"`
	ReportedAt *time.Time      `json:"reportedAt,omitempty"`
	Report     json.RawMessage `json:"report,omitempty"`
}
type ObservationHealth struct {
	Service Service
	Payload json.RawMessage
}
type ObservationRepository interface {
	ObservationLatest(context.Context, string, func(AuthResolution, Service) (string, bool, error)) ([]ObservationHealth, error)
	ObservationLatestInternal(context.Context, func(Service) string) ([]ObservationHealth, error)
	ObservationPublish(context.Context, []Observation) error
	ObservationReport(context.Context, json.RawMessage) error
	ObservationNodes(context.Context) ([]ObservationNode, error)
	ObservationNodesAuthorized(context.Context, string, func(AuthResolution) error) ([]ObservationNode, error)
	ObservationAudit(context.Context, string, ObservationAuditFilter, func(AuthResolution) (ObservationScope, error)) (ObservationAuditPage, error)
	ObservationSamples(context.Context, string, string, time.Time, int, func(AuthResolution, Service) (string, error)) ([]json.RawMessage, error)
	ObservationAuthorize(context.Context, string, func(AuthResolution) error) error
	ClusterDrain(context.Context, string, bool, func(AuthResolution) error) error
}
