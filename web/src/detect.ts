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

/** The base32 spelling of a v1 infohash: 32 characters rather than 40.
 *  Older trackers and some indexers still hand these out.
 *
 *  Uppercase only, matching the server. 32 mixed-case alphanumerics is the
 *  shape of every API key, session token and TOTP secret going, and treating
 *  those as torrents would be worse than missing the odd lowercase hash. The
 *  conversion to hex happens server-side — see normalizeMagnet in
 *  internal/api/add.go. */
const BASE32_INFOHASH = /^[A-Z2-7]{32}$/

/** Bytes that mean a decode produced binary rather than text. Written as
 *  escapes rather than the characters themselves so the file stays greppable
 *  as text.
 *
 *  no-control-regex is disabled on purpose: matching control characters is
 *  precisely the job here. A decode that produces them produced binary, not
 *  text — which is what keeps a bare infohash from being mangled by atob. */
// eslint-disable-next-line no-control-regex
const CONTROL_CHARS = /[\u0000-\u0008\u000e-\u001f\u007f]/

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
  // The same hash spelled in base32. The add endpoint converts it to hex, so
  // the two spellings of one torrent end up as a single canonical magnet.
  if (BASE32_INFOHASH.test(link)) return { kind: 'torrent', certain: true }

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

/** What an uploaded file turned out to be.
 *
 *  Two outcomes, because a file can be either a download in its own right or
 *  a *list* of them. A .torrent or .nzb is uploaded to the provider as a file;
 *  a text file of links is only a container, and what gets added is its
 *  contents, exactly as if they had been pasted. */
export type FileContent =
  | { type: 'file'; kind: Kind }
  | { type: 'links'; items: BatchItem[] }

/** Extensions the batch-file mode will read.
 *
 *  Deliberately tiny, and the point of the mode existing. A batch file is
 *  opened and everything inside it queued, so widening this to "anything that
 *  happens to contain a link" would mean reading files nobody meant to hand
 *  over. jDownloader's .crawljob and .dlc are the obvious next entries, and
 *  belong here once they are actually parsed rather than merely tolerated. */
const LIST_FILE_EXTENSIONS = new Set(['txt'])

// isListFilename reports whether a file is one the batch-file mode will open.
//
// No extension counts, and that is half the point: the lists people actually
// keep are as often "links" as "links.txt".
export function isListFilename(name: string): boolean {
  const base = name.slice(name.lastIndexOf('/') + 1)
  const dot = base.lastIndexOf('.')
  // dot <= 0 covers "links" and a dotfile like ".links" alike — neither has
  // an extension in any useful sense.
  if (dot <= 0) return true
  return LIST_FILE_EXTENSIONS.has(base.slice(dot + 1).toLowerCase())
}

/** How much of a file to read when looking for a list of links. Generous — a
 *  hundred links is a few kilobytes — while still refusing to pull a video
 *  into memory because it happened to be selected. */
const MAX_LIST_BYTES = 512 * 1024

// detectFromFile decides what an uploaded file is from its contents.
//
// Content, not filename: a browser will happily hand over "download.torrent"
// containing XML, and the extension is the least reliable thing about an
// uploaded file. That is also why a link list needs no extension rule — a
// .txt, a .list and a file with no extension at all are the same thing here,
// which is precisely the case that motivated this.
//
// Returns null when the file is none of them, which the caller surfaces as a
// rejection.
export async function detectFromFile(file: File): Promise<FileContent | null> {
  const head = await readHead(file, SNIFF_BYTES)

  // Bencode: a .torrent is a dictionary, so it opens with 'd', and every
  // real one carries an "announce" or "info" key near the front.
  if (head.startsWith('d') && (head.includes('announce') || head.includes('4:info'))) {
    return { type: 'file', kind: 'torrent' }
  }

  // NZB is XML with an <nzb> root. Checked before the generic XML test so a
  // stray XML file is not silently treated as usenet — and before the link
  // scan below, since an NZB is text and would otherwise be searched for
  // links and come back empty.
  if (/<nzb[\s>]/i.test(head)) {
    return { type: 'file', kind: 'usenet' }
  }

  // Anything else that reads as text: scan it for links. This is the same
  // extraction a paste gets, so prose, bullets, numbering and per-line base64
  // are all handled identically — a file is just a bigger clipboard.
  const text = await readText(file, MAX_LIST_BYTES)
  if (text === null) return null
  const items = sanitizeBatch(text)
  return items.length > 0 ? { type: 'links', items } : null
}

