package main

// Sandbox networking, modeled on substrate's ateom-gvisor: a veth pair bridges
// the worker POD netns to a persistent interior netns that the gVisor sandbox
// joins (via the OCI spec's network-namespace path, NOT --network=host, so the
// netstack remains CHECKPOINTABLE). nftables DNATs podIP:hostPort ->
// interiorIP:containerPort inbound and masquerades egress. Pure kernel
// networking, no userspace proxy. Survives C/R: rebuilt fresh on restore, fixed
// interior IP.
//
// Requires: NET_ADMIN (have it — privileged worker), vishvananda/netns+netlink,
// google/nftables. runsc --network must be "sandbox" (netstack) for this path.

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	interiorNetNSName = "sbx-net"          // persistent interior netns (per worker)
	hostVethName      = "sbx0"             // host end, stays in pod netns
	actorVethTempName = "sbx1"             // peer name before moving into interior netns
	actorVethName     = "eth0"             // final peer name inside interior netns
	hostVethCIDR      = "169.254.17.1/30"  // host veth (gateway)
	actorVethCIDR     = "169.254.17.2/30"  // sandbox veth
	actorVethGateway  = "169.254.17.1"
	interiorIP        = "169.254.17.2"     // the sandbox's stable interior IP
	nftTableName      = "sbx_net"
)

// interiorNetNSPath is where the named netns is bind-mounted; runsc joins it via
// the OCI spec network-namespace path.
const interiorNetNSPath = "/run/netns/" + interiorNetNSName

// ensureInteriorNetNS creates the persistent interior netns if absent and enables
// IPv4 forwarding in the pod netns. Idempotent.
func ensureInteriorNetNS() error {
	if _, err := os.Stat(interiorNetNSPath); err == nil {
		enableIPForward()
		return nil
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cur, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer netns.Set(cur) // restore this thread's netns no matter what
	h, err := netns.NewNamed(interiorNetNSName)
	if err != nil {
		return fmt.Errorf("create interior netns: %w", err)
	}
	h.Close()
	enableIPForward()
	return nil
}

func enableIPForward() {
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

// setupSandboxNet builds a fresh veth pair into the interior netns and installs
// nftables DNAT/masquerade for the given port mappings. podIP is the worker
// pod's routable IP. Call before runsc run/restore. Idempotent-ish: tears down
// any stale veth/nft first.
func setupSandboxNet(podIP string, ports []portMap) error {
	teardownSandboxNet() // clean stale state first

	nsHandle, err := netns.GetFromName(interiorNetNSName)
	if err != nil {
		return fmt.Errorf("open interior netns: %w", err)
	}
	defer nsHandle.Close()

	// 1) create veth pair in the pod netns
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostVethName},
		PeerName:  actorVethTempName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth: %w", err)
	}
	// 2) host side: addr + up
	hostLink, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return fmt.Errorf("lookup host veth: %w", err)
	}
	hostAddr, _ := netlink.ParseAddr(hostVethCIDR)
	if err := netlink.AddrReplace(hostLink, hostAddr); err != nil {
		return fmt.Errorf("host veth addr: %w", err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return fmt.Errorf("host veth up: %w", err)
	}
	// 3) move peer into interior netns
	peer, err := netlink.LinkByName(actorVethTempName)
	if err != nil {
		return fmt.Errorf("lookup peer: %w", err)
	}
	if err := netlink.LinkSetNsFd(peer, int(nsHandle)); err != nil {
		return fmt.Errorf("move peer to interior netns: %w", err)
	}
	// 4) configure interior side inside the netns
	if err := inNetNS(nsHandle, configureInterior); err != nil {
		return fmt.Errorf("configure interior: %w", err)
	}
	// 5) nftables in the pod netns
	if err := installNft(podIP, ports); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

func configureInterior() error {
	// bring up lo
	if lo, err := netlink.LinkByName("lo"); err == nil {
		netlink.LinkSetUp(lo)
	}
	// rename temp peer -> eth0
	peer, err := netlink.LinkByName(actorVethTempName)
	if err != nil {
		return fmt.Errorf("interior peer lookup: %w", err)
	}
	if err := netlink.LinkSetName(peer, actorVethName); err != nil {
		return fmt.Errorf("rename peer: %w", err)
	}
	eth0, err := netlink.LinkByName(actorVethName)
	if err != nil {
		return err
	}
	addr, _ := netlink.ParseAddr(actorVethCIDR)
	if err := netlink.AddrReplace(eth0, addr); err != nil {
		return fmt.Errorf("interior addr: %w", err)
	}
	if err := netlink.LinkSetUp(eth0); err != nil {
		return fmt.Errorf("interior up: %w", err)
	}
	gw := net.ParseIP(actorVethGateway).To4()
	if err := netlink.RouteReplace(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: gw}); err != nil {
		return fmt.Errorf("interior route: %w", err)
	}
	return nil
}

