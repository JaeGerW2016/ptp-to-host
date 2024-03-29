package main

import (
	"encoding/json"
	"fmt"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/j-keck/arping"
	"github.com/lyft/cni-ipvlan-vpc-k8s/nl"
	"github.com/vishvananda/netlink"
	"math"
	"math/rand"
	"net"
	"os"
	"ptp-to-host/sysctl"
	"runtime"
	"sort"
	"strconv"
	"time"
)

// constants for full jitter backoff in milliseconds, and for nodeport marks
const (
	maxSleep             = 10000 // 10.00s
	baseSleep            = 20    //  0.02
	RPFilterTemplate     = "net.ipv4.conf.%s.rp_filter"
	podRulePriority      = 1024
	nodePortRulePriority = 512
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// PluginConf is whatever you expect your configuration json to be. This is whatever
// is passed in on stdin. Your plugin may wish to expose its functionality via
// runtime args, see CONVENTIONS.md in the CNI spec.
type PluginConf struct {
	types.NetConf

	// This is the previous result, when called in the context of a chained
	// plugin. Because this plugin supports multiple versions, we'll have to
	// parse this in two passes. If your plugin is not chained, this can be
	// removed (though you may wish to error if a non-chainable plugin is
	// chained.
	// If you need to modify the result before returning it, you will need
	// to actually convert it to a concrete versioned struct.
	RawPrevResult *map[string]interface{} `json:"prevResult"`
	PrevResult    *current.Result         `json:"-"`

	IPMasq             bool   `json:"ipMasq"`
	HostInterface      string `json:"hostInterface"`
	ContainerInterface string `json:"containerInterface"`
	MTU                int    `json:"mtu"`
	TableStart         int    `json:"routeTableStart"`
	NodePortMark       int    `json:"nodePortMark"`
	NodePorts          string `json:"nodePorts"`
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result.
	if conf.RawPrevResult != nil {
		resultBytes, err := json.Marshal(conf.RawPrevResult)
		if err != nil {
			return nil, fmt.Errorf("could not serialize prevResult: %v", err)
		}
		res, err := version.NewResult(conf.CNIVersion, resultBytes)
		if err != nil {
			return nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
		conf.RawPrevResult = nil
		conf.PrevResult, err = current.NewResultFromResult(res)
		if err != nil {
			return nil, fmt.Errorf("could not convert result to current version: %v", err)
		}
	}
	// End previous result parsing

	if conf.HostInterface == "" {
		return nil, fmt.Errorf("hostInterface must be specified")
	}

	// If the MTU is not set, use the one of the hostInterface
	if conf.MTU == 0 {
		baseMtu, err := nl.GetMtu(conf.HostInterface)
		if err != nil {
			return nil, fmt.Errorf("unable to get MTU for hostInterface")
		}
		conf.MTU = baseMtu
	}

	if conf.ContainerInterface == "" {
		return nil, fmt.Errorf("containerInterface must be specified")
	}

	if conf.NodePorts == "" {
		conf.NodePorts = "30000:32767"
	}

	if conf.NodePortMark == 0 {
		conf.NodePortMark = 0x2000
	}

	// start using tables by default at 256
	if conf.TableStart == 0 {
		conf.TableStart = 256
	}

	return &conf, nil
}

func enableForwarding(ipv4 bool, ipv6 bool) error {
	if ipv4 {
		err := ip.EnableIP4Forward()
		if err != nil {
			return fmt.Errorf("Could not enable IPv6 forwarding: %v", err)
		}
	}
	if ipv6 {
		err := ip.EnableIP6Forward()
		if err != nil {
			return fmt.Errorf("Could not enable IPv6 forwarding: %v", err)
		}
	}
	return nil
}

func setupSNAT(ifName string, comment string) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to locate iptables: %v", err)
	}
	rulespec := []string{"-o", ifName, "-j", "MASQUERADE"}
	if ipt.HasRandomFully() {
		rulespec = append(rulespec, "--random-fully")
	}
	rulespec = append(rulespec, "-m", "comment", "--comment", comment)
	return ipt.AppendUnique("nat", "POSTROUTING", rulespec...)
}

func findFreeTable(start int) (int, error) {
	allocatedTableIDs := make(map[int]bool)
	// combine V4 and V6 tables
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		if err != nil {
			return -1, err
		}
		for _, rule := range rules {
			allocatedTableIDs[rule.Table] = true
		}
	}
	// find first slot that's available for both V4 and V6 usage
	for i := start; i < math.MaxUint32; i++ {
		if !allocatedTableIDs[i] {
			return i, nil
		}
	}
	return -1, fmt.Errorf("failed to find free route table")
}

