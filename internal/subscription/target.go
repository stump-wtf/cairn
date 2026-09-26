package subscription

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/stump-wtf/cairn/internal/errs"
)

// AllowHTTPEnv names the risky switch that admits http:// targets.
const AllowHTTPEnv = "CAIRN_OUTBOUND_ALLOW_HTTP"

// Delivery limits (SPEC-0023 REQ "Subscription Target Safety").
const (
	// AttemptTimeout bounds one delivery attempt, dial to last byte.
	AttemptTimeout = 5 * time.Second
	// maxURLBytes bounds a target URL.
	maxURLBytes = 2048
)

// ErrBlockedAddress is a target host that resolves to an address Cairn never
// dials: loopback, private, link-local, unique-local, multicast, unspecified,
// or another non-public range. It carries no host or URL.
var ErrBlockedAddress = errors.New("subscription: target resolves to a non-public address")

// Resolver looks a host name up. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy decides which targets a subscription may name and dials them. The
// same checks run when a subscription is created and again immediately
// before every dial, where the host is resolved afresh and every address it
// resolves to is re-checked, so a name that resolved to a public address at
// creation and to a private one later is never dialled (DNS rebinding).
// Redirects are never followed, proxies from the environment are never used,
// and each attempt times out after AttemptTimeout.
//
// Governing: ADR-0029 (section 6), SPEC-0023 REQ "Subscription Target Safety"
type Policy struct {
	// AllowHTTP admits http:// targets (CAIRN_OUTBOUND_ALLOW_HTTP). Off by
	// default: signed payloads and capability URLs would travel in cleartext.
	AllowHTTP bool
	// Resolver resolves target hosts; nil uses net.DefaultResolver.
	Resolver Resolver
	// PermitAddr, when set, replaces the non-public address check. It exists
	// for tests that deliver to a loopback httptest server; cairnd never sets
	// it (cmd/cairnd asserts so).
	PermitAddr func(netip.Addr) bool

	// dial replaces the network dial in tests that count dials.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p *Policy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p *Policy) permitted(a netip.Addr) bool {
	if p.PermitAddr != nil {
		return p.PermitAddr(a)
	}
	return PublicAddr(a)
}

// nonPublic lists ranges Go's netip predicates do not cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: can reach private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: can embed private IPv4
}

// PublicAddr reports whether a is an address Cairn may deliver to: a global
// unicast address outside every private, loopback, link-local, unique-local
// and reserved range. An IPv4-mapped IPv6 address is judged as its IPv4 form.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// CheckURL validates a target when a subscription is created or re-pointed:
// the URL's shape (CheckTarget) and every address its host resolves to now.
// Errors are validation errors that never quote the URL, which is a bearer
// capability for most receivers.
func (p *Policy) CheckURL(ctx context.Context, raw string) error {
	u, err := p.parse(raw)
	if err != nil {
		return err
	}
	if _, err := p.resolve(ctx, u.Hostname()); err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return errs.Validationf("subscriptions: the target resolves to a loopback, private, link-local or other non-public address")
		}
		return errs.Validationf("subscriptions: the target host does not resolve")
	}
	return nil
}

// CheckTarget re-validates a stored target's shape immediately before a
// delivery, so turning CAIRN_OUTBOUND_ALLOW_HTTP off stops http:// targets
// at once. Address checks happen in the dialer.
func (p *Policy) CheckTarget(raw string) error {
	_, err := p.parse(raw)
	return err
}

func (p *Policy) parse(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return nil, errs.Validationf("subscriptions: url is required and at most %d bytes", maxURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return nil, errs.Validationf("subscriptions: url must be an absolute https URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return nil, errs.Validationf("subscriptions: url must use https (the operator has not set %s)", AllowHTTPEnv)
		}
	default:
		return nil, errs.Validationf("subscriptions: url must be an absolute https URL")
	}
	switch {
	case u.User != nil:
		return nil, errs.Validationf("subscriptions: url must not carry credentials")
	case u.Hostname() == "":
		return nil, errs.Validationf("subscriptions: url must name a host")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, errs.Validationf("subscriptions: url must not carry a fragment")
	}
	return u, nil
}

// resolve returns every address host resolves to, refusing the whole host
// when any one of them is not permitted: a name with one public and one
// private record must not get the private one dialled on a retry.
func (p *Policy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = p.resolver().LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("subscription: resolve: %w", err)
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("subscription: resolve: no addresses")
	}
	for _, a := range addrs {
		if !p.permitted(a) {
			return nil, ErrBlockedAddress
		}
	}
	return addrs, nil
}

// dialContext resolves the target host afresh, re-checks every address, and
// dials only checked addresses — never the name, which could resolve again
// to something else between the check and the connect.
func (p *Policy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dial := p.dial
	if dial == nil {
		d := &net.Dialer{Timeout: AttemptTimeout}
		dial = d.DialContext
	}
	var lastErr error
	for _, a := range addrs {
		conn, err := dial(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Client returns the HTTP client every delivery uses: the checking dialer,
// no environment proxy (a proxy would dial on Cairn's behalf, past the
// check), no redirects (a 3xx is a failed delivery), no connection reuse (so
// every delivery dials, and so re-checks), and the per-attempt timeout.
func (p *Policy) Client() *http.Client {
	return &http.Client{
		Timeout: AttemptTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            p.dialContext,
			DisableKeepAlives:      true,
			ForceAttemptHTTP2:      true,
			TLSHandshakeTimeout:    AttemptTimeout,
			ResponseHeaderTimeout:  AttemptTimeout,
			MaxResponseHeaderBytes: 16 << 10,
		},
	}
}
