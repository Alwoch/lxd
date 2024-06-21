package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"errors"

	"github.com/gorilla/mux"
	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/canonical/lxd/client"
	"github.com/canonical/lxd/lxd/cluster"
	clusterRequest "github.com/canonical/lxd/lxd/cluster/request"
	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/lifecycle"
	"github.com/canonical/lxd/lxd/network"
	"github.com/canonical/lxd/lxd/network/openvswitch"
	"github.com/canonical/lxd/lxd/project"
	"github.com/canonical/lxd/lxd/request"
	"github.com/canonical/lxd/lxd/resources"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/lxd/revert"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/lxd/util"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/logger"
	"github.com/canonical/lxd/shared/version"
)

// Lock to prevent concurent networks creation
var networkCreateLock sync.Mutex

var networksCmd = APIEndpoint{
	Path: "networks",

	Get:  APIEndpointAction{Handler: networksGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: networksPost},
}

var networkCmd = APIEndpoint{
	Path: "networks/{name}",

	Delete: APIEndpointAction{Handler: networkDelete},
	Get:    APIEndpointAction{Handler: networkGet, AccessHandler: allowAuthenticated},
	Patch:  APIEndpointAction{Handler: networkPatch},
	Post:   APIEndpointAction{Handler: networkPost},
	Put:    APIEndpointAction{Handler: networkPut},
}

var networkLeasesCmd = APIEndpoint{
	Path: "networks/{name}/leases",

	Get: APIEndpointAction{Handler: networkLeasesGet, AccessHandler: allowAuthenticated},
}

var networkStateCmd = APIEndpoint{
	Path: "networks/{name}/state",

	Get: APIEndpointAction{Handler: networkStateGet, AccessHandler: allowAuthenticated},
}

// API endpoints

// swagger:operation GET /1.0/networks networks networks_get
//
// Get the networks
//
// Returns a list of networks (URLs).
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
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
//               "/1.0/networks/lxdbr0",
//               "/1.0/networks/lxdbr1"
//             ]
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/networks?recursion=1 networks networks_get_recursion1
//
// Get the networks
//
// Returns a list of networks (structs).
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
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
//           description: List of networks
//           items:
//             $ref: "#/definitions/Network"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networksGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)

	// Get list of managed networks (that may or may not have network interfaces on the host).
	networks, err := d.cluster.GetNetworks()
	if err != nil {
		return response.InternalError(err)
	}

	// Get list of actual network interfaces on the host as well.
	ifaces, err := net.Interfaces()
	if err != nil {
		return response.InternalError(err)
	}

	for _, iface := range ifaces {
		// Ignore veth pairs (for performance reasons).
		if strings.HasPrefix(iface.Name, "veth") {
			continue
		}

		// Append to the list of networks if a managed network of same name doesn't exist.
		if !shared.StringInSlice(iface.Name, networks) {
			networks = append(networks, iface.Name)
		}
	}

	resultString := []string{}
	resultMap := []api.Network{}
	for _, network := range networks {
		if !recursion {
			resultString = append(resultString, fmt.Sprintf("/%s/networks/%s", version.APIVersion, network))
		} else {
			net, err := doNetworkGet(d, r, network)
			if err != nil {
				continue
			}
			resultMap = append(resultMap, net)
		}
	}

	if !recursion {
		return response.SyncResponse(true, resultString)
	}

	return response.SyncResponse(true, resultMap)
}

