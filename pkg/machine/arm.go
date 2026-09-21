package machine

import (
	"fmt"
	"os"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// Arm resolves (or mints) the API secret key, syncs the plane's own tables and arms the routes.
// It is the second of the two lines this fork adds to cmd/webserver.go. Minting happens here, at
// boot — never at import, so no test writes a secret into a real home directory (apis.mdx §5.4).
//
// A failure to arm never stops the server: upstream's web app keeps working and the plane stays
// dark (every /machine/v1 route answers 404), which is fail-closed (R9).
func Arm(c core.Context, config *settings.Config) {
	if envBool("EZBK_MACHINE_DISABLE") {
		log.BootInfof(c, "[machine.Arm] machine plane disabled by EZBK_MACHINE_DISABLE")
		return
	}

	if config.Protocol == settings.SCHEME_SOCKET {
		socketListenerIsPrivate = false

		if info, err := os.Stat(config.UnixSocketPath); err == nil {
			if uid, ok := fileOwnerUid(info); ok && uid == os.Getuid() && info.Mode().Perm()&0o077 == 0 {
				socketListenerIsPrivate = true
			}
		}

		if !socketListenerIsPrivate {
			log.BootWarnf(c, "[machine.Arm] unix socket %s is not 0600 and owned by this user; the machine plane will refuse every request", config.UnixSocketPath)
		}
	}

	key, source, err := ResolveKey("server", true)

	if err != nil {
		log.BootErrorf(c, "[machine.Arm] machine plane NOT armed: %s", err.Error())
		return
	}

	if key == "" {
		log.BootErrorf(c, "[machine.Arm] machine plane NOT armed: no API secret key could be resolved or minted")
		return
	}

	if datastore.Container != nil && datastore.Container.UserDataStore != nil {
		if err := datastore.Container.UserDataStore.SyncStructs(machineTables...); err != nil {
			log.BootErrorf(c, "[machine.Arm] machine plane NOT armed: syncing machine_* tables failed: %s", err.Error())
			return
		}
	}

	credsPath, _ := CredentialsPath()
	st := newPlaneState(key, source, credsPath)
	stateHolder.Store(st)

	boundUser := "(none yet)"

	if user, err := ResolveBoundUser(c); err == nil {
		boundUser = fmt.Sprintf("%q", user.Username)
	} else if f := toFail(err); f != nil {
		boundUser = "(unbound: " + f.Message + ")"
	}

	writes := "DISABLED"

	if st.allowWrite {
		writes = "ENABLED"
	}

	admin := "DISABLED"

	if st.allowAdmin {
		admin = "ENABLED"
	}

	log.BootInfof(c, "[machine.Arm] Machine plane armed on %s (key %s from %s, user %s, writes %s, admin %s, %d routes)",
		BasePath, st.fingerprint, sourceLabel(source), boundUser, writes, admin, len(Routes()))

	if config.Protocol != settings.SCHEME_SOCKET && config.HttpAddr != "127.0.0.1" && config.HttpAddr != "localhost" && config.HttpAddr != "::1" {
		log.BootWarnf(c, "[machine.Arm] the server listens on %s, so the browser API is reachable from the network; /machine/v1 still answers loopback callers only", config.HttpAddr)
	}

	if config.EnableAPIToken || config.EnableMCPServer {
		log.BootWarnf(c, "[machine.Arm] upstream full-access doors are ON (enable_api_token=%t, enable_mcp=%t); this fork keeps them off", config.EnableAPIToken, config.EnableMCPServer)
	}
}

func sourceLabel(source string) string {
	if source == "" {
		return "a new mint"
	}

	return source
}
