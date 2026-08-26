// Works out which kind of download an input is, so the add form can offer a
// single field instead of making the user pick Torrent/Usenet/Web Link first.
//
// Detection is deliberately client-side and offline: no request is made to
// work out what something is. A link's kind is decided from its scheme and
// path, and a file's from its first bytes.
//
// The result carries whether the answer is *certain*, because one class of
// input genuinely cannot be told apart: an indexer API URL
// (https://indexer/api?t=get&id=123) and a hoster link are the same shape.
// Those fall back to a web download, and the form shows that as an assumption
// the user can override rather than as a fact.

export type Kind = 'torrent' | 'usenet' | 'webdl'

export interface Detection {
  kind: Kind
  /** false when the kind was assumed from a URL that could be anything. */
  certain: boolean
}

/** How many bytes of an uploaded file to inspect. Both formats identify
 *  themselves well inside this: bencode in the first few, NZB's root element
 *  within the XML prolog and doctype. */
const SNIFF_BYTES = 512

// detectFromLink decides a kind from a pasted link.
export function detectFromLink(raw: string): Detection {
  const link = raw.trim()
  if (link === '') return { kind: 'webdl', certain: false }

  // A magnet is unambiguous — nothing else uses the scheme.
  if (/^magnet:/i.test(link)) return { kind: 'torrent', certain: true }

  // A bare infohash. 40 hex characters is a v1 (SHA-1) hash and 64 a v2
  // (SHA-256) one; nothing else a person pastes here looks like that, so
  // this is treated as certain. The add endpoint wraps it into a magnet
  // before handing it to a provider, which will not accept a bare hash.
  if (/^[0-9a-f]{40}$/i.test(link) || /^[0-9a-f]{64}$/i.test(link)) {
    return { kind: 'torrent', certain: true }
  }

  // Extension, taken from the path only: a hoster link may well carry
  // ".torrent" inside a query parameter (a filename, a redirect target)
  // without being a torrent itself.
  const path = pathOf(link)
  if (path.endsWith('.torrent')) return { kind: 'torrent', certain: true }
  if (path.endsWith('.nzb')) return { kind: 'usenet', certain: true }

  // Anything else is assumed to be a hoster link. It might be an indexer API
  // URL for a .torrent or .nzb — there is no way to tell without fetching it
  // — so this is reported as uncertain and the form lets it be corrected.
  return { kind: 'webdl', certain: false }
}

// pathOf returns the lowercased path portion of a URL, without query or
// fragment. Falls back to crude trimming for anything URL() rejects, so a
// half-typed link still gets a sensible answer while the user is typing.
function pathOf(link: string): string {
  try {
    return new URL(link).pathname.toLowerCase()
  } catch {
    return link.split('#')[0].split('?')[0].toLowerCase()
  }
}

// detectFromFile decides a kind from an uploaded file's contents.
//
// Content, not filename: a browser will happily hand over "download.torrent"
// containing XML, and the extension is the least reliable thing about an
// uploaded file. Returns null when it is neither format, which the caller
// surfaces as a rejection — there is no third file type to fall back to,
// since web downloads have no file-upload variant at all.
export async function detectFromFile(file: File): Promise<Detection | null> {
  const head = await readHead(file, SNIFF_BYTES)

  // Bencode: a .torrent is a dictionary, so it opens with 'd', and every
  // real one carries an "announce" or "info" key near the front.
  if (head.startsWith('d') && (head.includes('announce') || head.includes('4:info'))) {
    return { kind: 'torrent', certain: true }
  }

  // NZB is XML with an <nzb> root. Checked before the generic XML test so a
  // stray XML file is not silently treated as usenet.
  if (/<nzb[\s>]/i.test(head)) {
    return { kind: 'usenet', certain: true }
  }

  return null
}

// readHead reads the first n bytes of a file as latin1-ish text. Deliberately
// not UTF-8 decoded: a .torrent is binary, and decoding it as UTF-8 would
// mangle the bytes this needs to look at.
async function readHead(file: File, n: number): Promise<string> {
  const buf = await file.slice(0, n).arrayBuffer()
  let out = ''
  const bytes = new Uint8Array(buf)
  for (let i = 0; i < bytes.length; i++) out += String.fromCharCode(bytes[i])
  return out
}

/** Human label for a kind, matching what the tabs used to say. */
export function kindLabel(kind: Kind): string {
  switch (kind) {
    case 'torrent':
      return 'Torrent'
    case 'usenet':
      return 'Usenet'
    case 'webdl':
      return 'Web Link'
  }
}
