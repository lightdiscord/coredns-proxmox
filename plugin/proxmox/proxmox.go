package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/file"
	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/request"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
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
	rules    []*Rule
}

// Rewrite data structures of underlying Proxmox client to avoid exposing internal fields and prevent breaking changes.

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

type Environment struct {
	Zone      string
	Vm        VirtualMachine
	Interface AgentNetworkIface
	Address   AgentNetworkIPAddress
}

func NewEnvironment(zone string, vm *proxmoxClient.VirtualMachine, iface *proxmoxClient.AgentNetworkIface, addr *proxmoxClient.AgentNetworkIPAddress) *Environment {
	var addrs []AgentNetworkIPAddress

	for _, addr := range iface.IPAddresses {
		addrs = append(addrs, AgentNetworkIPAddress{
			IPAddressType: addr.IPAddressType,
			IPAddress:     addr.IPAddress,
			Prefix:        addr.Prefix,
			MacAddress:    addr.MacAddress,
		})
	}

	return &Environment{
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
		if err := p.reloadZone(ctx); err != nil && ctx.Err() == nil {
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
				if err := p.reloadZone(ctx); err != nil && ctx.Err() == nil {
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

type Rule struct {
	ifs       []*vm.Program
	responses []*template.Template
}

func (r *Rule) MatchEnvironment(env *Environment) (bool, error) {
	for _, prog := range r.ifs {
		out, err := expr.Run(prog, env)
		if err != nil {
			return false, fmt.Errorf("failed to run prog: %v", err)
		}

		if out == false {
			return false, nil
		}
	}

	return true, nil
}

func createSOARecord(ttl uint32, zone string) *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: dns.Fqdn(zone), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      dns.Fqdn("ns1." + zone),
		Mbox:    dns.Fqdn("hostmaster." + zone),
		Serial:  0,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  30,
	}
}

func (p *Proxmox) reloadZone(ctx context.Context) error {
	// TODO: Unsure if virtual machines can return arbitrary values with Guest Agent. If values for a virtual machine
	//  are not valid, it should not break the records for other virtual machines.

	ttl := uint32(p.refresh.Seconds())

	nodes, err := p.client.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}

	zone := file.NewZone(p.zoneName, "")
	zone.Upstream = p.upstream

	// SOA record is necessary for the lookup function to work.
	// TODO: The record may be incorrectly constructed or may need manual configuration in the future.
	err = zone.Insert(createSOARecord(ttl, p.zoneName))
	if err != nil {
		return fmt.Errorf("failed to insert record: %v", err)
	}

	for _, node := range nodes {
		// Ignore offline or unreachable nodes
		if node.Status != "online" {
			continue
		}

		node, err := p.client.Node(ctx, node.Node)
		if err != nil {
			return fmt.Errorf("failed to get node: %v", err)
		}

		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			return fmt.Errorf("failed to get virtual machines: %v", err)
		}

		for _, vm := range vms {
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
					env := NewEnvironment(p.zoneName, vm, iface, addr)

					for _, r := range p.rules {
						if cond, err := r.MatchEnvironment(env); !cond {
							if err != nil {
								return err
							}
							continue
						}

						for _, tpl := range r.responses {
							rr, err := renderTemplate(tpl, env, addr, ttl)
							if err != nil {
								return err
							}
							if rr != nil {
								if err := zone.Insert(rr); err != nil {
									return fmt.Errorf("failed to insert record: %v", err)
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

func renderTemplate(tpl *template.Template, env *Environment, addr *proxmoxClient.AgentNetworkIPAddress, ttl uint32) (dns.RR, error) {
	var b bytes.Buffer

	if err := tpl.Execute(&b, env); err != nil {
		return nil, fmt.Errorf("failed to render response: %v", err)
	}

	domain := b.String()

	if _, ok := dns.IsDomainName(domain); !ok {
		return nil, fmt.Errorf("invalid domain name: %v", domain)
	}

	// TODO: Handle duplicate entries to avoid returning the same address multiple times

	var rrt string

	switch addr.IPAddressType {
	case "ipv4":
		rrt = "A"
	case "ipv6":
		rrt = "AAAA"
	default:
		log.Warningf("ignoring invalid resource record type: %v", addr.IPAddressType)
		return nil, nil
	}

	rfc1035 := fmt.Sprintf("%v %d IN %s %s", domain, ttl, rrt, addr.IPAddress)
	rr, err := dns.NewRR(rfc1035)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resource record: %v", err)
	}

	return rr, nil
}
