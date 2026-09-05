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
	if cloud == nil || cloud["type"] != "onedrive" ||
		!allowedKeys(cloud, "type", "token", "drive_id", "drive_type", "client_id", "client_secret", "region") ||
		!drivePattern.MatchString(cloud["drive_id"]) ||
		(cloud["drive_type"] != "personal" && cloud["drive_type"] != "business" && cloud["drive_type"] != "documentLibrary") ||
		(cloud["region"] != "" && cloud["region"] != "global") {
		return nil, ErrInvalidConfig
	}
	if cloud["client_id"] != "" && !drivePattern.MatchString(cloud["client_id"]) {
		return nil, ErrInvalidConfig
	}
	if cloud["client_secret"] != "" && !obscured(cloud["client_secret"]) {
		return nil, ErrInvalidConfig
	}
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
		return nil, ErrInvalidConfig
	}
	// Emit only understood token fields; duplicate JSON keys cannot survive normalization.
	encoded, err := json.Marshal(token)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	cloud["token"] = string(encoded)
	cloud["region"] = "global"
	return &Config{sections: sections, remote: remote}, nil
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

// sameExceptToken is stricter than administrator replacement: a child process
// may refresh OAuth tokens but must not change OAuth client identity or keys.
func (c *Config) sameExceptToken(other *Config) bool {
	if !c.SameTarget(other) {
		return false
	}
	for name, fields := range c.sections {
		if fields["type"] == "onedrive" &&
			(fields["client_id"] != other.sections[name]["client_id"] ||
				fields["client_secret"] != other.sections[name]["client_secret"]) {
			return false
		}
	}
	return true
}

// SameTarget prevents a token replacement from relocating existing repositories
// or silently changing the crypt key and making historical objects unreadable.
func (c *Config) SameTarget(other *Config) bool {
	if c == nil || other == nil {
		return false
	}
	if c.remote != other.remote || len(c.sections) != len(other.sections) {
		return false
	}
	for name, fields := range c.sections {
		next := other.sections[name]
		if next == nil {
			return false
		}
		for _, pair := range []struct{ a, b map[string]string }{{fields, next}, {next, fields}} {
			for key, value := range pair.a {
				if fields["type"] == "onedrive" && (key == "token" || key == "client_id" || key == "client_secret") {
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
