package firewall

import (
	"net"

	drivers "github.com/canonical/lxd/lxd/firewall/drivers"
)

// Firewall represents a LXD firewall.
type Firewall interface {
	String() string
	Compat() (bool, error)

	NetworkSetup(networkName string, opts drivers.Opts) error
	NetworkClear(networkName string, delete bool, ipVersions []uint) error

	InstanceSetupBridgeFilter(projectName string, instanceName string, deviceName string, parentName string, hostName string, hwAddr string, IPv4 net.IP, IPv6 net.IP, parentManaged bool) error
	InstanceClearBridgeFilter(projectName string, instanceName string, deviceName string, parentName string, hostName string, hwAddr string, IPv4 net.IP, IPv6 net.IP) error

	InstanceSetupProxyNAT(projectName string, instanceName string, deviceName string, forward *drivers.AddressForward) error
	InstanceClearProxyNAT(projectName string, instanceName string, deviceName string) error

	InstanceSetupRPFilter(projectName string, instanceName string, deviceName string, hostName string) error
	InstanceClearRPFilter(projectName string, instanceName string, deviceName string) error
}
