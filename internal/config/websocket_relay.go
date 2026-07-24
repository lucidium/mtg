package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	DefaultWebSocketRelayMaxConnections = 2048
	DefaultWebSocketRelayMaxMessageSize = 16 << 20

	minWebSocketRelayMessageSize = 64 << 10
	maxWebSocketRelayMessageSize = 64 << 20
	maxWebSocketRelayPathLength  = 1024
	maxWebSocketRelayTokens      = 4
	minWebSocketRelayTokenBytes  = 32
	maxWebSocketRelayTokenBytes  = 64
)

type WebSocketRelayConfig struct {
	Optional

	BindTo         TypeHostPort                  `json:"bindTo"`
	Path           string                        `json:"path"`
	Tokens         []string                      `json:"tokens"`
	MaxConnections TypeConcurrency               `json:"maxConnections"`
	MaxMessageSize TypeWebSocketRelayMessageSize `json:"maxMessageSize"`
}

type TypeWebSocketRelayMessageSize struct {
	TypeBytes

	defined bool
}

func (t *TypeWebSocketRelayMessageSize) Set(value string) error {
	t.defined = true

	return t.TypeBytes.Set(value)
}

func (t *TypeWebSocketRelayMessageSize) UnmarshalText(data []byte) error {
	return t.Set(string(data))
}

func (t TypeWebSocketRelayMessageSize) Get(defaultValue uint) uint {
	if !t.defined {
		return defaultValue
	}

	return uint(t.Value)
}

func (c WebSocketRelayConfig) GetMaxConnections() uint {
	return c.MaxConnections.Get(DefaultWebSocketRelayMaxConnections)
}

func (c WebSocketRelayConfig) GetMaxMessageSize() uint {
	return c.MaxMessageSize.Get(DefaultWebSocketRelayMaxMessageSize)
}

func (c WebSocketRelayConfig) validate(
	legacyBind TypeHostPort,
	prometheusEnabled bool,
	prometheusBind TypeHostPort,
) error {
	if !c.Enabled.Get(false) {
		return nil
	}

	bindIP := net.ParseIP(c.BindTo.Host)
	if c.BindTo.Get("") == "" || bindIP == nil || !bindIP.IsLoopback() {
		return fmt.Errorf("websocket-relay bind-to must be a loopback IP address")
	}

	if err := validateWebSocketRelayPath(c.Path); err != nil {
		return err
	}

	if err := validateWebSocketRelayTokens(c.Tokens); err != nil {
		return err
	}

	maxConnections := c.GetMaxConnections()
	if maxConnections == 0 || maxConnections > 65535 {
		return fmt.Errorf("websocket-relay max-connections must be between 1 and 65535")
	}

	maxMessageSize := c.GetMaxMessageSize()
	if maxMessageSize < minWebSocketRelayMessageSize || maxMessageSize > maxWebSocketRelayMessageSize {
		return fmt.Errorf(
			"websocket-relay max-message-size must be between %d and %d bytes",
			minWebSocketRelayMessageSize,
			maxWebSocketRelayMessageSize,
		)
	}

	if bindAddressesCollide(c.BindTo, legacyBind) {
		return fmt.Errorf("websocket-relay bind-to conflicts with the legacy proxy listener")
	}

	if prometheusEnabled && bindAddressesCollide(c.BindTo, prometheusBind) {
		return fmt.Errorf("websocket-relay bind-to conflicts with the prometheus listener")
	}

	return nil
}

func validateWebSocketRelayPath(value string) error {
	if value == "" || value[0] != '/' || value == "/" {
		return fmt.Errorf("websocket-relay path must be an absolute non-root path")
	}
	if len(value) > maxWebSocketRelayPathLength || !utf8.ValidString(value) {
		return fmt.Errorf("websocket-relay path is invalid")
	}

	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return fmt.Errorf("websocket-relay path must contain printable ASCII characters only")
		}
	}

	if strings.ContainsAny(value, "?#%\\") || path.Clean(value) != value {
		return fmt.Errorf("websocket-relay path must be canonical and unambiguous")
	}

	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Path != value || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("websocket-relay path must be a canonical request path")
	}

	return nil
}

func validateWebSocketRelayTokens(tokens []string) error {
	if len(tokens) == 0 || len(tokens) > maxWebSocketRelayTokens {
		return fmt.Errorf("websocket-relay requires between 1 and %d tokens", maxWebSocketRelayTokens)
	}

	seen := make(map[string]struct{}, len(tokens))

	for index, token := range tokens {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != token {
			return fmt.Errorf("websocket-relay token %d is not unpadded base64url", index+1)
		}
		if len(decoded) < minWebSocketRelayTokenBytes || len(decoded) > maxWebSocketRelayTokenBytes {
			return fmt.Errorf(
				"websocket-relay token %d must decode to between %d and %d bytes",
				index+1,
				minWebSocketRelayTokenBytes,
				maxWebSocketRelayTokenBytes,
			)
		}

		if _, ok := seen[token]; ok {
			return fmt.Errorf("websocket-relay token %d is duplicated", index+1)
		}
		seen[token] = struct{}{}
	}

	return nil
}

func bindAddressesCollide(left, right TypeHostPort) bool {
	if left.Get("") == "" || right.Get("") == "" || left.Port != right.Port {
		return false
	}

	leftIP := net.ParseIP(left.Host)
	rightIP := net.ParseIP(right.Host)
	if leftIP == nil || rightIP == nil {
		return false
	}

	return leftIP.Equal(rightIP) || leftIP.IsUnspecified() || rightIP.IsUnspecified()
}
