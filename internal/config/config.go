package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type Service struct {
	Target      string `json:"target"`
	Description string `json:"description,omitempty"`
}
type Server struct {
	Services map[string]Service `json:"services"`
}

func Load(path string) (Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Server{}, err
	}
	var cfg Server
	if json.Unmarshal(b, &cfg) != nil {
		cfg, err = parseSimpleYAML(string(b))
		if err != nil {
			return Server{}, fmt.Errorf("parse server configuration: %w", err)
		}
	}
	if len(cfg.Services) == 0 {
		return Server{}, fmt.Errorf("configuration has no services")
	}
	for name, svc := range cfg.Services {
		if name == "" || strings.ContainsAny(name, "/\\ ") {
			return Server{}, fmt.Errorf("invalid service name %q", name)
		}
		u, err := url.Parse(svc.Target)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return Server{}, fmt.Errorf("service %q has invalid target", name)
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && !ip.IsLoopback() {
			return Server{}, fmt.Errorf("service %q target must be loopback", name)
		}
	}
	return cfg, nil
}

// parseSimpleYAML accepts the documented services mapping. It intentionally
// avoids a runtime YAML dependency; use JSON for configuration that needs YAML
// features outside this small, human-editable shape.
func parseSimpleYAML(input string) (Server, error) {
	cfg := Server{Services: map[string]Service{}}
	var current string
	seenServices := false
	for lineNo, raw := range strings.Split(input, "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if indent == 0 && line == "services:" {
			seenServices = true
			continue
		}
		if !seenServices {
			return Server{}, fmt.Errorf("line %d: expected services:", lineNo+1)
		}
		if indent == 2 && strings.HasSuffix(line, ":") {
			current = strings.TrimSuffix(line, ":")
			if current == "" {
				return Server{}, fmt.Errorf("line %d: empty service name", lineNo+1)
			}
			cfg.Services[current] = Service{}
			continue
		}
		if indent >= 4 && current != "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				return Server{}, fmt.Errorf("line %d: expected key: value", lineNo+1)
			}
			key, val := strings.TrimSpace(parts[0]), unquote(strings.TrimSpace(parts[1]))
			svc := cfg.Services[current]
			switch key {
			case "target":
				svc.Target = val
			case "description":
				svc.Description = val
			default:
				return Server{}, fmt.Errorf("line %d: unsupported key %q", lineNo+1, key)
			}
			cfg.Services[current] = svc
			continue
		}
		return Server{}, fmt.Errorf("line %d: unsupported YAML structure", lineNo+1)
	}
	if !seenServices {
		return Server{}, fmt.Errorf("missing services:")
	}
	return cfg, nil
}
func unquote(s string) string {
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1]
	}
	return s
}
