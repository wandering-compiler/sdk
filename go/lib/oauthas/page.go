package oauthas

import (
	"crypto/rand"
	"encoding/base64"
	"html"
	"net/http"
)

// pageStyle is the look of the pages this server shows a person: the code
// page and the error page. It follows the console's dark theme (the same
// background, panel, line, text, muted and accent colours, radius 10), so a
// sign-in started from the console ends on a page that looks like it.
const pageStyle = `
:root{color-scheme:dark;--bg:#0f1115;--panel:#171a21;--line:#262b36;--text:#e6e9ef;--muted:#8b93a5;--accent:#4f8cff;--danger:#ff6b6b}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%;background:var(--bg);color:var(--text)}
body{font:15px/1.55 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;display:flex;justify-content:center;padding:64px 16px}
main{width:100%;max-width:34rem;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:28px;display:flex;flex-direction:column;gap:14px}
h1{margin:0;font-size:20px;font-weight:600;line-height:1.3}
p{margin:0}
.muted{color:var(--muted);font-size:13px}
.error{color:var(--danger);font-size:13px}
.code{font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;word-break:break-all;user-select:all;-webkit-user-select:all;background:var(--bg);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.row{display:flex;flex-wrap:wrap;align-items:center;gap:12px}
button{font:600 14px system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:#fff;background:var(--accent);border:0;border-radius:10px;padding:10px 18px;cursor:pointer}
button:hover{filter:brightness(1.08)}
button:focus-visible{outline:2px solid var(--text);outline-offset:2px}
#copied{color:var(--muted);font-size:13px}
@media (max-width:480px){body{padding:24px 16px}main{padding:20px}}
`

// copyScript copies the code to the clipboard; where the browser refuses
// (an insecure origin, a denied permission), it selects the code instead, so
// the person's own copy takes it.
const copyScript = `
(function(){
  var b=document.getElementById("copy"),c=document.getElementById("code"),s=document.getElementById("copied"),timer;
  if(!b||!c||!s)return;
  function select(){var r=document.createRange();r.selectNodeContents(c);var sel=window.getSelection();sel.removeAllRanges();sel.addRange(r)}
  // say clears the status first, so a second click is announced again, and
  // clears it a moment later, so "Copied" never describes an older click.
  function say(m){clearTimeout(timer);s.textContent="";setTimeout(function(){s.textContent=m},50);timer=setTimeout(function(){s.textContent=""},4000)}
  b.addEventListener("click",function(){
    var t=c.textContent;
    if(navigator.clipboard&&navigator.clipboard.writeText){
      navigator.clipboard.writeText(t).then(function(){say("Copied")},function(){select();say("Selected: press Ctrl+C or Cmd+C")});
    }else{select();say("Selected: press Ctrl+C or Cmd+C")}
  });
})();
`

// pageNonce is a fresh CSP nonce for one response: the page's own style and
// script are allowed by it, and nothing else is.
func pageNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

// writePage writes a page shown to a person. title is text and escaped here;
// body is HTML the caller built, with every value in it escaped. withCopy adds
// the script behind the code page's Copy button.
func writePage(w http.ResponseWriter, status int, title, body string, withCopy bool) {
	nonce := pageNonce()
	csp := "default-src 'none'; style-src 'nonce-" + nonce + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	script := ""
	if withCopy {
		csp = "default-src 'none'; style-src 'nonce-" + nonce + "'; script-src 'nonce-" + nonce + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
		script = `<script nonce="` + nonce + `">` + copyScript + `</script>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + html.EscapeString(title) + `</title>` +
		`<style nonce="` + nonce + `">` + pageStyle + `</style>` +
		`<body><main><h1>` + html.EscapeString(title) + `</h1>` + body + `</main>` + script + `</body></html>`))
}
