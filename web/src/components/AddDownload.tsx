import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  // Aliased: the unprefixed name would shadow the DOM KeyboardEvent that
  // the Escape-to-close handler below is typed against.
  type KeyboardEvent as ReactKeyboardEvent,
} from 'react'
import {
  addTorrent,
  addUsenet,
  addWebDownload,
  ApiError,
  checkCachedTorrent,
  checkCachedUsenet,
  checkCachedWebDownload,
  getGeneralSettings,
  getTorrentInfo,
  type ProviderStatus,
  type TorrentInfoResponse,
} from '../api'
import {
  batchSummary,
  detectField,
  detectFromFile,
  kindLabel,
  isListFilename,
  kindPlural,
  mergeBatches,
  noDecode,
  stepDecode,
  stepSanitize,
  undoDecode,
  type BatchItem,
  type Detection,
  type Kind,
} from '../detect'
import { formatBytes } from '../format'

type Protocol = 'torrent' | 'usenet' | 'webdl'
type InputMode = 'link' | 'file' | 'batchfile'

/** How many adds run at once. Each one hits the provider, and twenty at a
 *  time invites the 429 we would then have to explain; on the first one the
 *  rest are abandoned rather than piled on. */
const BATCH_CONCURRENCY = 3

/** How tall the input may grow for a batch before it scrolls instead. The
 *  overlay scrolls the whole panel, so this only has to stay short enough to
 *  keep the Add button reachable without one. */
const MAX_INPUT_HEIGHT = 400

/** Everything an add carries besides the link or file itself. */
type CommonAdd = Omit<Parameters<typeof addWebDownload>[1], 'link'>

interface Props {
  apiKey: string
  providers: ProviderStatus[]
  // isAdmin gates the Managed/Manual toggle entirely — a member has no
  // access to the Managed pipeline at all (see docs/providers.md#roles),
  // so they never see the choice; everything they add stays Manual, same
  // as before this existed. defaultManaged seeds the toggle's starting
  // position from whichever tab the button was opened from (see App.tsx) —
  // still just a default, not a restriction; an admin can flip it either
  // way regardless of which tab they started from.
  isAdmin: boolean
  defaultManaged: boolean
  onClose: () => void
  // onAdded reports which mode was actually used — not necessarily
  // defaultManaged, since an admin can flip the toggle before submitting —
  // so the caller can navigate to the tab the new download actually landed
  // in, not just the one the button happened to default to.
  onAdded: (addedManaged: boolean) => void
}


