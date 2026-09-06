# SimpleAmazon (fork)

A privacy-friendly frontend for Amazon. Search and browse full product pages
without JavaScript and without your browser ever talking to Amazon.

This is a fork of [SimpleWeb/SimpleAmazon](https://codeberg.org/SimpleWeb/SimpleAmazon),
which was archived in 2023. Upstream was a search engine only: results linked
straight to Amazon, so the whole point of not exposing yourself was defeated the
moment you clicked one. This fork implements the product page.

## What it does

- **Search** across any of the 26 supported Amazon marketplaces (`?tld=es`,
  `?tld=co.uk`, ...), with sorting, pagination, ratings and multi-format prices.
- **Product pages** at `/dp/{asin}`: title, summary line, image gallery,
  two-column specifications, "About this item", long description, customer
  reviews and a variant picker.
- **Image proxy** at `/mediaproxy/{host}/{path}`. Nothing on the page is loaded
  from Amazon by your browser.
- **Works without JavaScript.** The gallery is a scrollable strip, the review
  sorter is plain links and the variant picker is plain links. Two small scripts
  (both AGPL-marked for LibreJS) improve the experience if scripting is on:
  arrows for the carousel and in-place review reordering.

## Running it

### Docker (recommended)

```bash
cp docker-compose.example.yml docker-compose.yml
# edit it: attach the container to your reverse proxy network
docker compose up -d --build
```

The image is a `scratch` container with a single static binary; templates and
static files are embedded with `go:embed`. It listens on `0.0.0.0:8080` and
publishes no ports by default, so a reverse proxy reaches it by container name.

### From source

```bash
go build -o simpleamazon .
./simpleamazon
```

### Flags and environment

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-h` | `SIMPLEAMAZON_HOST` | `0.0.0.0` | Listen address |
| `-p` | `SIMPLEAMAZON_PORT` | `8080` | Listen port |
| `-tls` | | `go` | Outgoing TLS fingerprint: `go`, `navegador` or `curl` |
| `-huella` | | off | Print the client's TLS/HTTP2 fingerprint and exit |
| `-volcado` | | off | Log outgoing requests and their responses |

## What this fork adds

Everything below is new here; none of it existed upstream.

- Product pages, and result links rewritten to point at them instead of Amazon.
- Image gallery, extracted from the `colorImages` JSON Amazon embeds in a script.
- Specifications, split into two columns with an equal number of pairs.
- Long description, gathered from whichever of three containers the product
  happens to use.
- Customer reviews with avatars, photos, verified-purchase badges and sorting.
- Variant picker for colour, size and format, resolved from the twister JSON.
- Image proxy, in-memory caching and anti-bot detection.
- Configuration by environment variables, own favicon, Spanish UI strings.

## What this fork fixes

- Result links that produced URLs like `https://amazon.eshttps://www.amazon.es/...`
  when Amazon returned an absolute URL.
- Review counts always reading `0` in locales using `.` as a thousands separator.
- Pagination never rendering, because `ChildrenFiltered` did not match the items.
- Assorted goquery selectors that had drifted from Amazon's current HTML.

## Known limitations

- **Amazon blocks requests in bursts.** It comes and goes in windows of minutes
  and affects every client equally, including a real browser under the same
  request rate. Fifteen hypotheses were ruled out empirically; see the open
  issue. When it happens the app answers `503` with a clear message rather than
  a broken page.
- **Only the thirteen reviews Amazon embeds in the page** are shown. The
  `/product-reviews` endpoint redirects to a login page without cookies.
- **No sorting reviews by date.** Amazon publishes the date only as localised
  text, so this would need a table of month names for all 26 marketplaces.
- **Review videos are not shown yet.** They live in a separate widget that mixes
  customer, influencer and seller content, and the only full-quality source is
  an HLS stream that would need a JavaScript player and a much heavier proxy.

## Development notes

- Probe Amazon's raw HTML with real data before writing any code. The rendered
  DOM in your browser's dev tools is not what the parser sees.
- Any measurement against Amazon is only meaningful if the samples are paired or
  interleaved. The blocking is time-based, so a sequential comparison proves
  nothing.

## License

AGPL-3.0-or-later, same as upstream.
