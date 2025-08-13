package proxy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/lxc/incus/v6/shared/subprocess"
	"gopkg.in/yaml.v3"

	"github.com/lxc/incus-os/incus-osd/api"
)

type kpxConfig struct {
	Bind  string `yaml:"bind"`
	Port  int    `yaml:"port"`
	Check bool   `yaml:"check"`

	Proxies     map[string]kpxProxy      `yaml:"proxies,omitempty"`
	Credentials map[string]kpxCredential `yaml:"credentials,omitempty"`
	Rules       []kpxRule                `yaml:"rules"`
}

type kpxProxy struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port,omitempty"`
	SSL        bool   `yaml:"ssl"`
	Type       string `yaml:"type"`
	Realm      string `yaml:"realm,omitempty"`
	Credential string `yaml:"credential,omitempty"`
}

type kpxCredential struct {
	Login    string `yaml:"login"`
	Password string `yaml:"password"`
}

type kpxRule struct {
	Host  string `yaml:"host"`
	Proxy string `yaml:"proxy"`
}

// StartLocalProxy starts a local kpx proxy with a configuration based off of the
// contents from the provided SystemNetworkProxy struct.
func StartLocalProxy(ctx context.Context, proxyConfig *api.SystemNetworkProxy) error {
	// Remove any existing /etc/environment file.
	err := os.Remove("/etc/environment")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Set the http_proxy and https_proxy environment variables.
	for _, envVarName := range []string{"http_proxy", "https_proxy"} {
		err = writeAndSetEnvironment(envVarName, "http://localhost:3128")
		if err != nil {
			return err
		}
	}

	// Generate the kpx config.
	yamlConfig, err := GenerateKPXConfig(proxyConfig)
	if err != nil {
		return err
	}

	// Write file to /etc/kpx.yaml
	err = os.WriteFile("/etc/kpx.yaml", yamlConfig, 0o644)
	if err != nil {
		return err
	}

	// Start the kpx daemon; can't use the helper method from the systemd package,
	// since that causes an import loop.
	_, err = subprocess.RunCommandContext(ctx, "systemctl", "start", "kpx.service")

	return err
}

// GenerateKPXConfig takes a network config struct and generates the kpx yaml configuration.
func GenerateKPXConfig(proxyConfig *api.SystemNetworkProxy) ([]byte, error) {
	// If no proxy configuration exists, generate an empty one.
	if proxyConfig == nil {
		proxyConfig = new(api.SystemNetworkProxy)
	}

	// If no proxy rules are defined, ensure there's a default one in the generated config.
	if len(proxyConfig.Rules) == 0 {
		definedServers := slices.Sorted(maps.Keys(proxyConfig.Servers))
		if len(definedServers) > 0 {
			// If at least one server is defined, default to using the "first" one.
			proxyConfig.Rules = append(proxyConfig.Rules, api.SystemNetworkProxyRule{
				Destination: "*",
				Target:      definedServers[0],
			})
		} else {
			// With no servers defined either, default to an "allow all" policy.
			proxyConfig.Rules = append(proxyConfig.Rules, api.SystemNetworkProxyRule{
				Destination: "*",
				Target:      "direct",
			})
		}
	}

	// Set basic kpx configuration.
	cfg := kpxConfig{
		Bind:  "localhost",
		Port:  3128,
		Check: false, // Don't attempt to check for updates.
	}

	cfg.Proxies = make(map[string]kpxProxy)
	cfg.Credentials = make(map[string]kpxCredential)

	// Generate server and credential configuration.
	for serverKey, server := range proxyConfig.Servers {
		if serverKey == "direct" || serverKey == "none" {
			return nil, errors.New("cannot use reserved keyword '" + serverKey + "' for proxy definition")
		}

		if !slices.Contains([]string{"anonymous", "basic", "kerberos"}, server.Auth) {
			return nil, errors.New("unsupported proxy authentication type " + server.Auth)
		}

		// Bit of a hack: if server.Host doesn't begin with http, add it for url.Parse() to work correctly.
		serverHost := server.Host
		if !strings.HasPrefix(serverHost, "http") {
			serverHost = "http://" + serverHost
		}

		parsedHost, err := url.Parse(serverHost)
		if err != nil {
			return nil, err
		}

		useTLS := server.UseTLS
		serverPort := 0

		if parsedHost.Port() == "" {
			if strings.HasPrefix(server.Host, "http://") {
				serverPort = 80
				useTLS = false
			} else if strings.HasPrefix(server.Host, "https://") {
				serverPort = 443
				useTLS = true
			}
		} else {
			serverPort, err = strconv.Atoi(parsedHost.Port())
			if err != nil {
				return nil, err
			}
		}

		credential := serverKey
		if server.Auth == "anonymous" {
			credential = ""
		}

		cfg.Proxies[serverKey] = kpxProxy{
			Host:       parsedHost.Hostname(),
			Port:       serverPort,
			SSL:        useTLS,
			Type:       server.Auth,
			Realm:      server.Realm,
			Credential: credential,
		}

		if server.Auth != "anonymous" {
			cfg.Credentials[serverKey] = kpxCredential{
				Login:    server.Username,
				Password: server.Password,
			}
		}
	}

	// Generate proxy rules.
	for _, rule := range proxyConfig.Rules {
		_, targetExists := cfg.Proxies[rule.Target]
		if !targetExists && rule.Target != "direct" && rule.Target != "none" {
			return nil, errors.New("no proxy defined for target " + rule.Target)
		}

		cfg.Rules = append(cfg.Rules, kpxRule{
			Host:  rule.Destination,
			Proxy: rule.Target,
		})
	}

	return yaml.Marshal(cfg)
}

func writeAndSetEnvironment(key string, value string) error {
	envFile, err := os.OpenFile("/etc/environment", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o0644) //nolint:gosec
	if err != nil {
		return err
	}
	defer envFile.Close()

	_, err = fmt.Fprintf(envFile, "%s=%s\n", key, value)
	if err != nil {
		return err
	}

	return os.Setenv(key, value)
}
