# @openrails/contentkit-urls

Browser helpers for ContentKit content URLs, `[/{lang}]/{route}/{CODE}[/{slug}]`.
The server reads only the code; the slug is decoration. Zero dependencies.
The Go side is the `contenturl` package. Both sides are tested against the
same vectors (`contenturl/testdata/vectors.json`).

Each ContentKit release (`v*`) attaches the package:

```sh
pnpm add https://github.com/open-rails/contentkit/releases/download/v0.67.0/openrails-contentkit-urls-0.67.0.tgz
```

```ts
import { createContentURLs, parseCode } from "@openrails/contentkit-urls";

const urls = createContentURLs({ routes: { video: "watch", series: "series" }, languages: ["en", "es"] });

// Links come from the API: { content_kind, code, slug, slugs? }.
urls.path(link);                                    // "/watch/G4VRQ3ZQ5/night-before"
urls.path(link, { language: "es", query: { t: 30 } }); // "/es/watch/G4VRQ3ZQ5/la-noche?t=30"

// Route param -> the code to send to the API.
parseCode("g4vr-q3zq5");                            // "G4VRQ3ZQ5"; null for anything else

// After the page's data arrives, keep the address bar canonical.
const c = urls.canonical(location.pathname + location.search, link);
if (c?.redirect) history.replaceState(history.state, "", c.location);
```

Slugs are computed on the server (`contenturl.Slugify`), never in the browser.
That way a link the browser builds always matches the server's redirect target.
