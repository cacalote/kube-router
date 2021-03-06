package controllers

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudnativelabs/kube-router/app/options"
	"github.com/cloudnativelabs/kube-router/app/watchers"
	"github.com/cloudnativelabs/kube-router/utils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/golang/glog"
	bgpapi "github.com/osrg/gobgp/api"
	"github.com/osrg/gobgp/config"
	"github.com/osrg/gobgp/packet/bgp"
	gobgp "github.com/osrg/gobgp/server"
	"github.com/osrg/gobgp/table"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type NetworkRoutingController struct {
	nodeIP               net.IP
	nodeHostName         string
	mu                   sync.Mutex
	clientset            *kubernetes.Clientset
	bgpServer            *gobgp.BgpServer
	syncPeriod           time.Duration
	clusterCIDR          string
	hostnameOverride     string
	advertiseClusterIp   bool
	defaultNodeAsnNumber uint32
	nodeAsnNumber        uint32
	globalPeerRouters    []string
	globalPeerAsnNumber  uint32
	bgpFullMeshMode      bool
}

var (
	activeNodes = make(map[string]bool)
)

func (nrc *NetworkRoutingController) Run(stopCh <-chan struct{}, wg *sync.WaitGroup) {

	cidr, err := utils.GetPodCidrFromCniSpec("/etc/cni/net.d/10-kuberouter.conf")
	if err != nil {
		glog.Errorf("Failed to get pod CIDR from CNI conf file: %s", err.Error())
	}
	cidrlen, _ := cidr.Mask.Size()
	oldCidr := cidr.IP.String() + "/" + strconv.Itoa(cidrlen)

	currentCidr, err := utils.GetPodCidrFromNodeSpec(nrc.clientset, nrc.hostnameOverride)
	if err != nil {
		glog.Errorf("Failed to get pod CIDR from node spec: %s", err.Error())
	}

	if len(cidr.IP) == 0 || strings.Compare(oldCidr, currentCidr) != 0 {
		err = utils.InsertPodCidrInCniSpec("/etc/cni/net.d/10-kuberouter.conf", currentCidr)
		if err != nil {
			glog.Errorf("Failed to insert pod CIDR into CNI conf file: %s", err.Error())
		}
	}

	t := time.NewTicker(nrc.syncPeriod)
	defer t.Stop()
	defer wg.Done()

	glog.Infof("Starting network route controller")

	if len(nrc.clusterCIDR) != 0 {
		args := []string{"-s", nrc.clusterCIDR, "!", "-d", nrc.clusterCIDR, "-j", "MASQUERADE"}
		iptablesCmdHandler, err := iptables.New()
		if err != nil {
			glog.Errorf("Failed to add iptable rule to masqurade outbound traffic from pods due to %s. External connectivity will not work.", err.Error())
		}
		err = iptablesCmdHandler.AppendUnique("nat", "POSTROUTING", args...)
		if err != nil {
			glog.Errorf("Failed to add iptable rule to masqurade outbound traffic from pods due to %s. External connectivity will not work.", err.Error())
		}
	}

	// Wait till we are ready to launch BGP server
	for {
		ok := nrc.startBgpServer()
		if !ok {
			select {
			case <-stopCh:
				glog.Infof("Shutting down network routes controller")
				return
			case <-t.C:
			}
			continue
		} else {
			break
		}
	}

	// loop forever till notified to stop on stopCh
	for {
		select {
		case <-stopCh:
			glog.Infof("Shutting down network routes controller")
			return
		default:
		}

		// add the current set of nodes (excluding self) as BGP peers. Nodes form full mesh
		nrc.syncPeers()

		// advertise cluster IP for the service to be reachable via host
		if nrc.advertiseClusterIp {
			glog.Infof("Advertising cluster ips")
			for _, svc := range watchers.ServiceWatcher.List() {
				if svc.Spec.Type == "ClusterIP" || svc.Spec.Type == "NodePort" {

					// skip headless services
					if svc.Spec.ClusterIP == "None" || svc.Spec.ClusterIP == "" {
						continue
					}

					glog.Infof("found a service of cluster ip type")
					nrc.AdvertiseClusterIp(svc.Spec.ClusterIP)
				}
			}
		}

		glog.Infof("Performing periodic syn of the routes")
		err := nrc.advertiseRoute()
		if err != nil {
			glog.Errorf("Failed to advertise route: %s", err.Error())
		}

		select {
		case <-stopCh:
			glog.Infof("Shutting down network routes controller")
			return
		case <-t.C:
		}
	}
}

