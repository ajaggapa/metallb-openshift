// SPDX-License-Identifier:Apache-2.0

package bgptests

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.universe.tf/e2etest/pkg/config"
	"go.universe.tf/e2etest/pkg/executor"
	"go.universe.tf/e2etest/pkg/frr"
	frrcontainer "go.universe.tf/e2etest/pkg/frr/container"
	"go.universe.tf/e2etest/pkg/ipfamily"
	jigservice "go.universe.tf/e2etest/pkg/jigservice"
	"go.universe.tf/e2etest/pkg/k8s"
	"go.universe.tf/e2etest/pkg/metallb"
	"go.universe.tf/e2etest/pkg/routes"
	"go.universe.tf/e2etest/pkg/wget"
	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var ErrStaleRoute = errors.New("stale route")

func validateFRRPeeredWithAllNodes(cs clientset.Interface, c *frrcontainer.FRR, ipFamily ipfamily.Family) {
	allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	validateFRRPeeredWithNodes(allNodes.Items, c, ipFamily)
}

func validateFRRNotPeeredWithNodes(nodes []corev1.Node, c *frrcontainer.FRR, ipFamily ipfamily.Family) {
	for _, node := range nodes {
		ginkgo.By(fmt.Sprintf("checking node %s is not peered with the frr instance %s", node.Name, c.Name))
		Eventually(func() error {
			neighbors, err := frr.NeighborsInfo(c)
			Expect(err).NotTo(HaveOccurred())
			err = frr.NeighborsMatchNodes([]corev1.Node{node}, neighbors, ipFamily, c.RouterConfig.VRF)
			return err
		}, 4*time.Minute, 1*time.Second).Should(MatchError(ContainSubstring("not established")))
	}
}

func validateFRRPeeredWithNodes(nodes []corev1.Node, c *frrcontainer.FRR, ipFamily ipfamily.Family) {
	ginkgo.By(fmt.Sprintf("checking nodes are peered with the frr instance %s", c.Name))
	Eventually(func() error {
		neighbors, err := frr.NeighborsInfo(c)
		Expect(err).NotTo(HaveOccurred())
		err = frr.NeighborsMatchNodes(nodes, neighbors, ipFamily, c.RouterConfig.VRF)
		if err != nil {
			return fmt.Errorf("failed to match neighbors for %s, %w", c.Name, err)
		}
		return nil
	}, 4*time.Minute, 1*time.Second).ShouldNot(HaveOccurred(), "timed out waiting to validate nodes peered with the frr instance")
}

func validateService(svc *corev1.Service, nodes []corev1.Node, c *frrcontainer.FRR) {
	ginkgo.By(fmt.Sprintf("Validating service %s is announced to container: %s", svc.Name, c.Name))
	Eventually(func() error {
		return validateServiceNoWait(svc, nodes, c)
	}, 4*time.Minute, 1*time.Second).ShouldNot(HaveOccurred(), "timed out waiting to validate service")
}

