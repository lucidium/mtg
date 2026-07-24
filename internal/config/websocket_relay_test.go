package config_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/9seconds/mtg/v2/internal/config"
	"github.com/stretchr/testify/require"
)

const webSocketRelayTestSecret = "7oe1GqLy6TBc38CV3jx7q09nb29nbGUuY29t"

func webSocketRelayToken(fill byte, size int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, size))
}

func webSocketRelayConfig(extra string, tokens ...string) []byte {
	quotedTokens := make([]string, len(tokens))
	for index, token := range tokens {
		quotedTokens[index] = fmt.Sprintf("%q", token)
	}

	return fmt.Appendf(nil, `
secret = %q
bind-to = "0.0.0.0:3128"

[websocket-relay]
enabled = true
bind-to = "127.0.0.1:8081"
path = "/relay/v1/deployment-path"
tokens = [%s]
%s
`, webSocketRelayTestSecret, strings.Join(quotedTokens, ", "), extra)
}

func parseAndValidateWebSocketRelayConfig(t *testing.T, raw []byte) (*config.Config, error) {
	t.Helper()

	conf, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}

	return conf, conf.Validate()
}

func TestWebSocketRelayConfigValid(t *testing.T) {
	t.Parallel()

	currentToken := webSocketRelayToken(0x11, 32)
	nextToken := webSocketRelayToken(0x22, 64)
	conf, err := parseAndValidateWebSocketRelayConfig(
		t,
		webSocketRelayConfig("", currentToken, nextToken),
	)
	require.NoError(t, err)
	require.True(t, conf.WebSocketRelay.Enabled.Get(false))
	require.Equal(t, "127.0.0.1:8081", conf.WebSocketRelay.BindTo.Get(""))
	require.Equal(t, "/relay/v1/deployment-path", conf.WebSocketRelay.Path)
	require.Equal(t, []string{currentToken, nextToken}, conf.WebSocketRelay.Tokens)
	require.Equal(t, uint(config.DefaultWebSocketRelayMaxConnections), conf.WebSocketRelay.GetMaxConnections())
	require.Equal(t, uint(config.DefaultWebSocketRelayMaxMessageSize), conf.WebSocketRelay.GetMaxMessageSize())
}

func TestWebSocketRelayConfigDisabledPreservesLegacyValidation(t *testing.T) {
	t.Parallel()

	raw := fmt.Appendf(nil, `
secret = %q
bind-to = "0.0.0.0:3128"

[websocket-relay]
enabled = false
`, webSocketRelayTestSecret)

	conf, err := config.Parse(raw)
	require.NoError(t, err)
	require.False(t, conf.WebSocketRelay.Enabled.Get(false))
	require.NoError(t, conf.Validate())
}

func TestWebSocketRelayConfigRejectsInvalidBindAndCollisions(t *testing.T) {
	t.Parallel()

	token := webSocketRelayToken(0x33, 32)
	testCases := map[string]struct {
		relayBind      string
		legacyBind     string
		prometheusBind string
	}{
		"non-loopback": {
			relayBind: "192.0.2.1:8081",
		},
		"legacy exact": {
			relayBind:  "127.0.0.1:3128",
			legacyBind: "127.0.0.1:3128",
		},
		"legacy wildcard": {
			relayBind: "127.0.0.1:3128",
		},
		"prometheus": {
			relayBind:      "127.0.0.1:3129",
			prometheusBind: "127.0.0.1:3129",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw := string(webSocketRelayConfig("", token))
			raw = strings.Replace(
				raw,
				`bind-to = "127.0.0.1:8081"`,
				fmt.Sprintf("bind-to = %q", testCase.relayBind),
				1,
			)
			if testCase.legacyBind != "" {
				raw = strings.Replace(
					raw,
					`bind-to = "0.0.0.0:3128"`,
					fmt.Sprintf("bind-to = %q", testCase.legacyBind),
					1,
				)
			}
			if testCase.prometheusBind != "" {
				raw += fmt.Sprintf(`
[stats.prometheus]
enabled = true
bind-to = %q
`, testCase.prometheusBind)
			}

			_, err := parseAndValidateWebSocketRelayConfig(t, []byte(raw))
			require.Error(t, err)
		})
	}
}