func (nrc *NetworkRoutingController) watchBgpUpdates() {
	watcher := nrc.bgpServer.Watch(gobgp.WatchBestPath(false))
	for {
		select {
		case ev := <-watcher.Event():
			switch msg := ev.(type) {
			case *gobgp.WatchEventBestPath:
				glog.Infof("Processing bgp route advertisement from peer")
				for _, path := range msg.PathList {
					if path.IsLocal() {
						continue
					}
					if err := nrc.injectRoute(path); err != nil {
						glog.Errorf("Failed to inject routes due to: " + err.Error())
						continue
					}
				}
			}
		}
	}
}

func (nrc *NetworkRoutingController) advertiseRoute() error {

	cidr, err := utils.GetPodCidrFromNodeSpec(nrc.clientset, nrc.hostnameOverride)
	if err != nil {
		return err
	}
	cidrStr := strings.Split(cidr, "/")
	subnet := cidrStr[0]
	cidrLen, err := strconv.Atoi(cidrStr[1])
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		bgp.NewPathAttributeNextHop(nrc.nodeIP.String()),
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{4000, 400000, 300000, 40001})}),
	}
	glog.Infof("Advertising route: '%s/%s via %s' to peers", subnet, strconv.Itoa(cidrLen), nrc.nodeIP.String())
	if _, err := nrc.bgpServer.AddPath("", []*table.Path{table.NewPath(nil, bgp.NewIPAddrPrefix(uint8(cidrLen),
		subnet), false, attrs, time.Now(), false)}); err != nil {
		return fmt.Errorf(err.Error())
	}
	return nil
}

func (nrc *NetworkRoutingController) AdvertiseClusterIp(clusterIp string) error {

	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		bgp.NewPathAttributeNextHop(nrc.nodeIP.String()),
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{4000, 400000, 300000, 40001})}),
	}
	glog.Infof("Advertising route: '%s/%s via %s' to peers", clusterIp, strconv.Itoa(32), nrc.nodeIP.String())
	if _, err := nrc.bgpServer.AddPath("", []*table.Path{table.NewPath(nil, bgp.NewIPAddrPrefix(uint8(32),
		clusterIp), false, attrs, time.Now(), false)}); err != nil {
		return fmt.Errorf(err.Error())
	}
	return nil
}

func (nrc *NetworkRoutingController) injectRoute(path *table.Path) error {
	nexthop := path.GetNexthop()
	nlri := path.GetNlri()
	dst, _ := netlink.ParseIPNet(nlri.String())
	route := &netlink.Route{
		Dst:      dst,
		Gw:       nexthop,
		Protocol: 0x11,
	}

	glog.Infof("Inject route: '%s via %s' from peer to routing table", dst, nexthop)
	return netlink.RouteReplace(route)
}

func (nrc *NetworkRoutingController) Cleanup() {
}