export function AddDownload({ apiKey, providers, isAdmin, defaultManaged, onClose, onAdded }: Props) {
  // Only offered when there is genuinely a choice — with one provider the
  // picker would be a control with a single option.
  const [provider, setProvider] = useState('')
  const torrentAvailable = providers.some((p) => p.torrent_capable)
  const usenetAvailable = providers.some((p) => p.usenet_capable)
  const webdlAvailable = providers.some((p) => p.webdl_capable)
  const availableProtocols: Protocol[] = [
    ...(torrentAvailable ? (['torrent'] as const) : []),
    ...(usenetAvailable ? (['usenet'] as const) : []),
    ...(webdlAvailable ? (['webdl'] as const) : []),
  ]

  // What the current input looks like, and whether the user has corrected it.
  // override wins while set; it is cleared whenever the input changes, so a
  // correction made for one link never silently carries to the next.
  // Only an uploaded file needs its kind held in state — it is sniffed
  // asynchronously. A link's kind is derived from the field itself, below.
  const [fileDetected, setFileDetected] = useState<Detection>({ kind: 'torrent', certain: false })
  const [override, setOverride] = useState<Kind | null>(null)
  const [fileError, setFileError] = useState('')
  // What was decoded away, so the transformation is visible and reversible.
  // The whole transition lives in stepDecode/undoDecode — see detect.ts for
  // why this needs to be a state machine and not a single call.
  const [decode, setDecode] = useState(noDecode)
  // Web Downloads is genuinely link-only — TorBox's own createwebdownload API
  // has no file-upload variant, unlike torrent/usenet — so there's no mode
  // toggle to show for it (see handleSubmit's protocol==='webdl' branch).
  const [mode, setMode] = useState<InputMode>('link')
  const [link, setLink] = useState('')
  const [files, setFiles] = useState<File[]>([])
  // Uploads that are downloads in their own right: a .torrent or .nzb, sent
  // to the provider as a file.
  const [fileItems, setFileItems] = useState<{ file: File; kind: Kind }[]>([])
  // Links pulled out of an uploaded text file. These are added as links, not
  // uploaded — the file was only ever a container for them.
  const [fileLinks, setFileLinks] = useState<BatchItem[]>([])
  const [fileNotice, setFileNotice] = useState('')
  // Set by the input's onPaste and consumed by the effect below. Only a paste
  // triggers the aggressive multi-link clean-up: running it per keystroke
  // would eat a link halfway through being typed on a second line.
  const pasted = useRef(false)
  const linkInput = useRef<HTMLTextAreaElement>(null)
  // Which items of a batch failed, so a partial result can name them.
  const [batchErrors, setBatchErrors] = useState<{ link: string; message: string }[]>([])
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null)

  // What the field holds as a whole: one item, or a batch of them. Memoised
  // because it re-parses the entire paste, and this renders on every keystroke.
  const field = useMemo(() => detectField(link), [link])
  const isBatch = mode === 'link' && field.batch
  // Both file modes share one picker and one detection pass; they differ in
  // what they will accept from it.
  const isFileMode = mode === 'file' || mode === 'batchfile'
  // Everything an upload will produce, uploads and extracted links alike,
  // described uniformly so the chips and counts do not care which is which.
  const fileBatch: BatchItem[] = [
    ...fileItems.map((f) => ({ link: f.file.name, kind: f.kind, certain: true, layers: 0 })),
    ...fileLinks,
  ]
  // A batch of either kind, described the same way for the summary chips.
  const summaryItems: BatchItem[] | null = isBatch
    ? field.items
    : isFileMode && fileBatch.length > 1
      ? fileBatch
      : null
  const detected: Detection =
    isFileMode
      ? fileDetected
      : field.batch
        ? { kind: 'torrent', certain: false } // unused: a batch has no one kind
        : { kind: field.kind, certain: field.certain }
  const protocol: Protocol = override ?? detected.kind
  // managed is always false for a member — isAdmin gates whether the toggle
  // even renders, not just whether it's editable, so there's no path for a
  // non-admin to end up with this true.
  const [managed, setManaged] = useState(isAdmin && defaultManaged)
  const [category, setCategory] = useState('')
  // Per-add overrides of the configured Managed defaults. undefined until the
  // defaults arrive, so an add before then sends nothing and the server
  // applies its own defaults rather than a guessed false.
  const [deleteAfterFetch, setDeleteAfterFetch] = useState<boolean | undefined>(undefined)
  const [keepFiles, setKeepFiles] = useState<boolean | undefined>(undefined)
  // defaultsLoaded distinguishes "not fetched yet" from "fetched, both
  // false". Without it a failed fetch is indistinguishable from real
  // defaults of false — and an earlier version of this gated rendering on
  // the values being set, so a failure hid the controls entirely with no
  // way to tell why.
  const [defaultsLoaded, setDefaultsLoaded] = useState(false)

  // Seed the two Managed options from the configured defaults. Admin-only,
  // because the settings endpoint is and only an admin can add a Managed
  // download at all. On failure the controls still render, unchecked and
  // usable — an untouched box sends nothing, so the server applies its own
  // default and a failed fetch costs accuracy of the initial tick rather
  // than the whole feature.
  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    getGeneralSettings(apiKey)
      .then((g) => {
        if (cancelled) return
        setDeleteAfterFetch(g.managed_add_delete_after_fetch)
        setKeepFiles(g.managed_add_keep_files)
        setDefaultsLoaded(true)
      })
      .catch(() => {
        if (!cancelled) setDefaultsLoaded(true)
      })
    return () => {
      cancelled = true
    }
  }, [apiKey, isAdmin])
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  // Cached/metadata preview — cached is null until a check-cached response
  // comes back (or the input isn't recognizable yet); torrentInfo is only
  // ever populated for the torrent protocol, which is the only one TorBox
  // has a by-hash metadata-preview endpoint for at all (see
  // docs/providers.md#cached--metadata-previews-before-adding). Debounced
  // rather than firing on every keystroke — the torrent-info lookup in
  // particular searches the live BitTorrent network, not just TorBox's own
  // account, so it's real work worth not spamming.
  const [cached, setCached] = useState<boolean | null>(null)
  const [torrentInfo, setTorrentInfo] = useState<TorrentInfoResponse | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  useEffect(() => {
    setCached(null)
    setTorrentInfo(null)
    if (mode !== 'link') return
    const value = link.trim()
    // A batch is many links, so there is no single thing to preview — and
    // the torrent test below is not anchored, so a list of magnets would
    // otherwise match and fire a lookup for nonsense.
    if (/\s/.test(value)) return
    // Cheap client-side sanity checks before ever spending a round trip —
    // real validation still happens server-side either way, this just
    // avoids firing on a clearly-incomplete paste-in-progress.
    if (protocol === 'torrent' && !/xt=urn:btih:/i.test(value)) return
    if (protocol !== 'torrent' && !/^https?:\/\//i.test(value)) return

    let cancelled = false
    setPreviewLoading(true)

    async function loadPreview() {
      // Independent, best-effort — neither ever blocks submission, so one
      // failing (e.g. a transient network error) shouldn't hide the other
      // succeeding.
      const cachedPromise =
        protocol === 'torrent'
          ? checkCachedTorrent(apiKey, value)
          : protocol === 'usenet'
            ? checkCachedUsenet(apiKey, value)
            : checkCachedWebDownload(apiKey, value)
      const [cachedResult, infoResult] = await Promise.allSettled([
        cachedPromise,
        protocol === 'torrent' ? getTorrentInfo(apiKey, value) : Promise.resolve(null),
      ])
      if (cancelled) return
      setCached(cachedResult.status === 'fulfilled' ? cachedResult.value.cached : null)
      setTorrentInfo(infoResult.status === 'fulfilled' ? infoResult.value : null)
      setPreviewLoading(false)
    }

    const timer = setTimeout(loadPreview, 500)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiKey, protocol, mode, link])

  // Runs a batch's adds a few at a time, collecting per-item outcomes instead
  // of abandoning the rest on the first error.
  //
  // Bounded deliberately: every add hits the provider, and firing twenty at
  // once invites the 429 we would then have to explain. On the first
  // rate-limit the remaining items are dropped rather than piled on, and are
  // reported as not attempted rather than as failures of their own.
  async function runBatch(tasks: { label: string; run: () => Promise<unknown> }[]) {
    // undefined = never attempted, null = succeeded, string = failed.
    const outcome: (string | null | undefined)[] = new Array(tasks.length).fill(undefined)
    let next = 0
    let done = 0
    let rateLimited = false
    setProgress({ done: 0, total: tasks.length })

    async function worker() {
      for (;;) {
        if (rateLimited) return
        const index = next++
        if (index >= tasks.length) return
        try {
          await tasks[index].run()
          outcome[index] = null
        } catch (err) {
          outcome[index] = err instanceof ApiError ? err.message : String(err)
          if (err instanceof ApiError && err.status === 429) rateLimited = true
        }
        done++
        setProgress({ done, total: tasks.length })
      }
    }

    await Promise.all(
      Array.from({ length: Math.min(BATCH_CONCURRENCY, tasks.length) }, () => worker()),
    )

    const failures: { link: string; message: string }[] = []
    for (let i = 0; i < tasks.length; i++) {
      if (outcome[i] === null) continue
      failures.push({
        link: tasks[i].label,
        message:
          outcome[i] ?? (rateLimited ? 'not attempted — provider rate limited' : 'not attempted'),
      })
    }
    return failures
  }

  function addOne(kind: Kind, value: string, common: CommonAdd) {
    if (kind === 'torrent') return addTorrent(apiKey, { magnet: value, ...common })
    if (kind === 'usenet') return addUsenet(apiKey, { url: value, ...common })
    return addWebDownload(apiKey, { link: value, ...common })
  }

  // A single failure keeps the plain message it always had; only a real batch
  // gets the "n of m" summary and the per-item list.
  function reportPartial(total: number, failures: { link: string; message: string }[]) {
    if (total === 1) {
      setStatus({ kind: 'error', message: failures[0].message })
      return
    }
    setBatchErrors(failures)
    setStatus({
      kind: 'error',
      message: `${total - failures.length} of ${total} added — ${failures.length} failed`,
    })
  }

  async function handleSubmit(e: FormEvent | ReactKeyboardEvent<HTMLTextAreaElement>) {
    e.preventDefault()
    if (mode === 'link' && !link.trim()) return
    if (isFileMode && fileBatch.length === 0) return

    setStatus({ kind: 'saving' })
    setBatchErrors([])
    try {
      // Category only means anything for a Managed add — it drives
      // internal/importer's save-path resolution, the same as a category
      // Sonarr/Radarr declared through the compat shims (see
      // docs/configuration.md#categories-and-save-paths). A Manual add never
      // sends it: Manual downloads mirror TorBox's own web UI, which has no
      // category concept at all (see ROADMAP.md's "Manual categories" entry).
      //
      // The Managed-only options go the same way: the server ignores them for
      // a Manual add, and sending them anyway would imply they meant
      // something there. An empty provider means "didn't choose", which the
      // server reads as the default.
      const common: CommonAdd = {
        category: managed ? category.trim() : undefined,
        addedVia: managed ? 'arr' : undefined,
        provider: provider || undefined,
        deleteAfterFetch: managed ? deleteAfterFetch : undefined,
        keepFiles: managed ? keepFiles : undefined,
      }

      if (isFileMode) {
        // Uploads and extracted links go out together in one batch. A .torrent
        // is sent as a file; a link that came out of a list file is added as a
        // link, exactly as if it had been pasted.
        const tasks = [
          ...fileItems.map((item) => ({
            label: item.file.name,
            run: () =>
              item.kind === 'usenet'
                ? addUsenet(apiKey, { file: item.file, ...common })
                : addTorrent(apiKey, { file: item.file, ...common }),
          })),
          ...fileLinks.map((item) => ({
            label: item.link,
            run: () => addOne(item.kind, item.link, common),
          })),
        ]
        const failures = await runBatch(tasks)
        if (failures.length > 0) {
          reportPartial(tasks.length, failures)
          return
        }
      } else if (field.batch) {
        const failures = await runBatch(
          field.items.map((item) => ({
            label: item.link,
            run: () => addOne(item.kind, item.link, common),
          })),
        )
        if (failures.length > 0) {
          // Leave only what failed in the box, so it can be corrected and
          // sent again without having to hunt those links down a second time.
          setDecode(noDecode)
          setLink(failures.map((failure) => failure.link).join('\n'))
          reportPartial(field.items.length, failures)
          return
        }
      } else {
        await addOne(protocol, link.trim(), common)
      }
      onAdded(managed)
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    } finally {
      setProgress(null)
    }
  }

  // Enter still submits a single link, exactly as it did when this was an
  // <input>. In a batch it has to insert a newline instead — you are editing
  // a list at that point — so Ctrl/Cmd+Enter submits either way.
  function handleKeyDown(e: ReactKeyboardEvent<HTMLTextAreaElement>) {
    if (e.key !== 'Enter') return
    if (e.ctrlKey || e.metaKey || !isBatch) void handleSubmit(e)
  }

  // Detection runs on every input change, and clears any correction the user
  // made for the previous input — a link they told us was usenet must not
  // make the next one usenet too.
  useEffect(() => {
    if (mode !== 'link') return
    setOverride(null)
    // Peel any nested base64 before deciding what this is. This can rewrite
    // the field, which re-runs the effect; stepDecode is what keeps that
    // second pass from undoing the notice or re-decoding a restored value.
    // A paste gets the full clean-up: strip everything that is not a link,
    // then decode each survivor separately. Typing gets only the single-value
    // decoder, so a link being typed on a second line is left alone.
    const wasPasted = pasted.current
    pasted.current = false
    const step = wasPasted ? stepSanitize(link, decode) : stepDecode(link, decode)
    if (step.state !== decode) setDecode(step.state)
    if (step.link !== link) {
      setLink(step.link)
      return
    }
    // decode is deliberately not a dependency: it changes as a result of
    // this effect, and including it would re-enter on its own write.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [link, mode])

  // Grow the field to fit a batch rather than scrolling it.
  //
  // rows= cannot do this correctly: long links mean a horizontal scrollbar,
  // that scrollbar lives inside the content box, and so N rows renders fewer
  // than N lines with a vertical scrollbar to make up the difference.
  // Measuring is the only way to get it right.
  useEffect(() => {
    const el = linkInput.current
    if (!el) return
    if (!isBatch) {
      el.style.height = ''
      return
    }
    el.style.height = 'auto'
    // offsetHeight - clientHeight is the borders plus whatever the horizontal
    // scrollbar is occupying; scrollHeight on its own clips the last line.
    const chrome = el.offsetHeight - el.clientHeight
    el.style.height = `${Math.min(el.scrollHeight + chrome, MAX_INPUT_HEIGHT)}px`
  }, [link, isBatch, mode])

  // A file is identified by its first bytes rather than its name: a browser
  // will hand over "x.torrent" containing anything at all.
  useEffect(() => {
    if (!isFileMode || files.length === 0) {
      setFileItems([])
      setFileLinks([])
      setFileError('')
      setFileNotice('')
      return
    }
    let cancelled = false
    setOverride(null)
    setFileError('')
    setFileNotice('')
    Promise.all(
      files.map(async (f) => ({ file: f, content: await detectFromFile(f) })),
    ).then((results) => {
      if (cancelled) return
      const uploads: { file: File; kind: Kind }[] = []
      const lists: BatchItem[][] = []
      const listNames: string[] = []
      const bad: File[] = []
      // Files the other mode would have taken. Called out specifically rather
      // than lumped in with "unreadable", because the fix is one radio button
      // away and saying so is more use than saying no.
      const wrongMode: File[] = []
      // Batch file only opens what LIST_FILE_EXTENSIONS allows. Checked on the
      // name, before content: the restriction is about not reading files that
      // were never offered, so finding links inside one is not a reason to.
      const notAList: File[] = []
      for (const result of results) {
        if (mode === 'batchfile') {
          if (!isListFilename(result.file.name)) notAList.push(result.file)
          else if (result.content?.type === 'links') {
            lists.push(result.content.items)
            listNames.push(result.file.name)
          } else if (result.content?.type === 'file') wrongMode.push(result.file)
          else bad.push(result.file)
          continue
        }
        if (result.content === null) {
          bad.push(result.file)
        } else if (result.content.type === 'file') {
          uploads.push({ file: result.file, kind: result.content.kind })
        } else {
          wrongMode.push(result.file)
        }
      }
      // Deduped and capped across files together: five list files must not be
      // a way around the limit one paste gets.
      const links = mergeBatches(lists)
      setFileItems(uploads)
      setFileLinks(links)
      if (listNames.length > 0) {
        setFileNotice(
          `Found ${links.length} link${links.length === 1 ? '' : 's'} in ${listNames.join(', ')}`,
        )
      }
      // Rejected files are skipped rather than blocking the rest of the
      // upload, but they are always named — silently dropping one would look
      // like the add simply lost it. Ordered so the most actionable message
      // wins: wrong mode first, since that one has an obvious fix.
      const names = (fs: File[]) => fs.map((f) => f.name).join(', ')
      if (wrongMode.length > 0) {
        setFileError(
          mode === 'batchfile'
            ? `${names(wrongMode)} is a torrent or NZB, not a list — switch to File(s) to add it.`
            : `${names(wrongMode)} is a list of links — switch to Batch file to add it.`,
        )
      } else if (notAList.length > 0) {
        setFileError(
          `Batch file reads .txt files and files with no extension. Skipping ${names(notAList)}.`,
        )
      } else if (bad.length > 0) {
        setFileError(
          mode === 'batchfile'
            ? `No links found in ${names(bad)}.`
            : `Not a .torrent or an .nzb: ${names(bad)}.`,
        )
      }
      // Only meaningful when the upload came to exactly one thing; a batch
      // has no single kind and uses the summary chips instead.
      if (uploads.length === 1 && links.length === 0) {
        setFileDetected({ kind: uploads[0].kind, certain: true })
      } else if (uploads.length === 0 && links.length === 1) {
        setFileDetected({ kind: links[0].kind, certain: links[0].certain })
      }
    })
    return () => {
      cancelled = true
    }
    // isFileMode is derived from mode, which is already here; listed so the
    // dependency check can see that for itself.
  }, [files, mode, isFileMode])

  const kindAvailable = (kind: Kind) =>
    kind === 'torrent' ? torrentAvailable : kind === 'usenet' ? usenetAvailable : webdlAvailable
  // A batch keeps the form open if any item has somewhere to go. Items whose
  // kind has no provider fail individually with the server's own message,
  // rather than hiding the entire form because one line cannot be routed.
  const protocolAvailable = summaryItems
    ? summaryItems.some((item) => kindAvailable(item.kind))
    : kindAvailable(protocol)

  // Which providers can actually take the protocol currently selected — a
  // torrent-only provider shouldn't be offered for a usenet add.
  const providerHandles = (p: ProviderStatus, kind: Kind) =>
    kind === 'torrent' ? p.torrent_capable : kind === 'usenet' ? p.usenet_capable : p.webdl_capable
  // One provider is chosen for the whole batch, so only those able to take
  // every kind in it are offered. Leaving it on Default routes per kind.
  const capableProviders = providers.filter((p) =>
    summaryItems
      ? summaryItems.every((item) => providerHandles(p, item.kind))
      : providerHandles(p, protocol),
  )

  return (
    <div className="detail-overlay" onClick={onClose}>
      <div className="detail-panel add-download-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2>Add to {managed ? 'Managed' : 'Manual'}</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

        {isAdmin && (
          <div className="mode-toggle">
            <label>
              <input type="radio" checked={!managed} onChange={() => setManaged(false)} />
              Manual
            </label>
            <label>
              <input type="radio" checked={managed} onChange={() => setManaged(true)} />
              Managed
            </label>
          </div>
        )}
        {isAdmin && (
          <p className="settings-help">
            {managed
              ? 'Fetched to disk automatically, same as an *arr-added download — shows up in the Managed tab.'
              : "Grabbed on demand, mirroring TorBox's own web UI — shows up in the Manual tab, never auto-fetched."}
          </p>
        )}

        {/* Only for a Managed add, and only once the defaults have loaded —
            these override them for this one download. Nothing here applies to
            a download an *arr adds for itself. Defaults live in
            Settings → Managed adds. */}
        {isAdmin && managed && (
          <div className="managed-options">
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={deleteAfterFetch ?? false}
                disabled={!defaultsLoaded}
                onChange={(e) => setDeleteAfterFetch(e.target.checked)}
              />
              Delete from provider once fetched
            </label>
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={keepFiles ?? false}
                disabled={!defaultsLoaded}
                onChange={(e) => setKeepFiles(e.target.checked)}
              />
              Keep local files (exempt from cleanup)
            </label>
          </div>
        )}

        {/* What the input looks like, rather than a choice to make up front.
            A magnet, a .torrent/.nzb link and an uploaded file are identified
            with certainty; any other URL is assumed to be a web link, because
            an indexer API URL and a hoster link are genuinely the same shape.
            The assumption is shown as one, and can be corrected — the
            correction clears as soon as the input changes. */}
        {summaryItems && (
          // A batch has no single kind to correct, so this reports the mix
          // rather than offering chips to click.
          <div className="detected-kind">
            <span className="detected-label">Batch</span>
            {batchSummary(summaryItems).map(({ kind, count }) => (
              <span key={kind} className="cap cap-selected">
                {count} {kindPlural(kind, count)}
              </span>
            ))}
          </div>
        )}
        {!summaryItems && (link.trim() !== '' || files.length > 0) && availableProtocols.length > 1 && (
          <div className="detected-kind">
            <span className="detected-label">
              {detected.certain || override ? 'Type' : 'Looks like'}
            </span>
            {availableProtocols.map((p) => (
              <button
                key={p}
                type="button"
                className={protocol === p ? 'cap cap-selected' : 'cap'}
                onClick={() => setOverride(p)}
                title={
                  protocol === p
                    ? `Adding as ${kindLabel(p)}`
                    : `Add as ${kindLabel(p)} instead`
                }
              >
                {kindLabel(p)}
              </button>
            ))}
          </div>
        )}
        {/* The decode is shown rather than done silently: rewriting what
            someone pasted, with no sign it happened and no way back, is a
            poor trade for the convenience. */}
        {decode.from !== null && (
          <p className="settings-help decoded-notice">
            {decode.items > 1
              ? `Found ${decode.items} links${decode.layers > 0 ? ', decoding base64 along the way' : ''}.`
              : `Decoded from base64 ${decode.layers > 1 ? `×${decode.layers}` : ''}.`}{' '}
            <button
              type="button"
              className="link-button"
              onClick={() => {
                const step = undoDecode(decode)
                setDecode(step.state)
                setLink(step.link)
              }}
            >
              use what I pasted instead
            </button>
          </p>
        )}
        {fileError && <p className="settings-error">{fileError}</p>}

        {/* Only shown when more than one provider can handle the selected
            protocol — otherwise there is nothing to choose between, and a
            picker with one option is just noise. */}
        {capableProviders.length > 1 && (
          <label className="add-provider">
            Provider
            <select value={provider} onChange={(e) => setProvider(e.target.value)}>
              <option value="">Default</option>
              {capableProviders.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
        )}

        {availableProtocols.length === 0 && (
          <p className="settings-help">No provider is configured yet — add one under Settings first.</p>
        )}

        {protocolAvailable && (
          <form onSubmit={handleSubmit}>
            {/* How the input is supplied, which is decided before there is
                anything to detect — an empty form looks like a web link, and
                gating this on that would hide file upload until something was
                typed. */}
            <div className="mode-toggle">
              <label>
                <input type="radio" checked={mode === 'link'} onChange={() => setMode('link')} />
                Link(s)
              </label>
              <label>
                <input type="radio" checked={mode === 'file'} onChange={() => setMode('file')} />
                File(s)
              </label>
              <label>
                <input
                  type="radio"
                  checked={mode === 'batchfile'}
                  onChange={() => setMode('batchfile')}
                />
                Batch file
              </label>
            </div>

            {mode === 'link' ? (
              // Always a textarea, never an <input> swapped out for one when a
              // batch appears: changing the element type remounts the node and
              // drops focus mid-typing. At one row it is exactly the size the
              // input was, and it only grows once there is a list to show.
              <textarea
                ref={linkInput}
                className="add-link-input"
                rows={1}
                placeholder="Magnet, .torrent/.nzb URL, or hoster link"
                value={link}
                onChange={(e) => setLink(e.target.value)}
                onPaste={() => {
                  pasted.current = true
                }}
                onKeyDown={handleKeyDown}
                autoFocus
              />
            ) : (
              <>
                {/* One element across both file modes, with only accept
                    changing, so switching modes does not remount it and drop
                    the selection — the same files are simply re-judged under
                    the new rules, which is what makes "switch to Batch file"
                    actionable rather than a re-do.

                    Batch file sets no accept: a file with no extension cannot
                    be expressed in an accept list, and that is precisely the
                    case it exists for. isListFilename enforces the limit
                    after selection instead. */}
                <input
                  type="file"
                  // Both modes take several. Note the picker's own button text
                  // is the browser's, derived from this attribute and worded
                  // differently per browser — changing it here is not an
                  // option, it needs a styled label over a hidden input.
                  multiple
                  accept={mode === 'file' ? '.torrent,.nzb' : undefined}
                  onChange={(e) => setFiles(Array.from(e.target.files ?? []))}
                />
                {mode === 'batchfile' && (
                  <p className="settings-help">
                    A .txt file, or one with no extension, listing links. Anything a paste
                    accepts works here too.
                  </p>
                )}
              </>
            )}

            {mode === 'link' && !isBatch && (previewLoading || cached !== null || torrentInfo) && (
              <div className="add-preview">
                {previewLoading && <p className="settings-help">Checking…</p>}
                {!previewLoading && cached !== null && (
                  <p className={cached ? 'settings-success' : 'settings-help'}>
                    {cached ? '✓ Cached — instantly available' : 'Not cached — will need a real download'}
                  </p>
                )}
                {!previewLoading && torrentInfo && (
                  <>
                    {torrentInfo.available ? (
                      <p className="settings-help" title={torrentInfo.name}>
                        {torrentInfo.name}
                        {typeof torrentInfo.size_bytes === 'number' && ` · ${formatBytes(torrentInfo.size_bytes)}`}
                        {torrentInfo.files && ` · ${torrentInfo.files.length} file${torrentInfo.files.length === 1 ? '' : 's'}`}
                        {typeof torrentInfo.seeds === 'number' &&
                          ` · ${torrentInfo.seeds} seed${torrentInfo.seeds === 1 ? '' : 's'}, ${torrentInfo.peers} peer${torrentInfo.peers === 1 ? '' : 's'}`}
                      </p>
                    ) : (
                      // Routine, not an error — TorBox couldn't find this
                      // torrent on the network yet (or the provider doesn't
                      // support previews at all). The check-cached badge
                      // above still shows either way.
                      <p className="settings-help">No preview available yet.</p>
                    )}
                  </>
                )}
              </div>
            )}

            {managed && (
              <input
                type="text"
                placeholder="Category (optional) — subfolder, e.g. iso"
                value={category}
                onChange={(e) => setCategory(e.target.value)}
              />
            )}

            <button
              type="submit"
              disabled={
                status.kind === 'saving' ||
                (mode === 'link' ? !link.trim() : fileBatch.length === 0)
              }
            >
              {status.kind === 'saving'
                ? progress && progress.total > 1
                  ? `Adding ${Math.min(progress.done + 1, progress.total)} of ${progress.total}…`
                  : 'Adding…'
                : summaryItems
                  ? `Add ${summaryItems.length}`
                  : 'Add'}
            </button>
            {fileNotice !== '' && <p className="settings-help">{fileNotice}</p>}
            {status.kind === 'error' && (
              <p className="settings-error">
                {batchErrors.length > 0 ? status.message : `Failed to add: ${status.message}`}
              </p>
            )}
            {batchErrors.length > 0 && (
              <ul className="batch-errors">
                {batchErrors.map((failure) => (
                  <li key={failure.link}>
                    <span className="batch-error-link" title={failure.link}>
                      {failure.link}
                    </span>
                    <span className="batch-error-message">{failure.message}</span>
                  </li>
                ))}
              </ul>
            )}
          </form>
        )}
      </div>
    </div>
  )
}
