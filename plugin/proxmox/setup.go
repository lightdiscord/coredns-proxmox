package proxmox

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/file"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/expr-lang/expr"
	proxmoxClient "github.com/luthermonson/go-proxmox"
	"github.com/miekg/dns"
)

func init() {
	plugin.Register("proxmox", setup)
}

func incidr(ip string, cidr string) (bool, error) {
	ip2, err := netip.ParseAddr(ip)
	if err != nil {
		return false, err
	}
	cidr2, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false, err
	}
	return cidr2.Contains(ip2), nil
}

func parserule(c *caddy.Controller) (*Rule, error) {
	r := new(Rule)

	for c.NextBlock() {
		switch c.Val() {
		case "if":
			args := c.RemainingArgs()

			incidr := expr.Function(
				"incidr",
				func(params ...any) (any, error) {
					return incidr(params[0].(string), params[1].(string))
				},
				new(func(string, string) bool),
			)

			prog, err := expr.Compile(strings.Join(args, " "), expr.AsBool(), expr.Env(Environment{}), incidr)
			if err != nil {
				return nil, plugin.Error(name, err)
			}
			r.ifs = append(r.ifs, prog)
		case "respond":
			if !c.NextArg() {
				return nil, plugin.Error(name, c.ArgErr())
			}
			tmpl, err := template.New("condition").Parse(c.Val())
			if err != nil {
				return nil, plugin.Error(name, c.Errf("failed to parse rule template: %v", err))
			}
			r.responses = append(r.responses, tmpl.Option("missingkey=error"))
		default:
			return nil, plugin.Error(name, c.Errf("unknown rule property '%s'", c.Val()))
		}
	}

	return r, nil
}

func setup(c *caddy.Controller) error {
	c.Next() // 'proxmox'

	// Default refresh frequency to 1 minute
	refresh := time.Minute
	insecure := false
	var baseurl, token, secret string

	if !c.NextArg() {
		return plugin.Error(name, c.ArgErr())
	}

	zone := c.Val()
	if dns.IsFqdn(zone) {
		return plugin.Error(name, c.Errf("invalid zone '%s'", zone))
	}

	var rules []*Rule

	for c.NextBlock() {
		switch c.Val() {
		case "refresh":
			if !c.NextArg() {
				return plugin.Error(name, c.ArgErr())
			}
			refreshStr := c.Val()
			// If valid number without time unit treat it as a number of seconds
			_, err := strconv.Atoi(refreshStr)
			if err == nil {
				refreshStr = c.Val() + "s"
			}
			refresh, err = time.ParseDuration(refreshStr)
			if err != nil {
				return plugin.Error(name, c.Errf("unable to parse duration: %v", err))
			}
			// TODO: Should we allow using zero to disable updates ?
			if refresh <= 0 {
				return plugin.Error(name, c.Errf("Refresh frequency must be greater than 0: %q", refreshStr))
			}
		case "baseurl":
			if !c.NextArg() {
				return plugin.Error(name, c.ArgErr())
			}
			baseurl = c.Val()
		case "token":
			if !c.NextArg() {
				return plugin.Error(name, c.ArgErr())
			}
			token = c.Val()
		case "secret":
			if !c.NextArg() {
				return plugin.Error(name, c.ArgErr())
			}
			secret = c.Val()
		case "insecure":
			insecure = true
		case "rule":
			rule, err := parserule(c)
			if err != nil {
				return err
			}
			rules = append(rules, rule)
		default:
			return plugin.Error(name, c.Errf("unknown property '%s'", c.Val()))
		}
	}

	if baseurl == "" {
		return plugin.Error(name, c.Errf("baseurl is required"))
	}

	if token == "" {
		return plugin.Error(name, c.Errf("token id is required"))
	}

	if secret == "" {
		return plugin.Error(name, c.Errf("token secret is required"))
	}

	credentials := proxmoxClient.WithAPIToken(token, secret)

	if len(rules) == 0 {
		return plugin.Error(name, c.Errf("no rules provided"))
	}

	insecureHttpClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecure,
			},
		},
	}

	client := proxmoxClient.NewClient(baseurl, credentials, proxmoxClient.WithHTTPClient(&insecureHttpClient))

	ctx, cancel := context.WithCancel(context.Background())

	p := &Proxmox{
		refresh:  refresh,
		client:   client,
		upstream: upstream.New(),
		rules:    rules,
		zoneName: zone,
		zone:     file.NewZone(zone, ""),
	}

	if err := p.Run(ctx); err != nil {
		cancel()
		return plugin.Error(name, c.Errf("failed to initialize proxmox plugin: %v", err))
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		p.Next = next
		return p
	})

	c.OnShutdown(func() error {
		cancel()
		return nil
	})

	return nil
}