func validateServiceNoWait(svc *corev1.Service, nodes []corev1.Node, c *frrcontainer.FRR) error {
	port := strconv.Itoa(int(svc.Spec.Ports[0].Port))

	if len(svc.Status.LoadBalancer.Ingress) == 2 {
		ip1 := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
		ip2 := net.ParseIP(svc.Status.LoadBalancer.Ingress[1].IP)
		Expect(ip1.To4()).NotTo(Equal(ip2.To4()))
	}
	for _, ip := range svc.Status.LoadBalancer.Ingress {
		ingressIP := jigservice.GetIngressPoint(&ip)

		// TODO: in case of VRF there's currently no host wiring to the service.
		// We only validate the routes are propagated correctly but
		// we don't try to hit the service.
		if c.RouterConfig.VRF == "" {
			hostport := net.JoinHostPort(ingressIP, port)
			address := fmt.Sprintf("http://%s/", hostport)
			err := wget.Do(address, c)
			if err != nil {
				return fmt.Errorf("failed to wget from %s to %s: %w", c.Name, address, err)
			}
		}

		frrRoutesV4, frrRoutesV6, err := frr.Routes(c)
		if err != nil {
			return err
		}
		serviceIPFamily := ipfamily.IPv4
		frrRoutes, ok := frrRoutesV4[ingressIP]
		if !ok {
			frrRoutes, ok = frrRoutesV6[ingressIP]
			serviceIPFamily = ipfamily.IPv6
		}
		if !ok {
			return fmt.Errorf("%s not found in frr routes %v %v", ingressIP, frrRoutesV4, frrRoutesV6)
		}
		if !strings.EqualFold(frrRoutes.Origin, "IGP") {
			return fmt.Errorf("route for %s not set with igp origin", ingressIP)
		}

		err = frr.RoutesMatchNodes(nodes, frrRoutes, serviceIPFamily, c.RouterConfig.VRF)
		if err != nil {
			return fmt.Errorf("peer: %s errored: %w", c.Name, err)
		}

		// The BGP routes will not match the nodes if static routes were added.
		if c.Network != defaultNextHopSettings.multiHopNetwork &&
			c.Network != vrfNextHopSettings.multiHopNetwork {
			advertised := routes.ForIP(ingressIP, c)
			err = routes.MatchNodes(nodes, advertised, serviceIPFamily, c.RouterConfig.VRF)
			if err != nil {
				return err
			}
		}

		var serr error
		for k, v := range frrRoutesV4 {
			if v.Stale {
				serr = errors.Join(serr, errors.New(fmt.Sprintf("%s -%v", k, v)))
			}
		}
		for k, v := range frrRoutesV6 {
			if v.Stale {
				serr = errors.Join(serr, errors.New(fmt.Sprintf("%s -%v", k, v)))
			}
		}
		if serr != nil {
			return errors.Join(ErrStaleRoute, serr)
		}
	}
	return nil
}

func frrIsPairedOnPods(cs clientset.Interface, n *frrcontainer.FRR, ipFamily ipfamily.Family) {
	pods, err := metallb.SpeakerPods(cs)
	Expect(err).NotTo(HaveOccurred())
	podExecutor, err := FRRProvider.FRRExecutorFor(pods[0].Namespace, pods[0].Name)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() error {
		addresses := n.AddressesForFamily(ipFamily)
		for _, address := range addresses {
			vrfSelector := ""
			if n.RouterConfig.VRF != "" {
				vrfSelector = fmt.Sprintf("vrf %s", n.RouterConfig.VRF)
			}
			toParse, err := podExecutor.Exec("vtysh", "-c", fmt.Sprintf("show bgp %s neighbor %s json", vrfSelector, address))
			if err != nil {
				return err
			}
			res, err := frr.NeighborConnected(toParse)
			if err != nil {
				return err
			}
			if !res {
				return fmt.Errorf("expecting neighbor %s to be connected", n.Ipv4)
			}
		}
		return nil
	}, 4*time.Minute, 1*time.Second).ShouldNot(HaveOccurred())
}

func bfdDebugLog(nodeName, format string, args ...interface{}) {
	ginkgo.GinkgoWriter.Printf("[BFD debug %s] "+format+"\n", append([]interface{}{nodeName}, args...)...)
}

func execRouteGet(exec executor.Executor, family ipfamily.Family, dest string) (string, error) {
	ipFlag := "-4"
	if family == ipfamily.IPv6 {
		ipFlag = "-6"
	}
	return exec.Exec("bash", "-c", fmt.Sprintf("ip %s route get %s", ipFlag, dest))
}

func nodeForBFDDebug(nodes []corev1.Node) *corev1.Node {
	for i := range nodes {
		if _, ok := nodes[i].Labels["node-role.kubernetes.io/worker"]; ok {
			return &nodes[i]
		}
	}
	if len(nodes) > 0 {
		return &nodes[0]
	}
	return nil
}

