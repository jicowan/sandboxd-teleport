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
	"log"
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
	// credVendorIP is where the per-session AWS credential vendor listens. It MUST
	// be an address the AWS SDK's container-credentials provider allow-lists for a
	// FULL_URI (only loopback or 169.254.170.2 / 169.254.170.23), and the sandbox's
	// loopback is its own (not the worker's). We use 169.254.170.2 (the ECS
	// task-role address) — NOT 169.254.170.23, which is the EKS Pod Identity agent
	// address the WORKER's own SDK uses to load its identity; pinning .23 locally
	// would hijack the worker's own credential source. .2 is otherwise unused here.
	// It's added to the host veth (sbx0) so the sandbox reaches it on-link via its
	// default gateway. The vendor binds it (pinned on lo at boot as a bind anchor).
	credVendorIP = "169.254.170.2"
)

// interiorNetNSPath is where the named netns is bind-mounted; runsc joins it via
// the OCI spec network-namespace path.
const interiorNetNSPath = "/run/netns/" + interiorNetNSName

// ensureInteriorNetNS creates the persistent interior netns if absent, enables
// IPv4 forwarding, and pins the interior gateway address on `lo` in the pod netns.
// Idempotent.
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

// ensureCredVendorAddr pins the credential-vendor IP (credVendorIP =
// 169.254.170.2, the AWS-allow-listed container-creds address) as a /32 on `lo`
// in the POD netns, so the vendor can bind it at worker boot — independent of the
// per-session veth. The sandbox reaches it via its default route (-> host veth ->
// forwarded to this local lo address; ip_forward is enabled). This /32 survives
// session teardown. Idempotent (AddrReplace). Best-effort: a failure only
// disables the credential vendor, not the worker.
//
// MUST be called from the POD netns (not from within ensureInteriorNetNS, which
// switches the thread into the interior netns). It locks the OS thread so the
// goroutine can't migrate to a thread in a different netns mid-call.
func ensureCredVendorAddr() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		log.Printf("WARN: cred vendor addr: lookup lo: %v", err)
		return
	}
	addr, err := netlink.ParseAddr(credVendorIP + "/32")
	if err != nil {
		log.Printf("WARN: cred vendor addr: parse addr: %v", err)
		return
	}
	if err := netlink.AddrReplace(lo, addr); err != nil {
		log.Printf("WARN: cred vendor addr: add %s to lo: %v", credVendorIP, err)
		return
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		log.Printf("WARN: cred vendor addr: set lo up: %v", err)
		return
	}
	log.Printf("cred vendor addr: %s pinned on lo", credVendorIP)
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
	// Also put the credential-vendor IP on the host veth so a packet the sandbox
	// sends to credVendorIP (via its default gateway) is delivered locally on the
	// arrival interface — cross-interface delivery to a lo-only address fails.
	// The vendor listens on credVendorIP; this makes it reachable from the sandbox.
	if credAddr, perr := netlink.ParseAddr(credVendorIP + "/32"); perr == nil {
		if err := netlink.AddrReplace(hostLink, credAddr); err != nil {
			log.Printf("WARN: add cred vendor IP to host veth: %v", err)
		}
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

