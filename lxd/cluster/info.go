package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/inconshreveable/log15.v2"

	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/node"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/logger"
)

// Load information about the dqlite node associated with this LXD member
// should have, such as its ID, address and role.
func loadInfo(database *db.Node, cert *shared.CertInfo) (*db.RaftNode, error) {
	// Figure out if we actually need to act as dqlite node.
	var info *db.RaftNode
	err := database.Transaction(func(tx *db.NodeTx) error {
		var err error
		info, err = node.DetermineRaftNode(tx)
		return err
	})
	if err != nil {
		return nil, err
	}

	// If we're not part of the dqlite cluster, there's nothing to do.
	if info == nil {
		return nil, nil
	}

	if info.Address == "" {
		// This is a standalone node not exposed to the network.
		info.Address = "1"
	}

	logger.Info("Starting database node", log15.Ctx{"id": info.ID, "local": info.Address, "role": info.Role})

	// Rename legacy data directory if needed.
	dir := filepath.Join(database.Dir(), "global")
	legacyDir := filepath.Join(database.Dir(), "..", "raft")
	if shared.PathExists(legacyDir) {
		if shared.PathExists(dir) {
			return nil, fmt.Errorf("both legacy and new global database directories exist")
		}
		logger.Info("Renaming global database directory from raft/ to database/global/")
		err := os.Rename(legacyDir, dir)
		if err != nil {
			return nil, fmt.Errorf("failed to rename legacy global database directory: %w", err)
		}
	}

	// Data directory
	if !shared.PathExists(dir) {
		err := os.Mkdir(dir, 0750)
		if err != nil {
			return nil, err
		}
	}

	return info, nil
}