// Refresh the peer relationship rest of the nodes in the cluster. Node add/remove
// events should ensure peer relationship with only currently active nodes. In case
// we miss any events from API server this method which is called periodically
// ensure peer relationship with removed nodes is deleted.
func (nrc *NetworkRoutingController) syncPeers() {

	glog.Infof("Syncing BGP peers for the node.")

	// get the current list of the nodes from API server
	nodes, err := nrc.clientset.Core().Nodes().List(metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Failed to list nodes from API server due to: %s. Can not perform BGP peer sync", err.Error())
		return
	}

	// establish peer with current set of nodes
	currentNodes := make([]string, 0)
	for _, node := range nodes.Items {
		nodeIP, _ := getNodeIP(&node)

		// skip self
		if nodeIP.String() == nrc.nodeIP.String() {
			continue
		}

		// if node full mesh is not requested then just peer with nodes with same ASN (run iBGP among same ASN peers)
		if !nrc.bgpFullMeshMode {
			// if the node is not annotated with ASN number or with invalid ASN skip peering
			nodeasn, ok := node.ObjectMeta.Annotations["net.kuberouter.nodeasn"]
			if !ok {
				glog.Infof("Not peering with the Node %s as ASN number of the node is unknown.", nodeIP.String())
				continue
			}

			asnNo, err := strconv.ParseUint(nodeasn, 0, 32)
			if err != nil {
				glog.Infof("Not peering with the Node %s as ASN number of the node is invalid.", nodeIP.String())
				continue
			}

			// if the nodes ASN number is different from ASN number of current node skipp peering
			if nrc.nodeAsnNumber != uint32(asnNo) {
				glog.Infof("Not peering with the Node %s as ASN number of the node is different.", nodeIP.String())
				continue
			}
		}

		currentNodes = append(currentNodes, nodeIP.String())
		activeNodes[nodeIP.String()] = true
		n := &config.Neighbor{
			Config: config.NeighborConfig{
				NeighborAddress: nodeIP.String(),
				PeerAs:          nrc.defaultNodeAsnNumber,
			},
		}
		// TODO: check if a node is alredy added as nieighbour in a better way than add and catch error
		if err := nrc.bgpServer.AddNeighbor(n); err != nil {
			if !strings.Contains(err.Error(), "Can't overwrite the existing peer") {
				glog.Errorf("Failed to add node %s as peer due to %s", nodeIP.String(), err)
			}
		}
	}

	// find the list of the node removed, from the last known list of active nodes
	removedNodes := make([]string, 0)
	for ip, _ := range activeNodes {
		stillActive := false
		for _, node := range currentNodes {
			if ip == node {
				stillActive = true
				break
			}
		}
		if !stillActive {
			removedNodes = append(removedNodes, ip)
		}
	}

	// delete the neighbor for the node that is removed
	for _, ip := range removedNodes {
		n := &config.Neighbor{
			Config: config.NeighborConfig{
				NeighborAddress: ip,
				PeerAs:          nrc.defaultNodeAsnNumber,
			},
		}
		if err := nrc.bgpServer.DeleteNeighbor(n); err != nil {
			glog.Errorf("Failed to remove node %s as peer due to %s", ip, err)
		}
		delete(activeNodes, ip)
	}
}

// Handle updates from Node watcher. Node watcher calls this method whenever there is
// new node is added or old node is deleted. So peer up with new node and drop peering
// from old node
func (nrc *NetworkRoutingController) OnNodeUpdate(nodeUpdate *watchers.NodeUpdate) {
	nrc.mu.Lock()
	defer nrc.mu.Unlock()

	node := nodeUpdate.Node
	nodeIP, _ := getNodeIP(node)
	if nodeUpdate.Op == watchers.ADD {
		glog.Infof("Received node %s added update from watch API so peer with new node", nodeIP)
		n := &config.Neighbor{
			Config: config.NeighborConfig{
				NeighborAddress: nodeIP.String(),
				PeerAs:          nrc.defaultNodeAsnNumber,
			},
		}
		if err := nrc.bgpServer.AddNeighbor(n); err != nil {
			glog.Errorf("Failed to add node %s as peer due to %s", nodeIP, err)
		}
		activeNodes[nodeIP.String()] = true
	} else if nodeUpdate.Op == watchers.REMOVE {
		glog.Infof("Received node %s removed update from watch API, so remove node from peer", nodeIP)
		n := &config.Neighbor{
			Config: config.NeighborConfig{
				NeighborAddress: nodeIP.String(),
				PeerAs:          nrc.defaultNodeAsnNumber,
			},
		}
		if err := nrc.bgpServer.DeleteNeighbor(n); err != nil {
			glog.Errorf("Failed to remove node %s as peer due to %s", nodeIP, err)
		}
		delete(activeNodes, nodeIP.String())
	}
}

