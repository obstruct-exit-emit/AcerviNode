package config

import "testing"

func TestAddUser_FirstUserIsDefaultAdminRegardlessOfRequestedRole(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleMember); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	u := c.Auth.Find("alice")
	if u == nil {
		t.Fatal("alice not found after AddUser")
	}
	if !u.Default {
		t.Error("first user should be Default")
	}
	if u.EffectiveRole() != RoleAdmin {
		t.Errorf("first user role = %q, want admin regardless of requested role", u.EffectiveRole())
	}
}

func TestAddUser_SecondUserGetsRequestedRole(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", RoleMember); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	bob := c.Auth.Find("bob")
	if bob == nil || bob.EffectiveRole() != RoleMember || bob.Default {
		t.Errorf("bob = %+v, want role=member default=false", bob)
	}
}

func TestAddUser_InvalidRoleBecomesMember(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", "not-a-real-role"); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	if got := c.Auth.Find("bob").EffectiveRole(); got != RoleMember {
		t.Errorf("bob role = %q, want member (safer default for a garbage role value)", got)
	}
}

func TestAddUser_RejectsDuplicateUsername(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("first AddUser() error = %v", err)
	}
	if err := c.AddUser("alice", "hash2", RoleAdmin); err == nil {
		t.Error("expected error adding a duplicate username")
	}
	if err := c.AddUser("Alice", "hash2", RoleAdmin); err == nil {
		t.Error("expected error adding a duplicate username case-insensitively")
	}
}

func TestAddUser_RejectsEmptyUsername(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("  ", "hash1", RoleAdmin); err == nil {
		t.Error("expected error for a blank username")
	}
}

func TestRemoveUser_RefusesDefaultAccount(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	if err := c.RemoveUser("alice"); err == nil {
		t.Error("expected error removing the default account")
	}
	if c.Auth.Find("alice") == nil {
		t.Error("default account should still be present")
	}
}

func TestRemoveUser_RemovesNonDefaultAccount(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", RoleMember); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	if err := c.RemoveUser("bob"); err != nil {
		t.Fatalf("RemoveUser(bob) error = %v", err)
	}
	if c.Auth.Find("bob") != nil {
		t.Error("bob should be removed")
	}
	if c.Auth.Find("alice") == nil {
		t.Error("alice should be untouched")
	}
}

func TestRemoveUser_UnknownUsername(t *testing.T) {
	c := &Config{}
	if err := c.RemoveUser("nobody"); err == nil {
		t.Error("expected error removing an unknown user")
	}
}

func TestSetUserPassword(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "old-hash", RoleAdmin); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	if err := c.SetUserPassword("alice", "new-hash"); err != nil {
		t.Fatalf("SetUserPassword() error = %v", err)
	}
	if got := c.Auth.Find("alice").PasswordHash; got != "new-hash" {
		t.Errorf("PasswordHash = %q, want new-hash", got)
	}
}

func TestSetDefaultUser_PromotesAndDemotesCorrectly(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", RoleMember); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	if err := c.SetDefaultUser("bob"); err != nil {
		t.Fatalf("SetDefaultUser(bob) error = %v", err)
	}
	if !c.Auth.Find("bob").Default || c.Auth.Find("bob").EffectiveRole() != RoleAdmin {
		t.Errorf("bob = %+v, want default=true role=admin", c.Auth.Find("bob"))
	}
	if c.Auth.Find("alice").Default {
		t.Error("alice should no longer be default")
	}
}

func TestSetUserRole_RefusesToDemoteDefaultAccount(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	if err := c.SetUserRole("alice", RoleMember); err == nil {
		t.Error("expected error demoting the default account")
	}
	if got := c.Auth.Find("alice").EffectiveRole(); got != RoleAdmin {
		t.Errorf("alice role = %q, want still admin", got)
	}
}

func TestSetUserRole_PromotesAndDemotesNonDefaultAccount(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", RoleMember); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	if err := c.SetUserRole("bob", RoleAdmin); err != nil {
		t.Fatalf("SetUserRole(bob, admin) error = %v", err)
	}
	if got := c.Auth.Find("bob").EffectiveRole(); got != RoleAdmin {
		t.Errorf("bob role = %q, want admin", got)
	}
	if err := c.SetUserRole("bob", RoleMember); err != nil {
		t.Fatalf("SetUserRole(bob, member) error = %v", err)
	}
	if got := c.Auth.Find("bob").EffectiveRole(); got != RoleMember {
		t.Errorf("bob role = %q, want member", got)
	}
}

func TestSetUserRole_RejectsInvalidRole(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser(alice) error = %v", err)
	}
	if err := c.AddUser("bob", "hash2", RoleMember); err != nil {
		t.Fatalf("AddUser(bob) error = %v", err)
	}
	if err := c.SetUserRole("bob", "not-a-role"); err == nil {
		t.Error("expected error for an invalid role")
	}
}

func TestAuthSettings_EnabledReflectsUserCount(t *testing.T) {
	c := &Config{}
	if c.AuthSettings().Enabled() {
		t.Error("Enabled() = true with no users, want false")
	}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	if !c.AuthSettings().Enabled() {
		t.Error("Enabled() = false with a user present, want true")
	}
}

// TestAuthSettings_ReturnsDefensiveCopy proves mutating the returned
// AuthSettings' Users slice never touches the live config — the same
// defensive-copy guarantee CategoryPaths already gives callers.
func TestAuthSettings_ReturnsDefensiveCopy(t *testing.T) {
	c := &Config{}
	if err := c.AddUser("alice", "hash1", RoleAdmin); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	got := c.AuthSettings()
	got.Users[0].Username = "mutated"
	if c.Auth.Users[0].Username != "alice" {
		t.Error("mutating AuthSettings() result affected the live config")
	}
}

func TestUserAccount_EffectiveRole(t *testing.T) {
	admin := UserAccount{Role: ""}
	if admin.EffectiveRole() != RoleAdmin {
		t.Errorf("empty Role EffectiveRole() = %q, want admin", admin.EffectiveRole())
	}
	member := UserAccount{Role: RoleMember}
	if member.EffectiveRole() != RoleMember {
		t.Errorf("member EffectiveRole() = %q, want member", member.EffectiveRole())
	}
}
