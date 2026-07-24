package docs_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/9seconds/mtg/v2/mtglib"
	"github.com/stretchr/testify/require"
)

const nginxWebSocketRelayReference = "nginx-websocket-relay.conf"

func TestNginxWebSocketRelayReference(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(nginxWebSocketRelayReference)
	require.NoError(t, err)

	configuration := string(contents)
	relayLocation := nginxBlock(t, configuration, "location = __RELAY_PATH__ {")
	decoyLocation := nginxBlock(t, configuration, "location @relay_decoy {")
	ordinaryLocation := nginxBlock(t, configuration, "location / {")

	t.Run("keeps deployment values explicit", func(t *testing.T) {
		t.Parallel()

		placeholderPattern := regexp.MustCompile(`__[A-Z0-9_]+__`)
		placeholders := placeholderPattern.FindAllString(configuration, -1)

		require.ElementsMatch(t, []string{
			"__ACCESS_LOG_PATH__",
			"__DECOY_UPSTREAM_ADDRESS__",
			"__ERROR_LOG_PATH__",
			"__MTG_WEBSOCKET_RELAY_ADDRESS__",
			"__RELAY_PATH__",
			"__RELAY_SERVER_NAME__",
			"__TLS_CERTIFICATE_KEY_PATH__",
			"__TLS_CERTIFICATE_PATH__",
		}, uniqueStrings(placeholders))
	})

	t.Run("isolates and forwards the websocket route", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, 1, strings.Count(configuration, "proxy_pass http://mtg_websocket_relay;"))
		require.Contains(t, relayLocation, "proxy_http_version 1.1;")
		require.Contains(t, relayLocation, "proxy_set_header Upgrade $http_upgrade;")
		require.Contains(t, relayLocation, "proxy_set_header Connection $connection_upgrade;")
		require.Contains(t, relayLocation, "proxy_set_header Authorization $http_authorization;")
		require.Contains(t, relayLocation,
			"proxy_set_header Sec-WebSocket-Protocol $http_sec_websocket_protocol;")
		require.Contains(t, relayLocation, "proxy_set_header X-Real-IP $remote_addr;")
		require.Contains(t, relayLocation, "proxy_set_header X-Forwarded-For \"\";")
		require.Contains(t, relayLocation, "proxy_buffering off;")
		require.Contains(t, relayLocation, "proxy_request_buffering off;")
		require.Contains(t, relayLocation, "proxy_cache off;")
	})

	t.Run("intercepts relay failures without a decoy loop", func(t *testing.T) {
		t.Parallel()

		require.Contains(t, relayLocation, "proxy_intercept_errors on;")
		for _, status := range []string{"400", "401", "403", "404", "405", "413", "426", "429", "500", "502", "503", "504"} {
			require.Regexp(t, regexp.MustCompile(`\b`+status+`\b`), relayLocation)
		}
		require.Contains(t, relayLocation, "= @relay_decoy;")

		require.Contains(t, decoyLocation, "proxy_intercept_errors off;")
		require.Contains(t, decoyLocation, "rewrite ^ / break;")
		require.Contains(t, decoyLocation, "proxy_method GET;")
		require.Contains(t, decoyLocation, "proxy_pass_request_body off;")
		require.Contains(t, decoyLocation, "proxy_set_header Content-Length \"\";")
		require.Contains(t, decoyLocation, "proxy_set_header Transfer-Encoding \"\";")
		require.Contains(t, decoyLocation, "proxy_set_header Authorization \"\";")
		require.Contains(t, decoyLocation, "proxy_set_header Upgrade \"\";")
		require.Contains(t, decoyLocation, "proxy_set_header Sec-WebSocket-Protocol \"\";")
	})

	t.Run("strips relay credentials from the ordinary site", func(t *testing.T) {
		t.Parallel()

		for _, location := range []string{decoyLocation, ordinaryLocation} {
			require.Contains(t, location, "proxy_set_header Authorization \"\";")
			require.Contains(t, location, "proxy_set_header Upgrade \"\";")
			require.Contains(t, location, "proxy_set_header Connection \"\";")
			require.Contains(t, location, "proxy_set_header Sec-WebSocket-Protocol \"\";")
		}
	})

	t.Run("keeps nginx tunnel timeouts above the mtg default", func(t *testing.T) {
		t.Parallel()

		for _, directive := range []string{"proxy_read_timeout", "proxy_send_timeout"} {
			valuePattern := regexp.MustCompile(`(?m)^\s*` + directive + `\s+(\S+);`)
			match := valuePattern.FindStringSubmatch(relayLocation)
			require.Len(t, match, 2)

			configuredTimeout, err := time.ParseDuration(match[1])
			require.NoError(t, err)
			require.Greater(t, configuredTimeout, mtglib.DefaultIdleTimeout)
		}
	})

	t.Run("uses a credential-safe access log", func(t *testing.T) {
		t.Parallel()

		logFormat := nginxDirective(t, configuration, "log_format relay_site")
		require.Contains(t, logFormat, "$uri")
		for _, forbidden := range []string{
			"$request ",
			"$args",
			"$http_authorization",
			"$http_referer",
		} {
			require.NotContains(t, logFormat, forbidden)
		}
	})
}

func nginxBlock(t *testing.T, configuration, marker string) string {
	t.Helper()

	start := strings.Index(configuration, marker)
	require.NotEqual(t, -1, start, "missing nginx block %q", marker)

	depth := 0
	for offset, character := range configuration[start:] {
		switch character {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return configuration[start : start+offset+1]
			}
		}
	}

	t.Fatalf("unterminated nginx block %q", marker)

	return ""
}

func nginxDirective(t *testing.T, configuration, marker string) string {
	t.Helper()

	start := strings.Index(configuration, marker)
	require.NotEqual(t, -1, start, "missing nginx directive %q", marker)

	end := strings.Index(configuration[start:], ";")
	require.NotEqual(t, -1, end, "unterminated nginx directive %q", marker)

	return configuration[start : start+end+1]
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))

	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}

		seen[value] = struct{}{}
		unique = append(unique, value)
	}

	return unique
}
