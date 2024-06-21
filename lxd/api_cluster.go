package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/canonical/lxd/client"
	"github.com/canonical/lxd/lxd/cluster"
	clusterRequest "github.com/canonical/lxd/lxd/cluster/request"
	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/lifecycle"
	"github.com/canonical/lxd/lxd/node"
	"github.com/canonical/lxd/lxd/operations"
	"github.com/canonical/lxd/lxd/project"
	"github.com/canonical/lxd/lxd/request"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/lxd/revert"
	"github.com/canonical/lxd/lxd/util"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/logger"
	"github.com/canonical/lxd/shared/osarch"
	"github.com/canonical/lxd/shared/version"
)

var targetGroupPrefix = "@"

var clusterCmd = APIEndpoint{
	Path: "cluster",

	Get: APIEndpointAction{Handler: clusterGet, AccessHandler: allowAuthenticated},
	Put: APIEndpointAction{Handler: clusterPut},
}

var clusterNodesCmd = APIEndpoint{
	Path: "cluster/members",

	Get:  APIEndpointAction{Handler: clusterNodesGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: clusterNodesPost},
}

var clusterNodeCmd = APIEndpoint{
	Path: "cluster/members/{name}",

	Delete: APIEndpointAction{Handler: clusterNodeDelete},
	Get:    APIEndpointAction{Handler: clusterNodeGet, AccessHandler: allowAuthenticated},
	Patch:  APIEndpointAction{Handler: clusterNodePatch},
	Put:    APIEndpointAction{Handler: clusterNodePut},
	Post:   APIEndpointAction{Handler: clusterNodePost},
}

var clusterCertificateCmd = APIEndpoint{
	Path: "cluster/certificate",

	Put: APIEndpointAction{Handler: clusterCertificatePut},
}

var internalClusterAcceptCmd = APIEndpoint{
	Path: "cluster/accept",

	Post: APIEndpointAction{Handler: internalClusterPostAccept},
}

var internalClusterRebalanceCmd = APIEndpoint{
	Path: "cluster/rebalance",

	Post: APIEndpointAction{Handler: internalClusterPostRebalance},
}

var internalClusterAssignCmd = APIEndpoint{
	Path: "cluster/assign",

	Post: APIEndpointAction{Handler: internalClusterPostAssign},
}

var internalClusterHandoverCmd = APIEndpoint{
	Path: "cluster/handover",

	Post: APIEndpointAction{Handler: internalClusterPostHandover},
}

var internalClusterRaftNodeCmd = APIEndpoint{
	Path: "cluster/raft-node/{address}",

	Delete: APIEndpointAction{Handler: internalClusterRaftNodeDelete},
}

// swagger:operation GET /1.0/cluster cluster cluster_get
//
// Get the cluster configuration
//
// Gets the current cluster configuration.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Cluster configuration
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Cluster"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterGet(d *Daemon, r *http.Request) response.Response {
	name := ""
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		name, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// If the name is set to the hard-coded default node name, then
	// clustering is not enabled.
	if name == "none" {
		name = ""
	}

	memberConfig, err := clusterGetMemberConfig(d.cluster)
	if err != nil {
		return response.SmartError(err)
	}

	cluster := api.Cluster{
		ServerName:   name,
		Enabled:      name != "",
		MemberConfig: memberConfig,
	}

	return response.SyncResponseETag(true, cluster, cluster)
}