// teardownSandboxNet removes the veth (its peer goes with it) + nftables table.
// Idempotent; safe to call before setup and after checkpoint.
func teardownSandboxNet() {
	removeNft()
	if link, err := netlink.LinkByName(hostVethName); err == nil {
		netlink.LinkDel(link)
	}
	// also try to clean a stray peer left in the interior netns
	if h, err := netns.GetFromName(interiorNetNSName); err == nil {
		inNetNS(h, func() error {
			for _, n := range []string{actorVethName, actorVethTempName} {
				if l, err := netlink.LinkByName(n); err == nil {
					netlink.LinkDel(l)
				}
			}
			return nil
		})
		h.Close()
	}
}

// inNetNS runs fn with the calling OS thread switched into ns, then restores.
func inNetNS(ns netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cur, err := netns.Get()
	if err != nil {
		return err
	}
	defer netns.Set(cur)
	if err := netns.Set(ns); err != nil {
		return err
	}
	return fn()
}

// ---- nftables ----

type portMap struct {
	Container int `json:"container"` // port inside the sandbox
	Host      int `json:"host"`      // port on the worker pod IP
}

func installNft(podIP string, ports []portMap) error {
	c := &nftables.Conn{}
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: nftTableName})

	pre := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest,
	})
	post := c.AddChain(&nftables.Chain{
		Name: "postrouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource,
	})
	accept := nftables.ChainPolicyAccept
	fwd := c.AddChain(&nftables.Chain{
		Name: "forward", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept,
	})
	c.AddRule(&nftables.Rule{Table: table, Chain: fwd, Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}})

	pod := net.ParseIP(podIP).To4()
	dst := net.ParseIP(interiorIP).To4()
	for _, pm := range ports {
		// PREROUTING: ip daddr podIP tcp dport hostPort dnat to interiorIP:containerPort
		exprs := []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4}, // ip daddr
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: pod},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, // tcp dport
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(pm.Host))},
			&expr.Immediate{Register: 1, Data: dst},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(uint16(pm.Container))},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: pre, Exprs: exprs})
	}
	// POSTROUTING: ip saddr interiorIP masquerade
	c.AddRule(&nftables.Rule{Table: table, Chain: post, Exprs: []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4}, // ip saddr
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: dst},
		&expr.Masq{},
	}})
	return c.Flush()
}

func removeNft() {
	c := &nftables.Conn{}
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: nftTableName})
	c.Flush() // ignore error if table absent
}

// writeResolvConf drops an /etc/resolv.conf into the sandbox rootfs so workloads
// can resolve hostnames. Without it, gVisor netstack has no resolver and services
// doing DNS lookups fail with EAI_AGAIN ("Try again") — which stalled AIO's nginx.
// We copy the worker pod's own resolver (kube-dns) so in-cluster + external names
// resolve, and egress is masqueraded out the pod IP.
func writeResolvConf(rootfsDir string) error {
	etc := rootfsDir + "/etc"
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	// prefer the worker pod's resolver; fall back to a public one.
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil || len(data) == 0 {
		data = []byte("nameserver 8.8.8.8\noptions ndots:1\n")
	}
	return os.WriteFile(etc+"/resolv.conf", data, 0o644)
}
