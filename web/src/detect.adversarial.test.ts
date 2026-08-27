// Input constructed to exploit the shape of the parser rather than to
// represent anything a person would paste.
//
// Most of these are expected to pass. The ones that do not tend to be real,
// because nothing here is arbitrary — each case targets a specific decision
// in detect.ts and asks what happens when that decision is wrong.

import { describe, expect, it } from 'vitest'
import { detectFromFile, detectFromLink, sanitizeBatch, unwrapEncoded } from './detect'

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)))
const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
const MAGNET2 = 'magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c'
const WEB = 'https://host.example/file.zip'
const LF = String.fromCharCode(10)
const CR = String.fromCharCode(13)

describe('A01 line endings', () => {
  // Windows editors write CRLF. A stray carriage return riding along on the
  // end of a link would be invisible in the field and fatal at the provider.
  it('treats CRLF exactly like LF', () => {
    const crlf = [MAGNET, WEB, MAGNET2].join(CR + LF)
    const lf = [MAGNET, WEB, MAGNET2].join(LF)
    expect(sanitizeBatch(crlf).map((i) => i.link)).toEqual(sanitizeBatch(lf).map((i) => i.link))
  })

  it('leaves no carriage return on any link', () => {
    for (const item of sanitizeBatch([MAGNET, WEB].join(CR + LF))) {
      expect(item.link).not.toContain(CR)
    }
  })

  it('handles a lone CR, as an old Mac editor would write', () => {
    expect(sanitizeBatch([MAGNET, WEB].join(CR))).toHaveLength(2)
  })
})

describe('A02 missing trailing newline', () => {
  it('still parses the last line', async () => {
    const got = await detectFromFile(new File([[MAGNET, WEB].join(LF)], 'list.txt'))
    expect(got?.type).toBe('links')
    if (got?.type === 'links') expect(got.items).toHaveLength(2)
  })
})

describe('A03/A04 files that are two things at once', () => {
  it('reads a bencoded torrent as a torrent even when it contains links', async () => {
    const torrent = 'd8:announce31:https://tracker.example/announce4:infod4:name4:teste'
    await expect(detectFromFile(new File([torrent], 'x.torrent'))).resolves.toEqual({
      type: 'file',
      kind: 'torrent',
    })
  })

  it('does not read a base64-encoded torrent file as a link list', async () => {
    const torrent = 'd8:announce31:https://tracker.example/announce4:infod4:name4:teste'
    const got = await detectFromFile(new File([b64(torrent)], 'list.txt'))
    // It decodes to bencode, not to links — nothing recognisable lands.
    expect(got).toBeNull()
  })
})

describe('A05 very large single token', () => {
  it('keeps a magnet with 300 trackers intact', () => {
    const trackers = Array.from(
      { length: 300 },
      (_, i) => `&tr=${encodeURIComponent(`udp://tracker${i}.example:6969/announce`)}`,
    ).join('')
    const huge = MAGNET + trackers
    expect(huge.length).toBeGreaterThan(10_000)
    const got = sanitizeBatch(huge)
    expect(got).toHaveLength(1)
    expect(got[0].link).toBe(huge)
  })
})

