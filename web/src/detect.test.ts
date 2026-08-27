import { describe, expect, it } from 'vitest'
import {
  batchSummary,
  isListFilename,
  mergeBatches,
  batchText,
  detectField,
  detectFromFile,
  detectFromLink,
  kindLabel,
  kindPlural,
  noDecode,
  sanitizeBatch,
  stepDecode,
  stepSanitize,
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
      type: 'file',
      kind: 'torrent',
    })
  })

  it('identifies an NZB by its root element', async () => {
    const nzb = '<?xml version="1.0"?>\n<!DOCTYPE nzb>\n<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">'
    await expect(detectFromFile(asFile(nzb))).resolves.toEqual({ type: 'file', kind: 'usenet' })
  })

  // Content over filename: a browser will happily supply a .torrent name for
  // an NZB, and the extension is the least trustworthy thing about an upload.
  it('trusts content over the filename', async () => {
    const nzb = '<nzb><file/></nzb>'
    await expect(detectFromFile(asFile(nzb, 'actually-an-nzb.torrent'))).resolves.toEqual({
      type: 'file',
      kind: 'usenet',
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
      type: 'file',
      kind: 'torrent',
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

describe('sanitizeBatch', () => {
  const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)))
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
  const MAGNET2 = 'magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'
  const NZB = 'https://indexer.example/get/Some.Release.nzb'
  const WEB = 'https://mega.nz/file/abc123'

  // The whole point of splitting on whitespace: prose is not matched against
  // a blocklist, it simply fails to be a link and falls away.
  it('pulls links out of surrounding prose', () => {
    const text = `Here you go mate:\n\n${MAGNET}\n\nand the second one ${MAGNET2} enjoy!`
    expect(sanitizeBatch(text).map((i) => i.link)).toEqual([MAGNET, MAGNET2])
  })

  it('handles a mixed batch, each item keeping its own kind', () => {
    const items = sanitizeBatch([MAGNET, NZB, WEB].join('\n'))
    expect(items.map((i) => i.kind)).toEqual(['torrent', 'usenet', 'webdl'])
  })

  it.each([
    ['- ', ''],
    ['* ', ''],
    ['1. ', ''],
    ['2) ', ''],
    ['> ', ''],
    ['<', '>'],
    ['"', '"'],
    ['', ','],
    ['', ';'],
  ])('strips list decoration %s...%s', (before, after) => {
    const text = `${before}${MAGNET}${after}\n${MAGNET2}`
    expect(sanitizeBatch(text).map((i) => i.link)).toEqual([MAGNET, MAGNET2])
  })

  it('takes the URL out of a markdown link', () => {
    const text = `[Sintel](${MAGNET})\n[Other](${WEB})`
    expect(sanitizeBatch(text).map((i) => i.link)).toEqual([MAGNET, WEB])
  })

  // Each line decodes to its own depth: this is what "acting as it does now,
  // but for more than one" has to mean.
  it('decodes each item independently to its own depth', () => {
    let five = MAGNET
    for (let i = 0; i < 5; i++) five = b64(five)
    const items = sanitizeBatch([five, MAGNET2, b64(WEB)].join('\n'))
    expect(items.map((i) => i.link)).toEqual([MAGNET, MAGNET2, WEB])
    expect(items.map((i) => i.layers)).toEqual([5, 0, 1])
  })

  it('dedupes, first occurrence winning', () => {
    expect(sanitizeBatch([MAGNET, MAGNET2, MAGNET].join('\n')).map((i) => i.link)).toEqual([
      MAGNET,
      MAGNET2,
    ])
  })

  it('dedupes case-insensitively', () => {
    expect(sanitizeBatch([MAGNET, MAGNET.toUpperCase()].join('\n'))).toHaveLength(1)
  })

  it('drops everything that is not a link', () => {
    expect(sanitizeBatch('just some words, nothing here at all')).toEqual([])
    expect(sanitizeBatch('')).toEqual([])
    expect(sanitizeBatch('   \n  ')).toEqual([])
  })

  // The failure that started all this: text copied out of a truncated
  // terminal line is not valid base64 and must be dropped, not half-decoded.
  it('drops a truncated base64 item without disturbing the rest', () => {
    const truncated = b64(b64(MAGNET)).slice(23)
    expect(sanitizeBatch(`${MAGNET2}\n${truncated}`).map((i) => i.link)).toEqual([MAGNET2])
  })

  // A bare infohash is itself valid base64. It must survive a batch intact,
  // exactly as it does on the single path.
  it('keeps a bare infohash intact inside a batch', () => {
    const hash = 'dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'
    const items = sanitizeBatch(`${hash}\n${WEB}`)
    expect(items.map((i) => i.link)).toEqual([hash, WEB])
    expect(items[0].kind).toBe('torrent')
  })

  it('expands one blob that decodes to a whole list', () => {
    expect(sanitizeBatch(b64([MAGNET, NZB, WEB].join('\n'))).map((i) => i.link)).toEqual([
      MAGNET,
      NZB,
      WEB,
    ])
  })

  it('expands a list nested several layers deep', () => {
    let enc = [MAGNET, MAGNET2].join('\n')
    for (let i = 0; i < 3; i++) enc = b64(enc)
    expect(sanitizeBatch(enc).map((i) => i.link)).toEqual([MAGNET, MAGNET2])
  })

  it('caps a runaway paste', () => {
    const many = Array.from(
      { length: 250 },
      (_, i) => `magnet:?xt=urn:btih:${i.toString(16).padStart(40, '0')}`,
    ).join('\n')
    expect(sanitizeBatch(many)).toHaveLength(100)
  })
})

describe('detectField', () => {
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
  const WEB = 'https://mega.nz/file/abc123'

  it('reports a single link the way it always did', () => {
    expect(detectField(MAGNET)).toEqual({ batch: false, kind: 'torrent', certain: true })
  })

  // New behaviour on the single path, and deliberate: a lone link pasted with
  // a release title around it used to go to the server verbatim and fail.
  it('cleans a lone link pasted with a title around it', () => {
    expect(detectField(`Cool.Movie.2024.1080p ${MAGNET}`)).toEqual({
      batch: false,
      kind: 'torrent',
      certain: true,
    })
  })

  it('reports text with no links as an assumed web link, not a batch', () => {
    expect(detectField('hello there')).toEqual({ batch: false, kind: 'webdl', certain: false })
  })

  it('becomes a batch at two links', () => {
    const field = detectField([MAGNET, WEB].join('\n'))
    expect(field.batch).toBe(true)
    if (field.batch) expect(field.items).toHaveLength(2)
  })
})

describe('batchSummary / batchText / kindPlural', () => {
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
  const MAGNET2 = 'magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'
  const NZB = 'https://indexer.example/get/Some.Release.nzb'
  const WEB = 'https://mega.nz/file/abc123'

  it('counts per kind in chip order, skipping absent kinds', () => {
    const items = sanitizeBatch([MAGNET, MAGNET2, NZB].join('\n'))
    expect(batchSummary(items)).toEqual([
      { kind: 'torrent', count: 2 },
      { kind: 'usenet', count: 1 },
    ])
  })

  it('renders one link per line, in order', () => {
    const items = sanitizeBatch([WEB, MAGNET].join('\n'))
    expect(batchText(items)).toBe(`${WEB}\n${MAGNET}`)
  })

  it('pluralises for the summary, leaving usenet uncountable', () => {
    expect(kindPlural('torrent', 1)).toBe('torrent')
    expect(kindPlural('torrent', 3)).toBe('torrents')
    expect(kindPlural('webdl', 1)).toBe('web link')
    expect(kindPlural('webdl', 2)).toBe('web links')
    expect(kindPlural('usenet', 1)).toBe('usenet')
    expect(kindPlural('usenet', 4)).toBe('usenet')
  })
})

describe('stepSanitize', () => {
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
  const WEB = 'https://mega.nz/file/abc123'
  const MESSY = `grab these:\n- ${MAGNET}\n- ${WEB}\ncheers`
  const CLEAN = `${MAGNET}\n${WEB}`

  it('cleans a pasted list and records the original for undo', () => {
    const step = stepSanitize(MESSY, noDecode)
    expect(step.link).toBe(CLEAN)
    expect(step.state.from).toBe(MESSY)
    expect(step.state.to).toBe(CLEAN)
    expect(step.state.items).toBe(2)
  })

  // The cleaned value re-enters the effect as a normal change, where the
  // single-path decoder must leave it alone rather than fight it.
  it('leaves its own output alone on the pass that follows', () => {
    const cleaned = stepSanitize(MESSY, noDecode)
    const next = stepDecode(cleaned.link, cleaned.state)
    expect(next.link).toBe(CLEAN)
    expect(next.state).toBe(cleaned.state)
  })

  it('undo restores the messy paste and it stays restored', () => {
    const cleaned = stepSanitize(MESSY, noDecode)
    const undone = undoDecode(cleaned.state)
    expect(undone.link).toBe(MESSY)
    // Pinned, so pasting logic run again over the restored text is inert.
    expect(stepSanitize(undone.link, undone.state).link).toBe(MESSY)
    expect(stepDecode(undone.link, undone.state).link).toBe(MESSY)
  })

  it('leaves text holding no links alone', () => {
    const step = stepSanitize('hello there', noDecode)
    expect(step.link).toBe('hello there')
    expect(step.state.from).toBeNull()
  })

  it('is inert when the text is already clean', () => {
    const step = stepSanitize(CLEAN, noDecode)
    expect(step.link).toBe(CLEAN)
    expect(step.state).toBe(noDecode)
  })
})

describe('base32 infohashes', () => {
  // Real pairs: the base32 and hex spellings of the same three torrents.
  const PAIRS: [string, string][] = [
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQQ', '08ada5a7a6183aae1e09d831df6748d566095a10'],
    ['ZHQVOY7XELZD5GFCTXWN7LRUDOMNKMCW', 'c9e15763f722f23e98a29decdfae341b98d53056'],
    ['3WBFL3G4PSSV7MF37AJSHWDQMLNR63I4', 'dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'],
  ]

  it.each(PAIRS)('detects %s as a torrent', (base32) => {
    expect(detectFromLink(base32)).toEqual({ kind: 'torrent', certain: true })
  })

  // 32 characters, a multiple of four, and entirely within base64's alphabet
  // — so without the recognisable-first rule this would be fed to atob and
  // mangled into binary. Exactly the trap the hex spelling already sets.
  it.each(PAIRS)('is never decoded as base64 (%s)', (base32) => {
    expect(unwrapEncoded(base32)).toEqual({ value: base32, layers: 0 })
  })

  it('survives a batch alongside other kinds', () => {
    const text = ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQQ', 'https://mega.nz/file/abc#k'].join('\n')
    const items = sanitizeBatch(text)
    expect(items.map((i) => i.link)).toEqual([
      'BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQQ',
      'https://mega.nz/file/abc#k',
    ])
    expect(items[0].kind).toBe('torrent')
  })

  // Uppercase only. 32 mixed-case alphanumerics is the shape of every API key
  // going, and claiming those are torrents is worse than missing a hash.
  it.each([
    ['bcw2lj5gda5k4hqj3ay56z2i2vtaswqq', 'lowercase'],
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQ', '31 characters'],
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQQA', '33 characters'],
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASW01', 'contains 0 and 1, not base32'],
  ])('does not treat %s as an infohash (%s)', (notHash) => {
    expect(detectFromLink(notHash).kind).not.toBe('torrent')
  })
})

describe('percent-encoded links', () => {
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056&dn=Big+Buck'
  const WEB = 'https://mega.nz/file/abc123#somekey'

  it('unwraps a link copied out of a redirect URL', () => {
    expect(unwrapEncoded(encodeURIComponent(WEB))).toEqual({ value: WEB, layers: 1 })
  })

  it('unwraps a doubly-encoded link', () => {
    const twice = encodeURIComponent(encodeURIComponent(WEB))
    expect(unwrapEncoded(twice)).toEqual({ value: WEB, layers: 2 })
  })

  // A magnet's dn= uses "+" for spaces, and decodeURIComponent leaves "+"
  // alone. Turning it into a space would corrupt the display name.
  it('preserves + in a decoded magnet', () => {
    expect(unwrapEncoded(encodeURIComponent(MAGNET))).toEqual({ value: MAGNET, layers: 1 })
  })

  // The rule that makes percent-decoding safe at all: anything already usable
  // is returned before the decoder is consulted, so a legitimate %20 in a
  // filename is never turned into a raw space.
  it('leaves a normal URL containing a real escape alone', () => {
    const withSpace = 'https://host.example/My%20File%20(2024).zip'
    expect(unwrapEncoded(withSpace)).toEqual({ value: withSpace, layers: 0 })
    expect(detectFromLink(withSpace)).toEqual({ kind: 'webdl', certain: false })
  })

  it('mixes with base64 in one batch, each item to its own depth', () => {
    const b64 = (v: string) => btoa(String.fromCharCode(...new TextEncoder().encode(v)))
    const text = [encodeURIComponent(WEB), b64(MAGNET), WEB.replace('abc123', 'plain')].join('\n')
    const items = sanitizeBatch(text)
    expect(items.map((i) => i.link)).toEqual([WEB, MAGNET, WEB.replace('abc123', 'plain')])
    expect(items.map((i) => i.layers)).toEqual([1, 1, 0])
  })

  // The rule that makes percent-decoding safe, pinned properly. %2F is an
  // ENCODED slash and is not the same character as a path separator, so
  // decoding it produces a different URL that still looks perfectly valid —
  // no whitespace, right scheme — and would therefore be accepted. Only
  // returning early on already-usable input prevents that. The %20 case above
  // does not catch this: a raw space makes the result unrecognisable, so the
  // landing rule rejects it anyway.
  it('does not decode escapes inside an already-usable URL', () => {
    const encodedSlash = 'https://host.example/get?path=folder%2Ffile.zip'
    expect(unwrapEncoded(encodedSlash)).toEqual({ value: encodedSlash, layers: 0 })
    const encodedQuestion = 'https://host.example/go?next=a%3Fb%3Dc'
    expect(unwrapEncoded(encodedQuestion)).toEqual({ value: encodedQuestion, layers: 0 })
  })

  it('leaves a stray percent sign alone', () => {
    for (const s of ['100% free', 'https://host.example/50%off']) {
      expect(unwrapEncoded(s).layers).toBe(0)
    }
  })
})

describe('link-list files', () => {
  const asFile = (body: BlobPart, name = 'list.txt') => new File([body], name)
  const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)))
  const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
  const NZB = 'https://indexer.example/getnzb/Some.Release.nzb'
  const WEB = 'https://mega.nz/file/abc123#key'

  it('reads a text file of links', async () => {
    const content = await detectFromFile(asFile([MAGNET, WEB].join('\n')))
    expect(content).toEqual({
      type: 'links',
      items: [
        { link: MAGNET, kind: 'torrent', certain: true, layers: 0 },
        { link: WEB, kind: 'webdl', certain: false, layers: 0 },
      ],
    })
  })

  // The case this exists for. Detection is by content, so a file with no
  // extension at all is read exactly like a .txt.
  it('reads a file with no extension at all', async () => {
    const content = await detectFromFile(asFile(MAGNET + '\n' + WEB, 'links'))
    expect(content?.type).toBe('links')
    if (content?.type === 'links') expect(content.items).toHaveLength(2)
  })

  // A file is just a bigger clipboard: the same extraction a paste gets, so
  // prose and list decoration fall away the same way.
  it('pulls links out of a file full of prose', async () => {
    const messy = `Saved from the forum:\n\n1. ${MAGNET}\n  * <${WEB}>\n\nthat's the lot`
    const content = await detectFromFile(asFile(messy))
    expect(content?.type).toBe('links')
    if (content?.type === 'links') {
      expect(content.items.map((i) => i.link)).toEqual([MAGNET, WEB])
    }
  })

  it('keeps each kind in a mixed list', async () => {
    const content = await detectFromFile(asFile([MAGNET, NZB, WEB].join('\n')))
    expect(content?.type).toBe('links')
    if (content?.type === 'links') {
      expect(content.items.map((i) => i.kind)).toEqual(['torrent', 'usenet', 'webdl'])
    }
  })

  it('decodes encoded entries inside the file', async () => {
    const content = await detectFromFile(
      asFile([b64(b64(MAGNET)), encodeURIComponent(WEB)].join('\n')),
    )
    expect(content?.type).toBe('links')
    if (content?.type === 'links') {
      expect(content.items.map((i) => i.link)).toEqual([MAGNET, WEB])
      expect(content.items.map((i) => i.layers)).toEqual([2, 1])
    }
  })

  it('rejects a text file holding no links', async () => {
    await expect(detectFromFile(asFile('shopping list\nmilk\nbread'))).resolves.toBeNull()
    await expect(detectFromFile(asFile(''))).resolves.toBeNull()
  })

  it('rejects a binary file rather than reading it as text', async () => {
    const bytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01])
    await expect(detectFromFile(new File([bytes], 'image.png'))).resolves.toBeNull()
  })

  // A UTF-8 BOM is what Windows editors have historically written, and
  // TextDecoder strips it — without that the first line would begin with
  // U+FEFF and fail to match anything.
  it('handles a UTF-8 byte order mark', async () => {
    const withBom = new Uint8Array([
      0xef, 0xbb, 0xbf,
      ...new TextEncoder().encode([MAGNET, WEB].join('\n')),
    ])
    const content = await detectFromFile(new File([withBom], 'list.txt'))
    expect(content?.type).toBe('links')
    if (content?.type === 'links') {
      expect(content.items.map((i) => i.link)).toEqual([MAGNET, WEB])
    }
  })

  // UTF-16 is valid UTF-8 as far as the strict decoder is concerned — ASCII
  // interleaved with NULs — so it is the control-character check, not the
  // decoder, that catches it. Rejected rather than silently half-read.
  it('rejects a UTF-16 file rather than reading it as gibberish', async () => {
    const ascii = MAGNET
    const utf16 = new Uint8Array(ascii.length * 2)
    for (let i = 0; i < ascii.length; i++) utf16[i * 2] = ascii.charCodeAt(i)
    await expect(detectFromFile(new File([utf16], 'list.txt'))).resolves.toBeNull()
  })

  // Ordering matters: an NZB is text, so without the <nzb> check running
  // first it would be scanned for links instead of being recognised.
  it('still identifies an NZB, which is itself text', async () => {
    const nzb = `<?xml version="1.0"?>\n<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">\n<!-- see ${WEB} -->\n</nzb>`
    await expect(detectFromFile(asFile(nzb, 'release.nzb'))).resolves.toEqual({
      type: 'file',
      kind: 'usenet',
    })
  })
})

