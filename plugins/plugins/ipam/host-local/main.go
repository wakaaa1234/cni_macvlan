// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/allocator"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
)

const (
	AddLocalHostLog = "/var/log/add_local_host.log"
	DelLocalHostLog = "/var/log/del_local_host.log"
)

var DebugLog *log.Logger

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("host-local"))
}

func loadNetConf(bytes []byte) (*types.NetConf, string, error) {
	n := &types.NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}
	return n, n.CNIVersion, nil
}

func cmdCheck(args *skel.CmdArgs) error {

	ipamConf, _, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	// Look to see if there is at least one IP address allocated to the container
	// in the data dir, irrespective of what that address actually is
	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	containerIpFound := store.FindByID(args.ContainerID, args.IfName)
	if containerIpFound == false {
		return fmt.Errorf("host-local: Failed to find address added by container %v", args.ContainerID)
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {

	fileName := AddLocalHostLog
	logFile, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logFile, err = os.Create(fileName)
	}
	defer logFile.Close()

	DebugLog = log.New(logFile, "[Debug]", log.LstdFlags)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	DebugLog.Println("read input from ContainerID:", args.ContainerID)
	DebugLog.Println("read input from Netns:", args.Netns)
	DebugLog.Println("read input from IfName:", args.IfName)
	DebugLog.Println("read input from Args:", args.Args)
	DebugLog.Println("read input from Path:", args.Path)
	DebugLog.Println("read input from StdinData:", string(args.StdinData))

	ipamConf, confVersion, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	DebugLog.Println("allocator.LoadIPAMConfig confVersion", confVersion)
	DebugLog.Println("allocator.LoadIPAMConfig  IPAMConfig", *ipamConf)

	result := &current.Result{}

	if ipamConf.ResolvConf != "" {
		DebugLog.Println(" ipamConf.ResolvConf != \"\" ")
		dns, err := parseResolvConf(ipamConf.ResolvConf)
		if err != nil {
			return err
		}
		result.DNS = *dns
	}
	DebugLog.Println("disk.New ", ipamConf.Name, "datadir:", ipamConf.DataDir)
	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// Keep the allocators we used, so we can release all IPs if an error
	// occurs after we start allocating
	allocs := []*allocator.IPAllocator{}

	// Store all requested IPs in a map, so we can easily remove ones we use
	// and error if some remain
	requestedIPs := map[string]net.IP{} //net.IP cannot be a key

	for _, ip := range ipamConf.IPArgs {
		requestedIPs[ip.String()] = ip
		DebugLog.Println("for _, ip := range ipamConf.IPArgs ", ip.String(), " ip", ip)
	}

	for idx, rangeset := range ipamConf.Ranges {
		allocator := allocator.NewIPAllocator(&rangeset, store, idx)

		// Check to see if there are any custom IPs requested in this range.
		var requestedIP net.IP
		for k, ip := range requestedIPs {
			if rangeset.Contains(ip) {
				requestedIP = ip
				delete(requestedIPs, k)
				break
			}
		}

		ipConf, err := allocator.Get(args.ContainerID, args.IfName, requestedIP)
		if err != nil {
			// Deallocate all already allocated IPs
			for _, alloc := range allocs {
				_ = alloc.Release(args.ContainerID, args.IfName)
			}
			return fmt.Errorf("failed to allocate for range %d: %v", idx, err)
		}

		allocs = append(allocs, allocator)

		result.IPs = append(result.IPs, ipConf)
	}

	// If an IP was requested that wasn't fulfilled, fail
	if len(requestedIPs) != 0 {
		for _, alloc := range allocs {
			_ = alloc.Release(args.ContainerID, args.IfName)
		}
		errstr := "failed to allocate all requested IPs:"
		for _, ip := range requestedIPs {
			errstr = errstr + " " + ip.String()
		}
		return fmt.Errorf(errstr)
	}

	result.Routes = ipamConf.Routes

	/*

		type Result struct {
			CNIVersion string         `json:"cniVersion,omitempty"`
			Interfaces []*Interface   `json:"interfaces,omitempty"`
			IPs        []*IPConfig    `json:"ips,omitempty"`
			Routes     []*types.Route `json:"routes,omitempty"`
			DNS        types.DNS      `json:"dns,omitempty"`
		}
	*/

	newResult, err := result.GetAsVersion(confVersion)
	if err != nil {
		return err
	}
	//types.xxx
	DebugLog.Println("newResult  ", newResult.String())

	return types.PrintResult(result, confVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	fileName := DelLocalHostLog
	logFile, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logFile, err = os.Create(fileName)
	}
	defer logFile.Close()
	DebugLog = log.New(logFile, "[Debug]", log.LstdFlags)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	ipamConf, _, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// Loop through all ranges, releasing all IPs, even if an error occurs
	var errors []string
	for idx, rangeset := range ipamConf.Ranges {
		ipAllocator := allocator.NewIPAllocator(&rangeset, store, idx)

		err := ipAllocator.Release(args.ContainerID, args.IfName)
		if err != nil {
			errors = append(errors, err.Error())
		}
	}

	if errors != nil {
		return fmt.Errorf(strings.Join(errors, ";"))
	}
	return nil
}