// Fetch information about all node-specific configuration keys set on the
// storage pools and networks of this cluster.
func clusterGetMemberConfig(cluster *db.Cluster) ([]api.ClusterMemberConfigKey, error) {
	var pools map[string]map[string]string
	var networks map[string]map[string]string

	keys := []api.ClusterMemberConfigKey{}

	err := cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error

		pools, err = tx.GetStoragePoolsLocalConfig()
		if err != nil {
			return fmt.Errorf("Failed to fetch storage pools configuration: %w", err)
		}

		networks, err = tx.GetNetworksLocalConfig()
		if err != nil {
			return fmt.Errorf("Failed to fetch networks configuration: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	for pool, config := range pools {
		for key := range config {
			if strings.HasPrefix(key, shared.ConfigVolatilePrefix) {
				continue
			}

			key := api.ClusterMemberConfigKey{
				Entity:      "storage-pool",
				Name:        pool,
				Key:         key,
				Description: fmt.Sprintf("\"%s\" property for storage pool \"%s\"", key, pool),
			}
			keys = append(keys, key)
		}
	}

	for network, config := range networks {
		for key := range config {
			if strings.HasPrefix(key, shared.ConfigVolatilePrefix) {
				continue
			}

			key := api.ClusterMemberConfigKey{
				Entity:      "network",
				Name:        network,
				Key:         key,
				Description: fmt.Sprintf("\"%s\" property for network \"%s\"", key, network),
			}
			keys = append(keys, key)
		}
	}

	return keys, nil
}

// Depending on the parameters passed and on local state this endpoint will
// either:
//
// - bootstrap a new cluster (if this node is not clustered yet)
// - request to join an existing cluster
// - disable clustering on a node
//
// The client is required to be trusted.

// swagger:operation PUT /1.0/cluster cluster cluster_put
//
// Update the cluster configuration
//
// Updates the entire cluster configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterPut(d *Daemon, r *http.Request) response.Response {
	req := api.ClusterPut{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.ServerName == "" && req.Enabled {
		return response.BadRequest(fmt.Errorf("ServerName is required when enabling clustering"))
	}
	if req.ServerName != "" && !req.Enabled {
		return response.BadRequest(fmt.Errorf("ServerName must be empty when disabling clustering"))
	}

	if req.ServerName != "" && strings.HasPrefix(req.ServerName, targetGroupPrefix) {
		return response.BadRequest(fmt.Errorf("ServerName may not start with %q", targetGroupPrefix))
	}

	// Disable clustering.
	if !req.Enabled {
		return clusterPutDisable(d, r, req)
	}

	// Depending on the provided parameters we either bootstrap a brand new
	// cluster with this node as first node, or perform a request to join a
	// given cluster.
	if req.ClusterAddress == "" {
		return clusterPutBootstrap(d, r, req)
	}

	return clusterPutJoin(d, r, req)
}

func clusterPutBootstrap(d *Daemon, r *http.Request, req api.ClusterPut) response.Response {
	logger.Info("Bootstrapping cluster", log.Ctx{"serverName": req.ServerName})

	run := func(op *operations.Operation) error {
		// Start clustering tasks
		d.startClusterTasks()

		err := cluster.Bootstrap(d.State(), d.gateway, req.ServerName)
		if err != nil {
			d.stopClusterTasks()
			return err
		}

		// Restart the networks (to pickup forkdns and the like).
		err = networkStartup(d.State())
		if err != nil {
			return err
		}

		d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterEnabled.Event(req.ServerName, op.Requestor(), nil))

		return nil
	}
	resources := map[string][]string{}
	resources["cluster"] = []string{}

	// If there's no cluster.https_address set, but core.https_address is,
	// let's default to it.
	err := d.db.Transaction(func(tx *db.NodeTx) error {
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return fmt.Errorf("Failed to fetch member configuration: %w", err)
		}

		clusterAddress := config.ClusterAddress()
		if clusterAddress != "" {
			return nil
		}

		address := config.HTTPSAddress()

		if util.IsWildCardAddress(address) {
			return fmt.Errorf("Cannot use wildcard core.https_address %q for cluster.https_address. Please specify a new cluster.https_address or core.https_address", address)
		}

		_, err = config.Patch(map[string]interface{}{
			"cluster.https_address": address,
		})
		if err != nil {
			return fmt.Errorf("Copy core.https_address to cluster.https_address: %w", err)
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	op, err := operations.OperationCreate(d.State(), "", operations.OperationClassTask, db.OperationClusterBootstrap, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	// Add the cluster flag from the agent
	version.UserAgentFeatures([]string{"cluster"})

	return operations.OperationResponse(op)
}

func clusterPutJoin(d *Daemon, r *http.Request, req api.ClusterPut) response.Response {
	logger.Info("Joining cluster", log.Ctx{"serverName": req.ServerName})

	// Make sure basic pre-conditions are met.
	if len(req.ClusterCertificate) == 0 {
		return response.BadRequest(fmt.Errorf("No target cluster member certificate provided"))
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	if clustered {
		return response.BadRequest(fmt.Errorf("This server is already clustered"))
	}

	// The old pre 'clustering_join' join API approach is no longer supported.
	if req.ServerAddress == "" {
		return response.BadRequest(fmt.Errorf("No server address provided for this member"))
	}

	address, err := node.HTTPSAddress(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	if address == "" {
		// As the user always provides a server address, but no networking
		// was setup on this node, let's do the job and open the
		// port. We'll use the same address both for the REST API and
		// for clustering.

		// First try to listen to the provided address. If we fail, we
		// won't actually update the database config.
		err = d.endpoints.NetworkUpdateAddress(req.ServerAddress)
		if err != nil {
			return response.SmartError(err)
		}

		err := d.db.Transaction(func(tx *db.NodeTx) error {
			config, err := node.ConfigLoad(tx)
			if err != nil {
				return fmt.Errorf("Failed to load cluster config: %w", err)
			}

			_, err = config.Patch(map[string]interface{}{
				"core.https_address":    req.ServerAddress,
				"cluster.https_address": req.ServerAddress,
			})
			return err
		})
		if err != nil {
			return response.SmartError(err)
		}

		address = req.ServerAddress
	} else {
		// The user has previously set core.https_address and
		// is now providing a cluster address as well. If they
		// differ we need to listen to it.
		if !util.IsAddressCovered(req.ServerAddress, address) {
			err := d.endpoints.ClusterUpdateAddress(req.ServerAddress)
			if err != nil {
				return response.SmartError(err)
			}
			address = req.ServerAddress
		}

		// Update the cluster.https_address config key.
		err := d.db.Transaction(func(tx *db.NodeTx) error {
			config, err := node.ConfigLoad(tx)
			if err != nil {
				return fmt.Errorf("Failed to load cluster config: %w", err)
			}
			_, err = config.Patch(map[string]interface{}{
				"cluster.https_address": address,
			})
			return err
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	// Client parameters to connect to the target cluster node.
	serverCert := d.serverCert()
	args := &lxd.ConnectionArgs{
		TLSClientCert: string(serverCert.PublicKey()),
		TLSClientKey:  string(serverCert.PrivateKey()),
		TLSServerCert: string(req.ClusterCertificate),
		UserAgent:     version.UserAgent,
	}

	// Asynchronously join the cluster.
	run := func(op *operations.Operation) error {
		logger.Debug("Running cluster join operation")

		// If the user has provided a cluster password, setup the trust
		// relationship by adding our own certificate to the cluster.
		if req.ClusterPassword != "" {
			err = cluster.SetupTrust(serverCert, req.ServerName, req.ClusterAddress, req.ClusterCertificate, req.ClusterPassword)
			if err != nil {
				return fmt.Errorf("Failed to setup cluster trust: %w", err)
			}
		}

		// Now we are in the remote trust store, ensure our name and type are correct to allow the cluster
		// to associate our member name to the server certificate.
		err = cluster.UpdateTrust(serverCert, req.ServerName, req.ClusterAddress, req.ClusterCertificate)
		if err != nil {
			return fmt.Errorf("Failed to update cluster trust: %w", err)
		}

		// Connect to the target cluster node.
		client, err := lxd.ConnectLXD(fmt.Sprintf("https://%s", req.ClusterAddress), args)
		if err != nil {
			return err
		}

		// As ServerAddress field is required to be set it means that we're using the new join API
		// introduced with the 'clustering_join' extension.
		// Connect to ourselves to initialize storage pools and networks using the API.
		localClient, err := lxd.ConnectLXDUnix(d.UnixSocket(), &lxd.ConnectionArgs{UserAgent: clusterRequest.UserAgentJoiner})
		if err != nil {
			return fmt.Errorf("Failed to connect to local LXD: %w", err)
		}

		revert := revert.New()
		defer revert.Fail()

		localRevert, err := clusterInitMember(localClient, client, req.MemberConfig)
		if err != nil {
			return fmt.Errorf("Failed to initialize member: %w", err)
		}
		revert.Add(localRevert)

		// Get all defined storage pools and networks, so they can be compared to the ones in the cluster.
		pools := []api.StoragePool{}
		poolNames, err := d.cluster.GetStoragePoolNames()
		if err != nil && err != db.ErrNoSuchObject {
			return err
		}

		for _, name := range poolNames {
			_, pool, _, err := d.cluster.GetStoragePoolInAnyState(name)
			if err != nil {
				return err
			}
			pools = append(pools, *pool)
		}

		networks := []api.Network{}
		networkNames, err := d.cluster.GetNetworks()
		if err != nil && err != db.ErrNoSuchObject {
			return err
		}

		for _, name := range networkNames {
			_, network, _, err := d.cluster.GetNetworkInAnyState(name)
			if err != nil {
				return err
			}
			networks = append(networks, *network)
		}

		// Now request for this node to be added to the list of cluster nodes.
		info, err := clusterAcceptMember(
			client, req.ServerName, address, cluster.SchemaVersion,
			version.APIExtensionsCount(), pools, networks)
		if err != nil {
			return fmt.Errorf("Failed request to add member: %w", err)
		}

		// Update our TLS configuration using the returned cluster certificate.
		err = util.WriteCert(d.os.VarDir, "cluster", []byte(req.ClusterCertificate), info.PrivateKey, nil)
		if err != nil {
			return fmt.Errorf("Failed to save cluster certificate: %w", err)
		}

		networkCert, err := util.LoadClusterCert(d.os.VarDir)
		if err != nil {
			return fmt.Errorf("Failed to parse cluster certificate: %w", err)
		}

		d.endpoints.NetworkUpdateCert(networkCert)

		// Add trusted certificates of other members to local trust store.
		trustedCerts, err := client.GetCertificates()
		if err != nil {
			return fmt.Errorf("Failed to get trusted certificates: %w", err)
		}

		for _, trustedCert := range trustedCerts {
			if trustedCert.Type == api.CertificateTypeServer {
				dbType, err := db.CertificateAPITypeToDBType(trustedCert.Type)
				if err != nil {
					return err
				}

				// Store the certificate in the local database.
				dbCert := db.Certificate{
					Fingerprint: trustedCert.Fingerprint,
					Type:        dbType,
					Name:        trustedCert.Name,
					Certificate: trustedCert.Certificate,
				}

				logger.Debugf("Adding certificate %q (%s) to local trust store", trustedCert.Name, trustedCert.Fingerprint)
				err = d.cluster.CreateCertificate(dbCert)
				if err != nil && err.Error() != "This certificate already exists" {
					return fmt.Errorf("Failed adding local trusted certificate %q (%s): %w", trustedCert.Name, trustedCert.Fingerprint, err)
				}
			}
		}

		// Update cached trusted certificates.
		updateCertificateCache(d)

		// Update local setup and possibly join the raft dqlite cluster.
		nodes := make([]db.RaftNode, len(info.RaftNodes))
		for i, node := range info.RaftNodes {
			nodes[i].ID = node.ID
			nodes[i].Address = node.Address
			nodes[i].Role = db.RaftRole(node.Role)
		}

		err = cluster.Join(d.State(), d.gateway, networkCert, serverCert, req.ServerName, nodes)
		if err != nil {
			return err
		}

		// Start clustering tasks.
		d.startClusterTasks()
		revert.Add(func() { d.stopClusterTasks() })

		// Handle optional service integration on cluster join
		var clusterConfig *cluster.Config
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			var err error
			clusterConfig, err = cluster.ConfigLoad(tx)
			return err
		})
		if err != nil {
			return err
		}
		var nodeConfig *node.Config
		err = d.db.Transaction(func(tx *db.NodeTx) error {
			var err error
			nodeConfig, err = node.ConfigLoad(tx)
			return err
		})
		if err != nil {
			return err
		}

		// Connect to MAAS
		url, key := clusterConfig.MAASController()
		machine := nodeConfig.MAASMachine()
		err = d.setupMAASController(url, key, machine)
		if err != nil {
			return err
		}

		// Handle external authentication/RBAC
		candidAPIURL, candidAPIKey, candidExpiry, candidDomains := clusterConfig.CandidServer()
		rbacAPIURL, rbacAPIKey, rbacExpiry, rbacAgentURL, rbacAgentUsername, rbacAgentPrivateKey, rbacAgentPublicKey := clusterConfig.RBACServer()

		if rbacAPIURL != "" {
			err = d.setupRBACServer(rbacAPIURL, rbacAPIKey, rbacExpiry, rbacAgentURL, rbacAgentUsername, rbacAgentPrivateKey, rbacAgentPublicKey)
			if err != nil {
				return err
			}
		}

		if candidAPIURL != "" {
			err = d.setupExternalAuthentication(candidAPIURL, candidAPIKey, candidExpiry, candidDomains)
			if err != nil {
				return err
			}
		}

		// Start up networks so any post-join changes can be applied now that we have a Node ID.
		logger.Debug("Starting networks after cluster join")
		err = networkStartup(d.State())
		if err != nil {
			logger.Errorf("Failed starting networks: %v", err)
		}

		client, err = cluster.Connect(req.ClusterAddress, d.endpoints.NetworkCert(), serverCert, r, true)
		if err != nil {
			return err
		}

		// Add the cluster flag from the agent
		version.UserAgentFeatures([]string{"cluster"})

		// Notify the leader of successful join, possibly triggering
		// role changes.
		_, _, err = client.RawQuery("POST", "/internal/cluster/rebalance", nil, "")
		if err != nil {
			logger.Warnf("Failed to trigger cluster rebalance: %v", err)
		}

		// Ensure all images are available after this node has joined.
		err = autoSyncImages(d.shutdownCtx, d)
		if err != nil {
			logger.Warn("Failed to sync images")
		}

		d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterMemberAdded.Event(req.ServerName, op.Requestor(), nil))

		revert.Success()
		return nil
	}

	resources := map[string][]string{}
	resources["cluster"] = []string{}

	op, err := operations.OperationCreate(d.State(), "", operations.OperationClassTask, db.OperationClusterJoin, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

// clusterPutDisableMu is used to prevent the LXD process from being replaced/stopped during removal from the
// cluster until such time as the request that initiated the removal has finished. This allows for self removal
// from the cluster when not the leader.
var clusterPutDisableMu sync.Mutex

// Disable clustering on a node.
func clusterPutDisable(d *Daemon, r *http.Request, req api.ClusterPut) response.Response {
	logger.Info("Disabling clustering", log.Ctx{"serverName": req.ServerName})

	// Close the cluster database
	err := d.cluster.Close()
	if err != nil {
		return response.SmartError(err)
	}

	// Update our TLS configuration using our original certificate.
	for _, suffix := range []string{"crt", "key", "ca"} {
		path := filepath.Join(d.os.VarDir, "cluster."+suffix)
		if !shared.PathExists(path) {
			continue
		}
		err := os.Remove(path)
		if err != nil {
			return response.InternalError(err)
		}
	}

	networkCert, err := util.LoadCert(d.os.VarDir)
	if err != nil {
		return response.InternalError(fmt.Errorf("Failed to parse member certificate: %w", err))
	}

	// Reset the cluster database and make it local to this node.
	err = d.gateway.Reset(networkCert)
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterDisabled.Event(req.ServerName, requestor, nil))

	// Stop database cluster connection.
	d.gateway.Kill()

	go func() {
		<-r.Context().Done() // Wait until request has finished.

		// Wait until we can acquire the lock. This way if another request is holding the lock we won't
		// replace/stop the LXD daemon until that request has finished.
		clusterPutDisableMu.Lock()
		defer clusterPutDisableMu.Unlock()

		if d.systemdSocketActivated {
			logger.Info("Exiting LXD daemon following removal from cluster")
			os.Exit(0)
		} else {
			logger.Info("Restarting LXD daemon following removal from cluster")
			err = util.ReplaceDaemon()
			if err != nil {
				logger.Error("Failed restarting LXD daemon", log.Ctx{"err": err})
			}
		}
	}()

	return response.ManualResponse(func(w http.ResponseWriter) error {
		err := response.EmptySyncResponse.Render(w)
		if err != nil {
			return err
		}

		// Send the response before replacing the LXD daemon process.
		f, ok := w.(http.Flusher)
		if ok {
			f.Flush()
		} else {
			return fmt.Errorf("http.ResponseWriter is not type http.Flusher")
		}

		return nil
	})
}

// clusterInitMember initialises storage pools and networks on this node. We pass two LXD client instances, one
// connected to ourselves (the joining node) and one connected to the target cluster node to join.
// Returns a revert function that can be used to undo the setup if a subsequent step fails.
func clusterInitMember(d lxd.InstanceServer, client lxd.InstanceServer, memberConfig []api.ClusterMemberConfigKey) (func(), error) {
	data := initDataNode{}

	// Fetch all pools currently defined in the cluster.
	pools, err := client.GetStoragePools()
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch information about cluster storage pools: %w", err)
	}

	// Merge the returned storage pools configs with the node-specific
	// configs provided by the user.
	for _, pool := range pools {
		// Skip pending pools.
		if pool.Status == "Pending" {
			continue
		}

		logger.Debugf("Populating init data for storage pool %q", pool.Name)

		post := api.StoragePoolsPost{
			StoragePoolPut: pool.StoragePoolPut,
			Driver:         pool.Driver,
			Name:           pool.Name,
		}

		// Delete config keys that are automatically populated by LXD
		delete(post.Config, "volatile.initial_source")
		delete(post.Config, "zfs.pool_name")

		// Apply the node-specific config supplied by the user.
		for _, config := range memberConfig {
			if config.Entity != "storage-pool" {
				continue
			}

			if config.Name != pool.Name {
				continue
			}

			if !shared.StringInSlice(config.Key, db.StoragePoolNodeConfigKeys) {
				logger.Warnf("Ignoring config key %q for storage pool %q", config.Key, config.Name)
				continue
			}

			post.Config[config.Key] = config.Value
		}

		data.StoragePools = append(data.StoragePools, post)
	}

	// Fetch all networks currently defined in the cluster.
	networks, err := client.GetNetworks()
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch project information about cluster networks: %w", err)
	}

	// Merge the returned storage networks configs with the node-specific
	// configs provided by the user.
	for _, network := range networks {
		// Skip not-managed or pending networks
		if !network.Managed || network.Status != api.NetworkStatusCreated {
			continue
		}

		post := api.NetworksPost{
			NetworkPut: network.NetworkPut,
			Name:       network.Name,
			Type:       network.Type,
		}

		// Apply the node-specific config supplied by the user.
		for _, config := range memberConfig {
			if config.Entity != "network" {
				continue
			}

			if config.Name != network.Name {
				continue
			}

			if !shared.StringInSlice(config.Key, db.NodeSpecificNetworkConfig) {
				logger.Warnf("Ignoring config key %q for network %q", config.Key, config.Name)
				continue
			}

			post.Config[config.Key] = config.Value
		}

		data.Networks = append(data.Networks, post)
	}

	revert, err := initDataNodeApply(d, data)
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize storage pools and networks: %w", err)
	}

	return revert, nil
}

// Perform a request to the /internal/cluster/accept endpoint to check if a new
// node can be accepted into the cluster and obtain joining information such as
// the cluster private certificate.
func clusterAcceptMember(client lxd.InstanceServer, name string, address string, schema int, apiExt int, pools []api.StoragePool, networks []api.Network) (*internalClusterPostAcceptResponse, error) {
	architecture, err := osarch.ArchitectureGetLocalID()
	if err != nil {
		return nil, err
	}

	req := internalClusterPostAcceptRequest{
		Name:         name,
		Address:      address,
		Schema:       schema,
		API:          apiExt,
		StoragePools: pools,
		Networks:     networks,
		Architecture: architecture,
	}
	info := &internalClusterPostAcceptResponse{}
	resp, _, err := client.RawQuery("POST", "/internal/cluster/accept", req, "")
	if err != nil {
		return nil, err
	}

	err = resp.MetadataAsStruct(&info)
	if err != nil {
		return nil, err
	}

	return info, nil
}

// swagger:operation GET /1.0/cluster/members cluster cluster_members_get
//
// Get the cluster members
//
// Returns a list of cluster members (URLs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           type: array
//           description: List of endpoints
//           items:
//             type: string
//           example: |-
//             [
//               "/1.0/cluster/members/lxd01",
//               "/1.0/cluster/members/lxd02"
//             ]
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/cluster/members?recursion=1 cluster cluster_members_get_recursion1
//
// Get the cluster members
//
// Returns a list of cluster members (structs).
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: API endpoints
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           type: array
//           description: List of cluster members
//           items:
//             $ref: "#/definitions/ClusterMember"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodesGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)
	state := d.State()

	var err error
	var nodes []db.NodeInfo
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the nodes.
		nodes, err = tx.GetNodes()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}

	if recursion {
		result := []api.ClusterMember{}
		for _, node := range nodes {
			member, err := node.ToAPI(state.Cluster, state.Node, leader)
			if err != nil {
				return response.InternalError(err)
			}

			result = append(result, *member)
		}

		return response.SyncResponse(true, result)
	}

	urls := []string{}
	for _, node := range nodes {
		url := fmt.Sprintf("/%s/cluster/members/%s", version.APIVersion, node.Name)
		urls = append(urls, url)
	}

	return response.SyncResponse(true, urls)
}

var clusterNodesPostMu sync.Mutex // Used to prevent races when creating cluster join tokens.

// swagger:operation POST /1.0/cluster/members cluster cluster_members_post
//
// Request a join token
//
// Requests a join token to add a cluster member.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster member add request
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterMembersPost"
// responses:
//   "202":
//     $ref: "#/responses/Operation"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodesPost(d *Daemon, r *http.Request) response.Response {
	req := api.ClusterMembersPost{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	if !clustered {
		return response.BadRequest(fmt.Errorf("This server is not clustered"))
	}

	// Get target addresses for existing online members, so that it can be encoded into the join token so that
	// the joining member will not have to specify a joining address during the join process.
	// Use anonymous interface type to align with how the API response will be returned for consistency when
	// retrieving remote operations.
	onlineNodeAddresses := make([]interface{}, 0)

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the offline threshold.
		config, err := cluster.ConfigLoad(tx)
		if err != nil {
			return fmt.Errorf("Failed to load LXD config: %w", err)
		}

		// Get the nodes.
		nodes, err := tx.GetNodes()
		if err != nil {
			return err
		}

		// Filter to online members.
		for _, node := range nodes {
			if node.IsOffline(config.OfflineThreshold()) {
				continue
			}

			onlineNodeAddresses = append(onlineNodeAddresses, node.Address)
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	if len(onlineNodeAddresses) < 1 {
		return response.InternalError(fmt.Errorf("There are no online cluster members"))
	}

	// Lock to prevent concurrent requests racing the operationsGetByType function and creating duplicates.
	// We have to do this because collecting all of the operations from existing cluster members can take time.
	clusterNodesPostMu.Lock()
	defer clusterNodesPostMu.Unlock()

	// Remove any existing join tokens for the requested cluster member, this way we only ever have one active
	// join token for each potential new member, and it has the most recent active members list for joining.
	// This also ensures any historically unused (but potentially published) join tokens are removed.
	ops, err := operationsGetByType(d, r, project.Default, db.OperationClusterJoinToken)
	if err != nil {
		return response.InternalError(fmt.Errorf("Failed getting cluster join token operations: %w", err))
	}

	for _, op := range ops {
		if op.StatusCode != api.Running {
			continue // Tokens are single use, so if cancelled but not deleted yet its not available.
		}

		opServerName, ok := op.Metadata["serverName"]
		if !ok {
			continue
		}

		if opServerName == req.ServerName {
			// Join token operation matches requested server name, so lets cancel it.
			logger.Warn("Cancelling duplicate join token operation", log.Ctx{"operation": op.ID, "serverName": opServerName})
			err = operationCancel(d, r, project.Default, op)
			if err != nil {
				return response.InternalError(fmt.Errorf("Failed to cancel operation %q: %w", op.ID, err))
			}
		}
	}

	// Generate join secret for new member. This will be stored inside the join token operation and will be
	// supplied by the joining member (encoded inside the join token) which will allow us to lookup the correct
	// operation in order to validate the requested joining server name is correct and authorised.
	joinSecret, err := shared.RandomCryptoString()
	if err != nil {
		return response.InternalError(err)
	}

	// Generate fingerprint of network certificate so joining member can automatically trust the correct
	// certificate when it is presented during the join process.
	fingerprint, err := shared.CertFingerprintStr(string(d.endpoints.NetworkPublicKey()))
	if err != nil {
		return response.InternalError(err)
	}

	meta := map[string]interface{}{
		"serverName":  req.ServerName, // Add server name to allow validation of name during join process.
		"secret":      joinSecret,
		"fingerprint": fingerprint,
		"addresses":   onlineNodeAddresses,
	}

	resources := map[string][]string{}
	resources["cluster"] = []string{}

	op, err := operations.OperationCreate(d.State(), project.Default, operations.OperationClassToken, db.OperationClusterJoinToken, resources, meta, nil, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterTokenCreated.Event("members", op.Requestor(), nil))

	return operations.OperationResponse(op)
}

// swagger:operation GET /1.0/cluster/members/{name} cluster cluster_member_get
//
// Get the cluster member
//
// Gets a specific cluster member.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Profile
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/ClusterMember"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodeGet(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]
	state := d.State()

	var err error
	var nodes []db.NodeInfo
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node.
		nodes, err = tx.GetNodes()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}

	for _, node := range nodes {
		if node.Name != name {
			continue
		}

		member, err := node.ToAPI(state.Cluster, state.Node, leader)
		if err != nil {
			return response.InternalError(err)
		}

		return response.SyncResponseETag(true, member, member.ClusterMemberPut)
	}

	return response.NotFound(fmt.Errorf("Member '%s' not found", name))
}

