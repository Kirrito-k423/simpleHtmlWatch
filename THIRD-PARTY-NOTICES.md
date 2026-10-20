# Third-party notices

The offline SSH terminal embeds xterm.js 6.0.0 and @xterm/addon-fit 0.11.0 under the MIT license. Their full licenses are included in the embedded web assets (`xterm-LICENSE.txt` and `xterm-addon-fit-LICENSE.txt`). The four dynamically created xterm stylesheet elements are adapted to attach the page Content Security Policy nonce; `scripts/vendor-terminal.py` reproduces this adaptation from pinned npm packages. No terminal assets are fetched from a CDN at runtime.

Sources: [xterm.js](https://github.com/xtermjs/xterm.js), [coder/websocket](https://github.com/coder/websocket).

## github.com/coder/websocket v1.8.15

Copyright (c) 2025 Coder

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
