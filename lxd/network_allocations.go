package main

import (
	"context"
	"fmt"
	"net"
	"net/http"

	clusterRequest "github.com/canonical/lxd/lxd/cluster/request"
	"github.com/canonical/lxd/lxd/db"
	dbCluster "github.com/canonical/lxd/lxd/db/cluster"
	"github.com/canonical/lxd/lxd/network"
	"github.com/canonical/lxd/lxd/project"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/version"
)

var networkAllocationsCmd = APIEndpoint{
	Path: "network-allocations",

	Get: APIEndpointAction{Handler: networkAllocationsGet, AccessHandler: allowProjectPermission("networks", "view")},
}

// swagger:operation GET /1.0/network-allocations network-allocations network_allocations_get
//
//	Get the network allocations in use (`network`, `network-forward` and `load-balancer` and `instance`)
//
//	Returns a list of network allocations in use by a LXD deployment.
//
//	---
//	produces:
//	  - application/json
//	parameters:
//	  - in: query
//	    name: project
//	    description: Project name
//	    type: string
//	    example: default
//	  - in: query
//	    name: all-projects
//	    description: Retrieve entities from all projects
//	    type: boolean
//	responses:
//	  "200":
//	    description: API endpoints
//	    schema:
//	      type: object
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          type: array
//	          description: List of network allocations used by a consuming entity
//	          items:
//	            $ref: "#/definitions/NetworkAllocations"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func networkAllocationsGet(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.NetworkProject(d.State().DB.Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	allProjects := shared.IsTrue(queryParam(r, "all-projects"))

	var projectNames []string
	err = d.db.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Figure out the projects to retrieve.
		if !allProjects {
			projectNames = []string{projectName}
		} else {
			// Get all project names if no specific project requested.
			projectNames, err = dbCluster.GetProjectNames(ctx, tx.Tx())
			if err != nil {
				return fmt.Errorf("Failed loading projects: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Helper function to get the default CIDR address (/32 or /128 mask for ipv4 or ipv6 respectively)
	getDefaultCIDRAddr := func(addr string) (string, error) {
		ip := net.ParseIP(addr)
		if ip.To4() != nil {
			return fmt.Sprintf("%s/32", addr), nil
		} else if ip.To16() != nil {
			return fmt.Sprintf("%s/128", addr), nil
		} else {
			return "", fmt.Errorf("%s is not a valid IP address.", addr)
		}
	}

	result := make([]api.NetworkAllocations, 0)

	// Then, get all the networks, their network forwards and their network load balancers.
	for _, projectName := range projectNames {
		networkNames, err := d.db.Cluster.GetNetworks(projectName)
		if err != nil {
			return response.SmartError(err)
		}

		// Get all the networks, their attached instances, their network forwards and their network load balancers.
		for _, networkName := range networkNames {
			n, err := network.LoadByName(d.State(), projectName, networkName)
			if err != nil {
				return response.SmartError(err)
			}

			gwAddrs := make([]string, 0)
			netConf := n.Config()
			for _, key := range []string{"ipv4.address", "ipv6.address"} {
				ipNet, _ := network.ParseIPCIDRToNet(netConf[key])
				if ipNet != nil {
					gwAddrs = append(gwAddrs, ipNet.String())
				}
			}

			if len(gwAddrs) > 0 {
				result = append(result, api.NetworkAllocations{
					Addresses: gwAddrs,
					UsedBy:    api.NewURL().Path(version.APIVersion, "networks", networkName).Project(projectName).String(),
					Type:      "network",
				})
			}

			leases, err := n.Leases(projectName, clusterRequest.ClientTypeNormal)
			if err != nil {
				return response.SmartError(err)
			}

			instanceAllocs := make(map[string]api.NetworkAllocations, 0)
			for _, lease := range leases {
				if shared.StringInSlice(lease.Type, []string{"static", "dynamic"}) {
					cidrAddr, err := getDefaultCIDRAddr(lease.Address)
					if err != nil {
						return response.SmartError(err)
					}

					_, ok := instanceAllocs[lease.Hwaddr]
					if ok {
						instanceAlloc := instanceAllocs[lease.Hwaddr]
						instanceAlloc.Addresses = append(instanceAlloc.Addresses, cidrAddr)
						instanceAllocs[lease.Hwaddr] = instanceAlloc
					} else {
						instanceAllocs[lease.Hwaddr] = api.NetworkAllocations{
							Addresses: []string{cidrAddr},
							UsedBy:    api.NewURL().Path(version.APIVersion, "instances", lease.Hostname).Project(projectName).String(),
							Type:      "instance",
							Hwaddr:    lease.Hwaddr,
						}
					}
				}
			}

			for _, v := range instanceAllocs {
				result = append(result, v)
			}

			forwards, err := d.db.Cluster.GetNetworkForwards(r.Context(), n.ID(), false)
			if err != nil {
				return response.SmartError(err)
			}

			for _, forward := range forwards {
				cidrAddr, err := getDefaultCIDRAddr(forward.ListenAddress)
				if err != nil {
					return response.SmartError(err)
				}

				result = append(
					result,
					api.NetworkAllocations{
						Addresses: []string{cidrAddr},
						UsedBy:    api.NewURL().Path(version.APIVersion, "networks", networkName, "forwards", forward.ListenAddress).Project(projectName).String(),
						Type:      "network-forward",
					},
				)
			}
		}
	}

	return response.SyncResponse(true, result)
}
