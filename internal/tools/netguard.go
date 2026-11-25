package tools

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
)

// web_fetch must not become a way into the user's own network. A prompt
// injection could make the model fetch http://localhost:…, a router page, a
// tailnet service or a cloud metadata endpoint, or leak data through such a
// request. Fetches to those addresses therefore always need the user's
// approval, and a redirect can't lead from a public page into them.
//
// The check runs before the request and on every redirect. It can't stop DNS
// rebinding between the check and the connection; real isolation is the job
// of a sandbox.

type privateNetKey struct{}

// WithPrivateNetwork marks ctx as allowed to reach private addresses (the
// user approved a fetch to one).
func WithPrivateNetwork(ctx context.Context) context.Context {
	return context.WithValue(ctx, privateNetKey{}, true)
}

func privateNetworkAllowed(ctx context.Context) bool {
	ok, _ := ctx.Value(privateNetKey{}).(bool)
	return ok
}

// cgnat is 100.64.0.0/10: carrier-grade NAT, and the range Tailscale uses.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a)
}

// privateHostSuffixes are names that only resolve inside a local network.
var privateHostSuffixes = []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".ts.net"}

// PrivateURL reports whether rawURL points at the local machine or a
// private network (loopback, RFC 1918, link-local, CGNAT/Tailscale, ULA,
// .local / .internal / .ts.net names), and why. Hostnames are resolved; a
// name that doesn't resolve is not reported (the fetch will fail anyway).
func PrivateURL(ctx context.Context, rawURL string) (bool, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false, ""
	}
	if host == "localhost" {
		return true, "localhost"
	}
	for _, s := range privateHostSuffixes {
		if strings.HasSuffix(host, s) {
			return true, "local network name " + host
		}
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if privateAddr(a) {
			return true, "private address " + a.String()
		}
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false, ""
	}
	for _, a := range addrs {
		if privateAddr(a) {
			return true, host + " resolves to private address " + a.Unmap().String()
		}
	}
	return false, ""
}

var errPrivateRedirect = errors.New("redirect to a private address refused")

// guardedClient is an HTTP client for web tools. Unless ctx allows private
// addresses, it refuses redirects that lead to one.
func guardedClient(ctx context.Context, timeout time.Duration) *http.Client {
	allow := privateNetworkAllowed(ctx)
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if !allow {
				if bad, why := PrivateURL(req.Context(), req.URL.String()); bad {
					return fmt.Errorf("%w (%s): %s", errPrivateRedirect, why, req.URL.Redacted())
				}
			}
			return nil
		},
	}
}

// checkFetchTarget refuses a private target unless ctx allows it.
func checkFetchTarget(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("web_fetch: invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web_fetch: only http and https URLs are supported, not %q", u.Scheme)
	}
	if privateNetworkAllowed(ctx) {
		return nil
	}
	if bad, why := PrivateURL(ctx, rawURL); bad {
		return fmt.Errorf("web_fetch: refusing to fetch %s (%s) without the user's approval", u.Redacted(), why)
	}
	return nil
}
