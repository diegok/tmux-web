package front

import (
	"embed"
	"io/fs"
)

// distEmbed is the built frontend: Vite writes internal/front/dist (see
// web/vite.config.ts) and this is what puts it inside the binary.
//
// The `all:` prefix is load-bearing, and so is the tracked dist/.gitkeep it
// picks up. dist itself is gitignored -- it is build output, and committing
// hashed asset bundles would put a second copy of the frontend in every diff --
// so on a fresh clone the directory would not exist at all, and `//go:embed
// dist` is a *compile* error when its pattern matches nothing. That would mean
// `go build ./...` and `go test ./...` failing on a checkout until someone
// installs pnpm and runs the frontend build, which is a miserable way to find
// out that the Go tests never needed the frontend in the first place.
//
// A tracked dist/.gitkeep fixes that, but only with `all:`: without the prefix
// go:embed skips names beginning with "." and reports "contains no embeddable
// files", which is the same compile error with a more confusing message.
//
// The cost is that a binary built from a clean clone contains no index.html.
// That is a runtime condition rather than a build failure -- see spaAvailable
// and the placeholder page in server.go -- so the daemon starts, serves the
// API, and says what is missing.
//
//go:embed all:dist
var distEmbed embed.FS

// DistFS is the built SPA, rooted so that "index.html" and "assets/..." are the
// paths a browser asks for.
//
// It is exported because the HTTP handler takes its file system as a parameter:
// the SPA fallback, the honest 404 under /assets/, and the not-built
// placeholder are all decisions about a directory of files, and a test that has
// to run the Vite build to reach them is a test nobody runs.
func DistFS() fs.FS {
	// The error case is unreachable: "dist" is a valid path and the embed above
	// guarantees the directory exists in the binary. Returning the outer FS
	// would only turn an impossible error into a confusing 404, so fall back to
	// an empty FS, which the placeholder page already handles.
	sub, err := fs.Sub(distEmbed, "dist")
	if err != nil {
		return emptyFS{}
	}
	return sub
}

// spaAvailable reports whether fsys holds a built SPA rather than just the
// placeholder .gitkeep.
func spaAvailable(fsys fs.FS) bool {
	if fsys == nil {
		return false
	}
	st, err := fs.Stat(fsys, "index.html")
	return err == nil && !st.IsDir()
}

// emptyFS is an fs.FS with nothing in it.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
