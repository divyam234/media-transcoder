package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

var uiTemplates = template.Must(template.New("ui").Parse(`
{{define "head"}}
<!doctype html>
<html lang="en" class="bg-zinc-950 text-zinc-100">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
  <link rel="stylesheet" href="https://cdn.plyr.io/3.7.8/plyr.css">
  <style>
    :root { --plyr-video-background: #000; }
    .plyr, .plyr__video-wrapper { width: 100%; height: 100%; }
    .plyr__video-wrapper { aspect-ratio: 16 / 9; }
  </style>
</head>
<body class="min-h-screen bg-zinc-950 text-zinc-100">
<header class="border-b border-zinc-800 bg-zinc-950/95">
  <div class="mx-auto flex max-w-6xl items-center justify-between px-5 py-4">
    <a href="/" class="text-lg font-semibold tracking-tight">media-transcoder</a>
    <span class="text-sm text-zinc-500">Library browser</span>
  </div>
</header>
<main class="mx-auto max-w-6xl px-5 py-8">
{{end}}

{{define "foot"}}
</main>
</body>
</html>
{{end}}

{{define "index"}}
{{template "head" .}}
<div class="mb-6">
  <h1 class="text-2xl font-semibold">Libraries</h1>
  <p class="mt-1 text-sm text-zinc-500">Browse configured rclone VFS libraries.</p>
</div>
<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
{{range .Libraries}}
  <a href="{{.URL}}" class="rounded-lg border border-zinc-800 bg-zinc-900 p-4 hover:border-zinc-700">
    <div class="font-medium">{{.ID}}</div>
    <div class="mt-1 truncate text-xs text-zinc-500">{{.VFS}}</div>
  </a>
{{else}}
  <div class="rounded-lg border border-zinc-800 p-6 text-sm text-zinc-500">No libraries configured.</div>
{{end}}
</div>
{{template "foot" .}}
{{end}}

{{define "browse"}}
{{template "head" .}}
<div class="mb-5 flex flex-wrap items-end justify-between gap-4">
  <div>
    <a href="/" class="text-sm text-sky-400">Libraries</a>
    <h1 class="mt-2 text-xl font-semibold">{{.Library}}</h1>
    <p class="mt-1 break-all text-sm text-zinc-500">/{{.Path}}</p>
  </div>
  {{if .ParentURL}}<a href="{{.ParentURL}}" class="rounded-md border border-zinc-700 px-3 py-2 text-sm">Up</a>{{end}}
</div>
<div class="overflow-hidden rounded-lg border border-zinc-800">
  <table class="w-full text-left text-sm">
    <thead class="bg-zinc-900 text-zinc-400"><tr><th class="px-4 py-3 font-medium">Name</th><th class="px-4 py-3 text-right font-medium">Size</th></tr></thead>
    <tbody class="divide-y divide-zinc-800">
    {{range .Entries}}
      <tr class="bg-zinc-950 hover:bg-zinc-900">
        <td class="px-4 py-3">
          {{if .IsDir}}
            <a href="{{.URL}}" class="block font-medium text-sky-400">{{.Name}}/</a>
          {{else if .Playable}}
            <a href="{{.URL}}" class="block font-medium">{{.Name}}</a>
          {{else}}
            <span class="text-zinc-500">{{.Name}}</span>
          {{end}}
        </td>
        <td class="px-4 py-3 text-right text-zinc-500">{{.Size}}</td>
      </tr>
    {{else}}
      <tr><td colspan="2" class="px-4 py-8 text-center text-zinc-500">Empty directory</td></tr>
    {{end}}
    </tbody>
  </table>
</div>
{{template "foot" .}}
{{end}}

{{define "player"}}
{{template "head" .}}
<div class="mb-5 flex flex-wrap items-end justify-between gap-4">
  <div>
    <a href="{{.BackURL}}" class="text-sm text-sky-400">Back to folder</a>
    <h1 class="mt-2 break-all text-xl font-semibold">{{.Name}}</h1>
  </div>
  <form method="get" class="flex items-center gap-2">
    <label for="profile" class="text-sm text-zinc-400">Playback</label>
    <select id="profile" name="profile" onchange="this.form.submit()" class="rounded-md border border-zinc-700 bg-zinc-900 px-3 py-2 text-sm">
      {{range .Profiles}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
    </select>
  </form>
</div>
<div id="player-shell" class="relative aspect-video w-full overflow-hidden rounded-lg border border-zinc-800 bg-black">
  <div id="player-loading" class="absolute inset-0 z-10 grid place-items-center bg-black text-sm text-zinc-500">Loading media…</div>
  <video id="player" controls playsinline preload="metadata" class="h-full w-full object-contain"></video>
</div>
<div class="mt-3 break-all text-xs text-zinc-500">{{.Source}}</div>
<script src="https://cdn.plyr.io/3.7.8/plyr.js"></script>
<script src="https://cdn.jsdelivr.net/npm/hls.js@1"></script>
<script src="https://cdn.dashjs.org/latest/dash.all.min.js"></script>
<script>
  const source = {{.Source}};
  const mode = {{.Mode}};
  const video = document.getElementById('player');
  const loading = document.getElementById('player-loading');
  const ready = () => loading?.remove();
  const fail = (message) => {
    if (loading) {
      loading.textContent = message;
      loading.classList.remove('text-zinc-500');
      loading.classList.add('text-red-300');
    }
  };

  video.addEventListener('loadedmetadata', ready, { once: true });
  video.addEventListener('canplay', ready, { once: true });
  video.addEventListener('error', () => fail('Unable to load media.'));

  const basePlyr = { ratio: '16:9' };
  const qualityPlyr = (options, onChange) => new Plyr(video, {
    ...basePlyr,
    quality: {
      default: 0,
      options: [0, ...options],
      forced: true,
      onChange,
    },
    i18n: { qualityLabel: { 0: 'Auto' } },
  });

  if (mode === 'direct') {
    video.src = source;
    new Plyr(video, basePlyr);
  } else if (mode === 'dash') {
    if (!window.dashjs?.MediaPlayer) {
      fail('DASH player failed to load.');
    } else {
      const dash = dashjs.MediaPlayer().create();
      dash.initialize(video, source, false);
      dash.on(dashjs.MediaPlayer.events.STREAM_INITIALIZED, () => {
        ready();
        const qualities = dash.getBitrateInfoListFor('video');
        const heights = [...new Set(qualities.map((quality) => quality.height).filter(Boolean))].sort((a, b) => a - b);
        qualityPlyr(heights, (height) => {
          if (height === 0) {
            dash.updateSettings({ streaming: { abr: { autoSwitchBitrate: { video: true } } } });
            return;
          }
          const index = qualities.findIndex((quality) => quality.height === height);
          if (index >= 0) {
            dash.updateSettings({ streaming: { abr: { autoSwitchBitrate: { video: false } } } });
            dash.setRepresentationForTypeByIndex('video', index, true);
          }
        });
      });
      dash.on(dashjs.MediaPlayer.events.ERROR, () => fail('Unable to play DASH stream.'));
    }
  } else if (Hls.isSupported()) {
    const hls = new Hls();
    hls.loadSource(source);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, () => {
      ready();
      const heights = [...new Set(hls.levels.map((level) => level.height).filter(Boolean))].sort((a, b) => a - b);
      qualityPlyr(heights, (height) => {
        hls.currentLevel = height === 0 ? -1 : hls.levels.findIndex((level) => level.height === height);
      });
    });
    hls.on(Hls.Events.ERROR, (_event, data) => {
      if (data.fatal) fail('Unable to play HLS stream.');
    });
  } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = source;
    new Plyr(video, basePlyr);
  } else {
    fail('This browser cannot play HLS.');
  }
</script>
{{template "foot" .}}
{{end}}
`))

