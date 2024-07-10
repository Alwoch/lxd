//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/canonical/lxd/lxd/db/query"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/version"
)

// GetNetworksLocalConfig returns a map associating each network name to its
// node-specific config values on the local node (i.e. the ones where node_id
// equals the ID of the local node).
func (c *ClusterTx) GetNetworksLocalConfig() (map[string]map[string]string, error) {
	names, err := query.SelectStrings(c.tx, "SELECT name FROM networks")
	if err != nil {
		return nil, err
	}
	networks := make(map[string]map[string]string, len(names))
	for _, name := range names {
		table := "networks_config JOIN networks ON networks.id=networks_config.network_id"
		config, err := query.SelectConfig(
			c.tx, table, "networks.name=? AND networks_config.node_id=?",
			name, c.nodeID)
		if err != nil {
			return nil, err
		}
		networks[name] = config
	}
	return networks, nil
}

// GetNonPendingNetworkIDs returns a map associating each network name to its ID.
//
// Pending networks are skipped.
func (c *ClusterTx) GetNonPendingNetworkIDs() (map[string]map[string]int64, error) {
	networks := []struct {
		id          int64
		name        string
		projectName string
	}{}

	dest := func(i int) []interface{} {
		networks = append(networks, struct {
			id          int64
			name        string
			projectName string
		}{})
		return []interface{}{&networks[i].id, &networks[i].name, &networks[i].projectName}

	}

	stmt, err := c.tx.Prepare("SELECT networks.id, networks.name, 'default' FROM networks WHERE NOT networks.state=?")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, networkPending)
	if err != nil {
		return nil, err
	}

	ids := map[string]map[string]int64{}
	for _, network := range networks {
		if ids[network.projectName] == nil {
			ids[network.projectName] = map[string]int64{}
		}

		ids[network.projectName][network.name] = network.id
	}

	return ids, nil
}