// swagger:operation POST /1.0/networks networks networks_post
//
// Add a network
//
// Creates a new network.
// When clustered, most network types require individual POST for each cluster member prior to a global POST.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: body
//     name: network
//     description: Network
//     required: true
//     schema:
//       $ref: "#/definitions/NetworksPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networksPost(d *Daemon, r *http.Request) response.Response {
	networkCreateLock.Lock()
	defer networkCreateLock.Unlock()

	req := api.NetworksPost{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	if req.Type == "" {
		req.Type = "bridge"
	}

	if req.Config == nil {
		req.Config = map[string]string{}
	}

	netType, err := network.LoadByType(req.Type)
	if err != nil {
		return response.BadRequest(err)
	}

	err = netType.ValidateName(req.Name)
	if err != nil {
		return response.BadRequest(err)
	}

	netTypeInfo := netType.Info()
	url := fmt.Sprintf("/%s/networks/%s", version.APIVersion, req.Name)
	resp := response.SyncResponseLocation(true, nil, url)

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))

	if isClusterNotification(r) {
		n, err := network.LoadByName(d.State(), req.Name)
		if err != nil {
			return response.SmartError(err)
		}

		// This is an internal request which triggers the actual creation of the network across all nodes
		// after they have been previously defined.
		err = doNetworksCreate(d, n, clientType)
		if err != nil {
			return response.SmartError(err)
		}
		return resp
	}

	targetNode := queryParam(r, "target")
	if targetNode != "" {
		if !netTypeInfo.NodeSpecificConfig {
			return response.BadRequest(fmt.Errorf("Network type %q does not support node specific config", netType.Type()))
		}

		// A targetNode was specified, let's just define the node's network without actually creating it.
		// Check that only NodeSpecificNetworkConfig keys are specified.
		for key := range req.Config {
			if !shared.StringInSlice(key, db.NodeSpecificNetworkConfig) {
				return response.BadRequest(fmt.Errorf("Config key %q may not be used as node-specific key", key))
			}
		}

		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.CreatePendingNetwork(targetNode, req.Name, netType.DBType(), req.Config)
		})
		if err != nil {
			if err == db.ErrAlreadyDefined {
				return response.BadRequest(fmt.Errorf("The network is already defined on node %q", targetNode))
			}
			return response.SmartError(err)
		}
		return resp
	}

	// Load existing pool if exists, if not don't fail.
	_, netInfo, _, err := d.cluster.GetNetworkInAnyState(req.Name)
	if err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
		return response.InternalError(err)
	}

	// If the network has previously had a create attempt that failed, then because we cannot track per-node
	// status, we need to prevent any further create attempts and require the user to delete and re-create.
	if netInfo != nil && netInfo.Status == api.NetworkStatusErrored {
		return response.BadRequest(fmt.Errorf("Network is in errored state, please delete and re-create"))
	}

	// Check if we're clustered.
	count, err := cluster.Count(d.State())
	if err != nil {
		return response.SmartError(err)
	}

	// No targetNode was specified and we're clustered or there is an existing partially created single node
	// network, either way finalize the config in the db and actually create the network on all cluster nodes.
	if count > 1 || (netInfo != nil && netInfo.Status != api.NetworkStatusCreated) {
		// Simulate adding pending node network config when the driver doesn't support per-node config.
		if !netTypeInfo.NodeSpecificConfig && clientType != clusterRequest.ClientTypeJoiner {
			// Create pending entry for each node.
			err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
				nodes, err := tx.GetNodes()
				if err != nil {
					return err
				}

				for _, node := range nodes {
					// Don't pass in any config, as these nodes don't have any node-specific
					// config and we don't want to create duplicate global config.
					err = tx.CreatePendingNetwork(node.Name, req.Name, netType.DBType(), nil)
					if err != nil && !errors.Is(err, db.ErrAlreadyDefined) {
						return fmt.Errorf("Failed creating pending network for node %q: %w", node.Name, err)
					}
				}

				return nil
			})
			if err != nil {
				return response.SmartError(err)
			}
		}

		err = networksPostCluster(d, netInfo, req, clientType, netType)
		if err != nil {
			return response.SmartError(err)
		}

		return resp
	}

	// Non-clustered network creation.
	if netInfo != nil {
		return response.BadRequest(fmt.Errorf("The network already exists"))
	}

	revert := revert.New()
	defer revert.Fail()

	// Populate default config.
	err = netType.FillConfig(req.Config)
	if err != nil {
		return response.SmartError(err)
	}

	// Create the database entry.
	_, err = d.cluster.CreateNetwork(req.Name, req.Description, netType.DBType(), req.Config)
	if err != nil {
		return response.SmartError(fmt.Errorf("Error inserting %q into database: %w", req.Name, err))
	}
	revert.Add(func() { d.cluster.DeleteNetwork(req.Name) })

	n, err := network.LoadByName(d.State(), req.Name)
	if err != nil {
		return response.SmartError(err)
	}

	err = doNetworksCreate(d, n, clientType)
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Default, lifecycle.NetworkCreated.Event(n, requestor, nil))

	revert.Success()
	return resp
}

