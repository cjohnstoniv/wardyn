#!/bin/sh
# Grader for build-static-site.
#
# Runs in a FRESH container over the FINAL workspace state (never a transcript):
#   docker run --rm -v <run-workspace>:/ws:ro -v <task-dir>:/task:ro \
#     python:3.12-alpine sh /task/grade.sh
#
# Why python and not grep: the checks REALLY parse the HTML with html.parser
# (structure, not substring), so a flat blob of text that merely contains the
# right words cannot pass. And it strips CSS /* comments */ BEFORE matching, so a
# required marker hidden inside a comment cannot satisfy the stylesheet checks —
# that exact hole was called out in review.
#
# Prints PASS/FAIL <reason> lines and exits 0 (pass) / 1 (fail).
set -u

python3 - <<'PY'
import os, re, sys
from html.parser import HTMLParser

WS = "/ws"
fails = []
def check(cond, ok_msg, fail_msg):
    if cond:
        print("PASS " + ok_msg)
    else:
        print("FAIL " + fail_msg)
        fails.append(fail_msg)

idx = os.path.join(WS, "index.html")
css = os.path.join(WS, "style.css")

# ── files exist and are non-trivial ──────────────────────────────────────────
if not os.path.isfile(idx):
    print("FAIL index.html missing"); sys.exit(1)
if not os.path.isfile(css):
    print("FAIL style.css missing"); sys.exit(1)

html_bytes = open(idx, "rb").read()
check(len(html_bytes) > 500,
      "index.html non-trivial (%d bytes)" % len(html_bytes),
      "index.html too small (%d bytes, need >500)" % len(html_bytes))
html = html_bytes.decode("utf-8", "replace")
css_text = open(css, "r", encoding="utf-8", errors="replace").read()

# ── real HTML parse ──────────────────────────────────────────────────────────
VOID = {"area","base","br","col","embed","hr","img","input","link","meta",
        "param","source","track","wbr"}

class P(HTMLParser):
    def __init__(self):
        super().__init__()
        self.stack = []          # open non-void elements: (tag, id)
        self.nav_links = 0
        self.menu_items = 0
        self.saw_menu = False
        self.has_css_link = False
        self.h1_text = ""
    def _inside(self, pred):
        return any(pred(t, i) for (t, i) in self.stack)
    def _open(self, tag, attrs, void):
        a = dict(attrs)
        inside_nav = self._inside(lambda t, i: t == "nav")
        inside_menu = self._inside(lambda t, i: i == "menu")
        if tag == "link":
            rel = (a.get("rel") or "").lower().split()
            href = (a.get("href") or "").strip().lower()
            if "stylesheet" in rel and href == "style.css":
                self.has_css_link = True
        if inside_nav and tag == "a":
            self.nav_links += 1
        if inside_menu and ("menu-item" in (a.get("class") or "").split() or tag == "li"):
            self.menu_items += 1
        if a.get("id") == "menu":
            self.saw_menu = True
        if not void:
            self.stack.append((tag, a.get("id")))
    def handle_starttag(self, tag, attrs): self._open(tag, attrs, tag in VOID)
    def handle_startendtag(self, tag, attrs): self._open(tag, attrs, True)
    def handle_endtag(self, tag):
        for k in range(len(self.stack) - 1, -1, -1):
            if self.stack[k][0] == tag:
                del self.stack[k:]
                break
    def handle_data(self, data):
        if self._inside(lambda t, i: t == "h1"):
            self.h1_text += data

p = P()
p.feed(html)

check("larkspur coffee roasters" in p.h1_text.lower(),
      "<h1> contains 'Larkspur Coffee Roasters'",
      "<h1> text is %r (must contain 'Larkspur Coffee Roasters')" % p.h1_text.strip())
check(p.nav_links >= 3,
      "<nav> has %d links (>=3)" % p.nav_links,
      "<nav> has %d links (need >=3)" % p.nav_links)
check(p.saw_menu,
      "section id='menu' present",
      "no element with id='menu'")
check(p.menu_items >= 3,
      "#menu lists %d items (>=3)" % p.menu_items,
      "#menu has %d list/.menu-item children (need >=3)" % p.menu_items)
check(p.has_css_link,
      "<link rel=stylesheet href=style.css> present",
      "missing <link rel='stylesheet' href='style.css'>")

# ── CSS checks (strip comments first) ────────────────────────────────────────
css_nc = re.sub(r"/\*.*?\*/", "", css_text, flags=re.S)
check(re.search(r"body[^{]*\{[^}]*font-family", css_nc, re.I | re.S) is not None,
      "style.css sets a body font-family",
      "style.css has no body{...font-family...} rule")
check(re.search(r"\.menu-item[^{]*\{", css_nc) is not None,
      "style.css defines a .menu-item rule",
      "style.css has no .menu-item{...} rule")

if fails:
    print("FAIL build-static-site: %d check(s) failed" % len(fails))
    sys.exit(1)
print("PASS build-static-site")
sys.exit(0)
PY
