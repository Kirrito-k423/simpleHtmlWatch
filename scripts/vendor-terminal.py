"""Refresh pinned, offline terminal assets and attach CSP nonces to xterm styles."""
from pathlib import Path
import re
import shutil

root = Path(__file__).resolve().parents[1]
source = root / "node_modules/@xterm/xterm"
code = (source / "lib/xterm.js").read_text()
code, count = re.subn(r'([\w.$]+)\.createElement\("style"\)', r'globalThis.__shwTerminalStyle(\1)', code)
assert count == 4, "review xterm dynamic style creation before updating its pinned version"
code = re.sub(r'\n?//# sourceMappingURL=.*', '', code)
(root / "web/xterm.js").write_text(code)
for original, name in [(source / "css/xterm.css", "xterm.css"), (source / "LICENSE", "xterm-LICENSE.txt"),
                       (root / "node_modules/@xterm/addon-fit/lib/addon-fit.js", "xterm-addon-fit.js"),
                       (root / "node_modules/@xterm/addon-fit/LICENSE", "xterm-addon-fit-LICENSE.txt")]:
    shutil.copyfile(original, root / "web" / name)
