package network

import (
	"net"

	"github.com/canonical/lxd/lxd/cluster"
	"github.com/canonical/lxd/lxd/cluster/request"
	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
)

// Type represents a LXD network driver type.
type Type interface {
	FillConfig(config map[string]string) error
	Info() Info
	ValidateName(name string) error
	Type() string
	DBType() db.NetworkType
}

// Network represents an instantiated LXD network.
type Network interface {
	Type

	// Load.
	init(state *state.State, id int64, netInfo *api.Network, netNodes map[int64]db.NetworkNode)

	// Config.
	Validate(config map[string]string) error
	ID() int64
	Name() string
	Project() string
	Description() string
	Status() string
	LocalStatus() string
	Config() map[string]string
	IsUsed() (bool, error)
	IsManaged() bool
	DHCPv4Subnet() *net.IPNet
	DHCPv6Subnet() *net.IPNet
	DHCPv4Ranges() []shared.IPRange
	DHCPv6Ranges() []shared.IPRange

	// Actions.
	Create(clientType request.ClientType) error
	Start() error
	Stop() error
	Rename(name string) error
	Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error
	HandleHeartbeat(heartbeatData *cluster.APIHeartbeat) error
	Delete(clientType request.ClientType) error
	handleDependencyChange(netName string, netConfig map[string]string, changedKeys []string) error

	// Status.
	State() (*api.NetworkState, error)
	Leases(projectName string, clientType request.ClientType) ([]api.NetworkLease, error)
}
