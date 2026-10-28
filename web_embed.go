package main

import (
	"embed"
	"io/fs"
)

// webAdminFiles is the headless Web admin's own files (phase6-headless.md
// §5.1): the shim and the login page. They are not in frontend/dist, so the
// GUI build's output stays as it was. Tests (*.test.mjs) are not embedded.
//
//go:embed webadmin/headless-shim.js webadmin/login.html webadmin/login.js webadmin/login.css
var webAdminFiles embed.FS

// webAdminFS is webAdminFiles with the webadmin/ prefix removed.
func webAdminFS() fs.FS {
	sub, err := fs.Sub(webAdminFiles, "webadmin")
	if err != nil {
		panic(err)
	}
	return sub
}