// networkPartiallyCreated returns true of supplied network has properties that indicate it has had previous
// create attempts run on it but failed on one or more nodes.
func networkPartiallyCreated(netInfo *api.Network) bool {
	// If the network status is NetworkStatusErrored, this means create has been run in the past and has
	// failed on one or more nodes. Hence it is partially created.
	if netInfo.Status == api.NetworkStatusErrored {
		return true
	}

	// If the network has global config keys, then it has previously been created by having its global config
	// inserted, and this means it is partialled created.
	for key := range netInfo.Config {
		if !shared.StringInSlice(key, db.NodeSpecificNetworkConfig) {
			return true
		}
	}

	return false
}

// networksPostCluster checks that there is a pending network in the database and then attempts to setup the
// network on each node. If all nodes are successfully setup then the network's state is set to created.
// Accepts an optional existing network record, which will exist when performing subsequent re-create attempts.
func networksPostCluster(d *Daemon, netInfo *api.Network, req api.NetworksPost, clientType clusterRequest.ClientType, netType network.Type) error {
	// Check that no node-specific config key has been supplied in request.
	for key := range req.Config {
		if shared.StringInSlice(key, db.NodeSpecificNetworkConfig) {
			return fmt.Errorf("Config key %q is node-specific", key)
		}
	}

	// If network already exists, perform quick checks.
	if netInfo != nil {
		// Check network isn't already created.
		if netInfo.Status == api.NetworkStatusCreated {
			return fmt.Errorf("The network is already created")
		}

		// Check the requested network type matches the type created when adding the local node config.
		if req.Type != netInfo.Type {
			return fmt.Errorf("Requested network type %q doesn't match type in existing database record %q", req.Type, netInfo.Type)
		}
	}

	// Check that the network is properly defined, get the node-specific configs and merge with global config.
	var nodeConfigs map[string]map[string]string
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Check if any global config exists already, if so we should not create global config again.
		if netInfo != nil && networkPartiallyCreated(netInfo) {
			if len(req.Config) > 0 {
				return fmt.Errorf("Network already partially created. Please do not specify any global config when re-running create")
			}

			logger.Debug("Skipping global network create as global config already partially created", log.Ctx{"network": req.Name})
			return nil
		}

		// Fetch the network ID.
		networkID, err := tx.GetNetworkID(req.Name)
		if err != nil {
			return err
		}

		// Fetch the node-specific configs and check the network is defined for all nodes.
		nodeConfigs, err = tx.NetworkNodeConfigs(networkID)
		if err != nil {
			return err
		}

		// Add default values if we are inserting global config for first time.
		err = netType.FillConfig(req.Config)
		if err != nil {
			return err
		}

		// Insert the global config keys.
		err = tx.CreateNetworkConfig(networkID, 0, req.Config)
		if err != nil {
			return err
		}

		// Assume failure unless we succeed later on.
		return tx.NetworkErrored(req.Name)
	})
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Network not pending on any node (use --target <node> first)")
		}

		return err
	}

	// Create notifier for other nodes to create the network.
	notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), d.serverCert(), cluster.NotifyAll)
	if err != nil {
		return err
	}

	// Load the network from the database for the local node.
	n, err := network.LoadByName(d.State(), req.Name)
	if err != nil {
		return err
	}

	err = doNetworksCreate(d, n, clientType)
	if err != nil {
		return err
	}
	logger.Debug("Created network on local cluster member", log.Ctx{"network": req.Name})

	// Notify other nodes to create the network.
	err = notifier(func(client lxd.InstanceServer) error {
		server, _, err := client.GetServer()
		if err != nil {
			return err
		}

		// Create fresh request based on existing network to send to node.
		nodeReq := api.NetworksPost{
			NetworkPut: api.NetworkPut{
				Config:      n.Config(),
				Description: n.Description(),
			},
			Name: n.Name(),
			Type: n.Type(),
		}

		// Remove node-specific config keys.
		for _, key := range db.NodeSpecificNetworkConfig {
			delete(nodeReq.Config, key)
		}

		// Merge node specific config items into global config.
		for key, value := range nodeConfigs[server.Environment.ServerName] {
			nodeReq.Config[key] = value
		}

		err = client.UseProject(n.Project()).CreateNetwork(nodeReq)
		if err != nil {
			return err
		}
		logger.Debug("Created network on cluster member", log.Ctx{"project": n.Project(), "network": n.Name(), "member": server.Environment.ServerName, "config": nodeReq.Config})

		return nil
	})
	if err != nil {
		return err
	}

	// Mark network global status as networkCreated now that all nodes have succeeded.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.NetworkCreated(req.Name)
	})
	if err != nil {
		return err
	}
	logger.Debug("Marked network global status as created", log.Ctx{"network": req.Name})

	return nil
}

