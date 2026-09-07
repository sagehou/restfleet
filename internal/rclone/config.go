package rclone

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxConfigBytes = 256 << 10

var ErrInvalidConfig = errors.New("unsupported or invalid rclone configuration")
var remotePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,63}$`)
var drivePattern = regexp.MustCompile(`^[A-Za-z0-9!_-]{1,256}$`)

// Config is short-lived secret material. Never log it or return it through an API.
type Config struct {
	sections map[string]map[string]string
	remote   string
}

// ParseConfig accepts a deliberately restricted INI subset, then emits canonical
// config rather than passing user-supplied configuration syntax to rclone.
func ParseConfig(raw, remote string) (*Config, error) {
	return parseConfig(raw, remote, "")
}

// socket is an exact runtime-owned path, never an option supplied by an API caller.
func parseConfig(raw, remote, socket string) (*Config, error) {
	if len(raw) == 0 || len(raw) > MaxConfigBytes || !utf8.ValidString(raw) ||
		!remotePattern.MatchString(remote) {
		return nil, ErrInvalidConfig
	}
	sections := make(map[string]map[string]string)
	var section map[string]string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), MaxConfigBytes+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.IndexFunc(line, unicode.IsControl) >= 0 {
			return nil, ErrInvalidConfig
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := line[1 : len(line)-1]
			if !remotePattern.MatchString(name) || sections[name] != nil || len(sections) == 2 {
				return nil, ErrInvalidConfig
			}
			section = make(map[string]string)
			sections[name] = section
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || section == nil {
			return nil, ErrInvalidConfig
		}
		if _, duplicate := section[key]; duplicate {
			return nil, ErrInvalidConfig
		}
		section[key] = value
	}
	if scanner.Err() != nil || len(sections) != 2 {
		return nil, ErrInvalidConfig
	}
	crypt := sections[remote]
	if crypt == nil || crypt["type"] != "crypt" ||
		!allowedKeys(crypt, "type", "remote", "password", "password2", "filename_encryption", "directory_name_encryption") ||
		!obscured(crypt["password"]) || (crypt["password2"] != "" && !obscured(crypt["password2"])) {
		return nil, ErrInvalidConfig
	}
	if crypt["filename_encryption"] != "" && crypt["filename_encryption"] != "standard" {
		return nil, ErrInvalidConfig
	}
	if crypt["directory_name_encryption"] != "" && crypt["directory_name_encryption"] != "true" {
		return nil, ErrInvalidConfig
	}
	crypt["filename_encryption"] = "standard"
	crypt["directory_name_encryption"] = "true"
	upstream, root, ok := strings.Cut(crypt["remote"], ":")
	if !ok || upstream == remote || !safeRoot(root) {
		return nil, ErrInvalidConfig
	}
	cloud := sections[upstream]
	if cloud == nil {
		return nil, ErrInvalidConfig
	}
	if socket != "" {
		if cloud["type"] != "webdav" || cloud["unix_socket"] != socket {
			return nil, ErrInvalidConfig
		}
		delete(cloud, "unix_socket")
	}
	if !validateBackend(cloud) {
		return nil, ErrInvalidConfig
	}
	return &Config{sections: sections, remote: remote}, nil
}

// Backend is safe metadata derived from the validated configuration.
func (c *Config) Backend() string { return c.cloud()["type"] }

func (c *Config) cloud() map[string]string {
	upstream, _, _ := strings.Cut(c.sections[c.remote]["remote"], ":")
	return c.sections[upstream]
}

func normalizeToken(cloud map[string]string) bool {
	var token struct {
		AccessToken  string    `json:"access_token"`
		TokenType    string    `json:"token_type"`
		RefreshToken string    `json:"refresh_token"`
		Expiry       time.Time `json:"expiry"`
	}
	decoder := json.NewDecoder(strings.NewReader(cloud["token"]))
	decoder.DisallowUnknownFields()
	if !json.Valid([]byte(cloud["token"])) || decoder.Decode(&token) != nil ||
		token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" ||
		token.Expiry.IsZero() || strings.IndexFunc(token.AccessToken+token.RefreshToken, unicode.IsControl) >= 0 {
		return false
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return false
	}
	cloud["token"] = string(encoded)
	return true
}

func allowedKeys(values map[string]string, allowed ...string) bool {
	for key := range values {
		found := false
		for _, option := range allowed {
			found = found || key == option
		}
		if !found {
			return false
		}
	}
	return true
}

func obscured(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	defer clear(decoded)
	return err == nil && len(decoded) > 16 && len(decoded) <= 4096
}

func safeRoot(root string) bool {
	if root == "" {
		return true
	}
	return len(root) <= 512 && !strings.HasPrefix(root, "/") && root != "." &&
		root != ".." && !strings.HasPrefix(root, "../") && path.Clean(root) == root &&
		!strings.ContainsAny(root, "\\:%\x00") && strings.IndexFunc(root, unicode.IsControl) < 0
}

func (c *Config) Bytes() []byte {
	var out strings.Builder
	names := make([]string, 0, len(c.sections))
	for name := range c.sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.WriteString("[" + name + "]\n")
		keys := make([]string, 0, len(c.sections[name]))
		for key, value := range c.sections[name] {
			if value != "" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			out.WriteString(key + " = " + c.sections[name][key] + "\n")
		}
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// SameExceptToken allows only OAuth token refresh by a child process.
func (c *Config) SameExceptToken(other *Config) bool { return c.sameConfig(other, false) }

// SameTarget permits credential replacement, never relocation or a new identity.
func (c *Config) SameTarget(other *Config) bool { return c.sameConfig(other, true) }

func (c *Config) sameConfig(other *Config, replace bool) bool {
	if c == nil || other == nil || c.remote != other.remote || len(c.sections) != len(other.sections) {
		return false
	}
	for name, fields := range c.sections {
		next := other.sections[name]
		if next == nil {
			return false
		}
		for _, pair := range []struct{ a, b map[string]string }{{fields, next}, {next, fields}} {
			for key, value := range pair.a {
				oauth := fields["type"] == "onedrive" || fields["type"] == "drive"
				if oauth && (key == "token" || replace && (key == "client_id" || key == "client_secret")) {
					continue
				}
				if replace && fields["type"] == "webdav" && (key == "pass" || key == "bearer_token") {
					// Keep authentication mode, including whether this value is present.
					if (value == "") != (pair.b[key] == "") {
						return false
					}
					continue
				}
				if pair.b[key] != value {
					return false
				}
			}
		}
	}
	return true
}
