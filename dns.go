package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/providers/dns/alidns"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/dnspod"
	"github.com/go-acme/lego/v4/providers/dns/duckdns"
	"github.com/go-acme/lego/v4/providers/dns/route53"
	"github.com/go-acme/lego/v4/providers/dns/tencentcloud"
)

// dnsProviderInfo describes one supported DNS-01 provider.
type dnsProviderInfo struct {
	New         func() (challenge.Provider, error)
	Description string
	Credentials string // environment variables the provider reads
}

// dnsProviders maps the name used on the CLI / in the manifest to a lego DNS
// provider. Credentials are always passed through environment variables.
var dnsProviders = map[string]dnsProviderInfo{
	"cloudflare": {
		New:         providerFactory(cloudflare.NewDNSProvider),
		Description: "Cloudflare DNS",
		Credentials: "CLOUDFLARE_DNS_API_TOKEN",
	},
	"alidns": {
		New:         providerFactory(alidns.NewDNSProvider),
		Description: "Alibaba Cloud DNS (阿里云)",
		Credentials: "ALICLOUD_ACCESS_KEY, ALICLOUD_SECRET_KEY",
	},
	"dnspod": {
		New:         providerFactory(dnspod.NewDNSProvider),
		Description: "DNSPod (腾讯云 DNSPod)",
		Credentials: "DNSPOD_API_KEY",
	},
	"tencentcloud": {
		New:         providerFactory(tencentcloud.NewDNSProvider),
		Description: "Tencent Cloud DNS (腾讯云)",
		Credentials: "TENCENTCLOUD_SECRET_ID, TENCENTCLOUD_SECRET_KEY",
	},
	"duckdns": {
		New:         providerFactory(duckdns.NewDNSProvider),
		Description: "DuckDNS",
		Credentials: "DUCKDNS_TOKEN",
	},
	"route53": {
		New:         providerFactory(route53.NewDNSProvider),
		Description: "AWS Route 53",
		Credentials: "AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY",
	},
	"manual": {
		New:         newManualDNSProvider,
		Description: "Any provider — prints the TXT record and waits for you",
		Credentials: "(none — interactive)",
	},
}

// providerFactory adapts a concrete lego provider constructor to the generic factory.
func providerFactory[T challenge.Provider](construct func() (T, error)) func() (challenge.Provider, error) {
	return func() (challenge.Provider, error) {
		p, err := construct()
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// dnsProviderNames returns the supported provider names in sorted order.
func dnsProviderNames() []string {
	names := make([]string, 0, len(dnsProviders))
	for name := range dnsProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newDNSProvider builds the named DNS-01 provider.
func newDNSProvider(name string) (challenge.Provider, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	info, ok := dnsProviders[key]
	if !ok {
		return nil, fmt.Errorf("unknown DNS provider %q (supported: %s)", name, strings.Join(dnsProviderNames(), ", "))
	}
	return info.New()
}

// ==========================================
// manual DNS-01 provider (works with any DNS host)
// ==========================================

type manualDNSProvider struct {
	reader *bufio.Reader
}

func newManualDNSProvider() (challenge.Provider, error) {
	info, err := os.Stdin.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		return nil, fmt.Errorf(`the "manual" DNS provider is interactive and needs a terminal (it cannot renew unattended; use a provider with an API instead)`)
	}
	return &manualDNSProvider{reader: bufio.NewReader(os.Stdin)}, nil
}

// Present prints the TXT record the user has to create and waits for confirmation.
func (m *manualDNSProvider) Present(domain, token, keyAuth string) error {
	fqdn, value := dns01.GetRecord(domain, keyAuth)
	fmt.Println()
	fmt.Println(yellow(bold("Manual DNS-01 challenge for " + domain)))
	fmt.Println("Create this TXT record at your DNS provider:")
	fmt.Printf("    name : %s\n", cyan(fqdn))
	fmt.Printf("    value: %s\n", cyan(value))
	fmt.Println()
	fmt.Print("  Press Enter once the record exists and has propagated...")
	if _, err := m.reader.ReadString('\n'); err != nil {
		return fmt.Errorf("waiting for confirmation failed: %w", err)
	}
	return nil
}

// CleanUp tells the user the record is no longer needed.
func (m *manualDNSProvider) CleanUp(domain, token, keyAuth string) error {
	fqdn, value := dns01.GetRecord(domain, keyAuth)
	fmt.Printf("  You can now delete the TXT record %s (value %s)\n", fqdn, value)
	return nil
}

// Timeout allows generous time for DNS propagation checks.
func (m *manualDNSProvider) Timeout() (time.Duration, time.Duration) {
	return 10 * time.Minute, 5 * time.Second
}