// Create the network on the system. The clusterNotification flag is used to indicate whether creation request
// is coming from a cluster notification (and if so we should not delete the database record on error).
func doNetworksCreate(d *Daemon, n network.Network, clientType clusterRequest.ClientType) error {
	revert := revert.New()
	defer revert.Fail()

	// Don't validate network config during pre-cluster-join phase, as if network has ACLs they won't exist
	// in the local database yet. Once cluster join is completed, network will be restarted to give chance for
	// ACL firewall config to be applied.
	if clientType != clusterRequest.ClientTypeJoiner {
		// Validate so that when run on a cluster node the full config (including node specific config)
		// is checked.
		err := n.Validate(n.Config())
		if err != nil {
			return err
		}
	}

	if n.LocalStatus() == api.NetworkStatusCreated {
		logger.Debug("Skipping local network create as already created", log.Ctx{"project": n.Project(), "network": n.Name()})
		return nil
	}

	// Run initial creation setup for the network driver.
	err := n.Create(clientType)
	if err != nil {
		return err
	}

	revert.Add(func() { n.Delete(clientType) })

	// Only start networks when not doing a cluster pre-join phase (this ensures that networks are only started
	// once the node has fully joined the clustered database and has consistent config with rest of the nodes).
	if clientType != clusterRequest.ClientTypeJoiner {
		err = n.Start()
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// swagger:operation GET /1.0/networks/{name} networks network_get
//
// Get the network
//
// Gets a specific network.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
// responses:
//   "200":
//     description: Network
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
//           $ref: "#/definitions/Network"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networkGet(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	name := mux.Vars(r)["name"]

	n, err := doNetworkGet(d, r, name)
	if err != nil {
		return response.SmartError(err)
	}

	targetNode := queryParam(r, "target")
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	// If no target node is specified and the daemon is clustered, we omit the node-specific fields.
	if targetNode == "" && clustered {
		for _, key := range db.NodeSpecificNetworkConfig {
			delete(n.Config, key)
		}
	}

	etag := []interface{}{n.Name, n.Managed, n.Type, n.Description, n.Config}

	return response.SyncResponseETag(true, &n, etag)
}

func doNetworkGet(d *Daemon, r *http.Request, name string) (api.Network, error) {
	// Ignore veth pairs (for performance reasons).
	if strings.HasPrefix(name, "veth") {
		return api.Network{}, os.ErrNotExist
	}

	// Get some information.
	osInfo, _ := net.InterfaceByName(name)
	_, dbInfo, _, _ := d.cluster.GetNetworkInAnyState(name)

	// Quick check.
	if osInfo == nil && dbInfo == nil {
		return api.Network{}, os.ErrNotExist
	}

	// Prepare the response.
	n := api.Network{}
	n.Name = name
	n.UsedBy = []string{}
	n.Config = map[string]string{}

	// Set the device type as needed.
	if osInfo != nil && shared.IsLoopback(osInfo) {
		n.Type = "loopback"
	} else if dbInfo != nil {
		n.Managed = true
		n.Description = dbInfo.Description
		n.Config = dbInfo.Config
		n.Type = dbInfo.Type
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bridge", n.Name)) {
		n.Type = "bridge"
	} else if shared.PathExists(fmt.Sprintf("/proc/net/vlan/%s", n.Name)) {
		n.Type = "vlan"
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/device", n.Name)) {
		n.Type = "physical"
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bonding", n.Name)) {
		n.Type = "bond"
	} else {
		ovs := openvswitch.NewOVS()
		if exists, _ := ovs.BridgeExists(n.Name); exists {
			n.Type = "bridge"
		} else {
			n.Type = "unknown"
		}
	}

	// Look for instances using the interface.
	if n.Type != "loopback" {
		usedBy, err := network.UsedBy(d.State(), n.Name, false)
		if err != nil {
			return api.Network{}, err
		}

		n.UsedBy = project.FilterUsedBy(r, usedBy)
	}

	if dbInfo != nil {
		n.Status = dbInfo.Status
		n.Locations = dbInfo.Locations
	}

	return n, nil
}

// swagger:operation DELETE /1.0/networks/{name} networks network_delete
//
// Delete the network
//
// Removes the network.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networkDelete(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]
	state := d.State()

	// Check if the network is pending, if so we just need to delete it from the database.
	_, dbNetwork, _, err := d.cluster.GetNetworkInAnyState(name)
	if err != nil {
		return response.SmartError(err)
	}
	if dbNetwork.Status == api.NetworkStatusPending {
		err := d.cluster.DeleteNetwork(name)
		if err != nil {
			return response.SmartError(err)
		}
		return response.EmptySyncResponse
	}

	// Get the existing network.
	n, err := network.LoadByName(state, name)
	if err != nil {
		return response.SmartError(err)
	}

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))

	clusterNotification := isClusterNotification(r)
	if !clusterNotification {
		// Quick checks.
		inUse, err := n.IsUsed()
		if err != nil {
			return response.SmartError(err)
		}

		if inUse {
			return response.BadRequest(fmt.Errorf("The network is currently in use"))
		}
	}

	if n.LocalStatus() != api.NetworkStatusPending {
		err = n.Delete(clientType)
		if err != nil {
			return response.InternalError(err)
		}
	}

	// If this is a cluster notification, we're done, any database work will be done by the node that is
	// originally serving the request.
	if clusterNotification {
		return response.EmptySyncResponse
	}

	// If we are clustered, also notify all other nodes, if any.
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	if clustered {
		notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), d.serverCert(), cluster.NotifyAll)
		if err != nil {
			return response.SmartError(err)
		}
		err = notifier(func(client lxd.InstanceServer) error {
			return client.UseProject(n.Project()).DeleteNetwork(n.Name())
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	// Remove the network from the database.
	err = d.State().Cluster.DeleteNetwork(n.Name())
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Default, lifecycle.NetworkDeleted.Event(n, requestor, nil))

	return response.EmptySyncResponse
}