func dumpBFDPairDebugInfo(cs clientset.Interface, nodes []corev1.Node, containers []*frrcontainer.FRR) {
	node := nodeForBFDDebug(nodes)
	if node == nil {
		ginkgo.GinkgoWriter.Printf("[BFD debug] no nodes available for debug dump\n")
		return
	}
	nodeName := node.Name
	bfdDebugLog(nodeName, "collecting BFD pair debug info (selected from %d cluster nodes)", len(nodes))

	v4IPs, err := k8s.NodeIPsForFamily([]corev1.Node{*node}, ipfamily.IPv4, "")
	if err != nil {
		bfdDebugLog(nodeName, "failed to get node IPv4 addresses: %v", err)
	}
	v6IPs, err := k8s.NodeIPsForFamily([]corev1.Node{*node}, ipfamily.IPv6, "")
	if err != nil {
		bfdDebugLog(nodeName, "failed to get node IPv6 addresses: %v", err)
	}
	bfdDebugLog(nodeName, "node IPv4 addresses: %v", v4IPs)
	bfdDebugLog(nodeName, "node IPv6 addresses: %v", v6IPs)

	speakerPod, err := metallb.SpeakerPodInNode(cs, nodeName)
	if err != nil {
		bfdDebugLog(nodeName, "failed to get speaker pod: %v", err)
	} else {
		bfdDebugLog(nodeName, "speaker pod: %s/%s", speakerPod.Namespace, speakerPod.Name)
	}

	var speakerFRR executor.Executor
	if FRRProvider != nil && speakerPod != nil {
		speakerFRR, err = FRRProvider.FRRExecutorFor(speakerPod.Namespace, speakerPod.Name)
		if err != nil {
			bfdDebugLog(nodeName, "failed to get speaker FRR executor: %v", err)
		}
	}

	nodeExec := executor.ForContainer(nodeName)
	for _, family := range []ipfamily.Family{ipfamily.IPv4, ipfamily.IPv6} {
		for _, c := range containers {
			for _, addr := range c.AddressesForFamily(family) {
				if addr == "" {
					continue
				}
				if out, err := execRouteGet(nodeExec, family, addr); err != nil {
					bfdDebugLog(nodeName, "node (container exec) ip route get %s for container %s peer %s: err=%v out=%q", family, c.Name, addr, err, out)
				} else {
					bfdDebugLog(nodeName, "node (container exec) ip route get %s for container %s peer %s: %s", family, c.Name, addr, strings.TrimSpace(out))
				}
				if speakerFRR != nil {
					if out, err := execRouteGet(speakerFRR, family, addr); err != nil {
						bfdDebugLog(nodeName, "speaker frr ip route get %s for container %s peer %s: err=%v out=%q", family, c.Name, addr, err, out)
					} else {
						bfdDebugLog(nodeName, "speaker frr ip route get %s for container %s peer %s: %s", family, c.Name, addr, strings.TrimSpace(out))
					}
				}
			}
		}
	}

	if speakerFRR != nil {
		if out, err := speakerFRR.Exec("vtysh", "-c", "show bfd vrf all peer json"); err != nil {
			bfdDebugLog(nodeName, "speaker frr show bfd peers json failed: %v out=%q", err, out)
		} else {
			bfdDebugLog(nodeName, "speaker frr BFD peers json:\n%s", out)
		}
		if out, err := speakerFRR.Exec("vtysh", "-c", "show bfd vrf all peer"); err != nil {
			bfdDebugLog(nodeName, "speaker frr show bfd peers failed: %v out=%q", err, out)
		} else {
			bfdDebugLog(nodeName, "speaker frr BFD peers:\n%s", out)
		}
		if out, err := speakerFRR.Exec("bash", "-c", "ip -4 route; echo '---'; ip -6 route"); err != nil {
			bfdDebugLog(nodeName, "speaker frr routes failed: %v out=%q", err, out)
		} else {
			bfdDebugLog(nodeName, "speaker frr routes:\n%s", out)
		}
	}

	for _, c := range containers {
		bfdDebugLog(nodeName, "container %s addresses: ipv4=%s ipv6=%s vrf=%q", c.Name, c.Ipv4, c.Ipv6, c.RouterConfig.VRF)

		bfdPeers, err := frr.BFDPeers(c.Executor)
		if err != nil {
			bfdDebugLog(nodeName, "container %s failed to get BFD peers: %v", c.Name, err)
		} else {
			bfdDebugLog(nodeName, "container %s BFD peers (%d): [%s]", c.Name, len(bfdPeers), bfdPeersStatusSummary(bfdPeers))
			for _, peer := range bfdPeers {
				bfdDebugLog(nodeName, "container %s BFD peer %s: %s", c.Name, peer.Peer, bfdPeerDebugString(peer))
			}
		}

		if out, err := c.Executor.Exec("vtysh", "-c", "show bfd vrf all peer"); err != nil {
			bfdDebugLog(nodeName, "container %s show bfd peers failed: %v out=%q", c.Name, err, out)
		} else {
			bfdDebugLog(nodeName, "container %s BFD peers (vtysh):\n%s", c.Name, out)
		}

		for _, family := range []ipfamily.Family{ipfamily.IPv4, ipfamily.IPv6} {
			nodeIPs := v4IPs
			if family == ipfamily.IPv6 {
				nodeIPs = v6IPs
			}
			for _, ip := range nodeIPs {
				if out, err := execRouteGet(c.Executor, family, ip); err != nil {
					bfdDebugLog(nodeName, "container %s ip route get %s for node peer %s: err=%v out=%q", c.Name, family, ip, err, out)
				} else {
					bfdDebugLog(nodeName, "container %s ip route get %s for node peer %s: %s", c.Name, family, ip, strings.TrimSpace(out))
				}
			}
		}

		if out, err := c.Executor.Exec("bash", "-c", "ip -4 route; echo '---'; ip -6 route"); err != nil {
			bfdDebugLog(nodeName, "container %s routes failed: %v out=%q", c.Name, err, out)
		} else {
			bfdDebugLog(nodeName, "container %s routes:\n%s", c.Name, out)
		}
	}
}