func addPolicyRules(veth *net.Interface, ipc *current.IPConfig, routes []*types.Route, tableStart int) error {
	table := -1

	// depend on netlink atomicity to win races for table slots on initial route add
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Dst.String() < routes[j].Dst.String()
	})

	// try 10 times to write to an empty table slot
	for i := 0; i < 10 && table == -1; i++ {
		var err error
		// jitter looking for an initial free table slot
		table, err = findFreeTable(tableStart + rand.Intn(1000))
		if err != nil {
			return err
		}

		// add routes to the policy routing table
		for _, route := range routes {
			err := netlink.RouteAdd(&netlink.Route{
				LinkIndex: veth.Index,
				Dst:       &route.Dst,
				Gw:        ipc.Address.IP,
				Table:     table,
			})
			if err != nil {
				table = -1
				break
			}
		}

		if table == -1 {
			// failed to add routes so sleep and try again on a different table
			wait := time.Duration(rand.Intn(int(math.Min(maxSleep,
				baseSleep*math.Pow(2, float64(i)))))) * time.Millisecond
			fmt.Fprintf(os.Stderr, "route table collision, retrying in %v\n", wait)
			time.Sleep(wait)
		}
	}

	// ensure we have a route table selected
	if table == -1 {
		return fmt.Errorf("failed to add routes to a free table")
	}

	// add policy route for traffic originating from a Pod
	rule := netlink.NewRule()
	rule.IifName = veth.Name
	rule.Table = table
	rule.Priority = podRulePriority

	err := netlink.RuleAdd(rule)
	if err != nil {
		return fmt.Errorf("failed to add policy rule %v: %v", rule, err)
	}

	return nil
}

func setupNodePortRule(ifName string, nodePorts string, nodePortMark int) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to locate iptables: %v", err)
	}

	// Create iptables rules to ensure that nodeport traffic is marked
	if err := ipt.AppendUnique("mangle", "PREROUTING", "-i", ifName, "-p", "tcp", "--dport", nodePorts, "-j", "CONNMARK", "--set-mark", strconv.Itoa(nodePortMark), "-m", "comment", "--comment", "NodePort Mark"); err != nil {
		return err
	}
	if err := ipt.AppendUnique("mangle", "PREROUTING", "-i", ifName, "-p", "udp", "--dport", nodePorts, "-j", "CONNMARK", "--set-mark", strconv.Itoa(nodePortMark), "-m", "comment", "--comment", "NodePort Mark"); err != nil {
		return err
	}
	if err := ipt.AppendUnique("mangle", "PREROUTING", "-i", "veth+", "-j", "CONNMARK", "--restore-mark", "-m", "comment", "--comment", "NodePort Mark"); err != nil {
		return err
	}

	// Use loose RP filter on host interface (RP filter does not take mark-based rules into account)
	_, err = sysctl.Sysctl(fmt.Sprintf(RPFilterTemplate, ifName), "2")
	if err != nil {
		return fmt.Errorf("failed to set RP filter to loose for interface %q: %v", ifName, err)
	}

	// add policy route for traffic from marked as nodeport
	rule := netlink.NewRule()
	rule.Mark = nodePortMark
	rule.Table = 254 // main table
	rule.Priority = nodePortRulePriority

	exists := false
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("Unable to retrive IP rules %v", err)
	}

	for _, r := range rules {
		if r.Table == rule.Table && r.Mark == rule.Mark && r.Priority == rule.Priority {
			exists = true
			break
		}
	}
	if !exists {
		err := netlink.RuleAdd(rule)
		if err != nil {
			return fmt.Errorf("failed to add policy rule %v: %v", rule, err)
		}
	}

	return nil
}

