# coredns-proxmox

CoreDNS plugin to generate DNS entries from Proxmox virtual machines using QEMU Guest Agent.

## Basic Usage

Create a CodeDNS configuration file using the Proxmox plugin. Authentication details can be made available using
environment variables. Multiple rules can be specified, each with specific conditions (all conditions must be true) and
templated record outputs.

```Caddyfile
dns.test {
    proxmox vms.dns.test {
        # Address of JSON API of Proxmox Virtual Environment
        baseurl {$PROXMOX_BASE_URL}

        # API Token used for authentication
        token {$PROXMOX_API_TOKEN_ID}
        secret {$PROXMOX_API_TOKEN_SECRET}

        # Ignore untrusted TLS certificates
        insecure

        # Refresh rate of VM details
        refresh 5s

        # Rule description, multiple rules can be used
        rule {
            # Condition to verify, multiple conditions can be used and all need to be valid
            if incidr(Address.IPAddress, '192.168.0.0/16')

            # Record templates to return
            respond "{{.Vm.VMID}}.by-id.{{.Zone}}"
            respond "{{.Vm.VMID}}.sub.by-id.{{.Zone}}"
        }
    }
}
```

The authentication variables should have the following format.

```env
PROXMOX_BASE_URL=https://PROXMOX_HOST:8006/api2/json
PROXMOX_API_TOKEN_ID=account@realm!token
PROXMOX_API_TOKEN_SECRET=00000000-0000-4000-0000-000000000000
```

The permissions `VM.Audit` and `VM.GuestAgent.Audit` are required for the API token. The permissions can be given to
specific virtual machines or propagate from the `/vms` permission path.

## References

- [expr-lang](https://github.com/expr-lang/expr) is used to evaluate conditions.