// swagger:operation PATCH /1.0/cluster/members/{name} cluster cluster_member_patch
//
// Partially update the cluster member
//
// Updates a subset of the cluster member configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster member configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterMemberPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodePatch(d *Daemon, r *http.Request) response.Response {
	// Right now, Patch does the same as Put.
	return clusterNodePut(d, r)
}

// swagger:operation PUT /1.0/cluster/members/{name} cluster cluster_member_put
//
// Update the cluster member
//
// Updates the entire cluster member configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster member configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterMemberPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodePut(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]
	state := d.State()

	var err error
	var node db.NodeInfo
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node.
		node, err = tx.GetNodeByName(name)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}

	member, err := node.ToAPI(state.Cluster, state.Node, leader)
	if err != nil {
		return response.InternalError(err)
	}

	// Validate the request is fine
	err = util.EtagCheck(r, member.ClusterMemberPut)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Parse the request
	req := api.ClusterMemberPut{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Validate the request
	if shared.StringInSlice(string(db.ClusterRoleDatabase), member.Roles) && !shared.StringInSlice(string(db.ClusterRoleDatabase), req.Roles) {
		return response.BadRequest(fmt.Errorf("The '%s' role cannot be dropped at this time", db.ClusterRoleDatabase))
	}

	if !shared.StringInSlice(string(db.ClusterRoleDatabase), member.Roles) && shared.StringInSlice(string(db.ClusterRoleDatabase), req.Roles) {
		return response.BadRequest(fmt.Errorf("The '%s' role cannot be added at this time", db.ClusterRoleDatabase))
	}

	// Update the database
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		nodeInfo, err := tx.GetNodeByName(name)
		if err != nil {
			return fmt.Errorf("Loading node information: %w", err)
		}

		// Update the description.
		if req.Description != member.Description {
			err = tx.SetDescription(nodeInfo.ID, req.Description)
			if err != nil {
				return fmt.Errorf("Update description: %w", err)
			}
		}

		// Update the roles.
		dbRoles := []db.ClusterRole{}
		for _, role := range req.Roles {
			dbRoles = append(dbRoles, db.ClusterRole(role))
		}

		err = tx.UpdateNodeRoles(node.ID, dbRoles)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterMemberUpdated.Event(name, requestor, nil))

	return response.EmptySyncResponse
}