type uiLibrary struct {
	ID  string
	VFS string
	URL string
}

type uiEntry struct {
	Name     string
	Size     string
	IsDir    bool
	Playable bool
	URL      string
}

type uiProfile struct {
	Value    string
	Label    string
	Mode     string
	Selected bool
}

func (s *Server) uiIndex(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	ids := make([]string, 0, len(s.libraries))
	for id := range s.libraries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	libraries := make([]uiLibrary, 0, len(ids))
	for _, id := range ids {
		lib := s.libraries[id]
		libraries = append(libraries, uiLibrary{ID: id, VFS: lib.VFS, URL: "/ui/library/" + url.PathEscape(id) + "/"})
	}
	s.configMu.RUnlock()
	renderUI(w, "index", map[string]any{"Title": "Libraries", "Libraries": libraries})
}

func (s *Server) uiBrowse(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	libraryID := routeParam(r, "library")
	rel, err := cleanUIPath(chiWildcard(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, instance, err := s.uiLibraryVFS(ctx, libraryID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	instance.vfs.FlushDirCache()
	items, err := instance.vfs.ReadDir(rel)
	if err != nil {
		http.Error(w, fmt.Sprintf("read directory: %v", err), http.StatusBadRequest)
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir() != items[j].IsDir() {
			return items[i].IsDir()
		}
		return strings.ToLower(items[i].Name()) < strings.ToLower(items[j].Name())
	})
	entries := make([]uiEntry, 0, len(items))
	for _, item := range items {
		child := path.Join(rel, item.Name())
		entry := uiEntry{Name: item.Name(), IsDir: item.IsDir(), Size: humanSize(item.Size())}
		if item.IsDir() {
			entry.URL = uiBrowseURL(libraryID, child)
		} else if isMediaFile(item.Name()) {
			entry.Playable = true
			entry.URL = uiPlayerURL(libraryID, child)
		}
		entries = append(entries, entry)
	}
	parentURL := ""
	if rel != "" && rel != "." {
		parentURL = uiBrowseURL(libraryID, path.Dir(rel))
	}
	renderUI(w, "browse", map[string]any{
		"Title":     libraryID,
		"Library":   libraryID,
		"Path":      strings.TrimPrefix(rel, "."),
		"ParentURL": parentURL,
		"Entries":   entries,
	})
}

func (s *Server) uiPlayer(_ context.Context, w http.ResponseWriter, r *http.Request) {
	libraryID := routeParam(r, "library")
	rel, err := urlPathClean(chiWildcard(r))
	if err != nil || rel == "" || rel == "." {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}

	selected := strings.TrimSpace(r.URL.Query().Get("profile"))
	profiles := s.uiPlaybackProfiles(selected)
	if selected == "" {
		selected = "direct"
		profiles = s.uiPlaybackProfiles(selected)
	}

	mode := "direct"
	source := rawMediaURL(libraryID, rel)
	if selected != "direct" {
		profileID, container, ok := s.uiSelectedProfile(selected)
		if !ok {
			http.Error(w, "playback profile not found", http.StatusBadRequest)
			return
		}
		mode = container
		switch container {
		case "hls":
			source = "/play/hls/" + url.PathEscape(profileID) + "/" + url.PathEscape(libraryID) + "/" + escapePath(rel) + "/master.m3u8"
		case "dash":
			source = "/play/dash/" + url.PathEscape(profileID) + "/" + url.PathEscape(libraryID) + "/" + escapePath(rel) + "/manifest.mpd"
		default:
			http.Error(w, "unsupported playback profile", http.StatusBadRequest)
			return
		}
	}

	renderUI(w, "player", map[string]any{
		"Title":    path.Base(rel),
		"Name":     path.Base(rel),
		"BackURL":  uiBrowseURL(libraryID, path.Dir(rel)),
		"Source":   source,
		"Mode":     mode,
		"Profiles": profiles,
	})
}

func (s *Server) rawLibraryMedia(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	libraryID := routeParam(r, "library")
	rel, err := urlPathClean(chiWildcard(r))
	if err != nil || rel == "" || rel == "." {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	_, instance, err := s.uiLibraryVFS(ctx, libraryID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	handle, err := instance.vfs.Open(rel)
	if err != nil {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	defer handle.Close()
	defer handle.Release()
	info, err := handle.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), handle)
}

func (s *Server) uiLibraryVFS(ctx context.Context, libraryID string) (LibraryConfig, *libraryVFS, error) {
	s.configMu.RLock()
	lib, ok := s.libraries[libraryID]
	s.configMu.RUnlock()
	if !ok {
		return LibraryConfig{}, nil, errors.New("library not found")
	}
	instance, err := s.getLibraryVFS(ctx, libraryID, lib)
	return lib, instance, err
}

func renderUI(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uiTemplates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) uiPlaybackProfiles(selected string) []uiProfile {
	profiles := []uiProfile{{Value: "direct", Label: "Direct", Mode: "direct", Selected: selected == "direct"}}
	s.configMu.RLock()
	ids := make([]string, 0, len(s.profiles))
	for id, profile := range s.profiles {
		container := strings.ToLower(strings.TrimSpace(profile.Container))
		if container == "hls" || container == "dash" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		container := strings.ToLower(strings.TrimSpace(s.profiles[id].Container))
		value := container + ":" + id
		profiles = append(profiles, uiProfile{Value: value, Label: strings.ToUpper(container) + " · " + id, Mode: container, Selected: selected == value || selected == id})
	}
	s.configMu.RUnlock()
	return profiles
}

func (s *Server) uiSelectedProfile(value string) (id, container string, ok bool) {
	container, id, ok = strings.Cut(value, ":")
	if !ok {
		id = value
		container = ""
	}
	if id == "" {
		return "", "", false
	}
	s.configMu.RLock()
	profile, exists := s.profiles[id]
	s.configMu.RUnlock()
	if !exists {
		return "", "", false
	}
	actual := strings.ToLower(strings.TrimSpace(profile.Container))
	if container != "" && strings.ToLower(container) != actual {
		return "", "", false
	}
	if actual != "hls" && actual != "dash" {
		return "", "", false
	}
	return id, actual, true
}

func cleanUIPath(value string) (string, error) {
	value = strings.Trim(value, "/")
	if value == "" {
		return "", nil
	}
	parts := strings.Split(value, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." || strings.ContainsRune(part, '\x00') {
			return "", errors.New("invalid library path")
		}
		clean = append(clean, part)
	}
	return strings.Join(clean, "/"), nil
}

func chiWildcard(r *http.Request) string {
	return strings.TrimPrefix(routeParam(r, "*"), "/")
}

func uiBrowseURL(libraryID, rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	base := "/ui/library/" + url.PathEscape(libraryID) + "/"
	if rel == "" || rel == "." {
		return base
	}
	return base + escapePath(rel) + "/"
}

func uiPlayerURL(libraryID, rel string) string {
	return "/ui/player/" + url.PathEscape(libraryID) + "/" + escapePath(rel)
}

func rawMediaURL(libraryID, rel string) string {
	return "/media/" + url.PathEscape(libraryID) + "/" + escapePath(rel)
}

func escapePath(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func isMediaFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".m4v", ".mkv", ".webm", ".mov", ".avi", ".ts", ".m2ts", ".flv", ".wmv", ".mp3", ".m4a", ".aac", ".flac", ".ogg", ".opus", ".wav":
		return true
	default:
		return false
	}
}

func isDirectBrowserMedia(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".m4v", ".webm", ".mp3", ".m4a", ".ogg", ".opus", ".wav":
		return true
	default:
		return false
	}
}

func humanSize(size int64) string {
	if size < 0 {
		return ""
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := float64(size)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}
