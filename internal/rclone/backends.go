package rclone

import (
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var googleClientPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.apps\.googleusercontent\.com$`)
var googleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var dnsLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validateBackend(cloud map[string]string) bool {
	switch cloud["type"] {
	case "onedrive":
		if !allowedKeys(cloud, "type", "token", "drive_id", "drive_type", "client_id", "client_secret", "region") ||
			!drivePattern.MatchString(cloud["drive_id"]) ||
			(cloud["drive_type"] != "personal" && cloud["drive_type"] != "business" && cloud["drive_type"] != "documentLibrary") ||
			(cloud["region"] != "" && cloud["region"] != "global") ||
			(cloud["client_id"] != "" && !drivePattern.MatchString(cloud["client_id"])) ||
			len(cloud["client_secret"]) > 4096 {
			return false
		}
		cloud["region"] = "global"
		return normalizeToken(cloud)
	case "drive":
		if !allowedKeys(cloud, "type", "token", "client_id", "client_secret", "scope", "root_folder_id", "team_drive") ||
			len(cloud["client_id"]) > 256 || !googleClientPattern.MatchString(cloud["client_id"]) ||
			cloud["client_secret"] == "" || len(cloud["client_secret"]) > 4096 ||
			(cloud["root_folder_id"] == "" && cloud["team_drive"] == "") || cloud["root_folder_id"] == "root" || cloud["team_drive"] == "root" ||
			(cloud["root_folder_id"] != "" && !googleIDPattern.MatchString(cloud["root_folder_id"])) ||
			(cloud["team_drive"] != "" && !googleIDPattern.MatchString(cloud["team_drive"])) {
			return false
		}
		if cloud["scope"] == "" {
			cloud["scope"] = "drive"
		}
		return (cloud["scope"] == "drive" || cloud["scope"] == "drive.file") && normalizeToken(cloud)
	case "webdav":
		if !allowedKeys(cloud, "type", "url", "vendor", "user", "pass", "bearer_token") {
			return false
		}
		switch cloud["vendor"] {
		case "":
			cloud["vendor"] = "other"
		case "other", "nextcloud", "owncloud", "fastmail", "rclone":
		default:
			return false
		}
		basic := cloud["user"] != "" && len(cloud["user"]) <= 1024 && !strings.Contains(cloud["user"], ":") && obscured(cloud["pass"]) && cloud["bearer_token"] == ""
		bearer := cloud["user"] == "" && cloud["pass"] == "" && cloud["bearer_token"] != "" && len(cloud["bearer_token"]) <= 8192
		if !basic && !bearer {
			return false
		}
		endpoint, ok := webDAVURL(cloud["url"])
		if !ok {
			return false
		}
		cloud["url"] = endpoint.String()
		return true
	default:
		return false
	}
}

// Validate syntax here, resolve and pin all addresses at the runtime boundary.
func webDAVURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Scheme != "https" || u.Opaque != "" ||
		u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(raw, "#") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\x00") ||
		strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicIP(ip) {
			return nil, false
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") {
			return nil, false
		}
		for _, label := range strings.Split(host, ".") {
			if !dnsLabelPattern.MatchString(label) {
				return nil, false
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return nil, false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, false
	}
	if u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") {
		return nil, false
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" && port != "443" {
		u.Host = net.JoinHostPort(host, port)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		u.RawPath = ""
	}
	return u, true
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}
var ipv6Global = netip.MustParsePrefix("2000::/3")

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !ipv6Global.Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