// swagger:operation POST /1.0/cluster/members/{name} cluster cluster_member_post
//
// Rename the cluster member
//
// Renames an existing cluster member.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster member rename request
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterMemberPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodePost(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	req := api.ClusterMemberPost{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.RenameNode(name, req.ServerName)
	})
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterMemberRenamed.Event(req.ServerName, requestor, log.Ctx{"old_name": name}))

	return response.EmptySyncResponse
}

// swagger:operation DELETE /1.0/cluster/members/{name} cluster cluster_member_delete
//
// Delete the cluster member
//
// Removes the member from the cluster.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterNodeDelete(d *Daemon, r *http.Request) response.Response {
	force, err := strconv.Atoi(r.FormValue("force"))
	if err != nil {
		force = 0
	}

	name := mux.Vars(r)["name"]

	// Redirect all requests to the leader, which is the one with
	// knowning what nodes are part of the raft cluster.
	localAddress, err := node.ClusterAddress(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}

	var localInfo, leaderInfo db.NodeInfo
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		localInfo, err = tx.GetNodeByAddress(localAddress)
		if err != nil {
			return fmt.Errorf("Failed loading local member info %q: %w", localAddress, err)
		}

		leaderInfo, err = tx.GetNodeByAddress(leader)
		if err != nil {
			return fmt.Errorf("Failed loading leader member info %q: %w", leader, err)
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Get information about the cluster.
	var nodes []db.RaftNode
	err = d.db.Transaction(func(tx *db.NodeTx) error {
		var err error
		nodes, err = tx.GetRaftNodes()
		return err
	})
	if err != nil {
		return response.SmartError(fmt.Errorf("Unable to get raft nodes: %w", err))
	}

	if localAddress != leader {
		if localInfo.Name == name {
			// If the member being removed is ourselves and we are not the leader, then lock the
			// clusterPutDisableMu before we forward the request to the leader, so that when the leader
			// goes on to request clusterPutDisable back to ourselves it won't be actioned until we
			// have returned this request back to the original client.
			clusterPutDisableMu.Lock()
			logger.Info("Acquired cluster self removal lock", log.Ctx{"member": localInfo.Name})

			go func() {
				<-r.Context().Done() // Wait until request is finished.

				logger.Info("Releasing cluster self removal lock", log.Ctx{"member": localInfo.Name})
				clusterPutDisableMu.Unlock()
			}()
		}

		logger.Debugf("Redirect member delete request to %s", leader)
		client, err := cluster.Connect(leader, d.endpoints.NetworkCert(), d.serverCert(), r, false)
		if err != nil {
			return response.SmartError(err)
		}
		err = client.DeleteClusterMember(name, force == 1)
		if err != nil {
			return response.SmartError(err)
		}

		// If we are the only remaining node, wait until promotion to leader,
		// then update cluster certs.
		if name == leaderInfo.Name && len(nodes) == 2 {
			err = d.gateway.WaitLeadership()
			if err != nil {
				return response.SmartError(err)
			}

			updateCertificateCache(d)
		}

		return response.ManualResponse(func(w http.ResponseWriter) error {
			err := response.EmptySyncResponse.Render(w)
			if err != nil {
				return err
			}

			// Send the response before replacing the LXD daemon process.
			f, ok := w.(http.Flusher)
			if ok {
				f.Flush()
			} else {
				return fmt.Errorf("http.ResponseWriter is not type http.Flusher")
			}

			return nil
		})
	}

	// Get lock now we are on leader.
	d.clusterMembershipMutex.Lock()
	defer d.clusterMembershipMutex.Unlock()

	// If we are removing the leader of a 2 node cluster, ensure the other node can be a leader.
	if name == leaderInfo.Name && len(nodes) == 2 {
		for i := range nodes {
			if nodes[i].Address != leader && nodes[i].Role == db.RaftStandBy {
				// Promote the remaining node.
				nodes[i].Role = db.RaftVoter
				err := changeMemberRole(d, r, nodes[i].Address, nodes)
				if err != nil {
					return response.SmartError(fmt.Errorf("Unable to promote remaining cluster member to leader: %w", err))
				}

				break
			}
		}
	}

	logger.Info("Deleting member from cluster", log.Ctx{"name": name, "force": force})

	err = autoSyncImages(d.shutdownCtx, d)
	if err != nil {
		if force == 0 {
			return response.SmartError(fmt.Errorf("Failed to sync images: %w", err))
		}

		// If force is set, only show a warning instead of returning an error.
		logger.Warn("Failed to sync images")
	}

	// First check that the node is clear from containers and images and
	// make it leave the database cluster, if it's part of it.
	address, err := cluster.Leave(d.State(), d.gateway, name, force == 1)
	if err != nil {
		return response.SmartError(err)
	}

	if force != 1 {
		// Try to gracefully delete all networks and storage pools on it.
		// Delete all networks on this node
		client, err := cluster.Connect(address, d.endpoints.NetworkCert(), d.serverCert(), r, true)
		if err != nil {
			return response.SmartError(err)
		}

		networks, err := d.cluster.GetNetworks()
		if err != nil {
			return response.SmartError(err)
		}

		for _, name := range networks {
			err := client.DeleteNetwork(name)
			if err != nil {
				return response.SmartError(err)
			}
		}

		// Delete all the pools on this node
		pools, err := d.cluster.GetStoragePoolNames()
		if err != nil && err != db.ErrNoSuchObject {
			return response.SmartError(err)
		}

		for _, name := range pools {
			err := client.DeleteStoragePool(name)
			if err != nil {
				return response.SmartError(err)
			}
		}
	}

	// Remove node from the database
	err = cluster.Purge(d.cluster, name)
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed to remove member from database: %w", err))
	}

	err = rebalanceMemberRoles(d, r, nil)
	if err != nil {
		logger.Warnf("Failed to rebalance dqlite nodes: %v", err)
	}

	// If this leader node removed itself, just disable clustering.
	if address == localAddress {
		return clusterPutDisable(d, r, api.ClusterPut{})
	} else if force != 1 {
		// Try to gracefully reset the database on the node.
		client, err := cluster.Connect(address, d.endpoints.NetworkCert(), d.serverCert(), r, true)
		if err != nil {
			return response.SmartError(err)
		}

		put := api.ClusterPut{}
		put.Enabled = false
		_, err = client.UpdateCluster(put, "")
		if err != nil {
			return response.SmartError(fmt.Errorf("Failed to cleanup the member: %w", err))
		}
	}

	// Refresh the trusted certificate cache now that the member certificate has been removed.
	// We do not need to notify the other members here because the next heartbeat will trigger member change
	// detection and updateCertificateCache is called as part of that.
	updateCertificateCache(d)

	// Ensure all images are available after this node has been deleted.
	err = autoSyncImages(d.shutdownCtx, d)
	if err != nil {
		logger.Warn("Failed to sync images")
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterMemberRemoved.Event(name, requestor, nil))

	return response.EmptySyncResponse
}

