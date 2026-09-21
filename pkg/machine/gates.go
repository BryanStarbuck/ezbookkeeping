package machine

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// HeaderAPIKey carries the API secret key. Never a query parameter, never Authorization (§5.6).
const HeaderAPIKey = "X-Ezbk-Api-Key"

// HeaderClient is the advisory client identity, used for logs and the MCP admin fence only
const HeaderClient = "X-Ezbk-Client"

// isLoopbackSocket reads the kernel's view of the peer. It never calls gin's ClientIP(), which
// honours X-Forwarded-For from trusted proxies — and upstream's default trusted proxies include
// 127.0.0.0/8 (apis.mdx §5.7).
func isLoopbackSocket(c *gin.Context, config *settings.Config) bool {
	if config.Protocol == settings.SCHEME_SOCKET {
		return socketListenerIsPrivate
	}

	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)

	if err != nil {
		errfile.Expected("splitting the remote address of the request", err)
		host = c.Request.RemoteAddr
	}

	ip := net.ParseIP(strings.Trim(host, "[]"))

	return ip != nil && ip.IsLoopback()
}

// isBrowserRequest detects a browser by the headers browsers always send and our clients never do
func isBrowserRequest(c *gin.Context) bool {
	if c.GetHeader("Origin") != "" {
		return true
	}

	if c.GetHeader("Sec-Fetch-Site") != "" || c.GetHeader("Sec-Fetch-Mode") != "" {
		return true
	}

	return false
}

// hostAllowed pins the Host header against DNS rebinding
func hostAllowed(c *gin.Context, config *settings.Config) bool {
	if config.Protocol == settings.SCHEME_SOCKET {
		return true
	}

	host := strings.ToLower(c.Request.Host)
	port := strconv.Itoa(int(config.HttpPort))

	switch host {
	case "127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port:
		return true
	}

	if port == "80" {
		switch host {
		case "127.0.0.1", "localhost", "[::1]":
			return true
		}
	}

	return false
}

// keyMatches compares in constant time over fixed-width digests, so a length mismatch is never an
// oracle (apis.mdx §5.6)
func keyMatches(presented string, expectedDigest [32]byte) bool {
	got := sha256.Sum256([]byte(presented))

	return subtle.ConstantTimeCompare(got[:], expectedDigest[:]) == 1 && presented != ""
}

// gateMiddleware runs gates 1–3. Everything it refuses before the key is a 404 — probing teaches
// nothing — and the 401 body is constant for a missing and a wrong key.
func gateMiddleware(config *settings.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isLoopbackSocket(c, config) || isBrowserRequest(c) || !hostAllowed(c, config) {
			abortNotFound(c)
			return
		}

		st := currentState()

		if st == nil {
			abortNotFound(c)
			return
		}

		c.Header("Cache-Control", "no-store")
		c.Header("Vary", "Origin")

		if !keyMatches(c.GetHeader(HeaderAPIKey), st.keyDigest) {
			c.AbortWithStatusJSON(401, gin.H{"ok": false, "error": gin.H{"code": CodeUnauthorized}})
			return
		}

		c.Next()
	}
}

func abortNotFound(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(404, gin.H{"ok": false, "error": gin.H{"code": CodeNotFound}})
}
