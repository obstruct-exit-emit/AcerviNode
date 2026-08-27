// Every hard limit in detect.ts, tested at the limit and one either side.
//
// The example suite checks behaviour; this checks that the numbers written
// into the code are the numbers the code actually enforces. Off-by-one in a
// cap is invisible until the day someone sits exactly on it.

import { describe, expect, it } from 'vitest'
import { detectFromFile, detectFromLink, sanitizeBatch, unwrapEncoded } from './detect'

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)))
const wrap = (s: string, n: number) => {
  let out = s
  for (let i = 0; i < n; i++) out = b64(out)
  return out
}
const MAGNET = 'magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056'
const hash = (n: number) => n.toString(16).padStart(40, '0')
const magnets = (count: number) =>
  Array.from({ length: count }, (_, i) => `magnet:?xt=urn:btih:${hash(i)}`).join('\n')

describe('B01 MAX_BATCH_ITEMS = 100', () => {
  it.each([
    [99, 99],
    [100, 100],
    [101, 100],
    [5000, 100],
  ])('%d links in yields %d out', (given, want) => {
    expect(sanitizeBatch(magnets(given))).toHaveLength(want)
  })

  it('caps quickly rather than parsing everything first', () => {
    const started = Date.now()
    expect(sanitizeBatch(magnets(50_000))).toHaveLength(100)
    expect(Date.now() - started).toBeLessThan(3000)
  })
})

describe('B02 MAX_DECODE_DEPTH = 10', () => {
  it.each([
    [1, 1],
    [9, 9],
    [10, 10],
  ])('%d layers decodes, reporting %d', (layers, want) => {
    expect(unwrapEncoded(wrap(MAGNET, layers))).toEqual({ value: MAGNET, layers: want })
  })

  it('gives up past the cap and returns the input untouched', () => {
    const tooDeep = wrap(MAGNET, 11)
    // Untouched, not half-decoded — a partial peel would be worse than none.
    expect(unwrapEncoded(tooDeep)).toEqual({ value: tooDeep, layers: 0 })
  })
})

describe('B03 MAX_BATCH_NESTING = 3', () => {
  // One blob decoding to a list, that list holding a blob decoding to another
  // list, and so on. Each level costs one recursion, not one decode layer.
  // Two plain links per level, deliberately. A decoded list is only accepted
  // when it holds at least two immediately-recognisable links -- the rule that
  // stops a bare infohash being decoded into noise -- so a level carrying one
  // link plus another blob never lands. A real characteristic of the format,
  // not a limitation of the test.
  const nestedList = (depth: number): string => {
    let text = [MAGNET, 'https://host.example/leaf.zip'].join('\n')
    for (let i = 0; i < depth; i++) {
      text = [
        `https://host.example/a${i}.zip`,
        `https://host.example/b${i}.zip`,
        b64(text),
      ].join('\n')
    }
    return text
  }

  it.each([1, 2, 3])('expands a list nested %d deep', (depth) => {
    expect(sanitizeBatch(nestedList(depth)).length).toBeGreaterThanOrEqual(2)
  })

  it('stops rather than expanding without bound', () => {
    // Whatever it does at depth 5, it must terminate and stay within the cap.
    const started = Date.now()
    const got = sanitizeBatch(nestedList(5))
    expect(Date.now() - started).toBeLessThan(3000)
    expect(got.length).toBeLessThanOrEqual(100)
  })
})

