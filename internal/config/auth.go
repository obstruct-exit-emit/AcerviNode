package config

import (
	"fmt"
	"strings"
)

// Roles a login account can have. RoleAdmin can do everything — Settings,
// user management, and both the Managed and Manual tabs. RoleMember is
// scoped to Manual downloads only: adding/viewing/managing a magnet/NZB/
// hoster link grabbed directly, never the *arr-driven Managed pipeline
// (interfering with something Sonarr/Radarr is actively tracking is a
// bigger deal than a member managing their own manual grabs) or Settings.
// See docs/providers.md#roles.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// UserAccount is one login account. Login itself is optional — no accounts
// means authentication is disabled and the API key alone (still fully
// functional, unaffected by any of this) is how the web UI and every
// integration (Sonarr/Radarr) authenticates, exactly how AcerviNode always
// worked before this existed.
type UserAccount struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
	// Role is RoleAdmin or RoleMember; empty means admin (see
	// EffectiveRole) — an account saved before roles existed keeps full
	// access rather than being silently downgraded.
	Role string `yaml:"role,omitempty"`
	// Default marks the account created by the first-run setup wizard (or
	// promoted to replace it) — always admin, and the one account that
	// can't be removed or demoted, so an instance can never end up with
	// login enabled and zero admins.
	Default bool `yaml:"default,omitempty"`
}

// EffectiveRole returns the account's role, defaulting to admin for an
// empty Role (see UserAccount.Role).
func (u UserAccount) EffectiveRole() string {
	if u.Role == "" {
		return RoleAdmin
	}
	return u.Role
}

// AuthSettings holds every login account. No accounts means authentication
// is disabled entirely — the web UI falls back to the API-key prompt.
type AuthSettings struct {
	Users []UserAccount `yaml:"users,omitempty"`
}

// Enabled reports whether any login account is configured.
func (a AuthSettings) Enabled() bool { return len(a.Users) > 0 }

// Find returns the account with the given username (case-insensitive), or
// nil.
func (a AuthSettings) Find(username string) *UserAccount {
	for i := range a.Users {
		if strings.EqualFold(a.Users[i].Username, username) {
			return &a.Users[i]
		}
	}
	return nil
}

// AuthSettings returns a copy of the current login accounts (possibly
// none) — the same defensive-copy convention CategoryPaths already uses,
// so a caller mutating the result never touches the live config by
// accident.
func (c *Config) AuthSettings() AuthSettings {
	return AuthSettings{Users: append([]UserAccount(nil), c.Auth.Users...)}
}

// AddUser appends a login account. The very first account ever added
// becomes the protected Default admin regardless of the requested role —
// an instance with login enabled always has at least one guaranteed admin.
// Every account after that gets the requested role (RoleAdmin or
// RoleMember; anything else, including "", becomes RoleMember — the safer
// default for an account that isn't the instance owner).
func (c *Config) AddUser(username, passwordHash, role string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username must not be empty")
	}
	if c.Auth.Find(username) != nil {
		return fmt.Errorf("a user named %q already exists", username)
	}
	isFirst := len(c.Auth.Users) == 0
	switch {
	case isFirst:
		role = RoleAdmin
	case role != RoleAdmin:
		role = RoleMember
	}
	c.Auth.Users = append(c.Auth.Users, UserAccount{
		Username:     username,
		PasswordHash: passwordHash,
		Role:         role,
		Default:      isFirst,
	})
	return nil
}

// RemoveUser deletes a login account. The Default account is refused — an
// instance with login enabled must always keep at least one admin; use
// SetDefaultUser to promote a replacement first.
func (c *Config) RemoveUser(username string) error {
	for i := range c.Auth.Users {
		if !strings.EqualFold(c.Auth.Users[i].Username, username) {
			continue
		}
		if c.Auth.Users[i].Default {
			return fmt.Errorf("can't remove the default account — promote another user first")
		}
		c.Auth.Users = append(c.Auth.Users[:i], c.Auth.Users[i+1:]...)
		return nil
	}
	return fmt.Errorf("no user named %q", username)
}

// SetUserPassword changes one account's stored password hash.
func (c *Config) SetUserPassword(username, passwordHash string) error {
	u := c.Auth.Find(username)
	if u == nil {
		return fmt.Errorf("no user named %q", username)
	}
	u.PasswordHash = passwordHash
	return nil
}

// SetDefaultUser promotes username to the protected default account (and to
// admin in the same step, since the default account is always admin — see
// RemoveUser/SetUserRole for why that guarantee matters). Only one account
// is ever Default at a time.
func (c *Config) SetDefaultUser(username string) error {
	if c.Auth.Find(username) == nil {
		return fmt.Errorf("no user named %q", username)
	}
	for i := range c.Auth.Users {
		c.Auth.Users[i].Default = strings.EqualFold(c.Auth.Users[i].Username, username)
		if c.Auth.Users[i].Default {
			c.Auth.Users[i].Role = RoleAdmin
		}
	}
	return nil
}

// SetUserRole promotes/demotes an account between admin and member. The
// Default account can't be demoted — it's the one account guaranteed to
// stay admin, so login can never lock every admin out at once.
func (c *Config) SetUserRole(username, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return fmt.Errorf("role must be %q or %q", RoleAdmin, RoleMember)
	}
	u := c.Auth.Find(username)
	if u == nil {
		return fmt.Errorf("no user named %q", username)
	}
	if u.Default && role != RoleAdmin {
		return fmt.Errorf("can't demote the default account — promote another user to default first")
	}
	u.Role = role
	return nil
}