// readText decodes a file as UTF-8, returning null when it is not text.
//
// Strict decoding is the binary check: an image, a video or a .torrent fails
// here long before anything tries to find links in it. CONTROL_CHARS catches
// the remainder — bytes that happen to decode but were never text.
async function readText(file: File, maxBytes: number): Promise<string | null> {
  let bytes = new Uint8Array(await file.slice(0, maxBytes).arrayBuffer())
  if (file.size > maxBytes) {
    // Trim back to the last newline. 0x0A never appears inside a multi-byte
    // UTF-8 sequence, so it is a safe cut point — slicing mid-character would
    // fail the strict decode and reject the whole file. It also drops a final
    // line that the truncation had already ruined.
    const lastNewline = bytes.lastIndexOf(0x0a)
    if (lastNewline < 0) return null
    bytes = bytes.subarray(0, lastNewline)
  }
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    return CONTROL_CHARS.test(text) ? null : text
  } catch {
    return null
  }
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

/** How many nested layers of encoding to peel before giving up. Bounds the
 *  work on input that keeps decoding into more decodable nonsense, which is
 *  otherwise an unbounded loop in the browser. */
const MAX_DECODE_DEPTH = 10

export interface Unwrapped {
  /** The link to actually use — the original when nothing was decoded. */
  value: string
  /** How many layers were peeled. 0 means the input was left alone. */
  layers: number
}

// isRecognisable reports whether a string is already something we can act on:
// a magnet, a bare infohash, or an http(s) URL.
//
// This is the stopping condition for unwrapEncoded, and also its safety
// check — see there for why landing on one of these is what makes a decode
// safe to accept.
function isRecognisable(s: string): boolean {
  // One value, not a list. The scheme tests below only anchor at the start,
  // so without this a decoded list beginning with a magnet would be taken
  // whole as a single enormous link.
  if (/\s/.test(s)) return false
  if (/^magnet:/i.test(s)) return true
  if (/^[0-9a-f]{40}$/i.test(s) || /^[0-9a-f]{64}$/i.test(s)) return true
  if (BASE32_INFOHASH.test(s)) return true
  return /^https?:\/\//i.test(s)
}

// looksBase64 reports whether a string could be base64 or base64url. Length
// must be a multiple of 4 after padding is accounted for, which rules out a
// great deal of ordinary text.
function looksBase64(s: string): boolean {
  if (s.length < 8 || s.length % 4 !== 0) return false
  return /^[A-Za-z0-9+/]+={0,2}$/.test(s) || /^[A-Za-z0-9\-_]+={0,2}$/.test(s)
}

// decodeOnce peels exactly one layer of encoding, or returns null if it
// cannot.
//
// Percent-encoding is tried first: it is cheaper to rule out, and a link
// copied out of a redirect URL is far more common than a base64-wrapped one.
// The two cannot be confused — base64's alphabet has no "%", and a
// percent-encoded string is not valid base64.
function decodeOnce(s: string): string | null {
  return decodePercentOnce(s) ?? decodeBase64Once(s)
}

// decodePercentOnce undoes percent-encoding, the shape a link takes when it
// has been copied out of a redirect or tracking URL: https%3A%2F%2Fhost%2Ff.
//
// This is only safe because unwrapEncoded returns early on anything already
// recognisable. An ordinary URL carrying a legitimate escape — a filename
// with a space in it, say — never reaches this, so it cannot be mangled into
// a broken URL with a raw space in the middle. Note decodeURIComponent leaves
// "+" alone, which matters: a magnet's dn= uses it for spaces.
function decodePercentOnce(s: string): string | null {
  if (!/%[0-9A-Fa-f]{2}/.test(s)) return null
  let text: string
  try {
    text = decodeURIComponent(s)
  } catch {
    // A stray "%" that was never an escape.
    return null
  }
  if (text === s) return null
  if (CONTROL_CHARS.test(text)) return null
  return text.trim()
}

