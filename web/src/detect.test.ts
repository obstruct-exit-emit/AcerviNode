import { describe, expect, it } from 'vitest'
import { detectFromFile, detectFromLink, kindLabel } from './detect'

describe('detectFromLink', () => {
  it.each([
    ['magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c', 'torrent', true],
    ['MAGNET:?xt=urn:btih:ABC', 'torrent', true],
    ['https://example.com/release.torrent', 'torrent', true],
    ['https://example.com/path/Some.Release.NZB', 'usenet', true],
    ['https://mega.nz/file/abc#key', 'webdl', false],
    ['https://1fichier.com/?abc123', 'webdl', false],
  ])('%s is %s (certain=%s)', (link, kind, certain) => {
    expect(detectFromLink(link)).toEqual({ kind, certain })
  })

  // The case the whole "shown as an assumption" design exists for: an
  // indexer API URL is indistinguishable from a hoster link, so it must be
  // reported as a guess rather than asserted.
  it('treats an indexer API URL as an uncertain web link', () => {
    const d = detectFromLink('https://indexer.example/api?t=get&id=123&apikey=x')
    expect(d.kind).toBe('webdl')
    expect(d.certain).toBe(false)
  })

  // Extension is taken from the path, never the query: a hoster link can
  // carry ".torrent" in a filename parameter without being one.
  it('ignores an extension that appears only in the query string', () => {
    expect(detectFromLink('https://host.example/dl?file=movie.torrent')).toEqual({
      kind: 'webdl',
      certain: false,
    })
    expect(detectFromLink('https://host.example/get?name=stuff.nzb&id=9')).toEqual({
      kind: 'webdl',
      certain: false,
    })
  })

  it('ignores a fragment after a real extension', () => {
    expect(detectFromLink('https://example.com/a.torrent#frag')).toEqual({
      kind: 'torrent',
      certain: true,
    })
  })

  it('is not confused by an extension appearing mid-path', () => {
    expect(detectFromLink('https://example.com/torrent-files/readme.txt')).toEqual({
      kind: 'webdl',
      certain: false,
    })
  })

  it('handles whitespace and empty input without throwing', () => {
    expect(detectFromLink('   ')).toEqual({ kind: 'webdl', certain: false })
    expect(detectFromLink('  magnet:?xt=urn:btih:x  ')).toEqual({ kind: 'torrent', certain: true })
  })

  // Detection runs on every keystroke, so a half-typed URL must not throw —
  // new URL() rejects plenty of things a user is midway through writing.
  it('survives input that is not a parseable URL yet', () => {
    expect(() => detectFromLink('https:/')).not.toThrow()
    expect(() => detectFromLink('htt')).not.toThrow()
    expect(detectFromLink('example.com/a.nzb')).toEqual({ kind: 'usenet', certain: true })
  })
})

describe('detectFromFile', () => {
  const asFile = (body: string | Uint8Array, name = 'upload.bin') =>
    new File([body], name)

  it('identifies a bencoded torrent by its leading bytes', async () => {
    const torrent = 'd8:announce35:http://tracker.example/announce4:infod4:name4:teste'
    await expect(detectFromFile(asFile(torrent))).resolves.toEqual({
      kind: 'torrent',
      certain: true,
    })
  })

  it('identifies an NZB by its root element', async () => {
    const nzb = '<?xml version="1.0"?>\n<!DOCTYPE nzb>\n<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">'
    await expect(detectFromFile(asFile(nzb))).resolves.toEqual({ kind: 'usenet', certain: true })
  })

  // Content over filename: a browser will happily supply a .torrent name for
  // an NZB, and the extension is the least trustworthy thing about an upload.
  it('trusts content over the filename', async () => {
    const nzb = '<nzb><file/></nzb>'
    await expect(detectFromFile(asFile(nzb, 'actually-an-nzb.torrent'))).resolves.toEqual({
      kind: 'usenet',
      certain: true,
    })
  })

  it('rejects a file that is neither format', async () => {
    await expect(detectFromFile(asFile('just some text'))).resolves.toBeNull()
    await expect(detectFromFile(asFile(new Uint8Array([0x00, 0xff, 0x10])))).resolves.toBeNull()
  })

  // A stray XML file is not an NZB. Matching bare "<?xml" would have made
  // every XML upload look like usenet.
  it('does not treat arbitrary XML as an NZB', async () => {
    await expect(detectFromFile(asFile('<?xml version="1.0"?><rss><channel/></rss>'))).resolves.toBeNull()
  })

  it('does not treat a bencoded non-torrent as a torrent', async () => {
    // Opens with 'd' like a torrent, but carries neither key.
    await expect(detectFromFile(asFile('d3:foo3:bare'))).resolves.toBeNull()
  })

  it('reads binary content without mangling it', async () => {
    // High bytes ahead of the keys would be corrupted by UTF-8 decoding,
    // which is why the reader works byte-wise.
    const bytes = new Uint8Array([0x64, 0x38, 0x3a, 0x61, 0x6e, 0x6e, 0x6f, 0x75, 0x6e, 0x63, 0x65, 0xff, 0xfe])
    await expect(detectFromFile(new File([bytes], 'x.torrent'))).resolves.toEqual({
      kind: 'torrent',
      certain: true,
    })
  })
})

describe('kindLabel', () => {
  it('matches what the old tabs said', () => {
    expect(kindLabel('torrent')).toBe('Torrent')
    expect(kindLabel('usenet')).toBe('Usenet')
    expect(kindLabel('webdl')).toBe('Web Link')
  })
})