func (nrc *NetworkRoutingController) startBgpServer() bool {

	var nodeAsnNumber uint32
	node, err := utils.GetNodeObject(nrc.clientset, nrc.hostnameOverride)
	if err != nil {
		panic("Failed to get node object from api server due to " + err.Error())
	}

	if nrc.bgpFullMeshMode {
		nodeAsnNumber = nrc.defaultNodeAsnNumber
	} else {
		nodeasn, ok := node.ObjectMeta.Annotations["net.kuberouter.nodeasn"]
		if !ok {
			glog.Infof("Could not find ASN number for the node. Node need to be annotated with ASN number details to start BGP server.")
			return false
		} else {
			glog.Infof("Found ASN for the node to be %s from the node annotations", nodeasn)
			asnNo, err := strconv.ParseUint(nodeasn, 0, 32)
			if err != nil {
				glog.Errorf("Failed to parse ASN number specified for the the node")
				return false
			}
			nodeAsnNumber = uint32(asnNo)
		}
		nrc.nodeAsnNumber = nodeAsnNumber
	}

	nrc.bgpServer = gobgp.NewBgpServer()
	go nrc.bgpServer.Serve()

	g := bgpapi.NewGrpcServer(nrc.bgpServer, ":50051")
	go g.Serve()

	global := &config.Global{
		Config: config.GlobalConfig{
			As:       nodeAsnNumber,
			RouterId: nrc.nodeIP.String(),
		},
	}

	if err := nrc.bgpServer.Start(global); err != nil {
		panic("Failed to start BGP server due to : " + err.Error())
	}

	go nrc.watchBgpUpdates()

	// if the global routing peer is configured then peer with it
	// else peer with node specific BGP peer
	if len(nrc.globalPeerRouters) != 0 && nrc.globalPeerAsnNumber != 0 {
		for _, peer := range nrc.globalPeerRouters {
			n := &config.Neighbor{
				Config: config.NeighborConfig{
					NeighborAddress: peer,
					PeerAs:          nrc.globalPeerAsnNumber,
				},
			}
			if err := nrc.bgpServer.AddNeighbor(n); err != nil {
				panic("Failed to peer with global peer router due to: " + peer)
			}
		}
	} else {
		nodeBgpPeerAsn, ok := node.ObjectMeta.Annotations["net.kuberouter.node.bgppeer.asn"]
		if !ok {
			glog.Infof("Could not find BGP peer info for the node in the node annotations so skipping configuring peer.")
			return true
		}
		asnNo, err := strconv.ParseUint(nodeBgpPeerAsn, 0, 32)
		if err != nil {
			panic("Failed to parse ASN number specified for the the node in the annotations")
		}
		peerAsnNo := uint32(asnNo)

		nodeBgpPeersAnnotation, ok := node.ObjectMeta.Annotations["net.kuberouter.node.bgppeer.address"]
		if !ok {
			glog.Infof("Could not find BGP peer info for the node in the node annotations so skipping configuring peer.")
			return true
		}
		nodePeerRouters := make([]string, 0)
		if strings.Contains(nodeBgpPeersAnnotation, ",") {
			ips := strings.Split(nodeBgpPeersAnnotation, ",")
			for _, ip := range ips {
				if net.ParseIP(ip) == nil {
					panic("Invalid node BGP peer router ip in the annotation: " + ip)
				}
			}
			nodePeerRouters = append(nodePeerRouters, ips...)
		} else {
			if net.ParseIP(nodeBgpPeersAnnotation) == nil {
				panic("Invalid node BGP peer router ip: " + nodeBgpPeersAnnotation)
			}
			nodePeerRouters = append(nodePeerRouters, nodeBgpPeersAnnotation)
		}
		for _, peer := range nodePeerRouters {
			glog.Infof("Node is configured to peer with %s in ASN %v from the node annotations", peer, peerAsnNo)
			n := &config.Neighbor{
				Config: config.NeighborConfig{
					NeighborAddress: peer,
					PeerAs:          peerAsnNo,
				},
			}
			if err := nrc.bgpServer.AddNeighbor(n); err != nil {
				panic("Failed to peer with node specific BGP peer router: " + peer + " due to " + err.Error())
			}
		}

		glog.Infof("Successfully configured  %s in ASN %v as BGP peer to the node", nodeBgpPeersAnnotation, peerAsnNo)
	}

	return true
}