func bfdPeerDebugString(peer frr.BFDPeer) string {
	return fmt.Sprintf(
		"status=%q diagnostic=%q remote-diagnostic=%q local=%s vrf=%q multihop=%v "+
			"rx=%d tx=%d echo-rx=%d remote-rx=%d remote-tx=%d remote-echo-rx=%d uptime=%d",
		peer.Status, peer.Diagnostic, peer.RemoteDiagnostic, peer.Local, peer.Vrf, peer.Multihop,
		peer.ReceiveInterval, peer.TransmitInterval, peer.EchoReceiveInterval,
		peer.RemoteReceiveInterval, peer.RemoteTransmitInterval, peer.RemoteEchoReceiveInterval,
		peer.Uptime,
	)
}

func bfdPeersStatusSummary(peers map[string]frr.BFDPeer) string {
	parts := make([]string, 0, len(peers))
	for _, peer := range peers {
		parts = append(parts, fmt.Sprintf("%s=%s", peer.Peer, peer.Status))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func checkBFDConfigPropagated(nodeConfig metallbv1beta1.BFDProfile, peerConfig frr.BFDPeer) error {
	if peerConfig.Status != "up" {
		return fmt.Errorf("peer status not up: %s", bfdPeerDebugString(peerConfig))
	}
	if peerConfig.RemoteReceiveInterval != int(*nodeConfig.Spec.ReceiveInterval) {
		return fmt.Errorf("remoteReceiveInterval: expecting %d, got %d (%s)",
			*nodeConfig.Spec.ReceiveInterval, peerConfig.RemoteReceiveInterval, bfdPeerDebugString(peerConfig))
	}
	if peerConfig.RemoteTransmitInterval != int(*nodeConfig.Spec.TransmitInterval) {
		return fmt.Errorf("remoteTransmitInterval: expecting %d, got %d (%s)",
			*nodeConfig.Spec.TransmitInterval, peerConfig.RemoteTransmitInterval, bfdPeerDebugString(peerConfig))
	}
	if peerConfig.RemoteEchoReceiveInterval != int(*nodeConfig.Spec.EchoInterval) {
		return fmt.Errorf("echoInterval: expecting %d, got %d (%s)",
			*nodeConfig.Spec.EchoInterval, peerConfig.RemoteEchoReceiveInterval, bfdPeerDebugString(peerConfig))
	}
	return nil
}

func validateBFDPeersPropagated(containerName string, bfd metallbv1beta1.BFDProfile, peers map[string]frr.BFDPeer) error {
	var errs []error
	for _, peerConfig := range peers {
		toCompare := config.BFDProfileWithDefaults(bfd, peerConfig.Multihop)
		if err := checkBFDConfigPropagated(toCompare, peerConfig); err != nil {
			errs = append(errs, fmt.Errorf("container %s peer %s: %w", containerName, peerConfig.Peer, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%d BFD peer(s) not ready on %s (all peers: [%s]): %w",
		len(errs), containerName, bfdPeersStatusSummary(peers), errors.Join(errs...))
}

func checkServiceOnlyOnNodes(svc *corev1.Service, expectedNodes []corev1.Node, ipFamily ipfamily.Family) {
	if len(expectedNodes) == 0 {
		return
	}
	ip := svc.Status.LoadBalancer.Ingress[0].IP

	for _, c := range FRRContainers {
		nodeIps, err := k8s.NodeIPsForFamily(expectedNodes, ipFamily, c.RouterConfig.VRF)
		Expect(err).NotTo(HaveOccurred())
		validateService(svc, expectedNodes, c)
		Eventually(func() error {
			routes, err := frr.RoutesForFamily(c, ipFamily)
			if len(routes[ip].NextHops) != len(nodeIps) {
				return fmt.Errorf("%s: invalid number of routes for %s: expecting %d got %d", c.Name, ip, len(nodeIps), len(routes[ip].NextHops))
			}

		OUTER:
			for _, n := range routes[ip].NextHops {
				for _, ip := range nodeIps {
					if n.String() == ip {
						continue OUTER
					}
				}
				return fmt.Errorf("unexpectedIP found %s, nodes %s in container %s for service %s", n.String(), nodeIps, c.Name, ip)
			}
			return err
		}, time.Minute, time.Second).ShouldNot(HaveOccurred())
	}
}

func checkServiceNotOnNodes(svc *corev1.Service, expectedNodes []corev1.Node, ipFamily ipfamily.Family) {
	if len(expectedNodes) == 0 {
		return
	}
	ip := svc.Status.LoadBalancer.Ingress[0].IP

	for _, c := range FRRContainers {
		nodeIps, err := k8s.NodeIPsForFamily(expectedNodes, ipFamily, c.RouterConfig.VRF)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			routes, err := frr.RoutesForFamily(c, ipFamily)
			Expect(err).NotTo(HaveOccurred())
			for _, n := range routes[ip].NextHops {
				for _, ip := range nodeIps {
					if n.String() == ip {
						return true
					}
				}
			}
			return false
		}, time.Minute, time.Second).Should(BeFalse())
	}
}

func checkCommunitiesOnlyOnNodes(svc *corev1.Service, community string, expectedNodes []corev1.Node, ipFamily ipfamily.Family) {
	if len(expectedNodes) == 0 {
		return
	}
	ip := svc.Status.LoadBalancer.Ingress[0].IP

	for _, c := range FRRContainers {
		nodeIps, err := k8s.NodeIPsForFamily(expectedNodes, ipFamily, c.RouterConfig.VRF)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			routes, err := frr.RoutesForCommunity(c, community, ipFamily)
			if len(routes[ip].NextHops) != len(nodeIps) {
				return fmt.Errorf("%s: invalid number of routes for %s: expecting %d got %d", c.Name, ip, len(nodeIps), len(routes[ip].NextHops))
			}

		OUTER:
			for _, n := range routes[ip].NextHops {
				for _, ip := range nodeIps {
					if n.String() == ip {
						continue OUTER
					}
				}
				return fmt.Errorf("unexpectedIP found %s, nodes %s in container %s for service %s", n.String(), nodeIps, c.Name, ip)
			}
			return err
		}, 10*time.Minute, time.Second).ShouldNot(HaveOccurred())
	}
}

func nodesForSelection(nodes []corev1.Node, selected []int) []corev1.Node {
	selectedNodes := []corev1.Node{}
	for _, i := range selected {
		if i >= len(nodes) {
			ginkgo.Skip("not enough nodes")
		}
		selectedNodes = append(selectedNodes, nodes[i])
	}
	return selectedNodes
}

func nodesNotSelected(nodes []corev1.Node, selected []int) []corev1.Node {
	nonSelectedNodes := []corev1.Node{}
OUTER:
	for i, n := range nodes {
		for _, j := range selected {
			if i == j {
				continue OUTER
			}
		}
		nonSelectedNodes = append(nonSelectedNodes, n)
	}

	return nonSelectedNodes
}

func validateServiceNotAdvertised(svc *corev1.Service, frrContainers []*frrcontainer.FRR, advertised string, ipFamily ipfamily.Family) {
	for _, c := range frrContainers {
		if c.Name != advertised {
			for _, ip := range svc.Status.LoadBalancer.Ingress {
				ingressIP := jigservice.GetIngressPoint(&ip)

				Eventually(func() bool {
					frrRoutesV4, frrRoutesV6, err := frr.Routes(c)
					if err != nil {
						Expect(err).NotTo(HaveOccurred())
					}

					_, ok := frrRoutesV4[ingressIP]
					if ipFamily == ipfamily.IPv6 {
						_, ok = frrRoutesV6[ingressIP]
					}

					return ok
				}, 4*time.Minute, 1*time.Second).Should(Equal(false))
			}
		}
	}
}

func validateServiceInRoutesForCommunity(c *frrcontainer.FRR, community string, family ipfamily.Family, svc *corev1.Service) {
	Eventually(func() error {
		routes, err := frr.RoutesForCommunity(c, community, family)
		if err != nil {
			return err
		}
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			ingressIP := jigservice.GetIngressPoint(&ip)
			if _, ok := routes[ingressIP]; !ok {
				return fmt.Errorf("service IP %s not in routes", ingressIP)
			}
		}
		return nil
	}, 4*time.Minute, 1*time.Second).ShouldNot(HaveOccurred())
}