func setupContainerVeth(netns ns.NetNS, ifName string, mtu int, hostAddrs []netlink.Addr, masq, containerIPV4, containerIPV6 bool, k8sIfName string, pr *current.Result) (*current.Interface, *current.Interface, error) {
	hostInterface := &current.Interface{}
	containerInterface := &current.Interface{}

	err := netns.Do(func(hostNS ns.NetNS) error {
		hostVeth, contVeth0, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}
		hostInterface.Name = hostVeth.Name
		hostInterface.Mac = hostVeth.HardwareAddr.String()
		containerInterface.Name = contVeth0.Name
		// ip.SetupVeth does not retrieve MAC address from peer in veth
		containerNetlinkIface, _ := netlink.LinkByName(contVeth0.Name)
		containerInterface.Mac = containerNetlinkIface.Attrs().HardwareAddr.String()
		containerInterface.Sandbox = netns.Path()

		pr.Interfaces = append(pr.Interfaces, hostInterface, containerInterface)

		contVeth, err := net.InterfaceByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to look up %q: %v", ifName, err)
		}

		if masq {
			// enable forwarding and SNATing for traffic rerouted from kube-proxy
			err := enableForwarding(containerIPV4, containerIPV6)
			if err != nil {
				return err
			}

			err = setupSNAT(k8sIfName, "kube-proxy SNAT")
			if err != nil {
				return fmt.Errorf("failed to enable SNAT on %q: %v", k8sIfName, err)
			}
		}

		// add host routes for each dst hostInterface ip on dev contVeth
		for _, ipc := range hostAddrs {
			addrBits := 128
			if ipc.IP.To4() != nil {
				addrBits = 32
			}

			err := netlink.RouteAdd(&netlink.Route{
				LinkIndex: contVeth.Index,
				Scope:     netlink.SCOPE_LINK,
				Dst: &net.IPNet{
					IP:   ipc.IP,
					Mask: net.CIDRMask(addrBits, addrBits),
				},
			})

			if err != nil {
				return fmt.Errorf("failed to add host route dst %v: %v", ipc.IP, err)
			}
		}

		// add a default gateway pointed at the first hostAddr
		err = netlink.RouteAdd(&netlink.Route{
			LinkIndex: contVeth.Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       nil,
			Gw:        hostAddrs[0].IP,
		})
		if err != nil {
			return fmt.Errorf("failed to add default route %v: %v", hostAddrs[0].IP, err)
		}

		// Send a gratuitous arp for all borrowed v4 addresses
		for _, ipc := range pr.IPs {
			if ipc.Version == "4" {
				_ = arping.GratuitousArpOverIface(ipc.Address.IP, *contVeth)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return hostInterface, containerInterface, nil
}

func setupHostVeth(vethName string, hostAddrs []netlink.Addr, masq bool, tableStart int, result *current.Result) error {
	// no IPs to route
	if len(result.IPs) == 0 {
		return nil
	}

	// lookup by name as interface ids might have changed
	veth, err := net.InterfaceByName(vethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", vethName, err)
	}

	// add destination routes to Pod IPs
	for _, ipc := range result.IPs {
		addrBits := 128
		if ipc.Address.IP.To4() != nil {
			addrBits = 32
		}

		err := netlink.RouteAdd(&netlink.Route{
			LinkIndex: veth.Index,
			Scope:     netlink.SCOPE_LINK,
			Dst: &net.IPNet{
				IP:   ipc.Address.IP,
				Mask: net.CIDRMask(addrBits, addrBits),
			},
		})

		if err != nil {
			return fmt.Errorf("failed to add host route dst %v: %v", ipc.Address.IP, err)
		}
	}

	// add policy rules for traffic coming in from Pods and destined for the VPC
	err = addPolicyRules(veth, result.IPs[0], result.Routes, tableStart)
	if err != nil {
		return fmt.Errorf("failed to add policy rules: %v", err)
	}

	// Send a gratuitous arp for all borrowed v4 addresses
	for _, ipc := range hostAddrs {
		if ipc.IP.To4() != nil {
			_ = arping.GratuitousArpOverIface(ipc.IP, *veth)
		}
	}

	return nil
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	if conf.PrevResult == nil {
		return fmt.Errorf("must be called as chained plugin")
	}

	// This is some sample code to generate the list of container-side IPs.
	// We're casting the prevResult to a 0.3.0 response, which can also include
	// host-side IPs (but doesn't when converted from a 0.2.0 response).
	containerIPs := make([]net.IP, 0, len(conf.PrevResult.IPs))
	if conf.CNIVersion != "0.3.0" {
		for _, ip := range conf.PrevResult.IPs {
			containerIPs = append(containerIPs, ip.Address.IP)
		}
	} else {
		for _, ip := range conf.PrevResult.IPs {
			if ip.Interface == nil {
				continue
			}
			intIdx := *ip.Interface
			// Every IP is indexed in to the interfaces array, with "-1" standing
			// for an unknown interface (which we'll assume to be Container-side
			// Skip all IPs we know belong to an interface with the wrong name.
			if intIdx >= 0 && intIdx < len(conf.PrevResult.Interfaces) && conf.PrevResult.Interfaces[intIdx].Name != args.IfName {
				continue
			}
			containerIPs = append(containerIPs, ip.Address.IP)
		}
	}
	if len(containerIPs) == 0 {
		return fmt.Errorf("got no container IPs")
	}

	iface, err := netlink.LinkByName(conf.HostInterface)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", conf.HostInterface, err)
	}

	hostAddrs, err := netlink.AddrList(iface, netlink.FAMILY_ALL)
	if err != nil || len(hostAddrs) == 0 {
		return fmt.Errorf("failed to get host IP addresses for %q: %v", iface, err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	containerIPV4 := false
	containerIPV6 := false
	for _, ipc := range containerIPs {
		if ipc.To4() != nil {
			containerIPV4 = true
		} else {
			containerIPV6 = true
		}
	}

	hostInterface, _, err := setupContainerVeth(netns, conf.ContainerInterface, conf.MTU,
		hostAddrs, conf.IPMasq, containerIPV4, containerIPV6, args.IfName, conf.PrevResult)
	if err != nil {
		return err
	}

	if err = setupHostVeth(hostInterface.Name, hostAddrs, conf.IPMasq, conf.TableStart, conf.PrevResult); err != nil {
		return err
	}

	if conf.IPMasq {
		err := enableForwarding(containerIPV4, containerIPV6)
		if err != nil {
			return err
		}

		chain := utils.FormatChainName(conf.Name, args.ContainerID)
		comment := utils.FormatComment(conf.Name, args.ContainerID)
		for _, ipc := range containerIPs {
			addrBits := 128
			if ipc.To4() != nil {
				addrBits = 32
			}

			if err = ip.SetupIPMasq(&net.IPNet{IP: ipc, Mask: net.CIDRMask(addrBits, addrBits)}, chain, comment); err != nil {
				return err
			}
		}
	}

	if err = setupNodePortRule(conf.HostInterface, conf.NodePorts, conf.NodePortMark); err != nil {
		return err
	}

	// Pass through the result for the next plugin
	return types.PrintResult(conf.PrevResult, conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	// If the device isn't there then don't try to clean up IP masq either.
	var ipnets []netlink.Addr
	vethPeerIndex := -1
	_ = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		var err error

		// lookup pod IPs from the args.IfName device (usually eth0)
		if conf.IPMasq {
			iface, err := netlink.LinkByName(args.IfName)
			if err != nil {
				if err.Error() == "Link not found" {
					return ip.ErrLinkNotFound
				}
				return fmt.Errorf("failed to lookup %q: %v", args.IfName, err)
			}

			ipnets, err = netlink.AddrList(iface, netlink.FAMILY_ALL)
			if err != nil || len(ipnets) == 0 {
				return fmt.Errorf("failed to get IP addresses for %q: %v", args.IfName, err)
			}
		}

		vethIface, err := netlink.LinkByName(conf.ContainerInterface)
		if err != nil && err != ip.ErrLinkNotFound {
			return err
		}
		vethPeerIndex, _ = netlink.VethPeerIndex(&netlink.Veth{LinkAttrs: *vethIface.Attrs()})
		return nil
	})

	if conf.IPMasq {
		chain := utils.FormatChainName(conf.Name, args.ContainerID)
		comment := utils.FormatComment(conf.Name, args.ContainerID)
		for _, ipn := range ipnets {
			addrBits := 128
			if ipn.IP.To4() != nil {
				addrBits = 32
			}

			_ = ip.TeardownIPMasq(&net.IPNet{IP: ipn.IP, Mask: net.CIDRMask(addrBits, addrBits)}, chain, comment)
		}

		if vethPeerIndex != -1 {
			link, err := netlink.LinkByIndex(vethPeerIndex)
			if err != nil {
				return nil
			}

			rule := netlink.NewRule()
			rule.IifName = link.Attrs().Name
			// ignore errors as we might be called multiple times
			_ = netlink.RuleDel(rule)
			_ = netlink.LinkDel(link)
		}
	}

	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// run the IPAM plugin and get back the config to apply
	err = ipam.ExecCheck(conf.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}
	if conf.NetConf.RawPrevResult == nil {
		return fmt.Errorf("ptp: Required prevResult missing")
	}
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return err
	}
	// Convert whatever the IPAM result was into the current Result type
	result, err := current.NewResultFromResult(conf.PrevResult)
	if err != nil {
		return err
	}

	var contMap current.Interface
	// Find interfaces for name whe know, that of host-device inside container
	for _, intf := range result.Interfaces {
		if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("Sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	//
	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {

		// Check interface against values found in the container
		err := validateCniContainerInterface(contMap)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func validateCniContainerInterface(intf current.Interface) error {

	var link netlink.Link
	var err error

	if intf.Name == "" {
		return fmt.Errorf("Container interface name missing in prevResult: %v", intf.Name)
	}
	link, err = netlink.LinkByName(intf.Name)
	if err != nil {
		return fmt.Errorf("ptp: Container Interface name in prevResult: %s not found", intf.Name)
	}
	if intf.Sandbox == "" {
		return fmt.Errorf("ptp: Error: Container interface %s should not be in host namespace", link.Attrs().Name)
	}

	_, isVeth := link.(*netlink.Veth)
	if !isVeth {
		return fmt.Errorf("Error: Container interface %s not of type veth/p2p", link.Attrs().Name)
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return fmt.Errorf("ptp: Interface %s Mac %s doesn't match container Mac: %s", intf.Name, intf.Mac, link.Attrs().HardwareAddr)
		}
	}

	return nil
}

func main() {
	rand.Seed(time.Now().UnixNano())
	skel.PluginMain(cmdAdd, cmdDel, cmdCheck, version.All,"ptp-to-host")
}