describe('mergeBatches', () => {
  const link = (n: number) => `magnet:?xt=urn:btih:${n.toString(16).padStart(40, '0')}`
  const item = (n: number) => ({ link: link(n), kind: 'torrent' as const, certain: true, layers: 0 })

  it('concatenates in order', () => {
    expect(mergeBatches([[item(1)], [item(2), item(3)]]).map((i) => i.link)).toEqual([
      link(1),
      link(2),
      link(3),
    ])
  })

  // Deduping has to work across files, not just inside one: the same link in
  // two lists is still one download.
  it('dedupes across lists, first occurrence winning', () => {
    expect(mergeBatches([[item(1), item(2)], [item(2), item(3)]]).map((i) => i.link)).toEqual([
      link(1),
      link(2),
      link(3),
    ])
  })

  // Uploading five list files must not be a way around the cap one paste gets.
  it('applies the same cap across all lists together', () => {
    const lists = [
      Array.from({ length: 60 }, (_, i) => item(i)),
      Array.from({ length: 60 }, (_, i) => item(i + 60)),
    ]
    expect(mergeBatches(lists)).toHaveLength(100)
  })

  it('handles empty input', () => {
    expect(mergeBatches([])).toEqual([])
    expect(mergeBatches([[], []])).toEqual([])
  })
})

describe('isListFilename', () => {
  // The "only see what we want" rule for batch-file mode. Kept small on
  // purpose: a batch file is opened and its contents queued, so this must not
  // grow to cover anything that merely might contain a link.
  it.each([
    ['links.txt', true],
    ['links.TXT', true],
    ['my grabs.txt', true],
    ['links', true],
    ['.links', true],
    ['a.torrent', false],
    ['a.nzb', false],
    ['a.csv', false],
    ['a.html', false],
    ['a.crawljob', false],
  ])('%s is readable: %s', (name, want) => {
    expect(isListFilename(name)).toBe(want)
  })

  // The extension is the last dot, not the first, or "my.txt.torrent" would
  // sneak past as a text file.
  it('reads the extension from the last dot', () => {
    expect(isListFilename('my.list.txt')).toBe(true)
    expect(isListFilename('my.txt.torrent')).toBe(false)
  })

  it('ignores any directory portion', () => {
    expect(isListFilename('some.dir/links.txt')).toBe(true)
    expect(isListFilename('some.dir/links')).toBe(true)
  })
})