// swagger:operation PUT /1.0/cluster/certificate cluster clustering_update_cert
//
// Update the certificate for the cluster
//
// Replaces existing cluster certificate and reloads LXD on each cluster
// member.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: cluster
//     description: Cluster certificate replace request
//     required: true
//     schema:
//       $ref: "#/definitions/ClusterCertificatePut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func clusterCertificatePut(d *Daemon, r *http.Request) response.Response {
	req := api.ClusterCertificatePut{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	certBytes := []byte(req.ClusterCertificate)
	keyBytes := []byte(req.ClusterCertificateKey)

	certBlock, _ := pem.Decode(certBytes)
	if certBlock == nil {
		return response.BadRequest(fmt.Errorf("Certificate must be base64 encoded PEM certificate: %w", err))
	}

	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil {
		return response.BadRequest(fmt.Errorf("Private key must be base64 encoded PEM key: %w", err))
	}

	// First node forwards request to all other cluster nodes
	if !isClusterNotification(r) {
		servers, err := d.gateway.NodeStore().Get(context.Background())
		if err != nil {
			return response.SmartError(err)
		}

		localAddress, err := node.ClusterAddress(d.db)
		if err != nil {
			return response.SmartError(err)
		}

		for _, server := range servers {
			if server.Address == localAddress {
				continue
			}

			client, err := cluster.Connect(server.Address, d.endpoints.NetworkCert(), d.serverCert(), r, true)
			if err != nil {
				return response.SmartError(err)
			}

			err = client.UpdateClusterCertificate(req, "")
			if err != nil {
				return response.SmartError(err)
			}
		}
	}

	err = util.WriteCert(d.os.VarDir, "cluster", certBytes, keyBytes, nil)
	if err != nil {
		return response.SmartError(err)
	}

	// Get the new cluster certificate struct
	cert, err := util.LoadClusterCert(d.os.VarDir)
	if err != nil {
		return response.SmartError(err)
	}

	// Update the certificate on the network endpoint and gateway
	d.endpoints.NetworkUpdateCert(cert)
	d.gateway.NetworkUpdateCert(cert)

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectParam(r), lifecycle.ClusterCertificateUpdated.Event("certificate", requestor, nil))

	return response.EmptySyncResponse
}

