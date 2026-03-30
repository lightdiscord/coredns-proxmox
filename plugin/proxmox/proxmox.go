package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/file"
	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/request"
	"github.com/expr-lang/expr"
	proxmoxClient "github.com/luthermonson/go-proxmox"
	"github.com/miekg/dns"
)

const name = "proxmox"

type Proxmox struct {
	Next plugin.Handler

	client   *proxmoxClient.Client
	refresh  time.Duration
	upstream *upstream.Upstream
	zoneName string
	lock     sync.RWMutex
	zone     *file.Zone
	rules    []*rule
}

type AgentNetworkIPAddress struct {
	IPAddressType string
	IPAddress     string
	Prefix        int
	MacAddress    string
}

type AgentNetworkIface struct {
	Name            string
	HardwareAddress string
	IPAddresses     []AgentNetworkIPAddress
}

type VirtualMachine struct {
	Name string
	Node string

	VMID   uint64
	Status string
	CPU    float64
	Uptime uint64

	Mem    uint64
	MaxMem uint64

	CPUs   int
	NetIn  uint64
	Netout uint64

	PID      uint64
	Disk     uint64
	MaxDisk  uint64
	DiskRead uint64
	Tags     []string
	Template bool
}

type todoRenderEnv struct {
	Zone      string
	Vm        VirtualMachine
	Interface AgentNetworkIface
	Address   AgentNetworkIPAddress
}

func env(zone string, vm *proxmoxClient.VirtualMachine, iface *proxmoxClient.AgentNetworkIface, addr *proxmoxClient.AgentNetworkIPAddress) todoRenderEnv {
	var addrs []AgentNetworkIPAddress

	for _, addr := range iface.IPAddresses {
		addrs = append(addrs, AgentNetworkIPAddress{
			IPAddressType: addr.IPAddressType,
			IPAddress:     addr.IPAddress,
			Prefix:        addr.Prefix,
			MacAddress:    addr.MacAddress,
		})
	}

	return todoRenderEnv{
		Zone: zone,
		Vm: VirtualMachine{
			Name: vm.Name,
			Node: vm.Node,

			VMID:   uint64(vm.VMID),
			Status: vm.Status,
			CPU:    vm.CPU,
			Uptime: vm.Uptime,

			Mem:    vm.Mem,
			MaxMem: vm.MaxMem,

			CPUs:   vm.CPUs,
			NetIn:  vm.NetIn,
			Netout: vm.Netout,

			PID:      uint64(vm.PID),
			Disk:     vm.Disk,
			MaxDisk:  vm.MaxDisk,
			DiskRead: vm.DiskRead,
			Tags:     strings.Split(vm.Tags, ";"),
			Template: bool(vm.Template),
		},
		Interface: AgentNetworkIface{
			Name:            iface.Name,
			HardwareAddress: iface.HardwareAddress,
			IPAddresses:     addrs,
		},
		Address: AgentNetworkIPAddress{
			IPAddressType: addr.IPAddressType,
			IPAddress:     addr.IPAddress,
			Prefix:        addr.Prefix,
			MacAddress:    addr.MacAddress,
		},
	}
}

func Name() string { return name }

func (p *Proxmox) Name() string { return name }

func (p *Proxmox) Run(ctx context.Context) error {
	go func() {
		if err := p.todoUpdateRefresh(ctx); err != nil && ctx.Err() == nil {
			log.Errorf("failed to refresh: %v", err)
		}
		timer := time.NewTimer(p.refresh)
		defer timer.Stop()
		for {
			timer.Reset(p.refresh)
			select {
			case <-ctx.Done():
				log.Debugf("breaking out of Proxmox refresh loop: %v", ctx.Err())
				return
			case <-timer.C:
				if err := p.todoUpdateRefresh(ctx); err != nil && ctx.Err() == nil {
					log.Errorf("failed to refresh: %v", err)
				}
			}
		}
	}()
	return nil
}

