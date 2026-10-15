package anime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Public feed requests pin the resolved address at dial time, including on
// redirects. Feeds cannot use the NAS as a proxy to loopback or LAN services.
func publicHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("anime: feed host could not be resolved")
		}
		_, shared, _ := net.ParseCIDR("100.64.0.0/10")
		for _, ip := range ips {
			if !ip.IP.IsGlobalUnicast() || ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || shared.Contains(ip.IP) {
				return nil, errors.New("anime: feed resolves to a private or reserved address")
			}
		}
		var last error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = errors.New("anime: feed has no usable address")
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many feed redirects")
		}
		return validateHTTPURL(req.URL.String())
	}}
}