func internalClusterPostAccept(d *Daemon, r *http.Request) response.Response {
	req := internalClusterPostAcceptRequest{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	// Redirect all requests to the leader, which is the one with
	// knowning what nodes are part of the raft cluster.
	address, err := node.ClusterAddress(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}
	if address != leader {
		logger.Debugf("Redirect member accept request to %s", leader)

		if leader == "" {
			return response.SmartError(fmt.Errorf("Unable to find leader address"))
		}

		url := &url.URL{
			Scheme: "https",
			Path:   "/internal/cluster/accept",
			Host:   leader,
		}
		return response.SyncResponseRedirect(url.String())
	}

	// Get lock now we are on leader.
	d.clusterMembershipMutex.Lock()
	defer d.clusterMembershipMutex.Unlock()

	// Check that the pools and networks provided by the joining node have
	// configs that match the cluster ones.
	err = clusterCheckStoragePoolsMatch(d.cluster, req.StoragePools)
	if err != nil {
		return response.SmartError(err)
	}
	err = clusterCheckNetworksMatch(d.cluster, req.Networks)
	if err != nil {
		return response.SmartError(err)
	}

	nodes, err := cluster.Accept(d.State(), d.gateway, req.Name, req.Address, req.Schema, req.API, req.Architecture)
	if err != nil {
		return response.BadRequest(err)
	}
	accepted := internalClusterPostAcceptResponse{
		RaftNodes:  make([]internalRaftNode, len(nodes)),
		PrivateKey: d.endpoints.NetworkPrivateKey(),
	}
	for i, node := range nodes {
		accepted.RaftNodes[i].ID = node.ID
		accepted.RaftNodes[i].Address = node.Address
		accepted.RaftNodes[i].Role = int(node.Role)
	}
	return response.SyncResponse(true, accepted)
}

// A request for the /internal/cluster/accept endpoint.
type internalClusterPostAcceptRequest struct {
	Name         string            `json:"name" yaml:"name"`
	Address      string            `json:"address" yaml:"address"`
	Schema       int               `json:"schema" yaml:"schema"`
	API          int               `json:"api" yaml:"api"`
	StoragePools []api.StoragePool `json:"storage_pools" yaml:"storage_pools"`
	Networks     []api.Network     `json:"networks" yaml:"networks"`
	Architecture int               `json:"architecture" yaml:"architecture"`
}

// A Response for the /internal/cluster/accept endpoint.
type internalClusterPostAcceptResponse struct {
	RaftNodes  []internalRaftNode `json:"raft_nodes" yaml:"raft_nodes"`
	PrivateKey []byte             `json:"private_key" yaml:"private_key"`
}

// Represent a LXD node that is part of the dqlite raft cluster.
type internalRaftNode struct {
	ID      uint64 `json:"id" yaml:"id"`
	Address string `json:"address" yaml:"address"`
	Role    int    `json:"role" yaml:"role"`
	Name    string `json:"name" yaml:"name"`
}

// Used to update the cluster after a database node has been removed, and
// possibly promote another one as database node.
func internalClusterPostRebalance(d *Daemon, r *http.Request) response.Response {
	// Redirect all requests to the leader, which is the one with with
	// up-to-date knowledge of what nodes are part of the raft cluster.
	localAddress, err := node.ClusterAddress(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}
	if localAddress != leader {
		logger.Debugf("Redirect cluster rebalance request to %s", leader)
		url := &url.URL{
			Scheme: "https",
			Path:   "/internal/cluster/rebalance",
			Host:   leader,
		}
		return response.SyncResponseRedirect(url.String())
	}

	// Get lock now we are on leader.
	d.clusterMembershipMutex.Lock()
	defer d.clusterMembershipMutex.Unlock()

	err = rebalanceMemberRoles(d, r, nil)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, nil)
}