func (p *Proxmox) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	var result file.Result
	p.lock.RLock()
	m.Answer, m.Ns, m.Extra, result = p.zone.Lookup(ctx, state, state.Name())
	p.lock.RUnlock()

	switch result {
	case file.Success:
	case file.NoData:
	case file.NameError:
		m.Rcode = dns.RcodeNameError
	case file.Delegation:
		m.Authoritative = false
	case file.ServerFailure:
		return dns.RcodeServerFailure, nil
	}

	err := w.WriteMsg(m)
	if err != nil {
		return dns.RcodeServerFailure, err
	}

	return dns.RcodeSuccess, nil
}

func (p *Proxmox) todoUpdateRefresh(ctx context.Context) error {
	// TODO: Unsure if virtual machines can return arbitrary values with Guest Agent. If values for a virtual machine
	//  are not valid, it should not break the records for other virtual machines.

	nodes, err := p.client.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}

	// TODO: Remove
	p.zoneName = "vms.test"
	zone := file.NewZone(p.zoneName, "")
	zone.Upstream = p.upstream
	// TODO: Rework SOA
	rr, err := dns.NewRR("@ IN SOA ns.example. admin.example. (1 60 60 60 60)")
	if err != nil {
		return fmt.Errorf("failed to create SOA record: %v", err)
	}
	if err := zone.Insert(rr); err != nil {
		return fmt.Errorf("failed to insert SOA record: %v", err)
	}

	// TODO: Allow filtering based on node name

	for _, node := range nodes {
		// Ignore offline or unreachable nodes
		if node.Status != "online" {
			continue
		}

		// TODO: Do not refetch the node
		node, err := p.client.Node(ctx, node.Node)
		if err != nil {
			return fmt.Errorf("failed to get node: %v", err)
		}

		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			return fmt.Errorf("failed to get virtual machines: %v", err)
		}

		for _, vm := range vms {
			// TODO: Allow filtering based on vm id, name or tags

			if vm.Status != "running" {
				continue
			}

			ifaces, err := vm.AgentGetNetworkIFaces(ctx)
			if err != nil {
				if strings.Contains(err.Error(), "No QEMU guest agent configured") {
					continue
				}

				if strings.Contains(err.Error(), "QEMU guest agent is not running") {
					continue
				}

				return fmt.Errorf("failed to get network interfaces: %v", err)
			}

			for _, iface := range ifaces {
				for _, addr := range iface.IPAddresses {
					//domain := fmt.Sprintf("%d.vms.test", vm.VMID)
					todorename := env(p.zoneName, vm, iface, addr)

					for _, r := range p.rules {
						todobool := true

						for _, prog := range r.ifs {
							// TODO: Add information to program environment
							out, err := expr.Run(prog, &todorename)
							if err != nil {
								return fmt.Errorf("failed to run prog: %v", err)
							}

							if out == false {
								todobool = false
								break
							}
						}

						if todobool {

							for _, resp := range r.responses {

								var b bytes.Buffer

								if err := resp.Execute(&b, &todorename); err != nil {
									return fmt.Errorf("failed to render response: %v", err)
								}

								domain := b.String()

								if _, ok := dns.IsDomainName(b.String()); !ok {
									return fmt.Errorf("invalid domain name: %v", domain)
								}

								// TODO: Handle duplicate entries to avoid returning the same address multiple times

								if addr.IPAddressType == "ipv4" {
									rfc1035 := fmt.Sprintf("%v %d IN A %s", domain, int64(p.refresh.Seconds()), addr.IPAddress)
									rr, err := dns.NewRR(rfc1035)
									if err != nil {
										return fmt.Errorf("failed to parse resource record: %v", err)
									}
									if err := zone.Insert(rr); err != nil {
										return fmt.Errorf("failed to insert record: %v", err)
									}
								} else if addr.IPAddressType == "ipv6" {
									rfc1035 := fmt.Sprintf("%v %d IN AAAA %s", domain, int64(p.refresh.Seconds()), addr.IPAddress)
									rr, err := dns.NewRR(rfc1035)
									if err != nil {
										return fmt.Errorf("failed to parse resource record: %v", err)
									}
									if err := zone.Insert(rr); err != nil {
										return fmt.Errorf("failed to insert record: %v", err)
									}
								} else {
									// TODO: Log warning unknown address type. Plugin outlived IPv6 ?
								}
							}
						}
					}
				}
			}
		}
	}

	p.lock.Lock()
	p.zone = zone
	p.lock.Unlock()

	return nil
}