// swagger:operation POST /1.0/networks/{name} networks network_post
//
// Rename the network
//
// Renames an existing network.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: body
//     name: network
//     description: Network rename request
//     required: true
//     schema:
//       $ref: "#/definitions/NetworkPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networkPost(d *Daemon, r *http.Request) response.Response {
	// FIXME: renaming a network is currently not supported in clustering
	//        mode. The difficulty is that network.Start() depends on the
	//        network having already been renamed in the database, which is
	//        a chicken-and-egg problem for cluster notifications (the
	//        serving node should typically do the database job, so the
	//        network is not yet renamed inthe db when the notified node
	//        runs network.Start).
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	if clustered {
		return response.BadRequest(fmt.Errorf("Renaming clustered network not supported"))
	}

	name := mux.Vars(r)["name"]
	req := api.NetworkPost{}
	state := d.State()

	// Parse the request.
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Get the existing network.
	n, err := network.LoadByName(state, name)
	if err != nil {
		return response.SmartError(err)
	}

	if n.Status() != api.NetworkStatusCreated {
		return response.BadRequest(fmt.Errorf("Cannot rename network when not in created state"))
	}

	// Ensure new name is supplied.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("New network name not provided"))
	}

	err = n.ValidateName(req.Name)
	if err != nil {
		return response.BadRequest(err)
	}

	// Check network isn't in use.
	inUse, err := n.IsUsed()
	if err != nil {
		return response.InternalError(fmt.Errorf("Failed checking network in use: %w", err))
	}

	if inUse {
		return response.BadRequest(fmt.Errorf("Network is currently in use"))
	}

	// Check that the name isn't already in used by an existing managed network.
	networks, err := d.cluster.GetNetworks()
	if err != nil {
		return response.InternalError(err)
	}

	if shared.StringInSlice(req.Name, networks) {
		return response.Conflict(fmt.Errorf("Network %q already exists", req.Name))
	}

	// Rename it.
	err = n.Rename(req.Name)
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Default, lifecycle.NetworkRenamed.Event(n, requestor, map[string]interface{}{"old_name": name}))

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/networks/%s", version.APIVersion, req.Name))
}

