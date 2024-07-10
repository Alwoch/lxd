package main

import (
	"github.com/canonical/lxd/lxd/cluster"
	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/network"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/shared/logger"
)

func networkAutoAttach(cluster *db.Cluster, devName string) error {
	_, dbInfo, err := cluster.GetNetworkWithInterface(devName)
	if err != nil {
		// No match found, move on
		return nil
	}

	return network.AttachInterface(dbInfo.Name, devName)
}

// networkUpdateForkdnsServersTask runs every 30s and refreshes the forkdns servers list.
func networkUpdateForkdnsServersTask(s *state.State, heartbeatData *cluster.APIHeartbeat) error {
	logger.Debug("Refreshing forkdns servers")

	// Get a list of managed networks
	networks, err := s.Cluster.GetCreatedNetworks()
	if err != nil {
		return err
	}

	for _, name := range networks {
		n, err := network.LoadByName(s, name)
		if err != nil {
			logger.Errorf("Failed to load network %q for heartbeat", name)
			continue
		}

		if n.Type() == "bridge" && n.Config()["bridge.mode"] == "fan" {
			err := n.HandleHeartbeat(heartbeatData)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
