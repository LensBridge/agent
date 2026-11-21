package netinfo

import (
	"net"
	"net/http"
	"time"
)

// NewHTTPClient is the client every request off the board uses (content
// sync, the release channels): a bounded connect so a board with no route
// gives up fast, and an overall limit long enough for a large package on a
// slow link.
//
// HTTP/2 connections are health-checked with pings. Without them a
// connection that died silently (a NAT or firewall dropping it, a Wi-Fi
// roam) stays in the pool, and every request to that host waits on it until
// ResponseHeaderTimeout ("http2: timeout awaiting response headers").
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
			ForceAttemptHTTP2:     true,
			HTTP2: &http.HTTP2Config{
				SendPingTimeout: 30 * time.Second,
				PingTimeout:     15 * time.Second,
			},
		},
	}
}