// swagger:operation PUT /1.0/networks/{name} networks network_put
//
// Update the network
//
// Updates the entire network configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: body
//     name: network
//     description: Network configuration
//     required: true
//     schema:
//       $ref: "#/definitions/NetworkPut"
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
func networkPut(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	name := mux.Vars(r)["name"]

	// Get the existing network.
	n, err := network.LoadByName(d.State(), name)
	if err != nil {
		return response.SmartError(err)
	}

	// Prevent config changes on errored networks.
	if n.Status() == api.NetworkStatusErrored {
		return response.BadRequest(fmt.Errorf("Network config cannot be changed when pool is in errored state"))
	}

	targetNode := queryParam(r, "target")
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	if targetNode == "" && n.Status() != api.NetworkStatusCreated {
		return response.BadRequest(fmt.Errorf("Cannot update network global config when not in created state"))
	}

	// Duplicate config for etag modification and generation.
	etagConfig := util.CopyConfig(n.Config())

	// If no target node is specified and the daemon is clustered, we omit the node-specific fields so that
	// the e-tag can be generated correctly. This is because the GET request used to populate the request
	// will also remove node-specific keys when no target is specified.
	if targetNode == "" && clustered {
		for _, key := range db.NodeSpecificNetworkConfig {
			delete(etagConfig, key)
		}
	}

	// Validate the ETag.
	etag := []interface{}{n.Name(), n.IsManaged(), n.Type(), n.Description(), etagConfig}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Decode the request.
	req := api.NetworkPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// In clustered mode, we differentiate between node specific and non-node specific config keys based on
	// whether the user has specified a target to apply the config to.
	if clustered {
		if targetNode == "" {
			// If no target is specified, then ensure only non-node-specific config keys are changed.
			for k := range req.Config {
				if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
					return response.BadRequest(fmt.Errorf("Config key %q is node-specific", k))
				}
			}
		} else {
			curConfig := n.Config()

			// If a target is specified, then ensure only node-specific config keys are changed.
			for k, v := range req.Config {
				if !shared.StringInSlice(k, db.NodeSpecificNetworkConfig) && curConfig[k] != v {
					return response.BadRequest(fmt.Errorf("Config key %q may not be used as node-specific key", k))
				}
			}
		}
	}

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))

	response := doNetworkUpdate(d, n, req, targetNode, clientType, r.Method, clustered)

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Default, lifecycle.NetworkUpdated.Event(n, requestor, nil))

	return response
}

// swagger:operation PATCH /1.0/networks/{name} networks network_patch
//
// Partially update the network
//
// Updates a subset of the network configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: body
//     name: network
//     description: Network configuration
//     required: true
//     schema:
//       $ref: "#/definitions/NetworkPut"
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
func networkPatch(d *Daemon, r *http.Request) response.Response {
	return networkPut(d, r)
}

