package machine

import (
	"os"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// configuredUsername returns the username the operator chose, if any (apis.mdx §6.1)
func configuredUsername() (string, string) {
	if v := strings.TrimSpace(os.Getenv("EZBK_MACHINE_USERNAME")); v != "" {
		return v, "EZBK_MACHINE_USERNAME"
	}

	if creds, err := ReadCredentials(); err == nil && strings.TrimSpace(creds.Username) != "" {
		return strings.TrimSpace(creds.Username), "ezbookkeeping.machine.username"
	}

	return "", ""
}

// listActiveUsernames returns up to limit non-deleted usernames
func listActiveUsernames(c core.Context, limit int) ([]string, error) {
	if datastore.Container == nil || datastore.Container.UserStore == nil {
		return nil, NewFail(CodeNotReady, "wait for the server to finish starting", "the database is not open")
	}

	var users []*models.User

	err := datastore.Container.UserStore.Choose(0).NewSession(c).Where("deleted=?", false).Cols("uid", "username").OrderBy("uid asc").Limit(limit).Find(&users)

	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(users))

	for _, u := range users {
		names = append(names, u.Username)
	}

	return names, nil
}

// ResolveBoundUser returns the one user the plane acts as. It never guesses between several.
func ResolveBoundUser(c core.Context) (*models.User, error) {
	username, source := configuredUsername()

	if username != "" {
		user, err := services.Users.GetUserByUsername(c, username)

		if err != nil || user == nil {
			errfile.Warn("loading the configured bound user", err)
			return nil, NewFail(CodeNotReady, "fix "+source+" to name an existing user, or register \""+username+"\" at the web UI", "the configured user %q does not exist", username)
		}

		return checkUsable(user)
	}

	names, err := listActiveUsernames(c, 10)

	if err != nil {
		if f := toFail(err); f.Code == CodeNotReady {
			return nil, f
		}

		return nil, NewFail(CodeNotReady, "check ~/T/ezbookkeeping/error.err; the users table could not be read", "cannot list users")
	}

	switch len(names) {
	case 0:
		return nil, NewFail(CodeNotReady, "register the first user in the web UI (http://localhost:8080/); the plane binds to it automatically", "no user exists yet")
	case 1:
		user, err := services.Users.GetUserByUsername(c, names[0])

		if err != nil || user == nil {
			if err != nil {
				errfile.Caught("loading the only user", err)
			}

			return nil, NewFail(CodeNotReady, "retry; if it persists, set ezbookkeeping.machine.username", "the only user could not be loaded")
		}

		return checkUsable(user)
	default:
		return nil, NewFail(CodeNotReady, "set ezbookkeeping.machine.username in ~/.credentials/ezbookkeeping.json (or EZBK_MACHINE_USERNAME for the server) to one of: "+strings.Join(names, ", "), "%d users exist and none is chosen; the plane never guesses whose books to open", len(names)).WithDetails(map[string]any{"usernames": names})
	}
}

func checkUsable(user *models.User) (*models.User, error) {
	if user.Deleted {
		return nil, NewFail(CodeNotReady, "choose another user in ezbookkeeping.machine.username", "the bound user %q is deleted", user.Username)
	}

	if user.Disabled {
		return nil, NewFail(CodeNotReady, "enable the user (ezbookkeeping userdata user-enable) or choose another", "the bound user %q is disabled", user.Username)
	}

	return user, nil
}