// Check if there's a dqlite node whose role should be changed, and post a
// change role request if so.
func rebalanceMemberRoles(d *Daemon, r *http.Request, unavailableMembers []string) error {
	if d.shutdownCtx.Err() != nil {
		return nil
	}

again:
	address, nodes, err := cluster.Rebalance(d.State(), d.gateway, unavailableMembers)
	if err != nil {
		return err
	}

	if address == "" {
		// Nothing to do.
		return nil
	}

	// Process demotions of offline nodes immediately.
	for _, node := range nodes {
		if node.Address != address || node.Role != db.RaftSpare {
			continue
		}

		if cluster.HasConnectivity(d.endpoints.NetworkCert(), d.serverCert(), address) {
			break
		}

		logger.Info("Demoting offline member during rebalance", log.Ctx{"candidateAddress": node.Address})
		err := d.gateway.DemoteOfflineNode(node.ID)
		if err != nil {
			return fmt.Errorf("Demote offline node %s: %w", node.Address, err)
		}

		goto again
	}

	// Tell the node to promote itself.
	logger.Info("Promoting member during rebalance", log.Ctx{"candidateAddress": address})
	err = changeMemberRole(d, r, address, nodes)
	if err != nil {
		return err
	}

	goto again
}

// Check if there are nodes not part of the raft configuration and add them in
// case.
func upgradeNodesWithoutRaftRole(d *Daemon) error {
	if d.shutdownCtx.Err() != nil {
		return nil
	}

	var allNodes []db.NodeInfo
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		allNodes, err = tx.GetNodes()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed to get current cluster nodes: %w", err)

	}
	return cluster.UpgradeMembersWithoutRole(d.gateway, allNodes)
}

// Post a change role request to the member with the given address. The nodes
// slice contains details about all members, including the one being changed.
func changeMemberRole(d *Daemon, r *http.Request, address string, nodes []db.RaftNode) error {
	post := &internalClusterPostAssignRequest{}
	for _, node := range nodes {
		post.RaftNodes = append(post.RaftNodes, internalRaftNode{
			ID:      node.ID,
			Address: node.Address,
			Role:    int(node.Role),
			Name:    node.Name,
		})
	}

	client, err := cluster.Connect(address, d.endpoints.NetworkCert(), d.serverCert(), r, true)
	if err != nil {
		return err
	}

	_, _, err = client.RawQuery("POST", "/internal/cluster/assign", post, "")
	if err != nil {
		return err
	}

	return nil
}