// doNetworkUpdate loads the current local network config, merges with the requested network config, validates
// and applies the changes. Will also notify other cluster nodes of non-node specific config if needed.
func doNetworkUpdate(d *Daemon, n network.Network, req api.NetworkPut, targetNode string, clientType clusterRequest.ClientType, httpMethod string, clustered bool) response.Response {
	if req.Config == nil {
		req.Config = map[string]string{}
	}

	// Normally a "put" request will replace all existing config, however when clustered, we need to account
	// for the node specific config keys and not replace them when the request doesn't specify a specific node.
	if targetNode == "" && httpMethod != http.MethodPatch && clustered {
		// If non-node specific config being updated via "put" method in cluster, then merge the current
		// node-specific network config with the submitted config to allow validation.
		// This allows removal of non-node specific keys when they are absent from request config.
		for k, v := range n.Config() {
			if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
				req.Config[k] = v
			}
		}
	} else if httpMethod == http.MethodPatch {
		// If config being updated via "patch" method, then merge all existing config with the keys that
		// are present in the request config.
		for k, v := range n.Config() {
			_, ok := req.Config[k]
			if !ok {
				req.Config[k] = v
			}
		}
	}

	// Validate the merged configuration.
	err := n.Validate(req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	// Apply the new configuration (will also notify other cluster nodes if needed).
	err = n.Update(req, targetNode, clientType)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

// swagger:operation GET /1.0/networks/{name}/leases networks networks_leases_get
//
// Get the DHCP leases
//
// Returns a list of DHCP leases for the network.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
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
//           description: List of DHCP leases
//           items:
//             $ref: "#/definitions/NetworkLease"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networkLeasesGet(d *Daemon, r *http.Request) response.Response {
	projectName := projectParam(r)
	name := mux.Vars(r)["name"]

	// Attempt to load the network.
	n, err := network.LoadByName(d.State(), name)
	if err != nil {
		return response.SmartError(err)
	}

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))
	leases, err := n.Leases(projectName, clientType)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, leases)
}

func networkStartup(s *state.State) error {
	// Get a list of managed networks.
	networks, err := s.Cluster.GetCreatedNetworks()
	if err != nil {
		return fmt.Errorf("Failed to load networks: %w", err)
	}

	// Bring them all up.
	for _, name := range networks {
		n, err := network.LoadByName(s, name)
		if err != nil {
			return fmt.Errorf("Failed to load network %q: %w", name, err)
		}

		err = n.Validate(n.Config())
		if err != nil {
			// Don't cause LXD to fail to start entirely on network start up failure.
			logger.Error("Failed to validate network", log.Ctx{"err": err, "name": name})
			continue
		}

		err = n.Start()
		if err != nil {
			// Don't cause LXD to fail to start entirely on network start up failure.
			logger.Error("Failed to bring up network", log.Ctx{"err": err, "name": name})
			continue
		}
	}

	return nil
}

func networkShutdown(s *state.State) error {
	// Get a list of managed networks
	networks, err := s.Cluster.GetNetworks()
	if err != nil {
		return err
	}

	// Bring them all up
	for _, name := range networks {
		n, err := network.LoadByName(s, name)
		if err != nil {
			return err
		}

		err = n.Stop()
		if err != nil {
			logger.Error("Failed to bring down network", log.Ctx{"err": err, "name": name})
		}
	}

	return nil
}

// swagger:operation GET /1.0/networks/{name}/state networks networks_state_get
//
// Get the network state
//
// Returns the current network state information.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
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
//           $ref: "#/definitions/NetworkState"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func networkStateGet(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	networkName := mux.Vars(r)["name"]

	var state *api.NetworkState
	var err error
	n, networkLoadError := network.LoadByName(d.State(), networkName)
	if networkLoadError == nil {
		state, err = n.State()
		if err != nil {
			return response.SmartError(err)
		}
	} else {
		state, err = resources.GetNetworkState(networkName)
		if err != nil {
			return response.SmartError(err)
		}
	}

	return response.SyncResponse(true, state)
}