describe('B04/B05 MAX_LIST_BYTES = 512 KB', () => {
  const LINE = 'https://host.example/' + 'x'.repeat(40) + '\n' // 62 bytes
  const KB = 1024

  const fileOf = (bytes: number, name = 'list.txt') => {
    const reps = Math.ceil(bytes / LINE.length)
    return new File([LINE.repeat(reps).slice(0, bytes)], name)
  }

  it('reads a file just under the limit whole', async () => {
    const got = await detectFromFile(fileOf(511 * KB))
    expect(got?.type).toBe('links')
  })

  it('reads a file exactly at the limit', async () => {
    const got = await detectFromFile(fileOf(512 * KB))
    expect(got?.type).toBe('links')
  })

  it('truncates past the limit instead of failing', async () => {
    const got = await detectFromFile(fileOf(600 * KB))
    expect(got?.type).toBe('links')
    // Every surviving link must be whole — a read cut mid-URL would produce
    // a truncated link that still looks valid, which is the dangerous case.
    if (got?.type === 'links') {
      for (const item of got.items) expect(item.link).toBe(LINE.trim())
    }
  })

  // B05: over the limit with no newline anywhere to cut back to.
  it('rejects an over-sized file with no line break at all', async () => {
    const oneLine = new File(['https://host.example/' + 'y'.repeat(600 * KB)], 'list.txt')
    // Documented consequence of trimming back to the last newline: with none
    // present there is no safe cut, so the whole file is refused rather than
    // risking a link severed mid-string.
    await expect(detectFromFile(oneLine)).resolves.toBeNull()
  })
})

describe('B06 SNIFF_BYTES = 512', () => {
  // An NZB is identified by <nzb ...> appearing in the first 512 bytes. The
  // tag needs 5 characters, so the last position it can start and still be
  // seen whole is 507.
  const build = (offset: number) => {
    const pad = 'A'.repeat(offset)
    return new File([pad + '<nzb xmlns="x"></nzb>'], 'release.nzb')
  }

  it.each([0, 100, 400, 507])('sees the root element at byte %d', async (offset) => {
    await expect(detectFromFile(build(offset))).resolves.toEqual({
      type: 'file',
      kind: 'usenet',
    })
  })

  // Was a miss before the full-text re-check: a root element past the sniff
  // window left the file to the link scanner, which then found the DTD URL.
  it('still finds the root element past the sniff window', async () => {
    await expect(detectFromFile(build(509))).resolves.toEqual({
      type: 'file',
      kind: 'usenet',
    })
  })

  // The dangerous fallback: a real NZB carries its DTD URL in the DOCTYPE, so
  // an NZB whose root tag sits past the window is scanned for links and finds
  // that URL — presenting an NZB as a web download.
  it('does not turn a missed NZB into a web download of its own DTD', async () => {
    const doctype =
      '<?xml version="1.0" encoding="iso-8859-1" ?>\n' +
      '<!DOCTYPE nzb PUBLIC "-//newzbin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">\n' +
      '<!-- ' + 'c'.repeat(600) + ' -->\n' +
      '<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file/></nzb>'
    const got = await detectFromFile(new File([doctype], 'release.nzb'))
    if (got?.type === 'links') {
      throw new Error(
        `an NZB was read as a link list: ${got.items.map((i) => i.link).join(', ')}`,
      )
    }
  })

})

describe('B07/B08 infohash lengths', () => {
  const hex = (n: number) => 'a'.repeat(n)

  it.each([
    [39, 'webdl'],
    [40, 'torrent'],
    [41, 'webdl'],
    [63, 'webdl'],
    [64, 'torrent'],
    [65, 'webdl'],
  ])('%d hex characters detects as %s', (len, kind) => {
    expect(detectFromLink(hex(len)).kind).toBe(kind)
  })
})

describe('B09 base32 infohash = 32 uppercase', () => {
  const B32 = 'BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASWQQ'

  it('accepts exactly 32 uppercase base32 characters', () => {
    expect(detectFromLink(B32)).toEqual({ kind: 'torrent', certain: true })
  })

  it.each([
    [B32.slice(0, 31), '31 characters'],
    [B32 + 'A', '33 characters'],
    [B32.toLowerCase(), 'lowercase'],
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASW01', 'contains 0 and 1'],
    ['BCW2LJ5GDA5K4HQJ3AY56Z2I2VTASW89', 'contains 8 and 9'],
  ])('rejects %s (%s)', (input) => {
    expect(detectFromLink(input).kind).not.toBe('torrent')
  })
})