func NewNetworkRoutingController(clientset *kubernetes.Clientset, kubeRouterConfig *options.KubeRouterConfig) (*NetworkRoutingController, error) {

	nrc := NetworkRoutingController{}

	nrc.bgpFullMeshMode = kubeRouterConfig.FullMeshMode
	nrc.clusterCIDR = kubeRouterConfig.ClusterCIDR
	nrc.syncPeriod = kubeRouterConfig.RoutesSyncPeriod
	nrc.clientset = clientset

	if len(kubeRouterConfig.ClusterAsn) != 0 {
		asn, err := strconv.ParseUint(kubeRouterConfig.ClusterAsn, 0, 32)
		if err != nil {
			panic("Invalid cluster ASN: " + err.Error())
		}
		if asn > 65534 || asn < 64512 {
			panic("Invalid ASN number for cluster ASN")
		}
		nrc.defaultNodeAsnNumber = uint32(asn)
	} else {
		nrc.defaultNodeAsnNumber = 64512 // this magic number is first of the private ASN range, use it as default
	}

	nrc.advertiseClusterIp = kubeRouterConfig.AdvertiseClusterIp

	if (len(kubeRouterConfig.PeerRouter) != 0 && len(kubeRouterConfig.PeerAsn) == 0) ||
		(len(kubeRouterConfig.PeerRouter) == 0 && len(kubeRouterConfig.PeerAsn) != 0) {
		panic("Either both or none of the params --peer-asn, --peer-router must be specified")
	}

	if len(kubeRouterConfig.PeerRouter) != 0 && len(kubeRouterConfig.PeerAsn) != 0 {

		if strings.Contains(kubeRouterConfig.PeerRouter, ",") {
			ips := strings.Split(kubeRouterConfig.PeerRouter, ",")
			for _, ip := range ips {
				if net.ParseIP(ip) == nil {
					panic("Invalid global BGP peer router ip: " + kubeRouterConfig.PeerRouter)
				}
			}
			nrc.globalPeerRouters = append(nrc.globalPeerRouters, ips...)

		} else {
			if net.ParseIP(kubeRouterConfig.PeerRouter) == nil {
				panic("Invalid global BGP peer router ip: " + kubeRouterConfig.PeerRouter)
			}
			nrc.globalPeerRouters = append(nrc.globalPeerRouters, kubeRouterConfig.PeerRouter)
		}

		asn, err := strconv.ParseUint(kubeRouterConfig.PeerAsn, 0, 32)
		if err != nil {
			panic("Invalid global BGP peer ASN: " + err.Error())
		}
		if asn > 65534 {
			panic("Invalid ASN number for global BGP peer")
		}
		nrc.globalPeerAsnNumber = uint32(asn)
	}

	nrc.hostnameOverride = kubeRouterConfig.HostnameOverride
	node, err := utils.GetNodeObject(clientset, nrc.hostnameOverride)
	if err != nil {
		panic(err.Error())
	}

	nrc.nodeHostName = node.Name

	nodeIP, err := getNodeIP(node)
	if err != nil {
		panic(err.Error())
	}
	nrc.nodeIP = nodeIP

	watchers.NodeWatcher.RegisterHandler(&nrc)

	return &nrc, nil
}
