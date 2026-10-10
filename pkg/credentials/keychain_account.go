package credentials

import "regexp"

var claudeKeychainAccountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// claudeKeychainAccount matches Claude Code's native account selection. Never
// fall back to a service-only lookup, which can select another account's entry.
func claudeKeychainAccount(envUser string, lookup func() (string, error)) string {
	account := envUser
	if account == "" {
		var err error
		account, err = lookup()
		if err != nil {
			account = "claude-code-user"
		}
	}
	if !claudeKeychainAccountPattern.MatchString(account) {
		return "claude-code-user"
	}
	return account
}
