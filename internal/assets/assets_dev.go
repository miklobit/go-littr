// +build !prod,!qa

package assets

import (
	"bytes"
	"fmt"
	"github.com/go-chi/chi"
	"html/template"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
)

var walkFsFn = filepath.Walk
var openFsFn = os.Open

func writeAsset(s AssetFiles) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		asset := filepath.Clean(chi.URLParam(r, "path"))
		ext := path.Ext(r.RequestURI)
		mimeType := mime.TypeByExtension(ext)
		files, ok := s[asset]
		if !ok {
			w.Write([]byte("not found"))
			w.WriteHeader(http.StatusNotFound)
			return
		}

		buf := bytes.Buffer{}
		for _, file := range files {
			if piece, _ := getFileContent(assetPath(ext[1:], file)); len(piece) > 0 {
				buf.Write(piece)
			}
		}

		w.Header().Set("Cache-Control", fmt.Sprintf("public,max-age=%d", int(year.Seconds())))
		w.Header().Set("Content-Type", mimeType)
		w.Write(buf.Bytes())
	}
}

func assetLoad() func(string) template.HTML {
	return func(name string) template.HTML {
		b, _ := getFileContent(assetPath(name))
		return template.HTML(b)
	}
}