describe('A06 nested wrappers', () => {
  it.each([
    [`<${WEB}>`, WEB],
    [`(${WEB})`, WEB],
    [`"${WEB}"`, WEB],
    [`<(${WEB})>`, WEB],
    [`("${WEB}")`, WEB],
    [`((${WEB}))`, WEB],
  ])('unwraps %s', (wrapped, want) => {
    expect(sanitizeBatch(wrapped).map((i) => i.link)).toEqual([want])
  })

  it('never produces a partially unwrapped link', () => {
    for (const wrapped of [`<${WEB}`, `${WEB}>`, `(${WEB}`, `"${WEB}`]) {
      const got = sanitizeBatch(wrapped)
      // Either the clean link or nothing — never a link with a stray bracket.
      for (const item of got) {
        expect(item.link).not.toMatch(/^[<("']/)
        expect(item.link).not.toMatch(/[>)"']$/)
      }
    }
  })
})

describe('A07 invisible and directional characters', () => {
  const ZWSP = String.fromCharCode(0x200b)
  const RLO = String.fromCharCode(0x202e)

  it('never lets an invisible character corrupt the infohash', () => {
    for (const sneaky of [ZWSP, RLO]) {
      const link = `${MAGNET}&dn=Name${sneaky}Here`
      const got = sanitizeBatch(link)
      if (got.length > 0) {
        // Whatever happens to the display name, the hash must survive.
        expect(got[0].link).toContain('c9e15763f722f23e98a29decdfae341b98d53056')
      }
    }
  })

  it('does not split a token on a zero-width space', () => {
    // ZWSP is not whitespace for split purposes; treating it as such would
    // silently break one link into two useless halves.
    const got = sanitizeBatch(`${MAGNET}${ZWSP}`)
    expect(got.length).toBeLessThanOrEqual(1)
  })
})

describe('A09/A10 stacked and degenerate encodings', () => {
  it('unwraps percent over base64 over percent', () => {
    const stacked = encodeURIComponent(b64(encodeURIComponent(WEB)))
    expect(unwrapEncoded(stacked).value).toBe(WEB)
  })

  it('terminates on a value that encodes to itself', () => {
    // No string is its own base64, but the guard exists; prove the loop is
    // bounded rather than trusting that.
    const started = Date.now()
    unwrapEncoded('='.repeat(400))
    unwrapEncoded('A'.repeat(400))
    expect(Date.now() - started).toBeLessThan(1000)
  })
})

describe('A11 schemes that are not ours', () => {
  it.each([
    'javascript:alert(1)',
    'data:text/html;base64,PHNjcmlwdD4=',
    'file:///etc/passwd',
    'ftp://host.example/file.zip',
    'ws://host.example/socket',
    '//host.example/protocol-relative',
  ])('drops %s from a batch', (bad) => {
    const got = sanitizeBatch([bad, WEB].join(LF))
    expect(got.map((i) => i.link)).toEqual([WEB])
  })

  it('does not treat a non-http scheme as a single web link either', () => {
    expect(detectFromLink('javascript:alert(1)').certain).toBe(false)
  })
})

describe('A12/A13 things that look like hashes but are not', () => {
  it('reads a base32 TOTP secret as text by default', () => {
    // The whole reason the switch exists and defaults off: this shape is
    // exactly an infohash's, so accepting it unconditionally turned a
    // pasted secret into an add attempt.
    expect(detectFromLink('JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP').kind).not.toBe('torrent')
  })

  it('treats a base32 TOTP secret as a torrent once enabled — the trade', () => {
    // 32 uppercase base32 characters is exactly an infohash's shape. This is
    // a documented false positive: it can only fire when such a string is
    // pasted alone or on its own line, and the add then fails at the
    // provider rather than fetching anything wrong.
    expect(
      detectFromLink('JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP', { base32Infohashes: true }).kind,
    ).toBe('torrent')
  })

  it('does not treat a 32-character MD5 as a hash of any kind', () => {
    // Lowercase hex is not the base32 alphabet, and 32 is neither 40 nor 64.
    expect(detectFromLink('9e107d9d372bb6826bd81d3542a419d6').kind).toBe('webdl')
  })

  it('an uppercase MD5 without 0/1/8/9 collides with base32, by design', () => {
    // ABCDEF-only uppercase hex is a legal base32 string. Nothing can tell
    // them apart; the shape is genuinely ambiguous. Pinned so a change here
    // is a decision rather than a surprise.
    expect(
      detectFromLink('ABCDEFABCDEFABCDEFABCDEFABCDEFAB', { base32Infohashes: true }).kind,
    ).toBe('torrent')
  })
})

describe('A14 same torrent, different display name', () => {
  // Dedupe is by the whole link string, so these are two items. The provider
  // collapses them to one download, but the count shown before submitting is
  // higher than the number of rows that result.
  it('counts one infohash once, whatever the display names say', () => {
    const got = sanitizeBatch([`${MAGNET}&dn=A`, `${MAGNET}&dn=B`].join(LF))
    expect(got).toHaveLength(1)
    expect(got[0].link).toBe(`${MAGNET}&dn=A`)
  })

  it('collapses a bare hash and a magnet for the same torrent', () => {
    const bare = 'c9e15763f722f23e98a29decdfae341b98d53056'
    expect(sanitizeBatch([bare, MAGNET].join(LF))).toHaveLength(1)
  })

  it('does dedupe when the links are identical but differently cased', () => {
    expect(sanitizeBatch([MAGNET, MAGNET.toUpperCase()].join(LF))).toHaveLength(1)
  })
})

describe('A15/A16 pathological but plausible text', () => {
  it('handles a megabyte of prose with no links quickly', () => {
    const prose = ('the quick brown fox jumps over the lazy dog ' as string).repeat(24_000)
    expect(prose.length).toBeGreaterThan(1_000_000)
    const started = Date.now()
    expect(sanitizeBatch(prose)).toEqual([])
    expect(Date.now() - started).toBeLessThan(3000)
  })

  it('leaves a literal percent sign that is not an escape alone', () => {
    for (const link of ['https://host.example/50%off', 'https://host.example/100%']) {
      expect(sanitizeBatch(link).map((i) => i.link)).toEqual([link])
    }
  })

  it('survives a line of nothing but separators', () => {
    expect(() => sanitizeBatch(',,,;;;...' + LF + MAGNET)).not.toThrow()
    expect(sanitizeBatch(',,,;;;...' + LF + MAGNET).map((i) => i.link)).toEqual([MAGNET])
  })
})
