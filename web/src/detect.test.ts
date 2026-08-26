import { describe, expect, it } from 'vitest'
import {
  detectFromFile,
  detectFromLink,
  kindLabel,
  noDecode,
  stepDecode,
  undoDecode,
  unwrapEncoded,
} from './detect'

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

  it.each([
    ['08ada5a7a6183aae1e09d831df6748d566095a10', 'a 40-char v1 infohash'],
    ['08ADA5A7A6183AAE1E09D831DF6748D566095A10', 'the same uppercased'],
    ['a'.repeat(64), 'a 64-char v2 infohash'],
  ])('treats %s as a torrent (%s)', (hash) => {
    expect(detectFromLink(hash)).toEqual({ kind: 'torrent', certain: true })
  })

  // Guard the hash rule from being too eager: near-misses must not be
  // mistaken for one, or a short hoster path would become a torrent.
  it.each([
    ['08ada5a7a6183aae1e09d831df6748d566095a1', '39 chars'],
    ['08ada5a7a6183aae1e09d831df6748d566095a10a', '41 chars'],
    ['08ada5a7a6183aae1e09d831df6748d566095g10', 'non-hex character'],
  ])('does not treat %s as an infohash (%s)', (notHash) => {
    expect(detectFromLink(notHash).kind).toBe('webdl')
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
  // BlobPart rather than string | Uint8Array: the latter widens to
  // Uint8Array<ArrayBufferLike>, which TypeScript will not accept as a
  // BlobPart even though it is one at runtime. Caught by `tsc -b` in the
  // build, not by vitest, which does not typecheck.
  const asFile = (body: BlobPart, name = 'upload.bin') => new File([body], name)

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

describe('unwrapEncoded', () => {
  // btoa rather than Buffer: this file is typechecked against the browser
  // tsconfig, which has no Node types. Encoding through TextEncoder first
  // keeps non-ASCII intact, which btoa alone cannot do.
  const b64 = (s: string) =>
    btoa(String.fromCharCode(...new TextEncoder().encode(s)))
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'

  it('leaves something already usable alone', () => {
    for (const clear of [MAGNET, 'https://mega.nz/file/abc', 'a'.repeat(40)]) {
      expect(unwrapEncoded(clear)).toEqual({ value: clear, layers: 0 })
    }
  })

  it('peels a single layer', () => {
    expect(unwrapEncoded(b64(MAGNET))).toEqual({ value: MAGNET, layers: 1 })
  })

  it.each([2, 3, 5, 8])('peels %d nested layers', (n) => {
    let enc = MAGNET
    for (let i = 0; i < n; i++) enc = b64(enc)
    expect(unwrapEncoded(enc)).toEqual({ value: MAGNET, layers: n })
  })

  it('gives up past the depth cap rather than looping', () => {
    let enc = MAGNET
    for (let i = 0; i < 12; i++) enc = b64(enc)
    // Too deep to reach the magnet, so the input is returned untouched
    // rather than half-decoded into something meaningless.
    expect(unwrapEncoded(enc).layers).toBe(0)
  })

  // The reason the "must land somewhere recognisable" rule exists. Forty hex
  // characters is valid base64 and decodes cleanly to binary noise, so a
  // decoder that accepted decodes on their own merit would destroy a hash.
  it('never mangles a bare infohash, which is itself valid base64', () => {
    const hash = 'dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'
    expect(unwrapEncoded(hash)).toEqual({ value: hash, layers: 0 })
  })

  it('leaves ordinary text that is not encoded alone', () => {
    for (const s of ['Big Buck Bunny', 'not base64 at all!', '', '   ']) {
      expect(unwrapEncoded(s).layers).toBe(0)
    }
  })

  // Decoding to plausible-looking but useless text must not be accepted:
  // the result has to be a magnet, hash or URL to be kept.
  it('does not accept a decode that lands on nothing usable', () => {
    const enc = b64('just some words here')
    expect(unwrapEncoded(enc)).toEqual({ value: enc, layers: 0 })
  })

  it('handles base64url as well as standard base64', () => {
    const url = 'https://example.com/a?x=1&y=2'
    const std = b64(url)
    const urlsafe = std.replace(/\+/g, '-').replace(/\//g, '_')
    expect(unwrapEncoded(urlsafe)).toEqual({ value: url, layers: 1 })
  })

  it('preserves non-ASCII characters through the decode', () => {
    const link = 'https://example.com/café-résumé'
    expect(unwrapEncoded(b64(link))).toEqual({ value: link, layers: 1 })
  })

  it('detection then runs on the decoded value', () => {
    const { value } = unwrapEncoded(b64(b64(MAGNET)))
    expect(detectFromLink(value)).toEqual({ kind: 'torrent', certain: true })
  })
})

describe('stepDecode / undoDecode', () => {
  const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)))
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'

  // The effect rewrites the field and thereby re-runs itself, so correctness
  // is only visible across the settle, never in a single call. This drives it
  // the same way React does, to a fixed point.
  const settle = (link: string, state = noDecode, max = 6) => {
    let cur = { link, state }
    for (let i = 0; i < max; i++) {
      const next = stepDecode(cur.link, cur.state)
      if (next.link === cur.link && next.state === cur.state) return cur
      cur = next
    }
    throw new Error('never settled — the effect would loop')
  }

  it('settles rather than looping', () => {
    expect(() => settle(b64(b64(MAGNET)))).not.toThrow()
    expect(() => settle(MAGNET)).not.toThrow()
    expect(() => settle('')).not.toThrow()
  })

  it('leaves an untouched input with no notice', () => {
    const r = settle(MAGNET)
    expect(r.link).toBe(MAGNET)
    expect(r.state.from).toBeNull()
  })

  // Bug 1: the notice cleared itself on the re-render the decode caused,
  // because the field no longer matched the original — which is true by
  // definition after a successful decode.
  it('keeps the notice after the rewrite re-runs the effect', () => {
    const enc = b64(b64(b64(MAGNET)))
    const r = settle(enc)
    expect(r.link).toBe(MAGNET)
    expect(r.state.from).toBe(enc)
    expect(r.state.layers).toBe(3)
  })

  it('retires the notice once the user edits the field', () => {
    const after = settle(b64(MAGNET))
    const edited = settle('https://example.com/other', after.state)
    expect(edited.state.from).toBeNull()
    expect(edited.state.layers).toBe(0)
  })

  // Bug 2: undo restored the pasted text, which the effect immediately
  // decoded straight back — so the button appeared to do nothing at all.
  it('undo restores the original and it stays restored', () => {
    const enc = b64(b64(MAGNET))
    const decoded = settle(enc)
    expect(decoded.link).toBe(MAGNET)

    const undone = undoDecode(decoded.state)
    expect(undone.link).toBe(enc)

    const after = settle(undone.link, undone.state)
    expect(after.link).toBe(enc)
    expect(after.state.from).toBeNull()
  })

  it('decodes again once the user replaces a pinned value', () => {
    const first = b64(MAGNET)
    const undone = undoDecode(settle(first).state)
    const other = b64('https://example.com/x')
    const r = settle(other, undone.state)
    expect(r.link).toBe('https://example.com/x')
    expect(r.state.layers).toBe(1)
  })

  it('does nothing to input that was never encoded', () => {
    const r = settle('Big Buck Bunny')
    expect(r.link).toBe('Big Buck Bunny')
    expect(r.state).toBe(noDecode)
  })

  // The reported failure: text copied out of a truncated terminal line is no
  // longer valid base64, and must be left exactly as pasted rather than
  // half-decoded into something worse.
  it('leaves a truncated paste alone', () => {
    const truncated = b64(b64(MAGNET)).slice(23)
    const r = settle(truncated)
    expect(r.link).toBe(truncated)
    expect(r.state.from).toBeNull()
  })

  it('undo on a state with nothing decoded is inert', () => {
    expect(undoDecode(noDecode).state).toBe(noDecode)
  })
})
