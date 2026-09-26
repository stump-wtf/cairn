package subscription

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: SPEC-0023 REQ "Subscription Target Safety"

// switchResolver answers a fixed address per host, switchable mid-test.
type switchResolver struct {
	mu    sync.Mutex
	addrs map[string][]netip.Addr
}

func (r *switchResolver) set(host string, addrs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addrs == nil {
		r.addrs = map[string][]netip.Addr{}
	}
	r.addrs[host] = nil
	for _, a := range addrs {
		r.addrs[host] = append(r.addrs[host], netip.MustParseAddr(a))
	}
}

func (r *switchResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.addrs[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return a, nil
}

func TestPublicAddr(t *testing.T) {
	for _, c := range []struct {
		addr   string
		public bool
	}{
		{"93.184.216.34", true},
		{"2606:4700::1111", true},
		{"127.0.0.1", false},
		{"10.0.0.5", false},
		{"172.16.3.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"::ffff:10.0.0.1", false}, // IPv4-mapped private
		{"::ffff:93.184.216.34", true},
		{"64:ff9b::a00:1", false}, // NAT64 of 10.0.0.1
		{"224.0.0.1", false},
		{"255.255.255.255", false},
	} {
		if got := PublicAddr(netip.MustParseAddr(c.addr)); got != c.public {
			t.Errorf("PublicAddr(%s) = %v, want %v", c.addr, got, c.public)
		}
	}
}

func TestCheckURL(t *testing.T) {
	res := &switchResolver{}
	res.set("hooks.example.com", "93.184.216.34")
	res.set("inside.example.com", "10.1.2.3")
	res.set("mixed.example.com", "93.184.216.34", "10.1.2.3")
	p := &Policy{Resolver: res}
	ctx := context.Background()

	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/webhooks/w/token", true},
		{"https://93.184.216.34/hook", true},
		{"http://hooks.example.com/hook", false}, // http off by default
		{"https://inside.example.com/hook", false},
		{"https://mixed.example.com/hook", false}, // any private record refuses
		{"https://127.0.0.1/hook", false},
		{"https://[::1]/hook", false},
		{"https://localhost/hook", false}, // does not resolve here: refused
		{"https://user:pw@hooks.example.com/hook", false},
		{"https://hooks.example.com/hook#frag", false},
		{"ftp://hooks.example.com/hook", false},
		{"hooks.example.com/hook", false},
		{"", false},
		{"https://" + strings.Repeat("a", 2100) + ".example.com/", false},
	} {
		err := p.CheckURL(ctx, c.url)
		if c.ok && err != nil {
			t.Errorf("CheckURL(%.60q) = %v, want nil", c.url, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("CheckURL(%.60q) = nil, want a refusal", c.url)
				continue
			}
			if errs.CodeOf(err) != errs.CodeValidation {
				t.Errorf("CheckURL(%.60q) code = %s, want validation", c.url, errs.CodeOf(err))
			}
			// A target URL is a bearer capability: never echoed.
			if c.url != "" && strings.Contains(err.Error(), c.url) {
				t.Errorf("CheckURL error quotes the URL: %v", err)
			}
		}
	}

	p.AllowHTTP = true
	if err := p.CheckURL(ctx, "http://hooks.example.com/hook"); err != nil {
		t.Fatalf("http with AllowHTTP = %v", err)
	}
}

// TestRebindingTargetIsNotDialled is scenario "Rebinding target": a target
// resolving to a public address at creation and to a private (RFC 1918)
// address at delivery is not dialled, and the attempt fails.
func TestRebindingTargetIsNotDialled(t *testing.T) {
	res := &switchResolver{}
	res.set("rebind.example.com", "93.184.216.34")
	var dials atomic.Int32
	p := &Policy{Resolver: res}
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("test dialer: not connecting")
	}
	if err := p.CheckURL(context.Background(), "https://rebind.example.com/hook"); err != nil {
		t.Fatalf("creation-time check refused a public target: %v", err)
	}

	res.set("rebind.example.com", "10.0.0.7")
	req, _ := http.NewRequest(http.MethodPost, "https://rebind.example.com/hook", strings.NewReader("{}"))
	_, err := p.Client().Do(req)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("delivery error = %v, want ErrBlockedAddress", err)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("dialled %d times after the target rebound to a private address", n)
	}

	// Positive control: while public, the dialer is reached (and dials the
	// checked address, not the name).
	res.set("rebind.example.com", "93.184.216.34")
	var dialled string
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		dialled = addr
		return nil, errors.New("test dialer: not connecting")
	}
	_, _ = p.Client().Do(req.Clone(context.Background()))
	if dials.Load() == 0 || dialled != "93.184.216.34:443" {
		t.Fatalf("public target: dials=%d addr=%q, want one dial to 93.184.216.34:443", dials.Load(), dialled)
	}
}

func TestClientRefusesRedirectsAndProxies(t *testing.T) {
	c := (&Policy{}).Client()
	if c.CheckRedirect == nil {
		t.Fatal("client follows redirects")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("client uses an environment proxy, which would dial past the address check")
	}
	if !tr.DisableKeepAlives {
		t.Fatal("client reuses connections, so a delivery could skip the dial-time check")
	}
	if c.Timeout != AttemptTimeout || AttemptTimeout.Seconds() != 5 {
		t.Fatalf("timeout = %v, want 5s", c.Timeout)
	}
}
