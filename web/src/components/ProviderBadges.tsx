import type { ProviderStatus } from '../api'

export function ProviderBadges({ providers }: { providers: ProviderStatus[] }) {
  if (providers.length === 0) {
    return <span className="provider-badge provider-badge-none">No provider configured</span>
  }
  return (
    <>
      {providers.map((p) => (
        <span key={p.name} className="provider-badge" title={`torrents: ${p.torrent_capable}, usenet: ${p.usenet_capable}`}>
          {p.name}
          {p.torrent_capable && <span className="cap">⇩T</span>}
          {p.usenet_capable && <span className="cap">⇩U</span>}
        </span>
      ))}
    </>
  )
}
