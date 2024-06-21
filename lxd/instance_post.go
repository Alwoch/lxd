package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/pborman/uuid"

	"github.com/canonical/lxd/client"
	"github.com/canonical/lxd/lxd/cluster"
	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/instance"
	"github.com/canonical/lxd/lxd/instance/instancetype"
	"github.com/canonical/lxd/lxd/migration"
	"github.com/canonical/lxd/lxd/operations"
	"github.com/canonical/lxd/lxd/project"
	"github.com/canonical/lxd/lxd/response"
	storagePools "github.com/canonical/lxd/lxd/storage"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/logger"
)

var internalClusterInstanceMovedCmd = APIEndpoint{
	Path: "cluster/instance-moved/{name}",

	Post: APIEndpointAction{Handler: internalClusterInstanceMovedPost},
}

// swagger:operation POST /1.0/instances/{name} instances instance_post
//
// Rename or move/migrate an instance
//
// Renames, moves an instance between pools or migrates an instance to another server.
//
// The returned operation metadata will vary based on what's requested.
// For rename or move within the same server, this is a simple background operation with progress data.
// For migration, in the push case, this will similarly be a background
// operation with progress data, for the pull case, it will be a websocket
// operation with a number of secrets to be passed to the target server.
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
//     name: migration
//     description: Migration request
//     schema:
//       $ref: "#/definitions/InstancePost"
// responses:
//   "202":
//     $ref: "#/responses/Operation"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func instancePost(d *Daemon, r *http.Request) response.Response {
	instanceType, err := urlInstanceTypeDetect(r)
	if err != nil {
		return response.SmartError(err)
	}

	projectName := projectParam(r)

	name := mux.Vars(r)["name"]
	targetNode := queryParam(r, "target")

	// Flag indicating whether the node running the container is offline.
	sourceNodeOffline := false

	// Flag indicating whether the node the container should be moved to is
	// online (only relevant if "?target=<node>" was given).
	targetNodeOffline := false

	// A POST to /containers/<name>?target=<node> is meant to be used to
	// move a container from one node to another within a cluster.
	if targetNode != "" {
		// Determine if either the source node (the one currently
		// running the container) or the target node are offline.
		//
		// If the target node is offline, we return an error.
		//
		// If the source node is offline and the container is backed by
		// ceph, we'll just assume that the container is not running
		// and it's safe to move it.
		//
		// TODO: add some sort of "force" flag to the API, to signal
		//       that the user really wants to move the container even
		//       if we can't know for sure that it's indeed not
		//       running?
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			// Load cluster configuration.
			config, err := cluster.ConfigLoad(tx)
			if err != nil {
				return fmt.Errorf("Failed to load LXD config: %w", err)
			}

			// Load target node.
			node, err := tx.GetNodeByName(targetNode)
			if err != nil {
				return fmt.Errorf("Failed to get target node: %w", err)
			}
			targetNodeOffline = node.IsOffline(config.OfflineThreshold())

			// Load source node.
			address, err := tx.GetNodeAddressOfInstance(projectName, name, db.InstanceTypeFilter(instanceType))
			if err != nil {
				return fmt.Errorf("Failed to get address of instance's member: %w", err)
			}
			if address == "" {
				// Local node.
				sourceNodeOffline = false
				return nil
			}
			node, err = tx.GetNodeByAddress(address)
			if err != nil {
				return fmt.Errorf("Failed to get source member for %s: %w", address, err)
			}
			sourceNodeOffline = node.IsOffline(config.OfflineThreshold())

			return nil
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	if targetNode != "" && targetNodeOffline {
		return response.BadRequest(fmt.Errorf("Target node is offline"))
	}

	// Check whether to forward the request to the node that is running the
	// container. Here are the possible cases:
	//
	// 1. No "?target=<node>" parameter was passed. In this case this is
	//    just a container rename, with no move, and we want the request to be
	//    handled by the node which is actually running the container.
	//
	// 2. The "?target=<node>" parameter was set and the node running the
	//    container is online. In this case we want to forward the request to
	//    that node, which might do things like unmapping the RBD volume for
	//    ceph containers.
	//
	// 3. The "?target=<node>" parameter was set but the node running the
	//    container is offline. We don't want to forward to the request to
	//    that node and we don't want to load the container here (since
	//    it's not a local container): we'll be able to handle the request
	//    at all only if the container is backed by ceph. We'll check for
	//    that just below.
	//
	// Cases 1. and 2. are the ones for which the conditional will be true
	// and we'll either forward the request or load the container.
	if targetNode == "" || !sourceNodeOffline {
		// Handle requests targeted to a container on a different node.
		resp, err := forwardedResponseIfInstanceIsRemote(d, r, projectName, name, instanceType)
		if err != nil {
			return response.SmartError(err)
		}
		if resp != nil {
			return resp
		}
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return response.InternalError(err)
	}

	rdr1 := ioutil.NopCloser(bytes.NewBuffer(body))
	rdr2 := ioutil.NopCloser(bytes.NewBuffer(body))

	reqRaw := shared.Jmap{}
	err = json.NewDecoder(rdr1).Decode(&reqRaw)
	if err != nil {
		return response.BadRequest(err)
	}

	req := api.InstancePost{}
	err = json.NewDecoder(rdr2).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Check if stateful indicator supplied and default to true if not (for backward compatibility).
	_, err = reqRaw.GetBool("live")
	if err != nil {
		req.Live = true
	}

	inst, err := instance.LoadByProjectAndName(d.State(), projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	if req.Migration {
		if targetNode != "" {
			// Check if instance has backups.
			backups, err := d.cluster.GetInstanceBackups(projectName, name)
			if err != nil {
				err = fmt.Errorf("Failed to fetch instance's backups: %w", err)
				return response.SmartError(err)
			}
			if len(backups) > 0 {
				return response.BadRequest(fmt.Errorf("Instance has backups"))
			}

			// Check whether the instance is running.
			if !sourceNodeOffline && inst.IsRunning() {
				return response.BadRequest(fmt.Errorf("Instance is running"))
			}

			run := func(op *operations.Operation) error {
				return migrateInstance(d, r, inst, projectName, targetNode, sourceNodeOffline, name, instanceType, req, op)
			}

			resources := map[string][]string{}
			resources["instances"] = []string{name}

			if inst.Type() == instancetype.Container {
				resources["containers"] = resources["instances"]
			}

			op, err := operations.OperationCreate(d.State(), projectName, operations.OperationClassTask, db.OperationInstanceMigrate, resources, nil, run, nil, nil, r)
			if err != nil {
				return response.InternalError(err)
			}

			return operations.OperationResponse(op)
		}

		instanceOnly := req.InstanceOnly || req.ContainerOnly
		ws, err := newMigrationSource(inst, req.Live, instanceOnly)
		if err != nil {
			return response.InternalError(err)
		}

		resources := map[string][]string{}
		resources["instances"] = []string{name}

		if inst.Type() == instancetype.Container {
			resources["containers"] = resources["instances"]
		}

		run := func(op *operations.Operation) error {
			return ws.Do(d.State(), op)
		}

		cancel := func(op *operations.Operation) error {
			ws.disconnect()
			return nil
		}

		if req.Target != nil {
			// Push mode
			err := ws.ConnectContainerTarget(*req.Target)
			if err != nil {
				return response.InternalError(err)
			}

			op, err := operations.OperationCreate(d.State(), projectName, operations.OperationClassTask, db.OperationInstanceMigrate, resources, nil, run, nil, nil, r)
			if err != nil {
				return response.InternalError(err)
			}

			return operations.OperationResponse(op)
		}

		// Pull mode.
		op, err := operations.OperationCreate(d.State(), projectName, operations.OperationClassWebsocket, db.OperationInstanceMigrate, resources, ws.Metadata(), run, cancel, ws.Connect, r)
		if err != nil {
			return response.InternalError(err)
		}

		return operations.OperationResponse(op)
	}

	// Check that the name isn't already in use.
	id, _ := d.cluster.GetInstanceID(projectName, req.Name)
	if id > 0 {
		return response.Conflict(fmt.Errorf("Name %q already in use", req.Name))
	}

	run := func(*operations.Operation) error {
		return inst.Rename(req.Name, true)
	}

	resources := map[string][]string{}
	resources["instances"] = []string{name}

	if inst.Type() == instancetype.Container {
		resources["containers"] = resources["instances"]
	}

	op, err := operations.OperationCreate(d.State(), projectName, operations.OperationClassTask, db.OperationInstanceRename, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

// Move a non-ceph container to another cluster node.
func instancePostClusteringMigrate(d *Daemon, r *http.Request, inst instance.Instance, oldName, newName, newNode string) (func(op *operations.Operation) error, error) {
	var sourceAddress string
	var targetAddress string

	// Save the original value of the "volatile.apply_template" config key,
	// since we'll want to preserve it in the copied container.
	origVolatileApplyTemplate := inst.LocalConfig()["volatile.apply_template"]

	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error

		sourceAddress, err = tx.GetLocalNodeAddress()
		if err != nil {
			return fmt.Errorf("Failed to get local node address: %w", err)
		}

		node, err := tx.GetNodeByName(newNode)
		if err != nil {
			return fmt.Errorf("Failed to get new node address: %w", err)
		}
		targetAddress = node.Address

		return nil
	})
	if err != nil {
		return nil, err
	}

	run := func(op *operations.Operation) error {
		// Connect to the source host, i.e. ourselves (the node the instance is running on).
		source, err := cluster.Connect(sourceAddress, d.endpoints.NetworkCert(), d.serverCert(), r, true)
		if err != nil {
			return fmt.Errorf("Failed to connect to source server %q: %w", sourceAddress, err)
		}
		source = source.UseProject(inst.Project())

		// Connect to the destination host, i.e. the node to migrate the container to.
		dest, err := cluster.Connect(targetAddress, d.endpoints.NetworkCert(), d.serverCert(), r, false)
		if err != nil {
			return fmt.Errorf("Failed to connect to destination server %q: %w", targetAddress, err)
		}
		dest = dest.UseTarget(newNode).UseProject(inst.Project())

		destName := newName
		isSameName := false

		// If no new name was provided, the user wants to keep the same
		// container name. In that case we need to generate a temporary
		// name.
		if destName == "" || destName == oldName {
			isSameName = true
			destName = fmt.Sprintf("move-%s", uuid.NewRandom().String())
		}

		// First make a copy on the new node of the container to be moved.
		entry, _, err := source.GetInstance(oldName)
		if err != nil {
			return fmt.Errorf("Failed to get instance info: %w", err)
		}

		args := lxd.InstanceCopyArgs{
			Name: destName,
			Mode: "pull",
		}

		copyOp, err := dest.CopyInstance(source, *entry, &args)
		if err != nil {
			return fmt.Errorf("Failed to issue copy instance API request: %w", err)
		}

		handler := func(newOp api.Operation) {
			op.UpdateMetadata(newOp.Metadata)
		}

		_, err = copyOp.AddHandler(handler)
		if err != nil {
			return err
		}

		err = copyOp.Wait()
		if err != nil {
			return fmt.Errorf("Copy instance operation failed: %w", err)
		}

		// Delete the container on the original node.
		deleteOp, err := source.DeleteInstance(oldName)
		if err != nil {
			return fmt.Errorf("Failed to issue delete instance API request: %w", err)
		}

		err = deleteOp.Wait()
		if err != nil {
			return fmt.Errorf("Delete instance operation failed: %w", err)
		}

		// If the destination name is not set, we have generated a random name for
		// the new container, so we need to rename it.
		if isSameName {
			instancePost := api.InstancePost{
				Name: oldName,
			}

			op, err := dest.RenameInstance(destName, instancePost)
			if err != nil {
				return fmt.Errorf("Failed to issue rename instance API request: %w", err)
			}

			err = op.Wait()
			if err != nil {
				return fmt.Errorf("Rename instance operation failed: %w", err)
			}
			destName = oldName
		}

		// Restore the original value of "volatile.apply_template"
		project := inst.Project()
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			id, err := tx.GetInstanceID(project, destName)
			if err != nil {
				return fmt.Errorf("Failed to get ID of moved instance: %w", err)
			}
			err = tx.DeleteInstanceConfigKey(id, "volatile.apply_template")
			if err != nil {
				return fmt.Errorf("Failed to remove volatile.apply_template config key: %w", err)
			}

			if origVolatileApplyTemplate != "" {
				config := map[string]string{
					"volatile.apply_template": origVolatileApplyTemplate,
				}
				err = tx.CreateInstanceConfig(int(id), config)
				if err != nil {
					return fmt.Errorf("Failed to set volatile.apply_template config key: %w", err)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}

		return nil
	}

	return run, nil
}

// Special case migrating a container backed response.Responseby ceph across two cluster nodes.
func instancePostClusteringMigrateWithCeph(d *Daemon, r *http.Request, inst instance.Instance, projectName, oldName, newName, newNode string, instanceType instancetype.Type) (func(op *operations.Operation) error, error) {
	run := func(op *operations.Operation) error {
		// If source node is online (i.e. we're serving the request on
		// it, and c != nil), let's unmap the RBD volume locally
		logger.Debugf(`Renaming RBD storage volume for source container "%s" from "%s" to "%s"`, inst.Name(), inst.Name(), newName)
		pool, err := storagePools.GetPoolByInstance(d.State(), inst)
		if err != nil {
			return fmt.Errorf("Failed to get source instance's storage pool: %w", err)
		}

		if pool.Driver().Info().Name != "ceph" {
			return fmt.Errorf("Source instance's storage pool is not of type ceph")
		}

		args := migration.VolumeSourceArgs{
			Data: project.Instance(projectName, newName),
		}

		// Trigger a rename in the Ceph driver.
		err = pool.MigrateInstance(inst, nil, &args, op)
		if err != nil {
			return fmt.Errorf("Failed to migrate ceph RBD volume: %w", err)
		}

		// Re-link the database entries against the new node name.
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			err := tx.UpdateInstanceNode(projectName, oldName, newName, newNode)
			if err != nil {
				return fmt.Errorf("Move container %s to %s with new name %s: %w", oldName, newNode, newName, err)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("Failed to relink instance database data: %w", err)
		}

		// Create the instance mount point on the target node.
		client, err := cluster.ConnectIfInstanceIsRemote(d.cluster, projectName, newName, d.endpoints.NetworkCert(), d.serverCert(), r, instanceType)
		if err != nil {
			return fmt.Errorf("Failed to connect to target node: %w", err)
		}
		if client == nil {
			err := instancePostCreateInstanceMountPoint(d, projectName, newName)
			if err != nil {
				return fmt.Errorf("Failed creating mount point of instance on target node: %w", err)
			}
		} else {
			path := fmt.Sprintf("/internal/cluster/instance-moved/%s?project=%s", newName, projectName)
			resp, _, err := client.RawQuery("POST", path, nil, "")
			if err != nil {
				return fmt.Errorf("Failed to create mount point on target node: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("Failed to create mount point on target node: %s", resp.Error)
			}
		}

		return nil
	}

	return run, nil
}

// Notification that an instance was moved.
//
// At the moment it's used for ceph-based instances, where the target node needs
// to create the appropriate mount points.
func internalClusterInstanceMovedPost(d *Daemon, r *http.Request) response.Response {
	projectName := projectParam(r)
	instanceName := mux.Vars(r)["name"]

	err := instancePostCreateInstanceMountPoint(d, projectName, instanceName)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

// Used after to create the appropriate mounts point after an instance has been moved.
func instancePostCreateInstanceMountPoint(d *Daemon, project, instanceName string) error {
	inst, err := instance.LoadByProjectAndName(d.State(), project, instanceName)
	if err != nil {
		return fmt.Errorf("Failed loading instance on target node: %w", err)
	}

	pool, err := storagePools.GetPoolByInstance(d.State(), inst)
	if err != nil {
		return fmt.Errorf("Failed loading pool of instance on target node: %w", err)
	}

	err = pool.ImportInstance(inst, nil)
	if err != nil {
		return fmt.Errorf("Failed creating mount point of instance on target node: %w", err)
	}

	return nil
}

func migrateInstance(d *Daemon, r *http.Request, inst instance.Instance, projectName string, targetNode string, sourceNodeOffline bool, name string, instanceType instancetype.Type, req api.InstancePost, op *operations.Operation) error { // Check if we are migrating a ceph-based container.
	poolName, err := inst.StoragePool()
	if err != nil {
		err = fmt.Errorf("Failed to fetch instance's pool name: %w", err)
		return err
	}
	_, pool, _, err := d.cluster.GetStoragePool(poolName)
	if err != nil {
		err = fmt.Errorf("Failed to fetch instance's pool info: %w", err)
		return err
	}
	if pool.Driver == "ceph" {
		f, err := instancePostClusteringMigrateWithCeph(d, r, inst, projectName, name, req.Name, targetNode, instanceType)
		if err != nil {
			return err
		}

		return f(op)
	}

	// If this is not a ceph-based container, make sure
	// that the source node is online, and we didn't get
	// here only to handle the case where the container is
	// ceph-based.
	if sourceNodeOffline {
		err := fmt.Errorf("The cluster member hosting the instance is offline")
		return err
	}

	f, err := instancePostClusteringMigrate(d, r, inst, name, req.Name, targetNode)
	if err != nil {
		return err
	}

	return f(op)
}