// decodeBase64Once peels one base64 or base64url layer.
//
// Strict on purpose. atob will happily turn arbitrary bytes into mojibake, so
// a decode is only accepted when it round-trips as valid UTF-8 and contains
// nothing unprintable — anything else means the input was not really encoded
// text.
function decodeBase64Once(s: string): string | null {
  if (!looksBase64(s)) return null
  // base64url differs only in its alphabet; normalise before decoding.
  const normalised = s.replace(/-/g, '+').replace(/_/g, '/')
  let raw: string
  try {
    raw = atob(normalised)
  } catch {
    return null
  }
  let text: string
  try {
    // atob yields one char per byte; reinterpret those bytes as UTF-8 so a
    // decoded link with non-ASCII characters survives intact.
    const bytes = Uint8Array.from(raw, (c) => c.charCodeAt(0))
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return null
  }
  // Control characters mean this was binary, not text that happened to be
  // encoded — a bare infohash decodes to exactly that.
  if (CONTROL_CHARS.test(text)) return null
  if (text === s) return null
  return text.trim()
}

// hasLanded reports whether a decode has arrived somewhere worth keeping:
// a single usable value, or text holding at least two of them.
//
// The list case is what lets one base64 blob carry a whole batch. It does not
// weaken the rule that keeps a bare infohash intact — that protection lives in
// decodeOnce, which rejects the binary a hash decodes into long before landing
// is considered.
function hasLanded(s: string): boolean {
  if (isRecognisable(s)) return true
  let found = 0
  for (const token of s.split(/\s+/)) {
    if (isRecognisable(cleanToken(token))) {
      found++
      if (found >= 2) return true
    }
  }
  return false
}

// cleanToken strips the decoration a link picks up from being written down:
// list bullets, numbering, email quoting, markdown, wrapping brackets and
// trailing sentence punctuation. What survives is either a link or junk, and
// junk is dropped by failing to be recognisable rather than by being matched
// against a list of things to remove.
function cleanToken(raw: string): string {
  let out = raw.trim()
  const markdown = /^\[[^\]]*\]\(([^()\s]+)\)$/.exec(out)
  if (markdown) out = markdown[1]
  out = out.replace(/^[>\-*•]+/, '')
  out = out.replace(/^\d+[.)]/, '')
  // Sentence punctuation comes off before the brackets, so "(url)." can
  // still unwrap instead of leaving the closing paren stranded.
  out = out.replace(/[,;.]+$/, '')
  return unwrapPairs(out).trim()
}

/** Openers that wrap a written-down link, and what closes each. */
const WRAPPERS: Record<string, string> = {
  '<': '>',
  '(': ')',
  '[': ']',
  '"': '"',
  "'": "'",
}

