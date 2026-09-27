# Demo video source canon (#145)

The console copy for `EpisodeRow`/`EpisodeList` (`episode-card.tsx`) has two
twins of every source-facing sentence: one for the default, unconfigured
install (episodes stream from the Wardyn release on GitHub) and one for a
deployment with `WARDYN_DEMO_VIDEO_BASE_URL` configured (an operator-run
mirror, typically air-gapped). Which twin renders is decided once per mount
by `useDemoVideoBaseUrl()` returning a value (configured) or `undefined`
(not configured) — never by the sentence naming the actual host or URL.

Strings live in `ui/src/app/components/wardyn/copy/episodes.ts`
(`EPISODES_COPY`). This table is the canon those strings must match verbatim;
a PR changing either side without the other is a regression.

| Key | Default (GitHub) | Configured (operator mirror) |
|---|---|---|
| Stream note | `Streams from the Wardyn release on GitHub only after you press Watch. Nothing is prefetched.` | `Streams from the video source your admin configured, only after you press Watch. Nothing is prefetched.` |
| Load error | `Couldn't load this episode from GitHub.` | `Couldn't play this episode. This deployment only allows video from the source your admin configured, and this didn't come from it.` |
| Catalog summary | `{n} recorded · about {mins} minutes · streamed from GitHub on click` | `{n} recorded · about {mins} minutes · streamed from your admin's video source on click` |

`NOT_RECORDED` ("Not recorded yet"), `WATCH` and `CLOSE` have no configured
twin — they don't name a source.

## Decisions

- **Q145-1 — the release-page link.** `OPEN_RELEASE_PAGE` ("Open the release
  page") renders ONLY beside the default-source load error. `github.com/.../releases/tag/<tag>`
  is GitHub's page; a configured deployment has no equivalent release page to
  send anyone to, so `LOAD_ERROR_CONFIGURED` never carries a link.
- **Q145-2 — never the host in text.** No configured-twin sentence
  interpolates the operator's mirror host or URL, even though the component
  has it in hand (`useDemoVideoBaseUrl()`). An internal or air-gapped mirror
  address is not something the console should ever render into the DOM or a
  screenshot; the copy says "your admin configured" instead of the value
  itself.