// Try to handover the role of this member to another one.
func handoverMemberRole(d *Daemon) error {
	// If we aren't clustered, there's nothing to do.
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return err
	}
	if !clustered {
		return nil
	}

	// Figure out our own cluster address.
	address, err := node.ClusterAddress(d.db)
	if err != nil {
		return err
	}

	post := &internalClusterPostHandoverRequest{
		Address: address,
	}

	logCtx := log.Ctx{"address": address}

	// Find the cluster leader.
findLeader:
	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return err
	}
	if leader == "" {
		return fmt.Errorf("No leader address found")
	}

	if leader == address {
		logger.Info("Transferring leadership", logCtx)
		err := d.gateway.TransferLeadership()
		if err != nil {
			return fmt.Errorf("Failed to transfer leadership: %w", err)
		}
		goto findLeader
	}

	logger.Info("Handing over cluster member role", logCtx)
	client, err := cluster.Connect(leader, d.endpoints.NetworkCert(), d.serverCert(), nil, true)
	if err != nil {
		return fmt.Errorf("Failed handing over cluster member role: %w", err)
	}

	_, _, err = client.RawQuery("POST", "/internal/cluster/handover", post, "")
	if err != nil {
		return err
	}

	return nil
}

// Used to assign a new role to a the local dqlite node.
func internalClusterPostAssign(d *Daemon, r *http.Request) response.Response {
	req := internalClusterPostAssignRequest{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if len(req.RaftNodes) == 0 {
		return response.BadRequest(fmt.Errorf("No raft members provided"))
	}

	nodes := make([]db.RaftNode, len(req.RaftNodes))
	for i, node := range req.RaftNodes {
		nodes[i].ID = node.ID
		nodes[i].Address = node.Address
		nodes[i].Role = db.RaftRole(node.Role)
		nodes[i].Name = node.Name
	}
	err = cluster.Assign(d.State(), d.gateway, nodes)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, nil)
}

// A request for the /internal/cluster/assign endpoint.
type internalClusterPostAssignRequest struct {
	RaftNodes []internalRaftNode `json:"raft_nodes" yaml:"raft_nodes"`
}

// Used to to transfer the responsibilities of a member to another one
func internalClusterPostHandover(d *Daemon, r *http.Request) response.Response {
	req := internalClusterPostHandoverRequest{}

	// Parse the request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.Address == "" {
		return response.BadRequest(fmt.Errorf("No id provided"))
	}

	// Redirect all requests to the leader, which is the one with
	// authoritative knowledge of the current raft configuration.
	address, err := node.ClusterAddress(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	leader, err := d.gateway.LeaderAddress()
	if err != nil {
		return response.InternalError(err)
	}

	if leader == "" {
		return response.SmartError(fmt.Errorf("No leader address found"))
	}

	if address != leader {
		logger.Debugf("Redirect handover request to %s", leader)
		url := &url.URL{
			Scheme: "https",
			Path:   "/internal/cluster/handover",
			Host:   leader,
		}
		return response.SyncResponseRedirect(url.String())
	}

	// Get lock now we are on leader.
	d.clusterMembershipMutex.Lock()
	defer d.clusterMembershipMutex.Unlock()

	target, nodes, err := cluster.Handover(d.State(), d.gateway, req.Address)
	if err != nil {
		return response.SmartError(err)
	}

	// If there's no other member we can promote, there's nothing we can
	// do, just return.
	if target == "" {
		goto out
	}

	logger.Info("Promoting member during handover", log.Ctx{"address": address, "losingAddress": req.Address, "candidateAddress": target})
	err = changeMemberRole(d, r, target, nodes)
	if err != nil {
		return response.SmartError(err)
	}

	// Demote the member that is handing over.
	for i, node := range nodes {
		if node.Address == req.Address {
			nodes[i].Role = db.RaftSpare
		}
	}

	logger.Info("Demoting member during handover", log.Ctx{"address": address, "losingAddress": req.Address})
	err = changeMemberRole(d, r, req.Address, nodes)
	if err != nil {
		return response.SmartError(err)
	}

out:
	return response.SyncResponse(true, nil)
}

// A request for the /internal/cluster/handover endpoint.
type internalClusterPostHandoverRequest struct {
	// Address of the server whose role should be transferred.
	Address string `json:"address" yaml:"address"`
}

func clusterCheckStoragePoolsMatch(cluster *db.Cluster, reqPools []api.StoragePool) error {
	poolNames, err := cluster.GetCreatedStoragePoolNames()
	if err != nil && err != db.ErrNoSuchObject {
		return err
	}
	for _, name := range poolNames {
		found := false
		for _, reqPool := range reqPools {
			if reqPool.Name != name {
				continue
			}
			found = true
			_, pool, _, err := cluster.GetStoragePoolInAnyState(name)
			if err != nil {
				return err
			}
			if pool.Driver != reqPool.Driver {
				return fmt.Errorf("Mismatching driver for storage pool %s", name)
			}
			// Exclude the keys which are node-specific.
			exclude := db.StoragePoolNodeConfigKeys
			err = util.CompareConfigs(pool.Config, reqPool.Config, exclude)
			if err != nil {
				return fmt.Errorf("Mismatching config for storage pool %s: %w", name, err)
			}
			break
		}
		if !found {
			return fmt.Errorf("Missing storage pool %s", name)
		}
	}
	return nil
}

func clusterCheckNetworksMatch(cluster *db.Cluster, reqNetworks []api.Network) error {
	networkNames, err := cluster.GetCreatedNetworks()
	if err != nil && err != db.ErrNoSuchObject {
		return err
	}
	for _, name := range networkNames {
		found := false
		for _, reqNetwork := range reqNetworks {
			if reqNetwork.Name != name {
				continue
			}
			found = true
			_, network, _, err := cluster.GetNetworkInAnyState(name)
			if err != nil {
				return err
			}
			// Exclude the keys which are node-specific.
			exclude := db.NodeSpecificNetworkConfig
			err = util.CompareConfigs(network.Config, reqNetwork.Config, exclude)
			if err != nil {
				return fmt.Errorf("Mismatching config for network %s: %v", name, err)
			}
			break
		}
		if !found {
			return fmt.Errorf("Missing network %s", name)
		}
	}
	return nil
}

// Used as low-level recovering helper.
func internalClusterRaftNodeDelete(d *Daemon, r *http.Request) response.Response {
	address := mux.Vars(r)["address"]
	err := cluster.RemoveRaftNode(d.gateway, address)
	if err != nil {
		return response.SmartError(err)
	}

	err = rebalanceMemberRoles(d, r, nil)
	if err != nil && !errors.Is(err, cluster.ErrNotLeader) {
		logger.Warn("Could not rebalance cluster member roles after raft member removal", log.Ctx{"err": err})
	}

	return response.SyncResponse(true, nil)
}
