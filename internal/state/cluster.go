package state

import (
	"context"
	"errors"
	"time"
)

const ClusterProtocol = "tunnel-cluster-v1"
const ClusterNodeTTL = 10 * time.Second
const clusterRuntimeOwner = "cluster-v1"

var ErrClusterLease = errors.New("cluster incarnation or lease is no longer valid")
var ErrClusterBusy = errors.New("cluster node or Agent already has a live owner")

func ValidClusterNodeID(id string) bool { return resourceID.MatchString(id) }

type ClusterNode struct {
	ID, Epoch, URL, Fingerprint string
	Expires                     time.Time
}
type ClusterAgent struct {
	Key, SiteID, InstanceID, CredentialHash string
	OwnerID, OwnerEpoch                     string
	Generation                              uint64
	ConnectedAt                             time.Time
	RemoteIP, Version, PolicyVersion        string
	Channels                                int64
	Ready                                   bool
}
type ClusterRoute struct {
	Agent ClusterAgent
	Node  ClusterNode
}
type ClusterSnapshot struct {
	InstanceID string
	Now        time.Time
	Self       ClusterNode
	Nodes      []ClusterNode
	Routes     []ClusterRoute
}
type ClusterRepository interface {
	ClusterMembership(context.Context) (ClusterSnapshot, error)
	ClusterRegister(context.Context, ClusterNode) (ClusterSnapshot, error)
	ClusterHeartbeat(context.Context, []ClusterAgent) (ClusterSnapshot, error)
	ClusterCloseNode(context.Context) error
	ClusterClaimAgent(context.Context, ClusterAgent) (ClusterRoute, error)
	ClusterReleaseAgent(context.Context, string, uint64) error
	ClusterRoutes(context.Context, string) (ClusterSnapshot, error)
}