// GetCreatedNetworks returns a map of api.Network and network ID.
// Only networks that have are in state networkCreated are returned.
func (c *ClusterTx) GetCreatedNetworks() (map[int64]api.Network, error) {
	stmt, err := c.tx.Prepare("SELECT id, name, coalesce(description, ''), state FROM networks WHERE state = ?")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(networkCreated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	networks := make(map[int64]api.Network)

	for i := 0; rows.Next(); i++ {
		var networkID int64
		var networkState NetworkState
		var network api.Network

		err := rows.Scan(&networkID, &network.Name, &network.Description, &networkState)
		if err != nil {
			return nil, err
		}

		// Populate Status and Type fields by converting from DB values.
		network.Status = NetworkStateToAPIStatus(networkState)
		networkFillType(&network, NetworkTypeBridge)

		networks[networkID] = network
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}

	// Populate config.
	for networkID, network := range networks {
		networkConfig, err := query.SelectConfig(c.tx, "networks_config", "network_id=? AND (node_id=? OR node_id IS NULL)", networkID, c.nodeID)
		if err != nil {
			return nil, err
		}

		network.Config = networkConfig

		nodes, err := c.NetworkNodes(networkID)
		if err != nil {
			return nil, err
		}

		for _, node := range nodes {
			network.Locations = append(network.Locations, node.Name)
		}

		networks[networkID] = network
	}

	return networks, nil
}

// GetNetworkID returns the ID of the network with the given name.
func (c *ClusterTx) GetNetworkID(name string) (int64, error) {
	stmt := "SELECT id FROM networks WHERE name=?"
	ids, err := query.SelectIntegers(c.tx, stmt, name)
	if err != nil {
		return -1, err
	}
	switch len(ids) {
	case 0:
		return -1, ErrNoSuchObject
	case 1:
		return int64(ids[0]), nil
	default:
		return -1, fmt.Errorf("more than one network has the given name")
	}
}

// CreateNetworkConfig adds a new entry in the networks_config table
func (c *ClusterTx) CreateNetworkConfig(networkID, nodeID int64, config map[string]string) error {
	return networkConfigAdd(c.tx, networkID, nodeID, config)
}

// NetworkNodeJoin adds a new entry in the networks_nodes table.
//
// It should only be used when a new node joins the cluster, when it's safe to
// assume that the relevant network has already been created on the joining node,
// and we just need to track it.
func (c *ClusterTx) NetworkNodeJoin(networkID, nodeID int64) error {
	columns := []string{"network_id", "node_id"}
	values := []interface{}{networkID, nodeID}
	_, err := query.UpsertObject(c.tx, "networks_nodes", columns, values)
	return err
}

// NetworkNodeConfigs returns the node-specific configuration of all
// nodes grouped by node name, for the given networkID.
//
// If the network is not defined on all nodes, an error is returned.
func (c *ClusterTx) NetworkNodeConfigs(networkID int64) (map[string]map[string]string, error) {
	// Fetch all nodes.
	nodes, err := c.GetNodes()
	if err != nil {
		return nil, err
	}

	// Fetch the names of the nodes where the storage network is defined.
	stmt := `
SELECT nodes.name FROM nodes
  LEFT JOIN networks_nodes ON networks_nodes.node_id = nodes.id
  LEFT JOIN networks ON networks_nodes.network_id = networks.id
WHERE networks.id = ? AND networks.state = ?
`
	defined, err := query.SelectStrings(c.tx, stmt, networkID, networkPending)
	if err != nil {
		return nil, err
	}

	// Figure which nodes are missing
	missing := []string{}
	for _, node := range nodes {
		if !shared.StringInSlice(node.Name, defined) {
			missing = append(missing, node.Name)
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("Network not defined on nodes: %s", strings.Join(missing, ", "))
	}

	configs := map[string]map[string]string{}
	for _, node := range nodes {
		config, err := query.SelectConfig(c.tx, "networks_config", "node_id=?", node.ID)
		if err != nil {
			return nil, err
		}
		configs[node.Name] = config
	}

	return configs, nil
}

// CreatePendingNetwork creates a new pending network on the node with the given name.
func (c *ClusterTx) CreatePendingNetwork(node, name string, netType NetworkType, conf map[string]string) error {
	if netType != NetworkTypeBridge {
		return fmt.Errorf("Unsupported network type: %v", netType)
	}

	// First check if a network with the given name exists, and, if so, that it's in the pending state.
	network := struct {
		id    int64
		state NetworkState
	}{}

	var errConsistency error
	dest := func(i int) []interface{} {
		// Ensure that there is at most one network with the given name.
		if i != 0 {
			errConsistency = fmt.Errorf("More than one network exists with the given name")
		}
		return []interface{}{&network.id, &network.state}
	}

	stmt, err := c.tx.Prepare("SELECT id, state FROM networks WHERE name=?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, name)
	if err != nil {
		return err
	}

	if errConsistency != nil {
		return errConsistency
	}

	var networkID = network.id
	if networkID == 0 {
		// No existing network with the given name was found, let's create one.
		columns := []string{"name", "description"}
		values := []interface{}{name, ""}
		networkID, err = query.UpsertObject(c.tx, "networks", columns, values)
		if err != nil {
			return err
		}
	} else {
		// Check that the existing network is in the networkPending state.
		if network.state != networkPending {
			return fmt.Errorf("Network is not in pending state")
		}
	}

	// Get the ID of the node with the given name.
	nodeInfo, err := c.GetNodeByName(node)
	if err != nil {
		return err
	}

	// Check that no network entry for this node and network exists yet.
	count, err := query.Count(c.tx, "networks_nodes", "network_id=? AND node_id=?", networkID, nodeInfo.ID)
	if err != nil {
		return err
	}
	if count != 0 {
		return ErrAlreadyDefined
	}

	// Insert the node-specific configuration.
	columns := []string{"network_id", "node_id"}
	values := []interface{}{networkID, nodeInfo.ID}
	_, err = query.UpsertObject(c.tx, "networks_nodes", columns, values)
	if err != nil {
		return err
	}

	err = c.CreateNetworkConfig(networkID, nodeInfo.ID, conf)
	if err != nil {
		return err
	}

	return nil
}

// NetworkCreated sets the state of the given network to networkCreated.
func (c *ClusterTx) NetworkCreated(name string) error {
	return c.networkState(name, networkCreated)
}

// NetworkErrored sets the state of the given network to networkErrored.
func (c *ClusterTx) NetworkErrored(name string) error {
	return c.networkState(name, networkErrored)
}

func (c *ClusterTx) networkState(name string, state NetworkState) error {
	stmt := "UPDATE networks SET state=? WHERE name=?"
	result, err := c.tx.Exec(stmt, state, name)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNoSuchObject
	}
	return nil
}

// UpdateNetwork updates the network with the given ID.
func (c *ClusterTx) UpdateNetwork(id int64, description string, config map[string]string) error {
	err := updateNetworkDescription(c.tx, id, description)
	if err != nil {
		return err
	}

	err = clearNetworkConfig(c.tx, id, c.nodeID)
	if err != nil {
		return err
	}

	err = networkConfigAdd(c.tx, id, c.nodeID, config)
	if err != nil {
		return err
	}

	return nil
}

// NetworkNodes returns the nodes keyed by node ID that the given network is defined on.
func (c *ClusterTx) NetworkNodes(networkID int64) (map[int64]NetworkNode, error) {
	nodes := []NetworkNode{}
	dest := func(i int) []interface{} {
		nodes = append(nodes, NetworkNode{})
		return []interface{}{&nodes[i].ID, &nodes[i].Name}
	}

	stmt, err := c.tx.Prepare(`
		SELECT nodes.id, nodes.name FROM nodes
		JOIN networks_nodes ON networks_nodes.node_id = nodes.id
		WHERE networks_nodes.network_id = ?
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, networkID)
	if err != nil {
		return nil, err
	}

	netNodes := map[int64]NetworkNode{}
	for _, node := range nodes {
		node.State = -1
		netNodes[node.ID] = node
	}

	return netNodes, nil
}

// GetNetworkURIs returns the URIs for the networks with the given project.
func (c *ClusterTx) GetNetworkURIs(projectID int, project string) ([]string, error) {
	sql := `SELECT networks.name from networks`

	names, err := query.SelectStrings(c.tx, sql)
	if err != nil {
		return nil, fmt.Errorf("Unable to get URIs for network: %w", err)
	}
	uris := make([]string, len(names))
	for i := range names {
		uris[i] = api.NewURL().Path(version.APIVersion, "networks", names[i]).Project(project).String()
	}

	return uris, nil
}

// GetNetworks returns the names of existing networks.
func (c *Cluster) GetNetworks() ([]string, error) {
	return c.networks("")
}

// GetCreatedNetworks returns the names of all networks that are in state networkCreated.
func (c *Cluster) GetCreatedNetworks() ([]string, error) {
	return c.networks("state=?", networkCreated)
}

// Get all networks matching the given WHERE filter (if given).
func (c *Cluster) networks(where string, args ...interface{}) ([]string, error) {
	q := "SELECT name FROM networks"
	inargs := []interface{}{}

	if where != "" {
		q += fmt.Sprintf(" WHERE %s", where)
		for _, arg := range args {
			inargs = append(inargs, arg)
		}
	}

	var name string
	outfmt := []interface{}{name}
	result, err := queryScan(c, q, inargs, outfmt)
	if err != nil {
		return []string{}, err
	}

	response := []string{}
	for _, r := range result {
		response = append(response, r[0].(string))
	}

	return response, nil
}

// NetworkState indicates the state of the network or network node.
type NetworkState int

// Network state.
const (
	networkPending NetworkState = iota // Network defined but not yet created globally or on specific node.
	networkCreated                     // Network created globally or on specific node.
	networkErrored                     // Deprecated (should no longer occur).
)

// NetworkType indicates type of network.
type NetworkType int

// Network types.
const (
	NetworkTypeBridge NetworkType = iota // Network type bridge.
)

// NetworkNode represents a network node.
type NetworkNode struct {
	ID    int64
	Name  string
	State NetworkState
}

// GetNetworkInAnyState returns the network with the given name. The network can be in any state.
// Returns network ID, network info, and network cluster member info.
func (c *Cluster) GetNetworkInAnyState(networkName string) (int64, *api.Network, map[int64]NetworkNode, error) {
	return c.getNetworkByName(networkName, -1)
}

// getNetworkByName returns the network with the given name and state.
// If stateFilter is -1, then a network can be in any state.
// Returns network ID, network info, and network cluster member info.
func (c *Cluster) getNetworkByName(networkName string, stateFilter NetworkState) (int64, *api.Network, map[int64]NetworkNode, error) {
	networkID, networkState, networkType, network, err := c.getPartialNetworkByName(networkName, stateFilter)
	if err != nil {
		return -1, nil, nil, err
	}

	nodes, err := c.networkPopulatePeerInfo(networkID, network, networkState, networkType)
	if err != nil {
		return -1, nil, nil, err
	}

	return networkID, network, nodes, nil
}

// getPartialNetworkByName gets the network with the given name and state.
// If stateFilter is -1, then a network can be in any state.
// Returns network ID, network state, network type, and partially populated network info.
func (c *Cluster) getPartialNetworkByName(networkName string, stateFilter NetworkState) (int64, NetworkState, NetworkType, *api.Network, error) {
	var err error
	var networkID int64 = int64(-1)
	var network api.Network
	var networkState NetworkState
	var networkType NetworkType

	// Managed networks exist in the database.
	network.Managed = true

	var q strings.Builder

	q.WriteString(`SELECT n.id, n.name, IFNULL(n.description, "") as description, n.state
		FROM networks AS n
		WHERE n.name=?
	`)
	args := []interface{}{networkName}

	if stateFilter > -1 {
		q.WriteString(" AND n.state=?")
		args = append(args, networkCreated)
	}

	q.WriteString(" LIMIT 1")

	err = c.Transaction(func(tx *ClusterTx) error {
		err = tx.tx.QueryRow(q.String(), args...).Scan(&networkID, &network.Name, &network.Description, &networkState)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return api.StatusErrorf(http.StatusNotFound, "Network not found")
			}

			return err
		}

		return nil
	})
	if err != nil {
		return -1, -1, -1, nil, err
	}

	return networkID, networkState, networkType, &network, err
}

// networkPopulatePeerInfo takes a pointer to partially populated network info struct and enriches it.
// Returns the network cluster member info.
func (c *Cluster) networkPopulatePeerInfo(networkID int64, network *api.Network, networkState NetworkState, networkType NetworkType) (map[int64]NetworkNode, error) {
	var err error

	// Populate Status and Type fields by converting from DB values.
	network.Status = NetworkStateToAPIStatus(networkState)
	networkFillType(network, networkType)

	network.Config, err = c.getNetworkConfig(networkID)
	if err != nil {
		return nil, err
	}

	// Populate Location field.
	nodes, err := c.NetworkNodes(networkID)
	if err != nil {
		return nil, err
	}

	network.Locations = make([]string, 0, len(nodes))
	for _, node := range nodes {
		network.Locations = append(network.Locations, node.Name)
	}

	return nodes, nil
}

// NetworkStateToAPIStatus converts DB NetworkState to API status string.
func NetworkStateToAPIStatus(state NetworkState) string {
	switch state {
	case networkPending:
		return api.NetworkStatusPending
	case networkCreated:
		return api.NetworkStatusCreated
	case networkErrored:
		return api.NetworkStatusErrored
	default:
		return api.NetworkStatusUnknown
	}
}

func networkFillType(network *api.Network, netType NetworkType) {
	switch netType {
	case NetworkTypeBridge:
		network.Type = "bridge"
	default:
		network.Type = "" // Unknown
	}
}

// NetworkNodes returns the nodes keyed by node ID that the given network is defined on.
func (c *Cluster) NetworkNodes(networkID int64) (map[int64]NetworkNode, error) {
	var nodes map[int64]NetworkNode
	var err error

	err = c.Transaction(func(tx *ClusterTx) error {
		nodes, err = tx.NetworkNodes(networkID)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return nodes, nil
}

// GetNetworkWithInterface returns the network associated with the interface with the given name.
func (c *Cluster) GetNetworkWithInterface(devName string) (int64, *api.Network, error) {
	id := int64(-1)
	name := ""
	value := ""

	q := "SELECT networks.id, networks.name, networks_config.value FROM networks LEFT JOIN networks_config ON networks.id=networks_config.network_id WHERE networks_config.key=\"bridge.external_interfaces\" AND networks_config.node_id=?"
	arg1 := []interface{}{c.nodeID}
	arg2 := []interface{}{id, name, value}
	result, err := queryScan(c, q, arg1, arg2)
	if err != nil {
		return -1, nil, err
	}

	for _, r := range result {
		for _, entry := range strings.Split(r[2].(string), ",") {
			entry = strings.TrimSpace(entry)

			if entry == devName {
				id = r[0].(int64)
				name = r[1].(string)
			}
		}
	}

	if id == -1 {
		return -1, nil, fmt.Errorf("No network found for interface: %s", devName)
	}

	config, err := c.getNetworkConfig(id)
	if err != nil {
		return -1, nil, err
	}

	network := api.Network{
		Name:    name,
		Managed: true,
		Type:    "bridge",
	}
	network.Config = config

	return id, &network, nil
}

// Return the config map of the network with the given ID.
func (c *Cluster) getNetworkConfig(id int64) (map[string]string, error) {
	var key, value string
	query := `
        SELECT
            key, value
        FROM networks_config
		WHERE network_id=?
                AND (node_id=? OR node_id IS NULL)`
	inargs := []interface{}{id, c.nodeID}
	outfmt := []interface{}{key, value}
	results, err := queryScan(c, query, inargs, outfmt)
	if err != nil {
		return nil, fmt.Errorf("Failed to get network '%d'", id)
	}

	if len(results) == 0 {
		/*
		 * If we didn't get any rows here, let's check to make sure the
		 * network really exists; if it doesn't, let's send back a 404.
		 */
		query := "SELECT id FROM networks WHERE id=?"
		var r int
		results, err := queryScan(c, query, []interface{}{id}, []interface{}{r})
		if err != nil {
			return nil, err
		}

		if len(results) == 0 {
			return nil, ErrNoSuchObject
		}
	}

	config := map[string]string{}

	for _, r := range results {
		key = r[0].(string)
		value = r[1].(string)

		_, found := config[key]
		if found {
			return nil, fmt.Errorf("Duplicate config row found for key %q for network ID %d", key, id)
		}

		config[key] = value
	}

	return config, nil
}

// CreateNetwork creates a new network.
func (c *Cluster) CreateNetwork(name, description string, netType NetworkType, config map[string]string) (int64, error) {
	if netType != NetworkTypeBridge {
		return -1, fmt.Errorf("Unsupported network type: %v", netType)
	}

	var id int64
	err := c.Transaction(func(tx *ClusterTx) error {
		// Insert a new network record with state networkCreated.
		result, err := tx.tx.Exec("INSERT INTO networks (name, description, state) VALUES (?, ?, ?)", name, description, networkCreated)
		if err != nil {
			return err
		}

		id, err := result.LastInsertId()
		if err != nil {
			return err
		}

		// Insert a node-specific entry pointing to ourselves.
		columns := []string{"network_id", "node_id"}
		values := []interface{}{id, c.nodeID}
		_, err = query.UpsertObject(tx.tx, "networks_nodes", columns, values)
		if err != nil {
			return err
		}

		err = networkConfigAdd(tx.tx, id, c.nodeID, config)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		id = -1
	}
	return id, err
}

// UpdateNetwork updates the network with the given name.
func (c *Cluster) UpdateNetwork(name, description string, config map[string]string) error {
	id, _, _, err := c.GetNetworkInAnyState(name)
	if err != nil {
		return err
	}

	err = c.Transaction(func(tx *ClusterTx) error {
		err = tx.UpdateNetwork(id, description, config)
		if err != nil {
			return err
		}

		return nil
	})

	return err
}

// Update the description of the network with the given ID.
func updateNetworkDescription(tx *sql.Tx, id int64, description string) error {
	_, err := tx.Exec("UPDATE networks SET description=? WHERE id=?", description, id)
	return err
}

func networkConfigAdd(tx *sql.Tx, networkID, nodeID int64, config map[string]string) error {
	str := fmt.Sprintf("INSERT INTO networks_config (network_id, node_id, key, value) VALUES(?, ?, ?, ?)")
	stmt, err := tx.Prepare(str)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for k, v := range config {
		if v == "" {
			continue
		}
		var nodeIDValue interface{}
		if !shared.StringInSlice(k, NodeSpecificNetworkConfig) {
			nodeIDValue = nil
		} else {
			nodeIDValue = nodeID
		}

		_, err = stmt.Exec(networkID, nodeIDValue, k, v)
		if err != nil {
			return err
		}
	}

	return nil
}

// Remove any the config of the network with the given ID
// associated with the node with the given ID.
func clearNetworkConfig(tx *sql.Tx, networkID, nodeID int64) error {
	_, err := tx.Exec(
		"DELETE FROM networks_config WHERE network_id=? AND (node_id=? OR node_id IS NULL)",
		networkID, nodeID)
	if err != nil {
		return err
	}

	return nil
}

// DeleteNetwork deletes the network with the given name.
func (c *Cluster) DeleteNetwork(name string) error {
	id, _, _, err := c.GetNetworkInAnyState(name)
	if err != nil {
		return err
	}

	err = exec(c, "DELETE FROM networks WHERE id=?", id)
	if err != nil {
		return err
	}

	return nil
}

// RenameNetwork renames a network.
func (c *Cluster) RenameNetwork(oldName string, newName string) error {
	id, _, _, err := c.GetNetworkInAnyState(oldName)
	if err != nil {
		return err
	}

	err = c.Transaction(func(tx *ClusterTx) error {
		_, err = tx.tx.Exec("UPDATE networks SET name=? WHERE id=?", newName, id)
		return err
	})

	return err
}

// NodeSpecificNetworkConfig lists all network config keys which are node-specific.
var NodeSpecificNetworkConfig = []string{
	"bridge.external_interfaces",
}
