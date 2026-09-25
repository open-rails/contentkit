package media

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

func (r *Reader) hlsRoutes(mux *http.ServeMux, o HandlerOptions, log *slog.Logger) {
	serve := func(pattern, contentType string, body func(*http.Request, *Grant) ([]byte, error)) {
		mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Cache-Control", "private, no-store")
			ref, actor := requestRef(req, o)
			g, err := r.Grant(req.Context(), ref, actor)
			var b []byte
			if err == nil {
				b, err = body(req, g)
			}
			if err != nil {
				status, code, msg := classify(err)
				if status >= http.StatusInternalServerError {
					log.Error("media hls failed", "path", req.URL.Path, "err", err.Error())
				}
				writeJSON(w, status, map[string]string{"error": msg, "code": code})
				return
			}
			if c := g.Cookie(); c != nil {
				http.SetCookie(w, c)
			}
			g.logIssued(req, log)
			if contentType == "" {
				http.Redirect(w, req, string(b), http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", contentType)
			w.Write(b)
		})
	}
	const hls = "/{kind}/{id}/hls/{file}/"
	serve(hls+"master.m3u8", HLSContentType, func(req *http.Request, g *Grant) ([]byte, error) {
		q := req.URL.Query()
		return g.MasterPlaylist(req.PathValue("file"), MasterOptions{Audio: queryList(q, "audio"), Subs: queryList(q, "subs")})
	})
	serve(hls+"video/{track}", HLSContentType, func(req *http.Request, g *Grant) ([]byte, error) {
		rung, codec, _ := strings.Cut(playlist(req), "-")
		n, err := strconv.Atoi(rung)
		if err != nil {
			return nil, ErrNotAllowed
		}
		return g.VideoPlaylist(req.PathValue("file"), n, Codec(codec))
	})
	serve(hls+"audio/{track}", HLSContentType, func(req *http.Request, g *Grant) ([]byte, error) {
		return g.AudioPlaylist(req.PathValue("file"), playlist(req))
	})
	serve(hls+"subs/{track}", HLSContentType, func(req *http.Request, g *Grant) ([]byte, error) {
		return g.SubtitlePlaylist(req.PathValue("file"), playlist(req))
	})
	serve(hls+"sprite.vtt", VTTContentType, func(req *http.Request, g *Grant) ([]byte, error) {
		return g.SpriteVTT(req.PathValue("file"))
	})
	serve("/{kind}/{id}/download/{key}", "", func(req *http.Request, g *Grant) ([]byte, error) {
		_, u, err := g.DownloadURL(req.Context(), req.PathValue("key"))
		return []byte(u), err
	})
}

// playlist is the {track} of ".../{track}.m3u8", or "" for another suffix.
func playlist(req *http.Request) string {
	t, ok := strings.CutSuffix(req.PathValue("track"), ".m3u8")
	if !ok {
		return ""
	}
	return t
}

// queryList is a comma-separated query filter; nil when the parameter is absent.
func queryList(q map[string][]string, name string) []string {
	vs, ok := q[name]
	if !ok {
		return nil
	}
	out := []string{}
	for _, v := range vs {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