func TestWebSocketRelayConfigAcceptsIPv6Loopback(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(
		string(webSocketRelayConfig("", webSocketRelayToken(0x44, 32))),
		`bind-to = "127.0.0.1:8081"`,
		`bind-to = "[::1]:8081"`,
		1,
	)

	_, err := parseAndValidateWebSocketRelayConfig(t, []byte(raw))
	require.NoError(t, err)
}

func TestWebSocketRelayConfigRejectsInvalidPaths(t *testing.T) {
	t.Parallel()

	token := webSocketRelayToken(0x55, 32)
	paths := []string{
		"",
		"/",
		"relay",
		"//relay",
		"/relay/",
		"/relay/../other",
		"/relay?query",
		"/relay#fragment",
		"/relay%2fother",
		`/relay\other`,
		"/relay other",
		"/" + strings.Repeat("a", 1024),
	}

	for _, relayPath := range paths {
		t.Run(fmt.Sprintf("%q", relayPath), func(t *testing.T) {
			t.Parallel()

			raw := strings.Replace(
				string(webSocketRelayConfig("", token)),
				`path = "/relay/v1/deployment-path"`,
				fmt.Sprintf("path = %q", relayPath),
				1,
			)
			_, err := parseAndValidateWebSocketRelayConfig(t, []byte(raw))
			require.Error(t, err)
		})
	}
}

func TestWebSocketRelayConfigRejectsInvalidTokens(t *testing.T) {
	t.Parallel()

	validToken := webSocketRelayToken(0x66, 32)
	testCases := map[string][]string{
		"none":      nil,
		"malformed": {"not_base64url="},
		"short":     {webSocketRelayToken(0x77, 31)},
		"oversized": {webSocketRelayToken(0x88, 65)},
		"duplicate": {validToken, validToken},
		"too many":  {validToken, webSocketRelayToken(1, 32), webSocketRelayToken(2, 32), webSocketRelayToken(3, 32), webSocketRelayToken(4, 32)},
	}

	for name, tokens := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := parseAndValidateWebSocketRelayConfig(t, webSocketRelayConfig("", tokens...))
			require.Error(t, err)
			for _, token := range tokens {
				require.NotContains(t, err.Error(), token)
			}
		})
	}
}

func TestWebSocketRelayConfigRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	token := webSocketRelayToken(0x99, 32)
	testCases := map[string]string{
		"zero connections":  `max-connections = 0`,
		"large connections": `max-connections = 65536`,
		"small message":     `max-message-size = "63KiB"`,
		"large message":     `max-message-size = "65MiB"`,
		"zero message":      `max-message-size = "0"`,
	}

	for name, extra := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := parseAndValidateWebSocketRelayConfig(t, webSocketRelayConfig(extra, token))
			require.Error(t, err)
		})
	}
}

func TestConfigStringRedactsCredentials(t *testing.T) {
	t.Parallel()

	currentToken := webSocketRelayToken(0xaa, 32)
	nextToken := webSocketRelayToken(0xbb, 32)
	conf, err := parseAndValidateWebSocketRelayConfig(
		t,
		webSocketRelayConfig("", currentToken, nextToken),
	)
	require.NoError(t, err)

	diagnostic := conf.String()
	require.NotContains(t, diagnostic, webSocketRelayTestSecret)
	require.NotContains(t, diagnostic, currentToken)
	require.NotContains(t, diagnostic, nextToken)
	require.Contains(t, diagnostic, `"secret":"[redacted]"`)
	require.Contains(t, diagnostic, `"tokens":"[redacted: 2 configured]"`)
}
