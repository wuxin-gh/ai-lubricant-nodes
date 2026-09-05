package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	publicIPRequestTimeout  = 10 * time.Second
	publicIPRefreshInterval = 2 * time.Minute
	publicIPResponseLimit   = 4096
	publicIPConsensusVotes  = 3
)

var candidateIP = regexp.MustCompile(`(?i)(?:\b(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\b)|(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}`)

// localIPv4 returns this host's primary private IPv4 address (the source
// address the OS would use to reach an external host), or "" if it can't be
// determined. No packet is actually sent.
func localIPv4() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
		return addr.IP.String()
	}
	return ""
}

func internalNetworkLabels() map[string]string {
	labels := map[string]string{}
	if ip := localIPv4(); ip != "" {
		labels["internal_ip"] = ip
	}
	return labels
}

func publicIPHTTPClient(family int) *http.Client {
	network := "tcp4"
	if family == 6 {
		network = "tcp6"
	}
	dialer := &net.Dialer{Timeout: publicIPRequestTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{Transport: transport}
}

func fetchPublicIP(ctx context.Context, client *http.Client, rawURL string, family int) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, publicIPRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, publicIPResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > publicIPResponseLimit {
		return "", fmt.Errorf("response exceeds %d bytes", publicIPResponseLimit)
	}
	for _, candidate := range candidateIP.FindAllString(strings.TrimSpace(string(body)), -1) {
		parsed := net.ParseIP(candidate)
		if parsed == nil {
			continue
		}
		if family == 4 {
			if v4 := parsed.To4(); v4 != nil {
				return v4.String(), nil
			}
			continue
		}
		if parsed.To4() == nil {
			return parsed.String(), nil
		}
	}
	return "", fmt.Errorf("no valid IPv%d address", family)
}

// resolvePublicIP visits sources strictly sequentially. A value is trusted only
// after three distinct configured URLs return the same normalized address.
func resolvePublicIP(ctx context.Context, urls []string, family int) (string, error) {
	client := publicIPHTTPClient(family)
	votes := make(map[string]int)
	var failures int
	for _, rawURL := range urls {
		ip, err := fetchPublicIP(ctx, client, rawURL, family)
		if err != nil {
			failures++
			continue
		}
		votes[ip]++
		if votes[ip] >= publicIPConsensusVotes {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no IPv%d address reached %d votes (%d sources failed)", family, publicIPConsensusVotes, failures)
}