func validateServiceNotInRoutesForCommunity(c *frrcontainer.FRR, community string, family ipfamily.Family, svc *corev1.Service) {
	Eventually(func() error {
		routes, err := frr.RoutesForCommunity(c, community, family)
		if err != nil {
			return err
		}
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			ingressIP := jigservice.GetIngressPoint(&ip)
			if _, ok := routes[ingressIP]; !ok {
				return fmt.Errorf("service IP %s not in routes", ingressIP)
			}
		}
		return nil
	}, 4*time.Minute, 1*time.Second).Should(MatchError(ContainSubstring("not in routes")))
}

// isRouteInjected checks if the routeToCheck is injected in at least one pod, and
// returns the name of the first pod where it is found.
func isRouteInjected(pods []*corev1.Pod, pairingFamily ipfamily.Family, routeToCheck, vrf string) (bool, string) {
	for _, pod := range pods {
		podExec, err := FRRProvider.FRRExecutorFor(pod.Namespace, pod.Name)
		Expect(err).NotTo(HaveOccurred())

		routes, frrRoutesV6, err := frr.RoutesForVRF(vrf, podExec)
		Expect(err).NotTo(HaveOccurred())

		if pairingFamily == ipfamily.IPv6 {
			routes = frrRoutesV6
		}

		for _, route := range routes {
			if route.Destination.String() == routeToCheck {
				return true, pod.Name
			}
		}
	}
	return false, ""
}
