import { useEffect, useState, type FormEvent } from 'react'
import { ApiError, addUser, listUsers, makeDefaultUser, removeUser, setUserPassword, setUserRole, type Role, type UserAccount } from '../api'

// Settings → Security: login accounts on top of the API key, which keeps
// working unaffected by any of this (Sonarr/Radarr and scripts should keep
// using it). Only ever rendered for an admin — the Settings tab itself is
// hidden from a member (see App.tsx's isAdmin), and every endpoint here is
// admin-gated server-side too, so this isn't the only thing enforcing it.
export function SecuritySettings({ apiKey }: { apiKey: string }) {
  const [users, setUsers] = useState<UserAccount[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [newUsername, setNewUsername] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [newRole, setNewRole] = useState<Role>('member')
  const [addStatus, setAddStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  function load() {
    listUsers(apiKey)
      .then((r) => setUsers(r.users))
      .catch((err: unknown) => setLoadError(err instanceof ApiError ? err.message : String(err)))
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function handleAdd(e: FormEvent) {
    e.preventDefault()
    setAddStatus({ kind: 'saving' })
    addUser(apiKey, newUsername.trim(), newPassword, newRole)
      .then((r) => {
        setUsers(r.users)
        setNewUsername('')
        setNewPassword('')
        setNewRole('member')
        setAddStatus({ kind: 'idle' })
      })
      .catch((err: unknown) => setAddStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) }))
  }

  function handleRemove(username: string) {
    if (!confirm(`Remove the account "${username}"?`)) return
    removeUser(apiKey, username)
      .then((r) => setUsers(r.users))
      .catch((err: unknown) => alert(err instanceof Error ? err.message : String(err)))
  }

  function handleRoleChange(username: string, role: Role) {
    setUserRole(apiKey, username, role)
      .then((r) => setUsers(r.users))
      .catch((err: unknown) => alert(err instanceof Error ? err.message : String(err)))
  }

  function handleMakeDefault(username: string) {
    if (!confirm(`Make "${username}" the default account? It becomes the one account that can't be removed or demoted.`)) return
    makeDefaultUser(apiKey, username)
      .then((r) => setUsers(r.users))
      .catch((err: unknown) => alert(err instanceof Error ? err.message : String(err)))
  }

  function handlePasswordReset(username: string) {
    const password = prompt(`New password for "${username}" (min. 8 characters):`)
    if (!password) return
    if (password.length < 8) {
      alert('Password must be at least 8 characters.')
      return
    }
    setUserPassword(apiKey, username, password).catch((err: unknown) => alert(err instanceof Error ? err.message : String(err)))
  }

  return (
    <section className="settings-card">
      <h2>Security</h2>
      <p className="settings-help">
        Login accounts, on top of the API key (which keeps working unaffected by any of this —
        Sonarr/Radarr and scripts should keep using it). <strong>Admin</strong> can do everything;{' '}
        <strong>member</strong> is scoped to the Manual tab only — adding/viewing/managing a
        magnet/NZB/hoster link grabbed directly, never Settings or the *arr-driven Managed
        pipeline.
      </p>

      {loadError && <p className="settings-error">Failed to load users: {loadError}</p>}
      {users && users.length > 0 && (
        <ul className="rows user-rows">
          {users.map((u) => (
            <li key={u.username}>
              <div className="row">
                <span className="user-row-name">
                  {u.username} {u.default && <span className="badge badge-default">default</span>}
                </span>
                <select value={u.role} onChange={(e) => handleRoleChange(u.username, e.target.value as Role)} disabled={u.default}>
                  <option value="admin">admin</option>
                  <option value="member">member</option>
                </select>
                <button type="button" onClick={() => handlePasswordReset(u.username)}>
                  Set password
                </button>
                {!u.default && (
                  <button type="button" onClick={() => handleMakeDefault(u.username)}>
                    Make default
                  </button>
                )}
                {!u.default && (
                  <button type="button" className="delete-btn" onClick={() => handleRemove(u.username)}>
                    Remove
                  </button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      <form className="general-form" onSubmit={handleAdd}>
        <label>
          Username
          <input value={newUsername} onChange={(e) => setNewUsername(e.target.value)} />
        </label>
        <label>
          Password (min. 8 characters)
          <input type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
        </label>
        <label>
          Role
          <select value={newRole} onChange={(e) => setNewRole(e.target.value as Role)}>
            <option value="member">member</option>
            <option value="admin">admin</option>
          </select>
        </label>
        <div className="settings-actions">
          <button type="submit" disabled={addStatus.kind === 'saving' || !newUsername.trim() || newPassword.length < 8}>
            {addStatus.kind === 'saving' ? 'Adding…' : 'Add account'}
          </button>
          {addStatus.kind === 'error' && <span className="settings-error">{addStatus.message}</span>}
        </div>
      </form>
    </section>
  )
}
