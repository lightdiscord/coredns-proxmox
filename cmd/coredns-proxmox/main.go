package main

import _ "github.com/coredns/coredns/core/plugin"
import (
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/coremain"
	"github.com/lightdiscord/coredns-proxmox/plugin/proxmox"
)

func init() {
	dnsserver.Directives = append(dnsserver.Directives, proxmox.Name())
}

func main() {
	coremain.Run()
}
