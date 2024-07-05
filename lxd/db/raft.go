//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"fmt"

	"github.com/canonical/go-dqlite/client"
	"github.com/canonical/lxd/lxd/db/query"
	"github.com/pkg/errors"
)

// RaftNode holds information about a single node in the dqlite raft cluster.
//
// This is just a convenience alias for the equivalent data structure in the
// dqlite client package.
type RaftNode struct {
	client.NodeInfo
	Name string
}

// RaftRole captures the role of dqlite/raft node.
type RaftRole = client.NodeRole

// RaftNode roles.
const (
	RaftVoter   = client.Voter
	RaftStandBy = client.StandBy
	RaftSpare   = client.Spare
)

// GetRaftNodes returns information about all LXD nodes that are members of the
// dqlite Raft cluster (possibly including the local node). If this LXD
// instance is not running in clustered mode, an empty list is returned.
func (n *NodeTx) GetRaftNodes() ([]RaftNode, error) {
	nodes := []RaftNode{}
	dest := func(i int) []interface{} {
		nodes = append(nodes, RaftNode{})
		return []interface{}{&nodes[i].ID, &nodes[i].Address, &nodes[i].Role, &nodes[i].Name}
	}
	stmt, err := n.tx.Prepare("SELECT id, address, role, name FROM raft_nodes ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to fetch raft nodes")
	}
	return nodes, nil
}

// GetRaftNodeAddresses returns the addresses of all LXD nodes that are members of
// the dqlite Raft cluster (possibly including the local node). If this LXD
// instance is not running in clustered mode, an empty list is returned.
func (n *NodeTx) GetRaftNodeAddresses() ([]string, error) {
	return query.SelectStrings(n.tx, "SELECT address FROM raft_nodes")
}

// GetRaftNodeAddress returns the address of the LXD raft node with the given ID,
// if any matching row exists.
func (n *NodeTx) GetRaftNodeAddress(id int64) (string, error) {
	stmt := "SELECT address FROM raft_nodes WHERE id=?"
	addresses, err := query.SelectStrings(n.tx, stmt, id)
	if err != nil {
		return "", err
	}
	switch len(addresses) {
	case 0:
		return "", ErrNoSuchObject
	case 1:
		return addresses[0], nil
	default:
		// This should never happen since we have a UNIQUE constraint
		// on the raft_nodes.id column.
		return "", fmt.Errorf("more than one match found")
	}
}

// CreateFirstRaftNode adds a the first node of the cluster. It ensures that the
// database ID is 1, to match the server ID of the first raft log entry.
//
// This method is supposed to be called when there are no rows in raft_nodes,
// and it will replace whatever existing row has ID 1.
func (n *NodeTx) CreateFirstRaftNode(address string, name string) error {
	columns := []string{"id", "address", "name"}
	values := []interface{}{int64(1), address, name}
	id, err := query.UpsertObject(n.tx, "raft_nodes", columns, values)
	if err != nil {
		return err
	}
	if id != 1 {
		return fmt.Errorf("could not set raft node ID to 1")
	}
	return nil
}

// CreateRaftNode adds a node to the current list of LXD nodes that are part of the
// dqlite Raft cluster. It returns the ID of the newly inserted row.
func (n *NodeTx) CreateRaftNode(address string, name string) (int64, error) {
	columns := []string{"address", "name"}
	values := []interface{}{address, name}
	return query.UpsertObject(n.tx, "raft_nodes", columns, values)
}

// RemoveRaftNode removes a node from the current list of LXD nodes that are
// part of the dqlite Raft cluster.
func (n *NodeTx) RemoveRaftNode(id int64) error {
	deleted, err := query.DeleteObject(n.tx, "raft_nodes", id)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNoSuchObject
	}
	return nil
}

// ReplaceRaftNodes replaces the current list of raft nodes.
func (n *NodeTx) ReplaceRaftNodes(nodes []RaftNode) error {
	_, err := n.tx.Exec("DELETE FROM raft_nodes")
	if err != nil {
		return err
	}

	columns := []string{"id", "address", "role", "name"}
	for _, node := range nodes {
		values := []interface{}{node.ID, node.Address, node.Role, node.Name}
		_, err := query.UpsertObject(n.tx, "raft_nodes", columns, values)
		if err != nil {
			return err
		}
	}
	return nil
}
