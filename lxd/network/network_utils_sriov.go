package network

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canonical/lxd/lxd/db"
	"github.com/canonical/lxd/lxd/device/pci"
	"github.com/canonical/lxd/lxd/ip"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/logger"
)

// sriovReservedDevicesMutex used to coordinate access for checking reserved devices.
var sriovReservedDevicesMutex sync.Mutex

// SRIOVVirtualFunctionMutex used to coordinate access for finding and claiming free virtual functions.
var SRIOVVirtualFunctionMutex sync.Mutex

// SRIOVGetHostDevicesInUse returns a map of host device names that have been used by devices in other instances
// and networks on the local node. Used when selecting physical and SR-IOV VF devices to avoid conflicts.
func SRIOVGetHostDevicesInUse(s *state.State) (map[string]struct{}, error) {
	sriovReservedDevicesMutex.Lock()
	defer sriovReservedDevicesMutex.Unlock()

	var err error
	var localNode string
	var networks map[int64]api.Network

	err = s.Cluster.Transaction(func(tx *db.ClusterTx) error {
		localNode, err = tx.GetLocalNodeName()
		if err != nil {
			return fmt.Errorf("Failed to get local node name: %w", err)
		}

		// Get all managed networks across all projects.
		networks, err = tx.GetCreatedNetworks()
		if err != nil {
			return fmt.Errorf("Failed to load all networks: %w", err)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	filter := db.InstanceFilter{
		Node: &localNode,
	}

	reservedDevices := map[string]struct{}{}

	// Check if any instances are using the VF device.
	err = s.Cluster.InstanceList(&filter, func(dbInst db.Instance, p db.Project, profiles []api.Profile) error {
		// Expand configs so we take into account profile devices.
		dbInst.Config = db.ExpandInstanceConfig(dbInst.Config, profiles)
		// TODO: change parameters and return type for db.ExpandInstanceDevices so that we can expand devices without
		// converting back and forth between API and db Device types.
		for _, p := range profiles {
			devices, err := db.APIToDevices(p.Devices)
			if err != nil {
				return fmt.Errorf("Failed to expand devices: %w", err)
			}
			for _, device := range devices {
				dbInst.Devices[device.Name] = device
			}
		}

		for _, dev := range dbInst.Devices {
			// If device references a parent host interface name, mark that as reserved.
			parent := dev.Config["parent"]
			if parent != "" {
				reservedDevices[parent] = struct{}{}
			}

			// If device references a volatile host interface name, mark that as reserved.
			hostName := dbInst.Config[fmt.Sprintf("volatile.%s.host_name", dev.Name)]
			if hostName != "" {
				reservedDevices[hostName] = struct{}{}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Check if any networks are using the VF device.
	for _, ni := range networks {
		// If network references a parent host interface name, mark that as reserved.
		parent := ni.Config["parent"]
		if parent != "" {
			reservedDevices[parent] = struct{}{}
		}
	}

	return reservedDevices, nil
}

// SRIOVFindFreeVirtualFunction looks on the specified parent device for an unused virtual function.
// Returns the name of the interface and virtual function index ID if found, error if not.
func SRIOVFindFreeVirtualFunction(s *state.State, parentDev string) (string, int, error) {
	reservedDevices, err := SRIOVGetHostDevicesInUse(s)
	if err != nil {
		return "", -1, fmt.Errorf("Failed getting in use device list: %w", err)
	}

	sriovNumVFsFile := fmt.Sprintf("/sys/class/net/%s/device/sriov_numvfs", parentDev)
	sriovTotalVFsFile := fmt.Sprintf("/sys/class/net/%s/device/sriov_totalvfs", parentDev)

	// Verify that this is indeed a SR-IOV enabled device.
	if !shared.PathExists(sriovNumVFsFile) {
		return "", -1, fmt.Errorf("Parent device %q doesn't support SR-IOV", parentDev)
	}

	// Get parent dev_port and dev_id values.
	pfDevPort, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/dev_port", parentDev))
	if err != nil {
		return "", -1, err
	}

	pfDevID, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/dev_id", parentDev))
	if err != nil {
		return "", -1, err
	}

	// Get number of currently enabled VFs.
	sriovNumVFsBuf, err := ioutil.ReadFile(sriovNumVFsFile)
	if err != nil {
		return "", -1, err
	}

	sriovNumVFs, err := strconv.Atoi(strings.TrimSpace(string(sriovNumVFsBuf)))
	if err != nil {
		return "", -1, err
	}

	// Get number of possible VFs.
	sriovTotalVFsBuf, err := ioutil.ReadFile(sriovTotalVFsFile)
	if err != nil {
		return "", -1, err
	}

	sriovTotalVFs, err := strconv.Atoi(strings.TrimSpace(string(sriovTotalVFsBuf)))
	if err != nil {
		return "", -1, err
	}

	// Ensure parent is up (needed for Intel at least).
	link := &ip.Link{Name: parentDev}
	err = link.SetUp()
	if err != nil {
		return "", -1, err
	}

	// Check if any free VFs are already enabled.
	vfID, nicName, err := sriovGetFreeVFInterface(reservedDevices, parentDev, sriovNumVFs, 0, pfDevID, pfDevPort)
	if err != nil {
		return "", -1, err
	}

	// Found a free VF.
	if nicName != "" {
		return nicName, vfID, nil
	} else if sriovNumVFs < sriovTotalVFs {
		logger.Debugf("Attempting to grow available VFs from %d to %d on device %q", sriovNumVFs, sriovTotalVFs, parentDev)

		// Bump the number of VFs to the maximum if not there yet.
		err = ioutil.WriteFile(sriovNumVFsFile, []byte(fmt.Sprintf("%d", sriovTotalVFs)), 0644)
		if err != nil {
			return "", -1, fmt.Errorf("Failed growing available VFs from %d to %d on device %q: %w", sriovNumVFs, sriovTotalVFs, parentDev, err)
		}

		time.Sleep(time.Second) // Allow time for new VFs to appear.

		// Use next free VF index starting from the first newly created VF.
		vfID, nicName, err = sriovGetFreeVFInterface(reservedDevices, parentDev, sriovTotalVFs, sriovNumVFs, pfDevID, pfDevPort)
		if err != nil {
			return "", -1, err
		}

		// Found a free VF.
		if nicName != "" {
			return nicName, vfID, nil
		}
	}

	return "", -1, fmt.Errorf("All virtual functions on parent device %q are already in use", parentDev)
}

// sriovGetFreeVFInterface checks the system for a free VF interface that belongs to the same device and port as
// the parent device starting from the startVFID to the vfCount-1. Returns VF ID and VF interface name if found or
// -1 and empty string if no free interface found. A free interface is one that is bound on the host, not in the
// reservedDevices map, is down and has no global IPs defined on it.
func sriovGetFreeVFInterface(reservedDevices map[string]struct{}, parentDev string, vfCount int, startVFID int, pfDevID []byte, pfDevPort []byte) (int, string, error) {
	for vfID := startVFID; vfID < vfCount; vfID++ {
		vfListPath := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net", parentDev, vfID)

		if !shared.PathExists(vfListPath) {
			continue // The vfListPath won't exist if the VF has been unbound and used with a VM.
		}

		ents, err := ioutil.ReadDir(vfListPath)
		if err != nil {
			return -1, "", fmt.Errorf("Failed reading VF interface directory %q: %w", vfListPath, err)
		}

		for _, ent := range ents {
			// We expect the entry to be a directory for the VF's interface name.
			if !ent.IsDir() {
				continue
			}

			nicName := ent.Name()

			// We can't use this VF interface as it is reserved by another device.
			_, exists := reservedDevices[nicName]
			if exists {
				continue
			}

			// Get VF dev_port and dev_id values.
			vfDevPort, err := ioutil.ReadFile(fmt.Sprintf("%s/%s/dev_port", vfListPath, nicName))
			if err != nil {
				return -1, "", err
			}

			vfDevID, err := ioutil.ReadFile(fmt.Sprintf("%s/%s/dev_id", vfListPath, nicName))
			if err != nil {
				return -1, "", err
			}

			// Skip VFs if they do not relate to the same device and port as the parent PF.
			// Some card vendors change the device ID for each port.
			if bytes.Compare(pfDevPort, vfDevPort) != 0 || bytes.Compare(pfDevID, vfDevID) != 0 {
				continue
			}

			addresses, isUp, err := InterfaceStatus(nicName)
			if err != nil {
				return -1, "", err
			}

			// Ignore if interface is up or if interface has unicast IP addresses (may be in use by
			// another application already).
			if isUp || len(addresses) > 0 {
				continue
			}

			// Found a free VF.
			return vfID, nicName, err
		}
	}

	return -1, "", nil
}

// SRIOVGetVFDevicePCISlot returns the PCI slot name for a network virtual function device.
func SRIOVGetVFDevicePCISlot(parentDev string, vfID string) (pci.Device, error) {
	ueventFile := fmt.Sprintf("/sys/class/net/%s/device/virtfn%s/uevent", parentDev, vfID)
	pciDev, err := pci.ParseUeventFile(ueventFile)
	if err != nil {
		return pciDev, err
	}

	return pciDev, nil
}