// unwrapPairs removes wrapping punctuation, but only in matched pairs.
//
// A closing bracket with nothing opening it is part of the link, not
// decoration around it. Stripping unconditionally corrupted real input:
// "https://host/wiki/Thing_(disambiguation)" lost its final paren and 404s,
// and a magnet's "&dn=Movie+[1080p]" lost the bracket off its display name.
// A leading wrapper on its own is still dropped, since nothing legitimate
// starts with one.
function unwrapPairs(input: string): string {
  const leading = /^[<("'[]+/.exec(input)?.[0] ?? ''
  let out = input.slice(leading.length)
  // Outermost first, so "<(url)>" peels in the order it was written.
  for (const opener of leading) {
    const closer = WRAPPERS[opener]
    if (closer !== undefined && out.endsWith(closer)) out = out.slice(0, -1)
  }
  return out
}

// unwrapEncoded peels nested base64/base64url until it reaches something
// usable, and returns the input untouched if it never does.
//
// The "only if it lands somewhere recognisable" rule is what makes this safe
// rather than destructive. A bare infohash is itself valid base64 — forty hex
// characters, correct alphabet, length divisible by four — and decodes
// cleanly into binary noise. Accepting decodes on their own merit would
// silently mangle one. Requiring the *result* to be a magnet, hash or URL
// means a decode has to be going somewhere before it is kept.
//
// "Somewhere" includes a list of two or more, so one blob can carry a whole
// batch — see hasLanded.
export function unwrapEncoded(raw: string): Unwrapped {
  const input = raw.trim()
  if (input === '' || isRecognisable(input)) return { value: input, layers: 0 }

  let current = input
  for (let depth = 1; depth <= MAX_DECODE_DEPTH; depth++) {
    const next = decodeOnce(current)
    if (next === null) break
    current = next
    if (hasLanded(current)) return { value: current, layers: depth }
  }
  // Decoded into nothing meaningful, so it probably was not encoded at all.
  return { value: input, layers: 0 }
}

// --- decode state ------------------------------------------------------
//
// Peeling happens by rewriting the input field in place, which makes this a
// small state machine rather than a single call: the rewrite re-triggers the
// very effect that performed it, so the code has to tell its own rewrite from
// a genuine edit. Kept here as a pure function because the two bugs it fixes
// were both invisible in the component — the notice cleared itself on the
// re-render the decode caused, and the undo button re-decoded what it had
// just restored.

export interface DecodeState {
  /** The text exactly as pasted; null when nothing was rewritten. */
  from: string | null
  /** What the rewrite produced. Distinguishes our write from a user edit. */
  to: string | null
  /** How many base64 layers were peeled, for the notice. */
  layers: number
  /** How many links the rewrite produced; 1 for a plain single decode. */
  items: number
  /** A value the user asked to keep verbatim, never rewritten again. */
  pinned: string | null
}

export const noDecode: DecodeState = { from: null, to: null, layers: 0, items: 0, pinned: null }

export interface DecodeStep {
  /** What the field should now hold — unchanged unless a decode fired. */
  link: string
  /** The state to store. Identical to the input state when nothing changed,
   *  so callers can skip the update with a reference comparison. */
  state: DecodeState
}

// stepDecode computes the next field value and decode state for an input.
export function stepDecode(link: string, prev: DecodeState): DecodeStep {
  // Pinned input is left strictly alone; this is what makes undo stick.
  if (link === prev.pinned) return { link, state: prev }

  const unwrapped = unwrapEncoded(link)
  if (unwrapped.layers > 0 && unwrapped.value !== link) {
    return {
      link: unwrapped.value,
      state: { from: link, to: unwrapped.value, layers: unwrapped.layers, items: 1, pinned: prev.pinned },
    }
  }

  // The field holding exactly what the decode produced is not an edit; the
  // notice survives. Anything else means the user has moved on from it.
  if (prev.from !== null && link !== prev.to) {
    return { link, state: { from: null, to: null, layers: 0, items: 0, pinned: prev.pinned } }
  }
  return { link, state: prev }
}

// undoDecode restores what the user originally pasted and pins it, so the
// effect that runs on the restored value does not simply decode it again.
export function undoDecode(prev: DecodeState): DecodeStep {
  if (prev.from === null) return { link: '', state: prev }
  return { link: prev.from, state: { from: null, to: null, layers: 0, items: 0, pinned: prev.from } }
}

// --- batches -----------------------------------------------------------
//
// A batch is a property of the whole field, not a fourth kind: the three
// protocols stay exactly what they were, and a batch is simply more than one
// of them at once. Mixing is the point - a magnet, an .nzb link and a hoster
// link in the same paste each route to their own endpoint.

/** Hard cap on one batch, so pasting an entire file cannot melt the tab. */
const MAX_BATCH_ITEMS = 100

/** How deep a decoded list may itself contain another encoded list. */
const MAX_BATCH_NESTING = 3

export interface BatchItem {
  /** Cleaned and fully decoded - exactly what gets submitted. */
  link: string
  kind: Kind
  certain: boolean
  /** Base64 layers peeled off this item specifically. */
  layers: number
}

export type FieldDetection =
  | { batch: false; kind: Kind; certain: boolean }
  | { batch: true; items: BatchItem[] }

// sanitizeBatch pulls every usable link out of arbitrary pasted text.
//
// Splitting on whitespace is safe by construction: a magnet percent-encodes
// spaces, a URL cannot hold a raw one, base64 has no whitespace and an
// infohash is hex. Prose therefore falls away on its own - a word is just a
// token that fails to be, or to decode into, something recognisable. There is
// no "is this junk?" heuristic to get wrong, which is the whole trick.
//
// Each token is unwrapped independently, so one line five layers deep and
// another in clear text both come out right in the same paste.
export function sanitizeBatch(text: string): BatchItem[] {
  const out: BatchItem[] = []
  collectLinks(text, out, new Set<string>(), 0)
  return out
}

function collectLinks(text: string, out: BatchItem[], seen: Set<string>, depth: number): void {
  if (depth > MAX_BATCH_NESTING) return
  for (const token of text.split(/\s+/)) {
    if (out.length >= MAX_BATCH_ITEMS) return
    const cleaned = cleanToken(token)
    if (cleaned === '') continue
    const unwrapped = unwrapEncoded(cleaned)
    if (isRecognisable(unwrapped.value)) {
      // Deduped case-insensitively: the same magnet twice, or once upper- and
      // once lower-cased, is one download either way.
      const key = unwrapped.value.toLowerCase()
      if (seen.has(key)) continue
      seen.add(key)
      const detected = detectFromLink(unwrapped.value)
      out.push({
        link: unwrapped.value,
        kind: detected.kind,
        certain: detected.certain,
        layers: unwrapped.layers,
      })
      continue
    }
    // Decoded to something that was not one link but held several: a single
    // blob carrying a whole list. Recurse into what came out of it.
    if (unwrapped.layers > 0) collectLinks(unwrapped.value, out, seen, depth + 1)
  }
}

// detectField decides what the field holds as a whole.
//
// Two or more surviving links make it a batch. One stays on the single path,
// which means a lone link pasted with a title around it now gets cleaned up
// too. Zero falls back to detecting the raw text, so something that is not a
// link at all still reports as an assumed web link rather than vanishing.
export function detectField(text: string): FieldDetection {
  const items = sanitizeBatch(text)
  if (items.length >= 2) return { batch: true, items }
  if (items.length === 1) return { batch: false, kind: items[0].kind, certain: items[0].certain }
  const detected = detectFromLink(text)
  return { batch: false, kind: detected.kind, certain: detected.certain }
}

/** The text a batch occupies: one link per line, in the order pasted. */
export function batchText(items: BatchItem[]): string {
  return items.map((item) => item.link).join('\n')
}

// mergeBatches folds several batches into one, deduping across them and
// applying the same cap a single paste gets — uploading five list files must
// not be a way around the limit.
export function mergeBatches(lists: BatchItem[][]): BatchItem[] {
  const out: BatchItem[] = []
  const seen = new Set<string>()
  for (const list of lists) {
    for (const item of list) {
      if (out.length >= MAX_BATCH_ITEMS) return out
      const key = item.link.toLowerCase()
      if (seen.has(key)) continue
      seen.add(key)
      out.push(item)
    }
  }
  return out
}

/** Counts per kind, in the order the chips use, skipping kinds not present. */
export function batchSummary(items: BatchItem[]): { kind: Kind; count: number }[] {
  const order: Kind[] = ['torrent', 'usenet', 'webdl']
  return order
    .map((kind) => ({ kind, count: items.filter((item) => item.kind === kind).length }))
    .filter((entry) => entry.count > 0)
}

/** Lowercase plural for the batch summary - "3 torrents, 1 usenet". */
export function kindPlural(kind: Kind, count: number): string {
  if (kind === 'usenet') return 'usenet'
  const base = kind === 'torrent' ? 'torrent' : 'web link'
  return count === 1 ? base : base + 's'
}

// stepSanitize is the paste-time counterpart to stepDecode: it cleans the
// whole field down to the links inside it, rather than treating the field as
// a single value.
//
// Only a paste triggers this. Running it on every keystroke would eat a link
// halfway through being typed on a second line.
export function stepSanitize(link: string, prev: DecodeState): DecodeStep {
  if (link === prev.pinned) return { link, state: prev }
  const items = sanitizeBatch(link)
  // Nothing recognisable at all: leave the text alone and let the single path
  // have its say, so junk still reports as an assumed web link.
  if (items.length === 0) return stepDecode(link, prev)
  const cleaned = batchText(items)
  if (cleaned === link) return stepDecode(link, prev)
  const layers = items.reduce((most, item) => Math.max(most, item.layers), 0)
  return {
    link: cleaned,
    state: { from: link, to: cleaned, layers, items: items.length, pinned: prev.pinned },
  }
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
